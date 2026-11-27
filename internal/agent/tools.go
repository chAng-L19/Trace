package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"trace/internal/localtool"
	"trace/internal/worker"
)

func execToolDefinition() ToolDefinition {
	d := ToolDefinition{Type: "function"}
	d.Function.Name = "exec"
	d.Function.Description = "Execute a local shell command and return stdout, stderr, exit_code and errors. Unix uses /bin/sh (POSIX); Bash arrays/process substitution require explicit bash -lc. Inspect stderr and each required check; an && chain stops at its first failure. Use bounded timeouts and explicit child PIDs for cleanup, not broad pkill -f patterns."
	if limits, err := worker.ExecResourceLimits(); err == nil {
		if limits.Mode == worker.ResourceModeCgroup {
			d.Function.Description += fmt.Sprintf(" Each command has hard limits: memory=%d bytes; total processes AND threads=%d (Python ThreadPoolExecutor workers count); stdout+stderr=%d bytes. Keep concurrency at most %d, leave room for the shell/runtime, and batch larger jobs. Read TRACE_EXEC_RECOMMENDED_CONCURRENCY in generated scripts; checkpoint completed items between batches. Failures and unknown outcomes are returned for inspection and replanning; never assume partial external operations were undone or replay a historical call.", limits.MemoryBytes, limits.PIDs, limits.OutputBytes, recommendedExecConcurrency(limits))
		} else {
			d.Function.Description += fmt.Sprintf(" Command output is limited to %d bytes; process-group mode does not enforce per-command memory/PID limits. Keep concurrency bounded.", limits.OutputBytes)
		}
	}
	d.Function.Parameters = map[string]any{"type": "object", "properties": map[string]any{"command": map[string]string{"type": "string"}, "timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": maxExecTimeoutSeconds, "description": "Optional single-command timeout; otherwise use the run's persisted exec default. This is not an agent solving budget."}}, "required": []string{"command"}}
	return d
}

func (a *Agent) SubmitTool(ctx context.Context, id, callID, result string) (Snapshot, error) {
	if strings.TrimSpace(callID) == "" {
		return Snapshot{}, fmt.Errorf("tool call id is required")
	}
	release, err := a.store.AcquireRunLock(id)
	if err != nil {
		return Snapshot{}, err
	}
	defer release()
	snap, err := a.recordManualTool(id, callID, result)
	if err != nil {
		return Snapshot{}, err
	}
	if len(pendingToolCalls(&snap)) > 0 {
		return snap, nil
	}
	return a.runLocked(ctx, id)
}

func (a *Agent) recordManualTool(id, callID, result string) (Snapshot, error) {
	return a.recordToolObservation(id, "", callID, result, false)
}

// RecordToolObservation records a user observation without running the model,
// replaying the action, or promoting the original result to verified evidence.
func (a *Agent) RecordToolObservation(id, requestID, callID, result string) (Snapshot, error) {
	if strings.TrimSpace(requestID) == "" || strings.TrimSpace(callID) == "" || strings.TrimSpace(result) == "" {
		return Snapshot{}, fmt.Errorf("请填写调用身份和实际核对结果")
	}
	release, err := a.store.AcquireRunLock(id)
	if err != nil {
		return Snapshot{}, err
	}
	defer release()
	return a.recordToolObservation(id, requestID, callID, result, true)
}

