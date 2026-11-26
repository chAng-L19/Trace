package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// Adapted classification/bounded recovery contracts: pi e65b3b88 (MIT),
// ai/src/utils/retry.ts and coding-agent/src/core/agent-session.ts; Muteki
// c60c8fee reason.py overflow/complete-summary checks only, no CTF semantics.
// The existing Trace loop owns all attempts, persistence and cancellable waits.
// Recovery consumption also follows pi 5a14e69c067d40dfbff4f2fff7d3b34c897e2dc9
// (MIT), packages/coding-agent/src/core/agent-session.ts _checkCompaction:
// Trace persists the source/configuration identity instead of an in-memory flag.
type providerFailure string

const (
	providerPermanent providerFailure = "permanent"
	providerCancelled providerFailure = "cancelled"
	providerTransient providerFailure = "transient"
	providerOverflow  providerFailure = "context_overflow"
	providerQuota     providerFailure = "quota_exhausted"
)

// Code can be null, a string or a numeric provider code. Retain it without
// making successful error decoding depend on a provider's code representation.
type ProviderAPIError struct {
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
	Code    any    `json:"code,omitempty"`
}

func (e *ProviderAPIError) Error() string {
	return fmt.Sprintf("provider error: %s (type=%s code=%v)", e.Message, e.Type, e.Code)
}

type providerIncompleteError struct{ detail string }

func (e *providerIncompleteError) Error() string { return e.detail }

// A Store/event callback failure is not a provider transport fault, even if
// its wrapped cause is EOF or it occurs alongside a retryable HTTP error.
type providerStreamSinkError struct{ cause error }

func (e *providerStreamSinkError) Error() string { return e.cause.Error() }
func (e *providerStreamSinkError) Unwrap() error { return e.cause }

func providerErrorResponse(err error) ModelResponse {
	var failure *ProviderHTTPError
	if errors.As(err, &failure) {
		response := failure.Response
		response.Usage = failure.Usage
		return response
	}
	return ModelResponse{}
}

func containsProviderText(text string, values ...string) bool {
	for _, value := range values {
		if strings.Contains(text, value) {
			return true
		}
	}
	return false
}

func classifyProviderFailure(err error) providerFailure {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return providerCancelled
	}
	var sink *providerStreamSinkError
	if errors.As(err, &sink) {
		return providerPermanent
	}
	var httpErr *ProviderHTTPError
	var apiErr *ProviderAPIError
	text := ""
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode == http.StatusUnauthorized || httpErr.StatusCode == http.StatusForbidden {
			return providerPermanent
		}
		text = strings.ToLower(httpErr.Body)
	} else if errors.As(err, &apiErr) {
		text = strings.ToLower(apiErr.Error())
	}
	if containsProviderText(text, "invalid_api_key", "authentication_error", "permission_denied", "invalid_authentication") {
		return providerPermanent
	}
	// Only provider-owned error payloads participate in text classification.
	// Ordinary errors/mocks do not become retryable because of incidental prose.
	if containsProviderText(text, "insufficient_quota", "quota exceeded", "quota_exceeded", "billing",
		"insufficient balance", "out of budget", "credit balance is too low", "monthly usage limit reached",
		"gousagelimiterror", "freeusagelimiterror",
		"subscription_sharing_usage_limit_exceeded", "subscription_sharing_workplace_individual_usage_limit_exceeded") {
		return providerQuota
	}
	if httpErr != nil && httpErr.StatusCode == http.StatusPaymentRequired {
		return providerQuota
	}
	if containsProviderText(text, "context_length_exceeded", "context_window_exceeded", "prompt is too long",
		"maximum context length", "exceeds the context window", "exceeds model context", "too many input tokens") {
		return providerOverflow
	}
	if containsProviderText(text, "invalid_request_error", "invalid_parameter", "unsupported_parameter", "invalid_tool_schema") {
		return providerPermanent
	}
	if httpErr != nil {
		switch httpErr.StatusCode {
		case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError,
			http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return providerTransient
		default:
			return providerPermanent
		}
	}
	if apiErr != nil {
		// Do not blanket-retry every provider error or invalid request.
		if containsProviderText(text, "rate_limit_exceeded", "server_busy", "overloaded_error", "upstream_stream_read_error") {
			return providerTransient
		}
		return providerPermanent
	}
	var incomplete *providerIncompleteError
	if errors.As(err, &incomplete) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return providerTransient
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return providerTransient
	}
	return providerPermanent
}

