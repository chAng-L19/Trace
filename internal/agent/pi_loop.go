package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"trace/internal/agentloop"
)

var errPiHalt = errors.New("agent loop stopped at durable Trace boundary")

func (a *Agent) runPi(ctx context.Context, id string) (Snapshot, error) {
	loop := &piTurns{a: a, id: id}
	err := agentloop.Run(ctx, loop, errPiHalt)
	if err != nil {
		return a.settlePiLoopError(ctx, id, errors.Join(loop.modelErr, err))
	}
	snap, err := a.store.Load(id)
	if err != nil {
		return Snapshot{}, err
	}
	if !isStopped(snap.Run.Status) && snap.Run.Status != StatusCompleted && snap.Run.Status != StatusFailed && len(pendingToolCalls(&snap)) == 0 {
		return a.fail(id, errors.New("agent loop exited without a settled Trace completion decision"))
	}
	if loop.modelErr != nil {
		return snap, loop.modelErr
	}
	return snap, nil
}

// piTurns binds the Go loop (pi turn semantics) to Trace's durable
// transactions. Request/response/retry state is process-local and never
// authoritative; the Store remains the only history.
type piTurns struct {
	a            *Agent
	id           string
	request      ModelRequest
	lastResponse ModelResponse
	modelErr     error
	retries      int
}