func (a *Agent) recordToolObservation(id, requestID, callID, result string, allowPaused bool) (Snapshot, error) {
	return a.store.Update(id, func(s *Snapshot) error {
		if allowPaused {
			if handled, err := a.recordExecutorOwnershipReviewLocked(s, requestID, callID, result); handled || err != nil {
				return err
			}
		}
		if s.Run.Status != StatusWaitingTool && !(allowPaused && s.Run.Status == StatusPaused) {
			return fmt.Errorf("run is not waiting for a tool")
		}
		if requestID != "" && requestID != currentRequestID(s) {
			return fmt.Errorf("调用所属请求已变化，请重新读取后核对")
		}
		if _, ok := pendingToolCalls(s)[callID]; !ok {
			return fmt.Errorf("unknown pending tool call: %s", callID)
		}
		if allowPaused {
			batch := latestToolBatchIndex(s)
			declared := 0
			if batch >= 0 {
				message := s.Messages[batch]
				belongs := message.RequestID == requestID || message.RequestID == "" && message.ID != "" && requestID == "manual-batch:"+message.ID
				if belongs {
					for _, call := range message.ToolCalls {
						if call.ID == callID {
							declared++
						}
					}
				}
			}
			if declared != 1 {
				return fmt.Errorf("调用不属于当前请求的工具批次，请重新读取后核对")
			}
		}
		for i := range s.Actions {
			action := &s.Actions[i]
			if action.CallID == callID && actionRequestMayMatchCurrent(s, *action) && !actionRequestMatchesCurrent(s, *action) {
				return fmt.Errorf("legacy action batch identity is ambiguous: %s", callID)
			}
			if actionRequestMatchesCurrent(s, *action) && action.CallID == callID && (action.Result == nil || action.Status == "uncertain" || action.Status == "result_ready") {
				action.Status = "resolved_manually"
				if action.FinishedAt.IsZero() {
					action.FinishedAt = time.Now().UTC()
				}
			}
		}
		appendToolResult(s, callID, result, "manual")
		if allowPaused && s.Run.Status == StatusPaused && len(pendingToolCalls(s)) == 0 && toolCleanupError(*s) == nil && s.Run.PauseReason == "tool outcome uncertain; inspect side effects and submit its result manually" {
			s.Run.PauseReason = "tool outcomes reviewed; awaiting operator resume"
		}
		return nil
	})
}

func appendToolResult(s *Snapshot, callID, result, source string) {
	evidenceID := fmt.Sprintf("ev-%d", s.Run.EventSeq+1)
	s.Messages = append(s.Messages, Message{Role: "tool", ToolCallID: callID, Content: result, CreatedAt: time.Now().UTC()})
	if !isStopped(s.Run.Status) {
		if len(pendingToolCalls(s)) == 0 {
			s.Run.Status = StatusCreated
		} else {
			s.Run.Status = StatusWaitingTool
		}
	}
	addEvent(s, "tool_result", "工具结果已保存", map[string]string{"call_id": callID, "source": source, "evidence_id": evidenceID, "evidence_scope_hash": valueHash(executionEvidenceScope(s))})
	typ := "manual_observation"
	if source == "executed" {
		typ = "tool_execution"
	} else if source == "runtime_observation" {
		typ = source
	}
	s.Evidence = append(s.Evidence, Evidence{ID: evidenceID, RequestID: currentRequestID(s), CallID: callID, SourceEventSequence: s.Run.EventSeq, ContentHash: promptHash(result), Content: result, Type: typ, Verification: "unverified", Scope: executionEvidenceScope(s)})
}

func currentRequestID(s *Snapshot) string {
	for i := len(s.Requests) - 1; i >= 0; i-- {
		if s.Requests[i].Purpose != "evidence_review" {
			if s.Requests[i].ID != "" {
				return s.Requests[i].ID
			}
			break
		}
	}
	// Legacy/manual batches can have durable assistant calls without a model
	// request archive. Bind local ownership to that persisted batch identity,
	// never manufacture a model request/response or verified provenance.
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role == "assistant" && len(s.Messages[i].ToolCalls) > 0 {
			if s.Messages[i].ID == "" {
				return ""
			}
			return "manual-batch:" + s.Messages[i].ID
		}
	}
	return ""
}

func actionRequestMatchesCurrent(s *Snapshot, action Action) bool {
	requestID := currentRequestID(s)
	if action.RequestID != "" || requestID != "" && !strings.HasPrefix(requestID, "manual-batch:") {
		return action.RequestID == requestID
	}
	batch, known := legacyActionBatch(s, action)
	return known && batch == latestToolBatchIndex(s)
}

