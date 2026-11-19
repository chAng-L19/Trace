package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"
	"trace/internal/picore"
)

var errPiHalt = errors.New("pi loop stopped at durable Trace boundary")

func (a *Agent) runPi(ctx context.Context, id string) (Snapshot, error) {
	var request ModelRequest
	var lastResponse ModelResponse
	var modelErr error
	retries := 0
	var transport piContextTransport
	err := picore.Run(ctx, func(method string, params json.RawMessage) (any, error) {
		switch method {
		case "prepare":
			if recovery, pending, err := a.preparePiRecovery(id); err != nil {
				if !errors.Is(err, errPiRecoveryArchiveMissing) {
					return nil, err
				}
				if _, err := a.observeMissingPiArchive(id); err != nil {
					return nil, err
				}
			} else if pending {
				lastResponse = recovery.Response
				// Recovery may mix fresh calls with historical observations. Emit
				// the original batch in declaration order, without parallel writes.
				return map[string]any{"recovery": recovery.Response, "tools": recovery.Tools, "parallel_tools": []string{}}, nil
			}
			var err error
			request, err = a.preparePiConversation(ctx, id, &retries)
			if err != nil {
				return nil, err
			}
			var cached struct {
				Revision int `json:"revision"`
			}
			if err := json.Unmarshal(params, &cached); err != nil {
				return nil, err
			}
			return transport.prepare(request, a.piParallelTools(request.Tools), cached.Revision)
		case "model":
			// Retry only a failed provider attempt; pi alone schedules assistant turns.
			for {
				response, callErr := a.complete(ctx, id, request)
				snap, delay, err := a.settlePiResponse(ctx, id, request, response, callErr, &retries)
				if err != nil {
					return nil, err
				}
				if isStopped(snap.Run.Status) {
					return nil, errPiHalt
				}
				if snap.Run.Status == StatusFailed {
					var recovered bool
					snap, recovered, err = a.recoverProviderContext(ctx, id, request, response, callErr)
					if err != nil {
						return nil, err
					}
					if isStopped(snap.Run.Status) {
						return nil, errPiHalt
					}
					if recovered {
						request, err = a.preparePiConversation(ctx, id, &retries)
						if err != nil {
							return nil, err
						}
						continue
					}
					if callErr != nil && snap.Run.Error == callErr.Error() {
						modelErr = callErr
					} else {
						modelErr = errors.New(snap.Run.Error)
					}
					return map[string]string{"error": modelErr.Error()}, nil
				}
				if delay == 0 {
					if (reportDeliveryPending(&snap) || request.Purpose == "outcome_delivery") && len(response.ToolCalls) > 0 {
						request, err = a.preparePiConversation(ctx, id, &retries)
						if err != nil {
							return nil, err
						}
						continue
					}
					lastResponse = response
					return transport.modelResponse(request, response, a.piParallelTools(request.Tools))
				}
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
				}
				timer.Stop()
				request, err = a.preparePiConversation(ctx, id, &retries)
				if err != nil {
					return nil, err
				}
			}
		case "tool":
			var p struct {
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
				Manual    bool   `json:"manual"`
				Parallel  bool   `json:"parallel"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			snap, err := a.store.Load(id)
			if err != nil {
				return nil, err
			}
			if isStopped(snap.Run.Status) {
				return nil, errPiHalt
			}
			call, ok := pendingToolCalls(&snap)[p.CallID]
			if !ok || call.Name != p.Name {
				return nil, fmt.Errorf("pi requested unknown pending tool: %s", p.CallID)
			}
			if p.Manual {
				snap, err = a.recordUnavailableTool(id, call.ID, call.Name)
				if err != nil {
					return nil, err
				}
				if isStopped(snap.Run.Status) {
					return nil, errPiHalt
				}
				for i := len(snap.Messages) - 1; i >= 0; i-- {
					if m := snap.Messages[i]; m.Role == "tool" && m.ToolCallID == call.ID {
						return map[string]any{"content": m.Content}, nil
					}
				}
				return nil, fmt.Errorf("unavailable tool observation was not saved: %s", call.ID)
			}
			// Unknown tools may carry opaque manual input; executable tools must
			// carry exactly the JSON value that pi validated.
			if tool, known := a.registry.Lookup(call.Name); known || call.Name == "exec" {
				original, originalErr := strictPiArguments(call.Arguments)
				validated, validatedErr := strictPiArguments(p.Arguments)
				if originalErr != nil {
					return a.observePiToolFailure(id, call, originalErr, p.Parallel)
				}
				if validatedErr != nil || !reflect.DeepEqual(original, validated) {
					return a.observePiToolFailure(id, call, fmt.Errorf("pi validated arguments differ from durable call: %s", call.ID), p.Parallel)
				}
				if tool.Adapter == "builtin" || call.Name == "exec" {
					// Marshal through JSON: schemas may contain typed Go maps.
					var schema any
					raw, _ := json.Marshal(tool.Definition.Function.Parameters)
					if err := json.Unmarshal(raw, &schema); err != nil {
						return nil, err
					}
					if err := checkPiFieldAliases(original, schema); err != nil {
						return a.observePiToolFailure(id, call, err, p.Parallel)
					}
				}
			}
			if content, ready, err := readyPiResult(&snap, call.ID); ready || err != nil {
				if err == nil {
					err = validateReadyPiAction(&snap, call)
				}
				if err != nil {
					return a.observePiToolFailure(id, call, err, p.Parallel)
				}
				return map[string]any{"content": content}, err
			}
			toolCtx := ctx
			if p.Parallel {
				if !a.piReadOnlyBatch(lastResponse.ToolCalls) {
					return nil, fmt.Errorf("parallel request is not a read-only batch")
				}
				toolCtx = context.WithValue(ctx, piDeferredResultKey{}, true)
			}
			snap, err = a.executePendingTool(toolCtx, id, call)
			if err != nil {
				return nil, err
			}
			if isStopped(snap.Run.Status) {
				return nil, errPiHalt
			}
			if content, ready, err := readyPiResult(&snap, call.ID); ready || err != nil {
				if err != nil {
					return a.observePiToolFailure(id, call, err, p.Parallel)
				}
				return map[string]any{"content": content}, err
			}
			if _, pending := pendingToolCalls(&snap)[call.ID]; pending {
				return nil, errPiHalt
			}
			for i := len(snap.Messages) - 1; i >= 0; i-- {
				m := snap.Messages[i]
				if m.Role == "tool" && m.ToolCallID == call.ID {
					return map[string]any{"content": m.Content}, nil
				}
			}
			return nil, fmt.Errorf("tool completed without a durable result: %s", call.ID)
		case "tool_result":
			var p struct {
				CallID  string `json:"call_id"`
				Name    string `json:"name"`
				Content string `json:"content"`
				IsError bool   `json:"is_error"`
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, err
			}
			snap, err := a.store.Load(id)
			if err != nil {
				return nil, err
			}
			if isStopped(snap.Run.Status) {
				return nil, errPiHalt
			}
			declared := false
			for _, call := range lastResponse.ToolCalls {
				if call.ID == p.CallID && call.Name == p.Name {
					declared = true
					break
				}
			}
			if !declared {
				return nil, fmt.Errorf("pi result was not declared in current response: %s", p.CallID)
			}
			if p.IsError {
				// A validation failure cannot erase a historical side effect. Give
				// the model an explicit runtime observation without replaying it.
				if call, pending := pendingToolCalls(&snap)[p.CallID]; pending {
					_, isReady, readyErr := readyPiResult(&snap, p.CallID)
					if !isReady || readyErr != nil || validateReadyPiAction(&snap, call) != nil {
						observed := false
						_, err = a.store.Update(id, func(s *Snapshot) error {
							if isStopped(s.Run.Status) {
								return errPiHalt
							}
							var observationErr error
							observed, observationErr = appendHistoricalToolObservation(s, call, p.Content)
							return observationErr
						})
						if err != nil || observed {
							return map[string]bool{"ok": err == nil}, err
						}
					}
				}
			}
			if content, ready, readyErr := readyPiResult(&snap, p.CallID); ready || readyErr != nil {
				if readyErr != nil {
					return nil, readyErr
				}
				if p.Content != content {
					return nil, fmt.Errorf("pi result differs from executed ready result: %s", p.CallID)
				}
				call, ok := pendingToolCalls(&snap)[p.CallID]
				if !ok {
					return nil, errPiHalt
				}
				if err := validateReadyPiAction(&snap, call); err != nil {
					return nil, err
				}
				_, err := a.store.Update(id, func(s *Snapshot) error {
					if isStopped(s.Run.Status) {
						return errPiHalt
					}
					content, ready, err := readyPiResult(s, p.CallID)
					if err != nil {
						return err
					}
					if !ready || content != p.Content {
						return fmt.Errorf("ready result changed before settlement: %s", p.CallID)
					}
					call, ok := pendingToolCalls(s)[p.CallID]
					if !ok {
						return errPiHalt
					}
					if err := validateReadyPiAction(s, call); err != nil {
						return err
					}
					_, err = settleReadyPiResult(s, p.CallID)
					return err
				})
				return map[string]bool{"ok": err == nil}, err
			}
			if _, pending := pendingToolCalls(&snap)[p.CallID]; pending {
				if !p.IsError {
					return nil, fmt.Errorf("pi returned an unexecuted tool result: %s", p.CallID)
				}
				data, _ := json.Marshal(ToolResult{ExitCode: -1, Error: p.Content})
				_, err = a.store.Update(id, func(s *Snapshot) error {
					if isStopped(s.Run.Status) {
						return errPiHalt
					}
					if _, ok := pendingToolCalls(s)[p.CallID]; !ok {
						return nil
					}
					appendToolResult(s, p.CallID, string(data), "pi_validation")
					return nil
				})
			}
			return map[string]bool{"ok": err == nil}, err
		case "finish":
			return a.finishPiTurn(id, request, lastResponse)
		default:
			return nil, fmt.Errorf("unknown pi host method: %s", method)
		}
	}, errPiHalt)
	if err != nil {
		return a.settlePiLoopError(ctx, id, err)
	}
	snap, err := a.store.Load(id)
	if err != nil {
		return Snapshot{}, err
	}
	if !isStopped(snap.Run.Status) && snap.Run.Status != StatusCompleted && snap.Run.Status != StatusFailed && len(pendingToolCalls(&snap)) == 0 {
		return a.fail(id, errors.New("pi loop exited without a settled Trace completion decision"))
	}
	if modelErr != nil {
		return snap, modelErr
	}
	return snap, nil
}

func (a *Agent) observePiToolFailure(id string, call ToolCall, cause error, deferred bool) (any, error) {
	if deferred {
		raw, err := json.Marshal(ToolResult{ExitCode: -1, Error: cause.Error()})
		return map[string]any{"content": string(raw), "is_error": true}, err
	}
	snap, err := a.store.Update(id, func(s *Snapshot) error {
		if isStopped(s.Run.Status) {
			return errPiHalt
		}
		pending, ok := pendingToolCalls(s)[call.ID]
		if !ok || pending.Name != call.Name || pending.Arguments != call.Arguments {
			return fmt.Errorf("tool failure identity changed: %s", call.ID)
		}
		observed, err := appendHistoricalToolObservation(s, call, cause.Error())
		if err == nil && !observed {
			raw, marshalErr := json.Marshal(ToolResult{ExitCode: -1, Error: cause.Error()})
			if marshalErr != nil {
				return marshalErr
			}
			appendToolResult(s, call.ID, string(raw), "runtime_observation")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	for i := len(snap.Messages) - 1; i >= 0; i-- {
		if m := snap.Messages[i]; m.Role == "tool" && m.ToolCallID == call.ID {
			return map[string]any{"content": m.Content}, nil
		}
	}
	return nil, fmt.Errorf("historical observation was not saved: %s", call.ID)
}

// Preserve operator/budget state without turning an independent execution or
// persistence failure into a successful Load merely because the run stopped.
func (a *Agent) settlePiLoopError(ctx context.Context, id string, cause error) (Snapshot, error) {
	if picore.IsHaltOnly(cause, errPiHalt) {
		return a.store.Load(id)
	}
	controlOnly := ctx.Err() != nil && piCancellationOnly(cause, ctx)
	if paused, pauseErr := a.pauseIfBudgetTime(id, ctx); paused || pauseErr != nil {
		snap, err := a.store.Load(id)
		if controlOnly {
			return snap, errors.Join(pauseErr, err)
		}
		return snap, errors.Join(cause, pauseErr, err)
	}
	snap, err := a.fail(id, cause)
	if controlOnly || errors.Is(err, cause) {
		return snap, err
	}
	return snap, errors.Join(cause, err)
}

func piCancellationOnly(err error, ctx context.Context) bool {
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		causes := wrapped.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !piCancellationOnly(cause, ctx) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return piCancellationOnly(wrapped.Unwrap(), ctx)
	default:
		return err != nil && (errors.Is(err, errPiHalt) || errors.Is(err, ctx.Err()) || errors.Is(err, context.Cause(ctx)))
	}
}

// Summaries stay internal, including when late input triggers one during retry.
func (a *Agent) preparePiConversation(ctx context.Context, id string, retries *int) (ModelRequest, error) {
	for {
		request, err := a.preparePiRequest(ctx, id)
		if err != nil {
			return ModelRequest{}, err
		}
		if request.Purpose != "context_summary" {
			return request, nil
		}
		response, callErr := a.complete(ctx, id, request)
		snap, delay, err := a.settlePiResponse(ctx, id, request, response, callErr, retries)
		if err != nil {
			return ModelRequest{}, err
		}
		if isStopped(snap.Run.Status) {
			return ModelRequest{}, errPiHalt
		}
		if snap.Run.Status == StatusFailed {
			return ModelRequest{}, errors.New(snap.Run.Error)
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
			}
			timer.Stop()
		}
	}
}

// pi owns turn scheduling; Trace keeps one canonical Store and action journal.
func (a *Agent) preparePiRequest(ctx context.Context, id string) (ModelRequest, error) {
	prepareStarted := time.Now()
	snap, err := a.store.Load(id)
	if err != nil {
		return ModelRequest{}, err
	}
	if isStopped(snap.Run.Status) {
		return ModelRequest{}, errPiHalt
	}
	if ctx.Err() != nil {
		if paused, pauseErr := a.pauseIfBudgetTime(id, ctx); pauseErr != nil {
			return ModelRequest{}, errors.Join(context.Cause(ctx), pauseErr)
		} else if paused {
			return ModelRequest{}, errPiHalt
		}
		_, err = a.fail(id, context.Cause(ctx))
		return ModelRequest{}, err
	}
	if err := a.reconcileWorkerSettlements(id); err != nil {
		_, failErr := a.fail(id, err)
		if failErr == nil || errors.Is(failErr, err) {
			return ModelRequest{}, err
		}
		return ModelRequest{}, errors.Join(err, failErr)
	}
	if err := a.resumePausedWorkers(ctx, id); err != nil {
		_, failErr := a.fail(id, err)
		if failErr == nil || errors.Is(failErr, err) {
			return ModelRequest{}, err
		}
		return ModelRequest{}, errors.Join(err, failErr)
	}
	if len(pendingToolCalls(&snap)) > 0 {
		return ModelRequest{}, errors.New("pending batch must pass pi recovery before a new model request")
	}
	if !reportDeliveryPending(&snap) {
		if err := a.DeliverWorkerMessages(id); err != nil {
			_, err = a.fail(id, err)
			return ModelRequest{}, err
		}
	}
	workerContext := a.workerContextProjection(&snap)
	inherited, err := a.inheritedContextLedger(&snap)
	if err != nil {
		return ModelRequest{}, err
	}
	archiveStarted := time.Now()
	snap, err = a.store.Update(id, func(s *Snapshot) error {
		s.workerContextMessage = workerContext
		if isStopped(s.Run.Status) {
			return nil
		}
		if reason := budgetReason(s, "model"); reason != "" {
			pauseBudget(s, reason)
			return nil
		}
		s.Run.Status = StatusRunning
		now := time.Now().UTC()
		if len(s.Messages) == 0 {
			s.Messages = append(s.Messages, Message{Role: "user", Content: s.Run.Goal, CreatedAt: now})
		}
		idleContinuation := len(s.Requests) == 0 || (len(s.Messages) > 0 && s.Messages[len(s.Messages)-1].Role == "assistant")
		// Keep operator input pending while a source-bound report is being
		// delivered; otherwise a follow-up would be acknowledged and then
		// silently discarded by the delivery-only turn.
		if !reportDeliveryPending(s) {
			consumeMailbox(s, "steer")
			// On an idle continuation a follow-up is the next turn's input.
			if idleContinuation {
				consumeMailbox(s, "follow_up")
			}
		}
		if reportDeliveryPending(s) {
			continueForReport(s, true)
		}
		// Build planner observations from this verified request transaction, not
		// another full history load. The archived cursor covers exactly these facts.
		plannerContext, plannerThrough := plannerContextFrom(s)
		catalog := a.skillCatalog()
		a.prepareRecon(s)
		runtimeObservations := plannerContext + verificationContext(s) + contextLedgerContext(s, inherited) + a.stageModelContext() + a.reconContext(s)
		if err := a.prepareInputMessages(s); err != nil {
			s.Run.Status, s.Run.PauseReason, s.Run.Error = StatusFailed, "", err.Error()
			addEvent(s, "attachment_delivery_failed", "输入附件校验失败；运行结束并保留原始记录", map[string]string{"error": err.Error()})
			return nil
		}
		handoffOnly := outcomeHandoffPending(s)
		var definitions []ToolDefinition
		var toolHint string
		var contextErr error
		if reportDeliveryPending(s) {
			definitions, toolHint = nil, "Report delivery only: return source-bound bilingual report text. No tool execution is admitted in this phase."
		} else if handoffOnly {
			definitions, toolHint = nil, "Outcome delivery only: return trace_result from existing sources. No tools are admitted; unsupported outcomes remain incomplete."
		} else {
			definitions, toolHint, contextErr = a.modelTools(s, catalog, runtimeObservations)
		}
		var messages []Message
		var estimate int
		var compactionID string
		if contextErr == nil {
			messages, estimate, compactionID, contextErr = projectContext(s, definitions, catalog+toolHint, runtimeObservations)
		}
		var summary *summaryNeeded
		if contextErr != nil && !errors.As(contextErr, &summary) {
			if errors.Is(contextErr, errProviderRecoveryLedger) {
				s.Run.Status, s.Run.PauseReason, s.Run.Error = StatusFailed, "", contextErr.Error()
				addEvent(s, "context_recovery_failed", "恢复账本校验失败；保留原始记录", map[string]string{"error": contextErr.Error()})
				return nil
			}
			// An unprojectable context is an explicit context-budget gate and
			// remains resumable after the operator raises the persisted limit.
			s.Run.Status = StatusPaused
			s.Run.PauseReason = "context budget: " + contextErr.Error()
			addEvent(s, "context_paused", "上下文预算不足", map[string]string{"error": contextErr.Error()})
			return nil
		}
		s.Run.RequestSeq++
		request := ModelRequest{ID: fmt.Sprintf("%s-%04d", id, s.Run.RequestSeq), PromptHash: s.Run.PromptHash, Messages: messages, Tools: definitions, CreatedAt: now, MaxOutputTokens: s.Run.Context.OutputReserve, ContextTokens: estimate, CompactionID: compactionID, PlannerThrough: plannerThrough}
		if handoffOnly {
			request.Purpose = "outcome_delivery"
		}
		if summary != nil {
			request.Purpose = "context_summary"
			request.PlannerThrough = 0
			summary.record.ID = "compact-" + request.ID
			summary.record.RequestID = request.ID
			request.Messages, request.Tools = summary.messages, nil
			request.ContextTokens = EstimateContextTokens(request.Messages, nil)
			request.CompactionID = summary.record.ID
		}
		request.ToolMetrics = measureToolRequest(request.Tools, s)
		request.ContextManifest = buildContextManifest(s, request, a.registry.Definitions())
		a.stampRequest(&request)
		if err := a.admitTreeRequestLocked(s, request); err != nil {
			var limit *TreeBudgetError
			if errors.As(err, &limit) {
				s.Run.RequestSeq--
				pauseBudget(s, err.Error())
				return nil
			}
			return err
		}
		if summary != nil {
			s.Compactions = append(s.Compactions, summary.record)
			summary.recordRecovery(s, request)
		}
		s.Requests = append(s.Requests, request)
		addEvent(s, "model_request", "已提交模型请求", map[string]any{"request_id": request.ID, "prompt_hash": request.PromptHash, "purpose": request.Purpose})
		return nil
	})
	if err != nil {
		return ModelRequest{}, err
	}
	if isStopped(snap.Run.Status) {
		return ModelRequest{}, errPiHalt
	}
	if snap.Run.Status == StatusFailed {
		return ModelRequest{}, errors.New(snap.Run.Error)
	}

	request := snap.Requests[len(snap.Requests)-1]
	request.prepareMillis = elapsedMillis(prepareStarted)
	request.archiveMillis = elapsedMillis(archiveStarted)
	return request, nil
}
func (a *Agent) settlePiResponse(ctx context.Context, id string, request ModelRequest, response ModelResponse, callErr error, requestRetries *int) (Snapshot, time.Duration, error) {
	providerErr := callErr
	retryDelay := time.Duration(0)
	snap, err := a.store.Update(id, func(s *Snapshot) error {
		// Cancellation may arrive while waiting for the Store lock. Settle it
		// against this transaction's state, before accepting model progress.
		if ctx.Err() != nil {
			cause := context.Cause(ctx)
			var budgetCause *timeBudgetCause
			if errors.As(cause, &budgetCause) {
				pauseTimeBudget(s, budgetCause)
			}
			callErr = errors.Join(callErr, cause)
		}
		record := ResponseRecord{RequestID: request.ID, Response: response, CreatedAt: time.Now().UTC()}
		if callErr != nil {
			record.Error = callErr.Error()
			if record.Response.Diagnostics == nil {
				record.Response.Diagnostics = &ModelDiagnostics{}
			}
			record.Response.Diagnostics.FailureReason = string(classifyProviderFailure(callErr))
		}
		s.Responses = append(s.Responses, record)
		summaryCallErr := callErr
		if isStopped(s.Run.Status) {
			// A complete summary remains reusable across an operator/budget
			// pause. Provider failures still leave the original history intact.
			summaryCallErr = providerErr
		}
		if handled, summaryErr := finishCompaction(s, request, response, summaryCallErr); handled {
			if summaryErr != nil {
				s.Responses[len(s.Responses)-1].Error = summaryErr.Error()
				if !isStopped(s.Run.Status) && providerErr != nil {
					retryDelay = modelRetryDelay(providerErr, *requestRetries+1)
					if retryDelay > 0 {
						(*requestRetries)++
						if reason := budgetReason(s, "model"); reason != "" {
							pauseBudget(s, reason)
						}
					} else {
						s.Run.Status, s.Run.Error = StatusFailed, summaryErr.Error()
					}
				}
			} else {
				*requestRetries = 0
			}
			return nil
		}
		if isStopped(s.Run.Status) {
			addEvent(s, "model_result_deferred", "模型结果已归档，运行未推进", map[string]string{"request_id": request.ID})
			return nil
		}
		if callErr == nil {
			callErr = validateResponse(response)
		}
		if callErr != nil {
			s.Responses[len(s.Responses)-1].Error = callErr.Error()
			if s.Responses[len(s.Responses)-1].Response.Diagnostics == nil {
				s.Responses[len(s.Responses)-1].Response.Diagnostics = &ModelDiagnostics{}
			}
			s.Responses[len(s.Responses)-1].Response.Diagnostics.FailureReason = string(classifyProviderFailure(callErr))
			addEvent(s, "model_error", "模型请求失败", map[string]string{"request_id": request.ID, "error": callErr.Error()})
			if ctx.Err() == nil {
				retryDelay = modelRetryDelay(callErr, *requestRetries+1)
			}
			if retryDelay > 0 {
				if reason := budgetReason(s, "model"); reason != "" {
					pauseBudget(s, reason)
					return nil
				}
				(*requestRetries)++
				s.Run.Status, s.Run.Error = StatusRunning, ""
				addEvent(s, "model_retry", "模型服务暂时不可用，等待后重新请求", map[string]any{"request_id": request.ID, "attempt": *requestRetries, "delay_ms": retryDelay.Milliseconds()})
				return nil
			}
			if reportDeliveryPending(s) {
				s.Run.Status, s.Run.PauseReason = StatusFailed, ""
				addEvent(s, "report_translation_failed", "报告补译请求失败；保留已验证业务结果，等待恢复交付", map[string]string{"request_id": request.ID, "error": callErr.Error()})
			} else {
				s.Run.Status = StatusFailed
			}
			s.Run.Error = callErr.Error()
			return nil
		}
		*requestRetries = 0
		if request.Purpose == "outcome_delivery" && len(response.ToolCalls) > 0 {
			s.Responses[len(s.Responses)-1].Error = "outcome delivery does not admit tool execution"
			s.Run.Status, s.Run.PauseReason = StatusRunning, ""
			s.Messages = append(s.Messages, Message{Role: "user", Content: "Runtime completion audit: " + workerOutcomeHandoffReason + ". The attempted tool calls were not executed; return the outcome using the existing sources.", CreatedAt: time.Now().UTC()})
			addEvent(s, "outcome_delivery_tool_rejected", "结果补交回合未执行工具；保留已有观察等待结构化结果", map[string]string{"request_id": request.ID})
			return nil
		}
		if reportDeliveryPending(s) && len(response.ToolCalls) > 0 {
			s.Responses[len(s.Responses)-1].Error = "report delivery does not admit tool execution"
			s.Run.Status, s.Run.PauseReason = StatusRunning, ""
			s.Messages = append(s.Messages, Message{Role: "user", Content: "Report delivery accepts source-bound report text only. The attempted tool calls were not executed. Return the requested trace_report with complete zh_cn and en text using the existing observations.", CreatedAt: time.Now().UTC()})
			addEvent(s, "report_translation_missing", "补译请求包含工具调用；已保留业务结果，未执行工具", map[string]string{"request_id": request.ID})
			return nil
		}
		s.Messages = append(s.Messages, Message{RequestID: request.ID, Role: "assistant", ReportOnly: reportDeliveryPending(s), Content: response.Content, Thinking: response.Thinking, ToolCalls: response.ToolCalls, CreatedAt: time.Now().UTC()})
		if len(response.ToolCalls) > 0 {
			s.Run.Status = StatusWaitingTool
			addEvent(s, "tool_requested", "模型请求工具执行", response.ToolCalls)
		} else {
			s.Run.Status = StatusRunning
		}
		return nil
	})

	return snap, retryDelay, err
}
func (a *Agent) finishPiTurn(id string, request ModelRequest, response ModelResponse) (any, error) {
	snap, err := a.store.Update(id, func(s *Snapshot) error {
		if isStopped(s.Run.Status) || s.Run.Status == StatusFailed || s.Run.Status == StatusCompleted {
			return nil
		}
		if len(pendingToolCalls(s)) > 0 {
			return nil
		}
		if len(response.ToolCalls) > 0 {
			return nil
		}
		requestID := request.ID
		delivering := reportDeliveryPending(s)
		if !delivering {
			steered := consumeMailbox(s, "steer")
			followed := consumeMailbox(s, "follow_up")
			if steered || followed {
				s.Run.Status = StatusRunning
				return nil
			}
			pending, err := a.pendingWorkerInput(s)
			if err != nil {
				return err
			}
			if pending {
				s.Run.Status = StatusRunning
				addEvent(s, "completion_deferred", "Worker 消息待处理，继续运行", nil)
				return nil
			}
		}
		s.Run.Status = StatusCompleted
		s.Run.Error = ""
		if !reportDeliveryPending(s) && continueForEvidence(s) {
			return nil
		}
		if pending, err := a.deferForChildReportDelivery(s); err != nil || pending {
			return err
		}
		if continueForReport(s) {
			return nil
		}
		// Sources may have been revoked while translation was pending. Once
		// delivery clears, apply the original business audit before terminating.
		if delivering && continueForEvidence(s) {
			return nil
		}
		// A report-only turn may have just completed delivery. Deliver any
		// queued business input only after the report gate is gone.
		if delivering {
			if a.reportWorkerVisit(s) {
				addEvent(s, "run_completed", "报告补交已完成；保留排队业务输入供独立恢复处理", map[string]string{"request_id": requestID, "prompt_hash": s.Run.PromptHash})
				return nil
			}
			steered := consumeMailbox(s, "steer")
			followed := consumeMailbox(s, "follow_up")
			if steered || followed {
				s.Run.Status = StatusRunning
				return nil
			}
			pending, err := a.pendingWorkerInput(s)
			if err != nil {
				return err
			}
			if pending {
				s.Run.Status = StatusRunning
				addEvent(s, "completion_deferred", "报告交付完成，继续处理待投递 Worker 消息", nil)
				return nil
			}
		}
		addEvent(s, "run_completed", "模型回复已保存", map[string]string{"request_id": requestID, "prompt_hash": s.Run.PromptHash})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := a.publishWorkerResult(id); err != nil {
		return nil, err
	}
	action := "continue"
	if isStopped(snap.Run.Status) || snap.Run.Status == StatusCompleted || snap.Run.Status == StatusFailed {
		action = "end"
	}
	return map[string]string{"action": action}, nil
}