func loopTurn(response ModelResponse, definitions []ToolDefinition, parallel func(string) bool) agentloop.Turn {
	turn := agentloop.Turn{StopReason: response.StopReason}
	for _, call := range response.ToolCalls {
		turn.Calls = append(turn.Calls, agentloop.Call{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	for _, definition := range definitions {
		name := definition.Function.Name
		turn.Tools = append(turn.Tools, agentloop.Tool{Name: name, Parameters: definition.Function.Parameters, Parallel: parallel != nil && parallel(name)})
	}
	return turn
}

func (l *piTurns) Recover(context.Context) (*agentloop.Turn, error) {
	recovery, pending, err := l.a.preparePiRecovery(l.id)
	if err != nil {
		if !errors.Is(err, errPiRecoveryArchiveMissing) {
			return nil, err
		}
		_, err = l.a.observeMissingPiArchive(l.id)
		return nil, err
	}
	if !pending {
		return nil, nil
	}
	l.lastResponse = recovery.Response
	// Recovery may mix fresh calls with historical observations. The loop
	// emits the original batch in declaration order, without parallel writes.
	turn := loopTurn(recovery.Response, recovery.Tools, nil)
	for i := range turn.Calls {
		turn.Calls[i].Settled = recovery.Settled[turn.Calls[i].ID]
	}
	return &turn, nil
}

func (l *piTurns) Prepare(ctx context.Context) error {
	var err error
	l.request, err = l.a.preparePiConversation(ctx, l.id, &l.retries)
	return err
}

func (l *piTurns) Model(ctx context.Context) (agentloop.Turn, error) {
	a, id := l.a, l.id
	// Retry only a failed provider attempt; the loop alone schedules assistant turns.
	for {
		request := l.request
		response, callErr := a.complete(ctx, id, request)
		snap, decision, err := a.settlePiResponse(ctx, id, request, response, callErr, &l.retries)
		delay := decision.retryDelay
		if err != nil {
			return agentloop.Turn{}, errors.Join(callErr, err)
		}
		if snap.Run.Status == StatusFailed {
			return l.failedModelTurn(ctx, snap.Run.Error, callErr), nil
		}
		if isStopped(snap.Run.Status) && decision.budgetPaused {
			return agentloop.Turn{}, errPiHalt
		}
		if ctx.Err() != nil {
			return agentloop.Turn{}, errors.Join(context.Cause(ctx), callErr)
		}
		if isStopped(snap.Run.Status) {
			return agentloop.Turn{}, errors.Join(callErr, errPiHalt)
		}
		if snap.Run.Status == StatusRunning && request.Purpose == "" && providerRecoveryReason(response, callErr) != "" {
			var recovered bool
			snap, recovered, err = a.recoverProviderContext(ctx, id, request, response, callErr)
			if err != nil {
				return agentloop.Turn{}, err
			}
			if isStopped(snap.Run.Status) || snap.Run.Status == StatusCompleted {
				return agentloop.Turn{}, errPiHalt
			}
			if recovered {
				if err = l.Prepare(ctx); err != nil {
					return agentloop.Turn{}, err
				}
				continue
			}
		}
		if snap.Run.Status == StatusFailed {
			return l.failedModelTurn(ctx, snap.Run.Error, callErr), nil
		}
		if delay == 0 {
			if (reportDeliveryPending(&snap) || request.Purpose == "outcome_delivery") && len(response.ToolCalls) > 0 {
				if err = l.Prepare(ctx); err != nil {
					return agentloop.Turn{}, err
				}
				continue
			}
			l.lastResponse = response
			return loopTurn(response, request.Tools, a.piReadOnlyTool), nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		if err = l.Prepare(ctx); err != nil {
			return agentloop.Turn{}, err
		}
	}
}

// A failed response is already committed. Finish must observe that terminal
// state even if cancellation arrives between Model and Finish.
func (l *piTurns) failedModelTurn(ctx context.Context, runError string, providerErr error) agentloop.Turn {
	if providerErr != nil {
		l.modelErr = providerErr
	} else if ctx.Err() != nil && runError == context.Cause(ctx).Error() {
		l.modelErr = context.Cause(ctx)
	} else {
		l.modelErr = errors.New(runError)
	}
	if ctx.Err() != nil && !errors.Is(l.modelErr, context.Cause(ctx)) {
		l.modelErr = errors.Join(l.modelErr, context.Cause(ctx))
	}
	return agentloop.Turn{Failed: true}
}

func (l *piTurns) Execute(ctx context.Context, loopCall agentloop.Call, manual, parallel bool) (agentloop.Outcome, error) {
	content, isError, err := l.execute(ctx, loopCall, manual, parallel)
	return agentloop.Outcome{Content: content, IsError: isError}, err
}

func (l *piTurns) execute(ctx context.Context, loopCall agentloop.Call, manual, parallel bool) (string, bool, error) {
	a, id := l.a, l.id
	snap, err := a.store.Load(id)
	if err != nil {
		return "", false, err
	}
	if isStopped(snap.Run.Status) {
		return "", false, errPiHalt
	}
	call, ok := pendingToolCalls(&snap)[loopCall.ID]
	if !ok || call.Name != loopCall.Name {
		return "", false, fmt.Errorf("agent loop requested unknown pending tool: %s", loopCall.ID)
	}
	if manual {
		snap, err = a.recordUnavailableTool(id, call.ID, call.Name)
		if err != nil {
			return "", false, err
		}
		if isStopped(snap.Run.Status) {
			return "", false, errPiHalt
		}
		for i := len(snap.Messages) - 1; i >= 0; i-- {
			if m := snap.Messages[i]; m.Role == "tool" && m.ToolCallID == call.ID {
				return m.Content, false, nil
			}
		}
		return "", false, fmt.Errorf("unavailable tool observation was not saved: %s", call.ID)
	}
	// Executable tools run the journaled original arguments. Reject duplicate
	// fields and builtin case aliases that Go decoding would silently merge.
	if tool, known := a.registry.Lookup(call.Name); known || call.Name == "exec" {
		original, originalErr := strictPiArguments(call.Arguments)
		if originalErr != nil {
			return a.observePiToolFailure(id, call, originalErr, parallel)
		}
		if tool.Adapter == "builtin" || call.Name == "exec" {
			// Marshal through JSON: schemas may contain typed Go maps.
			var schema any
			raw, _ := json.Marshal(tool.Definition.Function.Parameters)
			if err := json.Unmarshal(raw, &schema); err != nil {
				return "", false, err
			}
			if err := checkPiFieldAliases(original, schema); err != nil {
				return a.observePiToolFailure(id, call, err, parallel)
			}
		}
	}
	if content, ready, err := readyPiResult(&snap, call.ID); ready || err != nil {
		if err == nil {
			err = validateReadyPiAction(&snap, call)
		}
		if err != nil {
			return a.observePiToolFailure(id, call, err, parallel)
		}
		return content, false, nil
	}
	toolCtx := ctx
	if parallel {
		if !a.piReadOnlyBatch(l.lastResponse.ToolCalls) {
			return "", false, fmt.Errorf("parallel request is not a read-only batch")
		}
		toolCtx = context.WithValue(ctx, piDeferredResultKey{}, true)
	}
	snap, err = a.executePendingTool(toolCtx, id, call)
	if err != nil {
		return "", false, err
	}
	if isStopped(snap.Run.Status) {
		return "", false, errPiHalt
	}
	if content, ready, err := readyPiResult(&snap, call.ID); ready || err != nil {
		if err != nil {
			return a.observePiToolFailure(id, call, err, parallel)
		}
		return content, false, nil
	}
	if _, pending := pendingToolCalls(&snap)[call.ID]; pending {
		return "", false, errPiHalt
	}
	for i := len(snap.Messages) - 1; i >= 0; i-- {
		m := snap.Messages[i]
		if m.Role == "tool" && m.ToolCallID == call.ID {
			return m.Content, false, nil
		}
	}
	return "", false, fmt.Errorf("tool completed without a durable result: %s", call.ID)
}

func (l *piTurns) Result(_ context.Context, loopCall agentloop.Call, outcome agentloop.Outcome) error {
	a, id := l.a, l.id
	snap, err := a.store.Load(id)
	if err != nil {
		return err
	}
	if isStopped(snap.Run.Status) {
		return errPiHalt
	}
	pending := pendingToolCalls(&snap)
	if outcome.IsError {
		// A validation failure cannot erase a historical side effect. Give the
		// model an explicit runtime observation without replaying it.
		if call, ok := pending[loopCall.ID]; ok {
			_, isReady, readyErr := readyPiResult(&snap, loopCall.ID)
			if !isReady || readyErr != nil || validateReadyPiAction(&snap, call) != nil {
				observed := false
				_, err = a.store.Update(id, func(s *Snapshot) error {
					if isStopped(s.Run.Status) {
						return errPiHalt
					}
					var observationErr error
					observed, observationErr = appendHistoricalToolObservation(s, call, outcome.Content)
					if !observed && observationErr == nil {
						tool, _ := a.registry.Lookup(call.Name)
						observed, observationErr = a.toolAdmissionObservation(s, call, tool.Adapter)
					}
					return observationErr
				})
				if err != nil || observed {
					return err
				}
			}
		}
	}
	if content, ready, readyErr := readyPiResult(&snap, loopCall.ID); ready || readyErr != nil {
		if readyErr != nil {
			return readyErr
		}
		if outcome.ValidationError {
			// Current admission limits cannot replace an identity-checked historical receipt.
			outcome.Content = content
		}
		_, err := a.store.Update(id, func(s *Snapshot) error {
			if isStopped(s.Run.Status) {
				return errPiHalt
			}
			content, ready, err := readyPiResult(s, loopCall.ID)
			if err != nil {
				return err
			}
			if !ready || content != outcome.Content {
				return fmt.Errorf("ready result changed before settlement: %s", loopCall.ID)
			}
			call, ok := pendingToolCalls(s)[loopCall.ID]
			if !ok {
				return errPiHalt
			}
			if err := validateReadyPiAction(s, call); err != nil {
				return err
			}
			_, err = settleReadyPiResult(s, loopCall.ID)
			return err
		})
		return err
	}
	if _, ok := pending[loopCall.ID]; ok {
		if !outcome.IsError {
			return fmt.Errorf("loop returned an unexecuted tool result: %s", loopCall.ID)
		}
		data, _ := json.Marshal(ToolResult{ExitCode: -1, Error: outcome.Content})
		_, err = a.store.Update(id, func(s *Snapshot) error {
			if isStopped(s.Run.Status) {
				return errPiHalt
			}
			if _, ok := pendingToolCalls(s)[loopCall.ID]; !ok {
				return nil
			}
			appendToolResult(s, loopCall.ID, string(data), "pi_validation")
			return nil
		})
	}
	return err
}

func (l *piTurns) Finish(ctx context.Context) (bool, error) {
	return l.a.finishPiTurn(ctx, l.id, l.request, l.lastResponse)
}

func (a *Agent) observePiToolFailure(id string, call ToolCall, cause error, deferred bool) (string, bool, error) {
	if deferred {
		raw, err := json.Marshal(ToolResult{ExitCode: -1, Error: cause.Error()})
		return string(raw), true, err
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
		return "", false, err
	}
	for i := len(snap.Messages) - 1; i >= 0; i-- {
		if m := snap.Messages[i]; m.Role == "tool" && m.ToolCallID == call.ID {
			return m.Content, false, nil
		}
	}
	return "", false, fmt.Errorf("historical observation was not saved: %s", call.ID)
}

// Preserve operator/budget state without turning an independent execution or
// persistence failure into a successful Load merely because the run stopped.
func (a *Agent) settlePiLoopError(ctx context.Context, id string, cause error) (Snapshot, error) {
	if agentloop.IsHaltOnly(cause, errPiHalt) {
		return a.store.Load(id)
	}
	saved, loadErr := a.store.Load(id)
	if loadErr != nil {
		return Snapshot{}, errors.Join(cause, loadErr)
	}
	if saved.Run.Status == StatusFailed {
		return saved, errors.Join(errors.New(saved.Run.Error), cause)
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
		snap, decision, err := a.settlePiResponse(ctx, id, request, response, callErr, retries)
		delay := decision.retryDelay
		if err != nil {
			return ModelRequest{}, errors.Join(callErr, err)
		}
		if isStopped(snap.Run.Status) {
			if decision.budgetPaused {
				return ModelRequest{}, errPiHalt
			}
			return ModelRequest{}, errors.Join(callErr, errPiHalt)
		}
		if snap.Run.Status == StatusFailed {
			return ModelRequest{}, errors.Join(callErr, errors.New(snap.Run.Error))
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

// Agentloop owns turn scheduling; Trace keeps one canonical Store and action journal.
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
		return ModelRequest{}, errors.New("pending batch must pass agentloop recovery before a new model request")
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

type piResponseDisposition struct {
	retryDelay   time.Duration
	budgetPaused bool
}

func (a *Agent) settlePiResponse(ctx context.Context, id string, request ModelRequest, response ModelResponse, callErr error, requestRetries *int) (Snapshot, piResponseDisposition, error) {
	providerErr := callErr
	decision := piResponseDisposition{}
	snap, err := a.store.Update(id, func(s *Snapshot) error {
		// Cancellation may arrive while waiting for the Store lock. Settle it
		// against this transaction's state, before accepting model progress.
		cancelled := ctx.Err() != nil
		if cancelled {
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
					if !cancelled {
						decision.retryDelay = modelRetryDelay(providerErr, *requestRetries+1)
					}
					if decision.retryDelay > 0 {
						(*requestRetries)++
						if reason := budgetReason(s, "model"); reason != "" {
							pauseBudget(s, reason)
							decision.budgetPaused = true
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
				decision.retryDelay = modelRetryDelay(callErr, *requestRetries+1)
			}
			if decision.retryDelay > 0 {
				if reason := budgetReason(s, "model"); reason != "" {
					pauseBudget(s, reason)
					decision.budgetPaused = true
					return nil
				}
				(*requestRetries)++
				s.Run.Status, s.Run.Error = StatusRunning, ""
				addEvent(s, "model_retry", "模型服务暂时不可用，等待后重新请求", map[string]any{"request_id": request.ID, "attempt": *requestRetries, "delay_ms": decision.retryDelay.Milliseconds()})
				return nil
			}
			if ctx.Err() == nil && request.Purpose == "" {
				if reason := providerRecoveryReason(response, providerErr); reason != "" {
					decision.budgetPaused = a.prepareProviderRecoveryLocked(s, request, reason)
					return nil
				}
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

	return snap, decision, err
}