// Unknown legacy ownership blocks execution but never authorizes consuming a
// cached result, resolving an action, or changing the historical action.
func actionRequestMayMatchCurrent(s *Snapshot, action Action) bool {
	if actionRequestMatchesCurrent(s, action) {
		return true
	}
	requestID := currentRequestID(s)
	if action.RequestID != "" || requestID != "" && !strings.HasPrefix(requestID, "manual-batch:") {
		return false
	}
	_, known := legacyActionBatch(s, action)
	return !known
}

func latestToolBatchIndex(s *Snapshot) int {
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role == "assistant" && len(s.Messages[i].ToolCalls) > 0 {
			return i
		}
	}
	return -1
}

// Empty RequestID predates batch ownership. A unique declaring batch is
// sufficient; repeated call IDs require an exact result receipt. Wall-clock
// timestamps alone cannot establish batch ownership across old snapshots.
func legacyActionBatch(s *Snapshot, action Action) (int, bool) {
	candidates := map[int]bool{}
	for i, message := range s.Messages {
		if message.Role != "assistant" {
			continue
		}
		for _, call := range message.ToolCalls {
			if call.ID == action.CallID {
				candidates[i] = true
			}
		}
	}
	if len(candidates) == 1 {
		for index := range candidates {
			return index, true
		}
	}
	if action.Result != nil && action.Result.EvidenceID != "" {
		raw, _ := json.Marshal(action.Result)
		batch, found := -1, -1
		for i, message := range s.Messages {
			if message.Role == "assistant" && len(message.ToolCalls) > 0 {
				batch = i
			}
			if message.Role == "tool" && message.ToolCallID == action.CallID && message.Content == string(raw) && candidates[batch] {
				if found != -1 && found != batch {
					return -1, false
				}
				found = batch
			}
		}
		if found != -1 {
			return found, true
		}
	}
	return -1, false
}

func (a *Agent) ExecuteTool(parent context.Context, id, callID string, command []string) (final Snapshot, executeErr error) {
	release, err := a.store.AcquireRunLock(id)
	if err != nil {
		return Snapshot{}, err
	}
	defer release()
	snapshot, err := a.store.Load(id)
	if err != nil {
		return Snapshot{}, err
	}
	call, ok := pendingToolCalls(&snapshot)[callID]
	if snapshot.Run.Status != StatusWaitingTool || !ok {
		return snapshot, fmt.Errorf("unknown pending tool call: %s", callID)
	}
	// Allowed historical calls must pass through their original manual
	// continuation and child admission before consuming the observation.
	if !a.registry.permitsRunTool(snapshot.Run, call.Name, "exec") {
		if observed, handled, admissionErr := a.observePendingToolAdmission(snapshot, call, "exec"); handled || admissionErr != nil {
			return observed, admissionErr
		}
	}
	if len(command) == 0 && !hasHistoricalToolIntent(&snapshot, call) {
		return snapshot, fmt.Errorf("command is required")
	}
	operation := func(agent *Agent, ctx context.Context, id string) (Snapshot, error) {
		return agent.executeToolLocked(ctx, id, callID, command)
	}
	if snapshot.Run.ParentTaskID != "" && a.workerRunID != id {
		return a.resumeWorkerOperation(parent, snapshot, operation)
	}
	return operation(a, parent, id)
}

func (a *Agent) executeToolLocked(parent context.Context, id, callID string, command []string) (Snapshot, error) {
	return a.executeManualToolCycle(parent, id, func(ctx context.Context) (Snapshot, error) {
		return a.executeCommand(ctx, id, callID, command)
	})
}

// The caller holds the run lock and validates against the actual executing Agent.
func (a *Agent) executeManualToolCycle(parent context.Context, id string, execute func(context.Context) (Snapshot, error)) (final Snapshot, executeErr error) {
	_, err := a.store.Update(id, func(s *Snapshot) error { settleBudgetTime(s); applyBudget(s, a.budget); return nil })
	if err != nil {
		return Snapshot{}, err
	}
	ctx, stop, err := a.beginBudgetContext(parent, id)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() {
		stop()
		if saved, err := a.store.Load(id); err == nil {
			final = saved
		} else if executeErr == nil {
			executeErr = err
		}
	}()
	snap, err := execute(ctx)
	if err != nil || isStopped(snap.Run.Status) || len(pendingToolCalls(&snap)) > 0 {
		return snap, err
	}
	stop()
	return a.runLocked(parent, id)
}