func providerRecoveryReason(response ModelResponse, err error) string {
	if err == nil && response.StopReason == "length" {
		return "length"
	}
	if classifyProviderFailure(err) == providerOverflow {
		return string(providerOverflow)
	}
	return ""
}

// Admit compaction in the same commit as the failed provider attempt. Like pi's
// AgentSession overflow compaction, this continues the turn, not a failed Run.
func (a *Agent) prepareProviderRecoveryLocked(s *Snapshot, failed ModelRequest, reason string) bool {
	fail := func(detail string) {
		s.Run.Status, s.Run.Error = StatusFailed, "context recovery: "+detail
		addEvent(s, "context_recovery_failed", "上下文恢复已停止，原始响应与历史保留", map[string]string{"request_id": failed.ID, "reason": reason, "error": detail})
	}
	for _, event := range s.Events {
		if event.Type == "context_recovery_started" && recoveryEvent(event).RequestID != "" && recoveryOriginMatches(s, recoveryEvent(event)) {
			fail("one compact-and-retry attempt already used for this history")
			return false
		}
	}
	plan, planErr := providerRecoverySummary(s, failed)
	if planErr != nil {
		fail(planErr.Error())
		return false
	}
	if budget := budgetReason(s, "model"); budget != "" {
		pauseBudget(s, budget)
		return true
	}
	now := time.Now().UTC()
	request := ModelRequest{ID: fmt.Sprintf("%s-%04d", s.Run.ID, s.Run.RequestSeq+1), PromptHash: s.Run.PromptHash, Purpose: "context_summary",
		Messages: plan.messages, MaxOutputTokens: s.Run.Context.OutputReserve, ContextTokens: EstimateContextTokens(plan.messages, nil), CreatedAt: now}
	plan.record.ID, plan.record.RequestID = "compact-"+request.ID, request.ID
	request.CompactionID = plan.record.ID
	request.ContextManifest = buildContextManifest(s, request, a.registry.Definitions())
	a.stampRequest(&request)
	if err := a.admitTreeRequestLocked(s, request); err != nil {
		var limit *TreeBudgetError
		if errors.As(err, &limit) {
			pauseBudget(s, err.Error())
			return true
		}
		fail(err.Error())
		return false
	}
	s.Run.RequestSeq++
	s.Compactions = append(s.Compactions, plan.record)
	s.Requests = append(s.Requests, request)
	s.Run.Status, s.Run.Error = StatusRunning, ""
	plan.recovery.RequestID = failed.ID
	plan.recordRecovery(s, request)
	addEvent(s, "model_request", "已提交模型请求", map[string]any{"request_id": request.ID, "prompt_hash": request.PromptHash, "purpose": request.Purpose})
	return false
}

// Execute only the summary already admitted for this failed request. In
// particular, reopening a permanently failed Run does not re-admit recovery.
func (a *Agent) recoverProviderContext(ctx context.Context, id string, failed ModelRequest, response ModelResponse, callErr error) (Snapshot, bool, error) {
	snap, err := a.store.Load(id)
	if err != nil || snap.Run.Status != StatusRunning || failed.Purpose != "" || providerRecoveryReason(response, callErr) == "" {
		return snap, false, err
	}
	if ctx.Err() != nil {
		return snap, false, context.Cause(ctx)
	}
	var summaryID string
	for i := len(snap.Events) - 1; i >= 0; i-- {
		if snap.Events[i].Type == "context_recovery_started" {
			origin := recoveryEvent(snap.Events[i])
			if origin.RequestID == failed.ID {
				summaryID = origin.SummaryRequestID
			}
			break
		}
	}
	var request ModelRequest
	for i := len(snap.Requests) - 1; i >= 0; i-- {
		if snap.Requests[i].ID == summaryID && snap.Requests[i].Purpose == "context_summary" {
			request = snap.Requests[i]
			break
		}
	}
	if request.ID == "" || len(snap.Compactions) == 0 {
		return snap, false, fmt.Errorf("%w: missing admitted summary request", errProviderRecoveryLedger)
	}
	if c := snap.Compactions[len(snap.Compactions)-1]; c.RequestID != request.ID || c.ID != request.CompactionID || c.Status != "pending" {
		return snap, false, fmt.Errorf("%w: summary is not pending for this request", errProviderRecoveryLedger)
	}
	// Only transient transport failures return to the existing cancellable retry
	// path. Deterministic failures commit their terminal state before returning.
	answer, summaryErr := a.complete(ctx, id, request)
	retries := 0
	var decision piResponseDisposition
	snap, decision, err = a.settlePiResponse(ctx, id, request, answer, summaryErr, &retries)
	delay := decision.retryDelay
	if err != nil || isStopped(snap.Run.Status) || snap.Run.Status == StatusFailed {
		if err == nil && isStopped(snap.Run.Status) && decision.budgetPaused {
			return snap, false, nil
		}
		if snap.Run.Status == StatusFailed {
			err = errors.Join(err, errors.New(snap.Run.Error))
		}
		return snap, false, errors.Join(summaryErr, err)
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}
	if ctx.Err() != nil {
		return snap, false, context.Cause(ctx)
	}
	return snap, true, nil
}