// Exactly one journaled call; live pi batches and recovery share this path.
func (a *Agent) executePendingTool(ctx context.Context, id string, call ToolCall) (Snapshot, error) {
	snap, err := a.store.Load(id)
	if err != nil || isStopped(snap.Run.Status) {
		return snap, err
	}
	adapter := ""
	if tool, ok := a.registry.Lookup(call.Name); ok {
		adapter = tool.Adapter
	}
	if observed, handled, admissionErr := a.observePendingToolAdmission(snap, call, adapter); handled || admissionErr != nil {
		return observed, admissionErr
	}
	if tool, ok := a.registry.Lookup(call.Name); ok && tool.Adapter == "builtin" {
		snap, err = a.executeBuiltin(ctx, id, call)
		if err != nil || isStopped(snap.Run.Status) {
			return snap, err
		}
		return snap, nil
	}
	if tool, ok := a.registry.Lookup(call.Name); ok && tool.Local != nil {
		command, _, argumentErr := tool.Local.Invocation(json.RawMessage(call.Arguments))
		if argumentErr != nil {
			data, _ := json.Marshal(ToolResult{ExitCode: -1, Error: argumentErr.Error()})
			snap, err = a.recordManualTool(id, call.ID, string(data))
		} else {
			snap, err = a.executeLocal(ctx, id, call, tool.Local, command)
		}
		if err != nil || isStopped(snap.Run.Status) {
			return snap, err
		}
		return snap, nil
	}
	if tool, ok := a.registry.Lookup(call.Name); ok && tool.Client != nil {
		if argumentErr := validateMCPArguments(call.Arguments); argumentErr != nil {
			data, _ := json.Marshal(ToolResult{ExitCode: -1, Error: argumentErr.Error()})
			snap, err = a.recordManualTool(id, call.ID, string(data))
		} else {
			snap, err = a.executeMCP(ctx, id, call)
		}
		if err != nil || isStopped(snap.Run.Status) {
			return snap, err
		}
		return snap, nil
	}
	if call.Name != "exec" {
		return a.recordUnavailableTool(id, call.ID, call.Name)
	}
	var args struct {
		Command string `json:"command"`
	}
	if json.Unmarshal([]byte(call.Arguments), &args) != nil || strings.TrimSpace(args.Command) == "" {
		snap, err = a.recordManualTool(id, call.ID, `{"stdout":"","stderr":"","exit_code":-1,"error":"invalid exec arguments"}`)
	} else {
		command := []string{"/bin/sh", "-c", args.Command}
		if runtime.GOOS == "windows" {
			command = []string{"cmd.exe", "/d", "/s", "/c", args.Command}
		}
		snap, err = a.executeProcess(ctx, id, call.ID, command, args.Command)
	}
	if err != nil {
		return snap, err
	}
	if isStopped(snap.Run.Status) {
		return snap, nil
	}
	return a.store.Load(id)
}

func (a *Agent) executeCommand(ctx context.Context, id, callID string, command []string) (Snapshot, error) {
	return a.executeProcess(ctx, id, callID, command, "")
}

func (a *Agent) executeProcess(ctx context.Context, id, callID string, command []string, shellCommand string) (Snapshot, error) {
	snapshot, err := a.store.Load(id)
	if err != nil {
		return Snapshot{}, err
	}
	call, ok := pendingToolCalls(&snapshot)[callID]
	if !ok {
		return Snapshot{}, fmt.Errorf("unknown pending tool call: %s", callID)
	}
	if observed, handled, admissionErr := a.observePendingToolAdmission(snapshot, call, "exec"); handled || admissionErr != nil {
		return observed, admissionErr
	}
	if snapshot.Run.Status == StatusWaitingTool && currentRequestID(&snapshot) == "" {
		// Pre-message-ID snapshots need the existing Store migration committed
		// before either the Action or the worker derives its resource owner.
		snapshot, err = a.store.Update(id, func(s *Snapshot) error {
			if s.Run.Status != StatusWaitingTool {
				return fmt.Errorf("run is not waiting for a tool")
			}
			if _, ok := pendingToolCalls(s)[callID]; !ok {
				return fmt.Errorf("unknown pending tool call: %s", callID)
			}
			return nil // Update persists assignMessageIDs before returning.
		})
		if err != nil {
			return Snapshot{}, err
		}
		call = pendingToolCalls(&snapshot)[callID]
	}
	timeoutArgs := call.Arguments
	// ExecuteTool can resolve an external/manual call with explicit argv. Its
	// foreign JSON schema must not be interpreted as exec timeout configuration.
	if call.Name != "exec" {
		timeoutArgs = `{}`
	}
	seconds, err := execTimeout(snapshot.Run, timeoutArgs)
	if err != nil {
		data, _ := json.Marshal(ToolResult{ExitCode: -1, Error: err.Error()})
		return a.recordManualTool(id, callID, string(data))
	}
	ctx = context.WithValue(ctx, execTimeoutKey{}, seconds)
	return a.executeAction(ctx, id, callID, "exec", command, func() ToolResult {
		toolCtx := ctx
		cancel := func() {}
		if seconds > 0 {
			toolCtx, cancel = context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
		}
		defer cancel()
		limits, limitErr := worker.ExecResourceLimits()
		if limitErr != nil {
			return ToolResult{ExitCode: -1, Error: limitErr.Error(), WorkerStatus: worker.StatusNotStarted}
		}
		emit, drain := a.outputEvents(id, callID)
		output, runErr := worker.Run(toolCtx, worker.Request{Resources: limits, Env: execResourceEnvironment(limits), RunID: id, RequestID: currentRequestID(&snapshot), CallID: callID, Command: command, ShellCommand: shellCommand, CaptureDir: filepath.Join(a.store.root, "artifacts"), OnOutput: emit})
		executionEndedAt := time.Now().UTC()
		runErr = errors.Join(runErr, drain())
		// Keep the established exec result contract: an observed interrupted
		// process is archived as unverified evidence, not silently retried.
		// Classify the observed execution, not a parent context that can be cancelled
		// during output archival. Legacy nil/zero boundaries retain their contract.
		result := ToolResult{Stdout: output.Stdout, Stderr: output.Stderr, StdoutPath: output.StdoutPath, StderrPath: output.StderrPath, ExitCode: -1, Interrupted: output.Status == worker.StatusTimedOut || output.Status == worker.StatusCancelled, WorkerPID: output.PID, WorkerStatus: output.Status, WorkerOutcomeUncertain: output.OutcomeUncertain, OutcomeUncertain: seconds > 0 && output.OutcomeUncertain}
		result.WorkerCleanupError = output.CleanupError
		result.WorkerResourceOwner = output.ResourceOwner
		result.WorkerResourceLimit = output.ResourceLimit
		if len(output.CleanupError) > 0 || output.ResourceLimit != "" && output.Status != worker.StatusFailed {
			result.OutcomeUncertain = true
		}
		if output.ExitCode != nil {
			result.ExitCode = *output.ExitCode
		}
		if runErr != nil {
			result.Error = runErr.Error()
		}
		result.Diagnostic = execResultDiagnostic(result, limits)
		result.executionEndedAt = executionEndedAt
		return result
	})
}