type providerRecoveryEvent struct {
	RequestID         string `json:"request_id"`
	SummaryRequestID  string `json:"summary_request_id"`
	SourceHash        string `json:"source_hash"`
	ConfigurationHash string `json:"configuration_hash,omitempty"`
}

// Only settings that affect the effective request unlock a closed origin.
// Persisted model selection excludes credentials and request/event counters.
func recoveryConfigurationHash(s *Snapshot) string {
	c, err := normalizeContext(s.Run.Context)
	if err != nil {
		c = s.Run.Context
	}
	var model, provider, effort string
	if selected := s.Run.ModelSelection; selected != nil {
		model, provider, effort = selected.Model, selected.ProviderID, selected.ReasoningEffort
	}
	return valueHash(struct {
		Prompt                                string
		Mode                                  string
		InputLimit, OutputReserve, KeepRecent int
		Model, Provider, Effort               string
	}{s.Prompt, c.CompactionMode, contextInputLimit(c), c.OutputReserve, contextRecentBudget(c), model, provider, effort})
}

func recoveryOriginMatches(s *Snapshot, origin providerRecoveryEvent) bool {
	return origin.SourceHash == messageHash(s.Messages) && (origin.ConfigurationHash == "" || origin.ConfigurationHash == recoveryConfigurationHash(s))
}

// Recovery provenance failures are terminal admission errors, not evidence
// that the configured context window or an explicit run budget was exhausted.
var errProviderRecoveryLedger = errors.New("context recovery ledger invalid")

func recoveryEvent(event Event) providerRecoveryEvent {
	var data providerRecoveryEvent
	raw, _ := json.Marshal(event.Data)
	_ = json.Unmarshal(raw, &data)
	return data
}

// Called only after the normal pi request has passed budget admission. Binding
// the new ID here keeps retry lineage atomic with the request and compaction.
func (plan *summaryNeeded) recordRecovery(s *Snapshot, request ModelRequest) {
	if plan.recovery == nil {
		return
	}
	origin := *plan.recovery
	origin.SummaryRequestID = request.ID
	addEvent(s, "context_recovery_started", "按固定历史与有效配置登记摘要来源", origin)
}

func providerRecoverySummary(s *Snapshot, failed ModelRequest) (*summaryNeeded, error) {
	if s.Run.Context.CompactionMode != "semantic" {
		return nil, errors.New("automatic recovery requires semantic compaction; reduce input or change context settings")
	}
	if len(failed.Messages) == 0 || failed.Messages[0].Role != "system" || !strings.HasPrefix(failed.Messages[0].Content, s.Prompt) {
		return nil, fmt.Errorf("%w: failed request has no matching system projection", errProviderRecoveryLedger)
	}
	catalog := strings.TrimSuffix(strings.TrimPrefix(failed.Messages[0].Content, s.Prompt), runtimeDataHint)
	for _, message := range failed.Messages {
		if message.ID == s.Run.ID+"-worker-context" {
			s.workerContextMessage = message.Content
		}
	}
	_, _, _, err := projectSemanticContextMode(s, failed.Tools, catalog, true, requestRuntimeObservations(s.Run.ID, failed))
	var plan *summaryNeeded
	if !errors.As(err, &plan) {
		return nil, fmt.Errorf("no complete history prefix can be compacted: %v", err)
	}
	return plan, nil
}