func (a *Agent) executeLocal(ctx context.Context, id string, call ToolCall, manifest *localtool.Manifest, command []string) (Snapshot, error) {
	return a.executeAction(ctx, id, call.ID, "local", command, func() ToolResult {
		request, requestErr := manifest.Request(id, call.ID, json.RawMessage(call.Arguments))
		if requestErr != nil {
			return ToolResult{ExitCode: -1, Error: requestErr.Error()}
		}
		emit, drain := a.outputEvents(id, call.ID)
		request.OnOutput = emit
		request.CaptureDir = filepath.Join(a.store.root, "artifacts")
		if manifest.TimeoutSeconds > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, time.Duration(manifest.TimeoutSeconds)*time.Second)
			defer cancel()
		}
		output, runErr := worker.Run(ctx, request)
		executionEndedAt := time.Now().UTC()
		runErr = errors.Join(runErr, drain())
		result := ToolResult{Stdout: output.Stdout, Stderr: output.Stderr, StdoutPath: output.StdoutPath, StderrPath: output.StderrPath, ExitCode: -1, Interrupted: output.Status == worker.StatusTimedOut || output.Status == worker.StatusCancelled, WorkerPID: output.PID, WorkerStatus: output.Status, WorkerOutcomeUncertain: output.OutcomeUncertain, OutcomeUncertain: output.OutcomeUncertain}
		result.WorkerCleanupError = output.CleanupError
		result.executionEndedAt = executionEndedAt
		if output.ExitCode != nil {
			result.ExitCode = *output.ExitCode
		}
		if runErr != nil {
			result.Error = runErr.Error()
		}
		return result
	})
}

// Stream events are bounded previews; the final artifact is the complete byte stream.
// Coalescing avoids rewriting the full snapshot for each OS pipe read.
func (a *Agent) outputEvents(id, callID string) (func(string, string), func() error) {
	return outputPreviewBatch(func(batch []outputPreview) error {
		events := make([]Event, 0, len(batch))
		for _, p := range batch {
			events = append(events, Event{Type: "tool_output", Summary: "工具输出预览", Data: map[string]any{"call_id": callID, "stream": p.stream, "content": p.content, "offset": p.offset, "end_offset": p.end, "next_offset": p.next, "preview": true}})
		}
		return a.store.AppendEvents(id, events)
	})
}

type outputPreview struct {
	stream, content   string
	offset, end, next int64
}

const outputPreviewBytes = 4096

// Only bounded previews wait behind a slow disk transaction. Capture files
// receive every byte before this callback; offsets expose any preview gaps.
// No Store lock or filesystem operation runs on the process pipe consumers.
func outputPreviewBatch(persist func([]outputPreview) error) (func(string, string), func() error) {
	ticker := time.NewTicker(200 * time.Millisecond)
	return outputPreviewBatchTicks(persist, ticker.C, ticker.Stop)
}

func outputPreviewBatchTicks(persist func([]outputPreview) error, ticks <-chan time.Time, stopTicks func()) (func(string, string), func() error) {
	var mu sync.Mutex
	pending := map[string]outputPreview{}
	tails := map[string]outputPreview{}
	offsets := map[string]int64{}
	published := map[string]int{}
	ends := map[string]int64{}
	stop, done := make(chan struct{}), make(chan struct{})
	var persistErr error
	go func() {
		defer close(done)
		defer stopTicks()
		flush := func(final bool) {
			mu.Lock()
			batch := make([]outputPreview, 0, 4)
			for _, stream := range []string{"stdout", "stderr"} {
				if p, ok := pending[stream]; ok {
					delete(pending, stream)
					p.content = p.content[:min(len(p.content), artifactThreshold-published[stream])]
					part := previewPart([]byte(p.content), p.offset, p.offset > 0, true)
					if part.Text != "" {
						batch = append(batch, outputPreview{stream: stream, content: part.Text, offset: part.Offset, end: part.EndOffset, next: p.next})
						published[stream] += len(part.Text)
						ends[stream] = part.EndOffset
					}
					// Carry an incomplete UTF-8 suffix only when no bytes were skipped.
					if !final && published[stream] < artifactThreshold && p.next == p.offset+int64(len(p.content)) && part.EndOffset < p.next {
						pending[stream] = outputPreview{stream: stream, content: strings.Clone(p.content[part.EndOffset-p.offset:]), offset: part.EndOffset, next: p.next}
					}
				}
				if final {
					p := tails[stream]
					start := max(p.offset, ends[stream])
					if start < p.next {
						part := previewPart([]byte(p.content[start-p.offset:]), start, start > 0, false)
						if part.Text != "" {
							batch = append(batch, outputPreview{stream: stream, content: part.Text, offset: part.Offset, end: part.EndOffset, next: p.next})
						} else {
							// A binary tail can contain only continuation bytes. Keep
							// its watermark so the omitted output remains visible.
							batch = append(batch, outputPreview{stream: stream, offset: p.next, end: p.next, next: p.next})
						}
					}
				}
			}
			mu.Unlock()
			if len(batch) > 0 && persistErr == nil {
				persistErr = persist(batch)
			}
		}
		for {
			select {
			case <-stop:
				flush(true)
				return
			case <-ticks:
				flush(false)
			}
		}
	}()
	emit := func(stream, content string) {
		mu.Lock()
		defer mu.Unlock()
		offset := offsets[stream]
		offsets[stream] += int64(len(content))
		tail := tails[stream]
		if len(content) >= outputPreviewBytes {
			tail.content = strings.Clone(content[len(content)-outputPreviewBytes:])
		} else {
			tail.content = tail.content[max(0, len(tail.content)+len(content)-outputPreviewBytes):] + content
		}
		tail.stream, tail.next = stream, offsets[stream]
		tail.offset = tail.next - int64(len(tail.content))
		tails[stream] = tail
		// A per-flush bound alone duplicates slow output in the durable event log.
		// Keep at most 16 KiB per stream across all ticks, plus one final 4 KiB tail.
		if published[stream] >= artifactThreshold {
			return
		}
		p, ok := pending[stream]
		if !ok {
			p = outputPreview{stream: stream, offset: offset}
		}
		p.content += content[:min(len(content), outputPreviewBytes-len(p.content))]
		p.next = offsets[stream]
		pending[stream] = p
	}
	return emit, func() error { close(stop); <-done; return persistErr }
}

// executeAction owns the durable intent/result boundary for every real tool.
func (a *Agent) executeAction(ctx context.Context, id, callID, adapter string, command []string, execute func() ToolResult) (Snapshot, error) {
	return a.executeActionWithResourceIdentity(ctx, id, callID, adapter, command, execute, worker.ResourceRootIdentity)
}

func (a *Agent) executeActionWithResourceIdentity(ctx context.Context, id, callID, adapter string, command []string, execute func() ToolResult, rootIdentity func(string) (string, error)) (Snapshot, error) {
	actionIndex := -1
	var ownership []executorOwner
	checkOwnership := false
	uncertain := false
	snap, err := a.store.Update(id, func(s *Snapshot) error {
		if isStopped(s.Run.Status) {
			return nil
		}
		if s.Run.Status != StatusWaitingTool {
			return fmt.Errorf("run is not waiting for a tool")
		}
		call, ok := pendingToolCalls(s)[callID]
		if !ok {
			return fmt.Errorf("unknown pending tool call: %s", callID)
		}
		requestID := currentRequestID(s)
		var observationErr error
		uncertain, observationErr = a.toolAdmissionObservation(s, call, adapter)
		if uncertain || observationErr != nil {
			return observationErr
		}
		if reason := budgetReason(s, "action"); reason != "" {
			pauseBudget(s, reason)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		checkOwnership = adapter != "builtin" || !a.registry.permitsRunTool(Run{Responsibility: ResponsibilityCoordinator}, call.Name, adapter)
		if checkOwnership {
			var err error
			ownership, err = a.executorOwnershipLocked(s)
			if err != nil {
				return err
			}
		}
		actionIndex = len(s.Actions)
		s.Actions = append(s.Actions, Action{RequestID: requestID, CallID: callID, Name: call.Name, Adapter: adapter, Arguments: call.Arguments, Command: append([]string(nil), command...), Status: "started", StartedAt: time.Now().UTC()})
		if checkOwnership {
			localTask, candidates := "", 0
			for _, task := range s.Plan.Tasks {
				if task.ChildRunID == "" && task.Status == "running" && (!task.LocalExecution || task.LocalWorkerVisit == a.workerVisit) {
					localTask, candidates = task.ID, candidates+1
				}
			}
			if candidates == 1 {
				s.Actions[actionIndex].PlanTaskID = localTask
			}
		}
		if seconds, ok := ctx.Value(execTimeoutKey{}).(int); ok {
			s.Actions[actionIndex].TimeoutSeconds = seconds
		}
		if adapter == "exec" {
			s.Actions[actionIndex].ResourceOwner = worker.ResourceOwner(id, requestID, callID)
			// Persist the selected backend before launch. Recovery uses this root,
			// never a later operator choice of TRACE_EXEC_RESOURCE_MODE.
			s.Actions[actionIndex].ResourceRoot, _ = worker.ExecResourceRoot()
			identity, identityErr := rootIdentity(s.Actions[actionIndex].ResourceRoot)
			if s.Actions[actionIndex].ResourceRoot == worker.ProcessGroupResourceRoot && identityErr != nil {
				return fmt.Errorf("persist process-group resource identity before exec: %w", identityErr)
			}
			s.Actions[actionIndex].ResourceRootIdentity = identity
		}
		if tool, ok := a.registry.Lookup(call.Name); ok {
			if adapter == "mcp" && tool.Client != nil {
				s.Actions[actionIndex].RemoteName = tool.RemoteName
				s.Actions[actionIndex].RemoteMethod = tool.RemoteMethod
				s.Actions[actionIndex].ServerName = tool.ServerName
			}
			if adapter == "local" && tool.Local != nil {
				s.Actions[actionIndex].LocalName = tool.LocalName
			}
		}
		addEvent(s, "action_started", "工具动作开始", map[string]string{"call_id": callID, "request_id": requestID})
		if len(ownership) > 0 {
			addEvent(s, "action_executor_admitted", "工具动作已核对 Worker 执行代次", map[string]any{"call_id": callID, "request_id": requestID, "owners": ownership})
		}
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	if actionIndex < 0 || uncertain || isStopped(snap.Run.Status) {
		return snap, nil
	}
	result := execute()
	captures := []string{result.StdoutPath, result.StderrPath}
	retainedError := func(err error) error {
		var paths []string
		for _, path := range captures {
			if path != "" {
				paths = append(paths, path)
			}
		}
		if len(paths) > 0 {
			return fmt.Errorf("%w; output captures retained: %s", err, strings.Join(paths, ", "))
		}
		return err
	}
	if err := a.archiveOutput(&result); err != nil {
		return Snapshot{}, retainedError(fmt.Errorf("archive tool output: %w", err))
	}
	if adapter == "exec" && result.Diagnostic == nil {
		result.Diagnostic = scriptResultDiagnostic(result)
	}
	if _, err := a.pauseIfBudgetTime(id, ctx); err != nil {
		return Snapshot{}, retainedError(err)
	}
	snap, err = a.store.Update(id, func(s *Snapshot) error {
		action := &s.Actions[actionIndex]
		if checkOwnership {
			a.revalidateExecutorOwnershipLocked(s, action, ownership, &result)
		}
		action.Status = "completed"
		if result.Interrupted {
			action.Status = "interrupted"
		}
		action.FinishedAt = time.Now().UTC()
		action.Result = &result
		if result.Diagnostic == nil {
			result.Diagnostic = controlResultDiagnostic(s, *action)
		}
		if result.OutcomeUncertain {
			action.Status = "uncertain"
			addEvent(s, "action_uncertain", "工具执行结果不确定，交回模型核对并重新规划", map[string]string{"call_id": callID, "error": result.Error})
		}
		if ctx.Value(piDeferredResultKey{}) == true {
			if !result.Interrupted && !result.OutcomeUncertain {
				action.Status = "result_ready"
			}
			return nil
		}
		result.EvidenceID = fmt.Sprintf("ev-%d", s.Run.EventSeq+1)
		data, err := json.Marshal(result)
		if err != nil {
			return err
		}
		appendToolResult(s, callID, string(data), "executed")
		return nil
	})
	if err != nil {
		return Snapshot{}, retainedError(err)
	}
	for _, path := range captures {
		if path != "" {
			_ = os.Remove(path)
		}
	}
	if adapter == "exec" && toolCleanupError(snap) != nil {
		return a.reconcileExecResourcesLocked(id, worker.ReconcileResourceOwner)
	}
	return snap, nil
}