// Both admission paths consume the same durable origin. Failed summaries only
// re-enter for an archived transient provider error, never for rejected content.
func providerRecoveryProjectionError(s *Snapshot) error {
	for i := len(s.Events) - 1; i >= 0; i-- {
		event := s.Events[i]
		if event.Type != "context_recovery_started" {
			continue
		}
		origin := recoveryEvent(event)
		var compaction *Compaction
		for j := range s.Compactions {
			if s.Compactions[j].RequestID == origin.SummaryRequestID {
				compaction = &s.Compactions[j]
				break
			}
		}
		ready := origin.RequestID == ""
		for _, receipt := range s.Events[i+1:] {
			ready = ready || receipt.Type == "context_recovery_ready" && recoveryEvent(receipt).SummaryRequestID == origin.SummaryRequestID
		}
		if compaction != nil && compaction.Status == "completed" && !ready {
			// Legacy completed-but-unchecked recovery summaries remain unusable.
			compaction.Status = "failed"
		}
		if !recoveryOriginMatches(s, origin) {
			continue
		}
		if origin.RequestID == "" && compaction != nil && compaction.Status == "interrupted" {
			return nil // Replan an interrupted proactive summary; its request still consumes budget.
		}
		if compaction != nil && compaction.Status == "completed" && ready {
			return nil
		}
		if compaction != nil && compaction.Status == "failed" {
			for _, response := range s.Responses {
				if response.RequestID != origin.SummaryRequestID || response.Error == "" || response.Response.Diagnostics == nil || response.Response.Diagnostics.FailureReason != string(providerTransient) {
					continue
				}
				var plan *summaryNeeded
				var err error
				if origin.RequestID == "" {
					// Normal semantic compaction is replanned by its existing projector.
					return nil
				}
				for _, request := range s.Requests {
					if request.ID == origin.RequestID {
						plan, err = providerRecoverySummary(s, request)
						break
					}
				}
				if err != nil {
					return err
				}
				if plan != nil {
					plan.recovery = &origin
					return plan
				}
			}
		}
		return fmt.Errorf("%w: context recovery did not commit a smaller complete summary; original requests and history are retained", errProviderRecoveryLedger)
	}
	return nil
}

// Called by finishCompaction within the response settlement transaction, after
// provisionally completing the candidate. Failure revokes it before any save.
func finishProviderRecoveryCompaction(s *Snapshot, summary ModelRequest) error {
	var origin providerRecoveryEvent
	for _, event := range s.Events {
		if event.Type == "context_recovery_started" && recoveryEvent(event).SummaryRequestID == summary.ID {
			origin = recoveryEvent(event)
			break
		}
	}
	if origin.SummaryRequestID == "" {
		return nil
	}
	var failed *ModelRequest
	for i := range s.Requests {
		if s.Requests[i].ID == origin.RequestID {
			failed = &s.Requests[i]
			break
		}
	}
	sourceMatches := recoveryOriginMatches(s, origin)
	if origin.RequestID == "" && sourceMatches {
		return nil // The normal projector checks full admission before its next request.
	}
	reduced := false
	sourceMatches = sourceMatches && failed != nil && len(failed.Messages) > 0 && failed.Messages[0].Role == "system" && strings.HasPrefix(failed.Messages[0].Content, s.Prompt)
	if sourceMatches {
		catalog := strings.TrimSuffix(strings.TrimPrefix(failed.Messages[0].Content, s.Prompt), runtimeDataHint)
		for _, message := range failed.Messages {
			if message.ID == s.Run.ID+"-worker-context" {
				s.workerContextMessage = message.Content
			}
		}
		projected, _, compactionID, err := projectSemanticContextMode(s, failed.Tools, catalog, false, requestRuntimeObservations(s.Run.ID, *failed))
		failedRequired, _ := splitWorkerMethodMessages(s.Run.ID, failed.Messages)
		reduced = err == nil && compactionID == summary.CompactionID && EstimateContextTokens(projected, failed.Tools) < EstimateContextTokens(failedRequired, failed.Tools)
	}
	if !reduced {
		err := errors.New("context recovery: complete summary is not smaller and admissible for the same source/configuration; reduce input or change context settings")
		if !isStopped(s.Run.Status) {
			s.Run.Status, s.Run.PauseReason, s.Run.Error = StatusFailed, "", err.Error()
		}
		addEvent(s, "context_recovery_failed", "恢复摘要未通过完整上下文与来源校验，保留原始历史", map[string]string{"request_id": origin.RequestID, "summary_request_id": summary.ID, "error": err.Error()})
		return err
	}
	addEvent(s, "context_recovery_ready", "压缩已减少上下文，重新请求模型", map[string]string{"request_id": origin.RequestID, "summary_request_id": summary.ID})
	return nil
}
