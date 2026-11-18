package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type resumeWorkerProvider struct {
	mu               sync.Mutex
	entered          chan struct{}
	blockAfterAction bool
	inputWait        bool
	blocked          bool
	parentCalls      int
	pauseParent      func() error
}

func (p *resumeWorkerProvider) Complete(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	child := false
	for _, m := range req.Messages {
		child = child || strings.HasPrefix(m.Content, "Worker task context (")
	}
	if !child {
		p.mu.Lock()
		p.parentCalls++
		n := p.parentCalls
		p.mu.Unlock()
		if n == 1 {
			return ModelResponse{ToolCalls: []ToolCall{{ID: "wait", Name: "worker_wait", Arguments: `{"seconds":5}`}}}, nil
		}
		// The fixture operator stops after the dependency settles; request_input
		// now returns an observation and is deliberately not a stop mechanism.
		if p.pauseParent == nil {
			return ModelResponse{}, fmt.Errorf("missing fixture operator pause")
		}
		return ModelResponse{}, p.pauseParent()
	}
	hasAction, hasInputWait, hasAnswer := false, false, false
	for _, m := range req.Messages {
		hasAction = hasAction || m.Role == "tool" && m.ToolCallID == "once"
		hasInputWait = hasInputWait || m.Role == "tool" && m.ToolCallID == "need-input"
		hasAnswer = hasAnswer || m.Role == "user" && strings.Contains(m.Content, "operator-answer")
	}
	if p.inputWait && !hasInputWait {
		return ModelResponse{ToolCalls: []ToolCall{{ID: "need-input", Name: "request_input", Arguments: `{"reason":"provide operator-answer"}`}}}, nil
	}
	if p.inputWait {
		if !hasAnswer {
			return ModelResponse{}, fmt.Errorf("input wait resumed without input")
		}
		return ModelResponse{Content: fixtureReport("input supplied")}, nil
	}
	p.mu.Lock()
	block := !p.blocked && (hasAction || !p.blockAfterAction)
	if block {
		p.blocked = true
		close(p.entered)
	}
	p.mu.Unlock()
	if block {
		<-ctx.Done()
		return ModelResponse{}, ctx.Err()
	}
	if !hasAction {
		return ModelResponse{ToolCalls: []ToolCall{{ID: "once", Name: "read", Arguments: `{"path":"missing-resume-fixture"}`}}}, nil
	}
	return ModelResponse{Content: fixtureReport("worker completed")}, nil
}

func resumeWorkerFixture(t *testing.T, p Provider) (*Agent, *Store, string, AgentWorker) {
	t.Helper()
	_, scripted := p.(*scriptedProvider)
	resume, isResume := p.(*resumeWorkerProvider)
	if scripted || isResume && resume.inputWait {
		// Build a legacy stopped boundary through the normal settlement writer.
		// No live request_input/unknown-tool call is expected to pause today.
		a, store, id, w := settlementFixture(t)
		a.provider = p
		if _, err := store.Update(id, func(s *Snapshot) error {
			s.Plan.Tasks[0].Goal = "continue original task"
			s.Plan.Tasks[1].Goal = "consume dependency"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		unlock, err := store.AcquireRunLock(w.RunID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = a.runChildOperation(context.Background(), id, w, func(child *Agent, _ context.Context, runID string) (Snapshot, error) {
			waitTestRun(t, child, runID)
			return store.Update(runID, func(s *Snapshot) error {
				for _, call := range latestPiCalls(s) {
					if call.Name == "request_input" {
						appendToolResult(s, call.ID, `{"input_requested":true}`, "executed")
						s.Run.Status, s.Run.PauseReason = StatusPaused, "waiting_input: legacy fixture"
					}
				}
				return nil
			})
		})
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.StopWorkers(context.Background()); a.CloseMCP() })
		return a, store, id, w
	}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	if isResume {
		resume.pauseParent = func() error { _, err := a.Pause(id); return err }
	}
	if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "continue original task"}, {ID: "next", Goal: "consume dependency", Dependencies: []string{"work"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), id, "work")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.StopWorkers(context.Background()); a.CloseMCP() })
	return a, store, id, w
}

func TestWorkerResumeParentPausePreservesChildAndActions(t *testing.T) {
	for _, afterAction := range []bool{false, true} {
		t.Run(fmt.Sprint(afterAction), func(t *testing.T) {
			p := &resumeWorkerProvider{entered: make(chan struct{}), blockAfterAction: afterAction}
			a, store, id, w := resumeWorkerFixture(t, p)
			select {
			case <-p.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("child did not reach pause boundary")
			}
			if _, err := a.Pause(id); err != nil {
				t.Fatal(err)
			}
			if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
				t.Fatal(err)
			}
			child, err := store.Load(w.RunID)
			if err != nil || child.Run.Status != StatusPaused || child.Run.PauseReason != "parent_pause" {
				t.Fatalf("child not resumable: %+v %v", child.Run, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := a.Run(ctx, id); err != nil {
				t.Fatal(err)
			}
			parent, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(parent.Workers) != 1 || parent.Workers[0].RunID != w.RunID || parent.Workers[0].ID != w.ID || parent.Plan.Tasks[0].Status != "completed" || !hasString(parent.Plan.Frontier, "next") {
				t.Fatalf("parent not settled: %+v %+v", parent.Workers, parent.Plan)
			}
			child, _ = store.Load(w.RunID)
			if child.Run.Status != StatusCompleted || len(child.Actions) != 1 || child.Actions[0].CallID != "once" {
				t.Fatalf("child replayed or replaced: %+v %+v", child.Run, child.Actions)
			}
		})
	}
}

func TestWorkerResumeInputWaitUsesExistingChild(t *testing.T) {
	p := manualProvider(ToolCall{ID: "need-input", Name: "request_input", Arguments: `{"reason":"provide operator-answer"}`})
	a, store, id, w := resumeWorkerFixture(t, p)
	if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Load(w.RunID)
	if !strings.HasPrefix(before.Run.PauseReason, "waiting_input:") {
		t.Fatal("child did not request input")
	}
	if err := a.resumePausedWorkers(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	after, _ := store.Load(w.RunID)
	if len(after.Requests) != len(before.Requests) {
		t.Fatal("automatic resume skipped missing input")
	}
	if _, err := a.QueueMessage(w.RunID, "steer", "operator-answer"); err != nil {
		t.Fatal(err)
	}
	unlock, err := store.AcquireRunLock(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), w.RunID); err == nil {
		t.Fatal("concurrent child resume acquired a second execution owner")
	}
	unlock()
	if _, err := a.Run(context.Background(), w.RunID); err != nil {
		t.Fatal(err)
	}
	parent, _ := store.Load(id)
	if parent.Plan.Tasks[0].Status != "completed" || !hasString(parent.Plan.Frontier, "next") || len(parent.Workers) != 1 {
		t.Fatalf("direct child resume lost settlement: %+v", parent.Plan)
	}
}

func TestWorkerResumeInputFollowUpDelivered(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(fmt.Sprint(automatic), func(t *testing.T) {
			p := manualProvider(ToolCall{ID: "need-input", Name: "request_input", Arguments: `{"reason":"provide operator-answer"}`})
			a, store, id, w := resumeWorkerFixture(t, p)
			if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
				t.Fatal(err)
			}
			if _, err := a.QueueMessage(w.RunID, "follow_up", "operator-answer"); err != nil {
				t.Fatal(err)
			}
			if automatic {
				if err := a.resumePausedWorkers(context.Background(), id); err != nil {
					t.Fatal(err)
				}
				if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
					t.Fatal(err)
				}
			} else if _, err := a.Run(context.Background(), w.RunID); err != nil {
				t.Fatal(err)
			}
			child, err := store.Load(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if child.Run.Status != StatusCompleted || len(child.Requests) != 2 || len(child.Mailbox) != 1 || child.Mailbox[0].ConsumedAt.IsZero() {
				t.Fatalf("follow-up not delivered: %+v %+v", child.Run, child.Mailbox)
			}
			deliveries := 0
			for _, m := range child.Requests[1].Messages {
				if m.Role == "user" && m.Content == "operator-answer" {
					deliveries++
				}
			}
			if deliveries != 1 {
				t.Fatalf("answer deliveries=%d", deliveries)
			}
			parent, _ := store.Load(id)
			if parent.Plan.Tasks[0].Status != "completed" || !hasString(parent.Plan.Frontier, "next") {
				t.Fatal("answered child did not settle parent")
			}
		})
	}
}

func TestWorkerResumeCapacityAdmission(t *testing.T) {
	p := manualProvider(ToolCall{ID: "need-input", Name: "request_input", Arguments: `{"reason":"provide operator-answer"}`})
	a, store, id, w := resumeWorkerFixture(t, p)
	if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	parent, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	tasks := append(parent.Plan.Tasks, PlanTask{ID: "other", Goal: "hold the only worker slot"})
	if _, err := a.SetPlan(id, tasks); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlanCapacity(id, 1); err != nil {
		t.Fatal(err)
	}
	block := &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a.provider = block
	other, err := a.DispatchTask(context.Background(), id, "other")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-block.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("sibling did not start")
	}
	if _, err := a.QueueMessage(w.RunID, "follow_up", "operator-answer"); err != nil {
		t.Fatal(err)
	}
	if err := a.resumePausedWorkers(context.Background(), id); err != nil {
		t.Fatalf("capacity must defer automatic resume: %v", err)
	}
	if _, err := a.Run(context.Background(), w.RunID); !errors.Is(err, errWorkerCapacity) {
		t.Fatalf("direct resume exceeded capacity: %v", err)
	}
	parent, _ = store.Load(id)
	child, _ := store.Load(w.RunID)
	if parent.Run.Status != StatusRunning || parent.Plan.Tasks[0].Status != "paused" || parent.Workers[0].Status != "paused" || child.Run.Status != StatusPaused || len(child.Requests) != 1 || len(parent.Workers) != 2 {
		t.Fatal("capacity refusal mutated execution identity/status")
	}
	if _, err := a.CancelWorker(id, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	a.provider = p
	if err := a.resumePausedWorkers(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	parent, _ = store.Load(id)
	if parent.Plan.Tasks[0].Status != "completed" || parent.Workers[0].RunID != w.RunID || len(parent.Workers) != 2 {
		t.Fatal("original worker did not resume when capacity became available")
	}
}

func TestWorkerResumeAncestorBudgetKeepsOwnBudget(t *testing.T) {
	p := &resumeWorkerProvider{entered: make(chan struct{})}
	a, store := newTestAgent(t, p)
	a.ConfigureBudget(BudgetOverride{MaxSeconds: int64Pointer(60)})
	id := createTestRun(t, a)
	p.pauseParent = func() error { _, err := a.Pause(id); return err }
	if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "continue after ancestor budget"}, {ID: "next", Goal: "dependency", Dependencies: []string{"work"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusRunning
		s.Run.Budget.MaxSeconds = 1
		s.Run.ElapsedMillis = 500
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, stop, err := a.beginBudgetContext(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	w, err := a.DispatchTask(ctx, id, "work")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.StopWorkers(context.Background()); a.CloseMCP() })
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not start")
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("parent budget did not expire")
	}
	if paused, err := a.pauseIfBudgetTime(id, ctx); err != nil || !paused {
		t.Fatalf("parent budget not recognized: %v", err)
	}
	stop()
	if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	child, err := store.Load(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Run.PauseReason != "parent_pause" || child.Run.Budget.MaxSeconds != 60 || child.Run.ElapsedMillis >= 60000 {
		t.Fatalf("ancestor budget misclassified as child budget: %+v", child.Run)
	}
	resumeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := a.Run(resumeCtx, id); err != nil {
		t.Fatal(err)
	}
	parent, _ := store.Load(id)
	if parent.Plan.Tasks[0].Status != "completed" || parent.Workers[0].RunID != w.RunID {
		t.Fatal("parent budget extension did not resume existing child")
	}
}

func TestWorkerResumeDoesNotBypassOwnPause(t *testing.T) {
	for _, reason := range []string{"waiting_input: supply target", "model request budget exhausted", "context budget: increase limit", "operator_pause"} {
		t.Run(reason, func(t *testing.T) {
			a, store, id, w := resumeWorkerFixture(t, manualProvider(ToolCall{ID: "hold", Name: "external", Arguments: `{}`}))
			if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
				t.Fatal(err)
			}
			_, err := store.Update(w.RunID, func(s *Snapshot) error {
				s.Messages = nil
				s.Run.Status, s.Run.PauseReason = StatusPaused, reason
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Update(id, func(s *Snapshot) error { s.Workers[0].Status = "paused"; s.Plan.Tasks[0].Status = "paused"; return nil })
			if err != nil {
				t.Fatal(err)
			}
			if err := a.resumePausedWorkers(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			child, err := store.Load(w.RunID)
			if err != nil || child.Run.Status != StatusPaused || child.Run.PauseReason != reason || len(child.Requests) != 1 {
				t.Fatalf("child pause bypassed: %+v %v", child.Run, err)
			}
		})
	}
}

func TestWorkerResumeUncertainReturnsObservationWithoutReplay(t *testing.T) {
	a, store, id, w := resumeWorkerFixture(t, manualProvider(ToolCall{ID: "uncertain", Name: "external", Arguments: `{}`}))
	if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	// This is the same persisted boundary written after a lost MCP reply:
	// execution intent exists, its pending call has no settled tool result.
	_, err := store.Update(w.RunID, func(s *Snapshot) error {
		s.Actions = append(s.Actions, Action{RequestID: currentRequestID(s), CallID: "uncertain", Name: "external", Adapter: "mcp", Arguments: `{}`, Status: "uncertain"})
		s.Run.Status, s.Run.PauseReason = StatusPaused, "parent_pause"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	store.Update(id, func(s *Snapshot) error { s.Workers[0].Status = "paused"; s.Plan.Tasks[0].Status = "paused"; return nil })
	if err := a.resumePausedWorkers(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	child, _ := store.Load(w.RunID)
	if child.Run.Status != StatusPaused || len(child.Actions) != 1 {
		t.Fatal("automatic resume crossed uncertain action")
	}
	if _, err := a.Run(context.Background(), w.RunID); err != nil {
		t.Fatal(err)
	}
	child, _ = store.Load(w.RunID)
	if child.Run.Status != StatusWaitingTool || len(pendingToolCalls(&child)) != 1 || len(child.Actions) != 1 || child.Actions[0].Result != nil || len(child.Evidence) != 0 {
		t.Fatal("first resume did not retain the paused pending boundary")
	}
	// The first explicit resume restores the legacy paused boundary. A second
	// scheduling turn returns its historical outcome, without executing it.
	if _, err := a.Run(context.Background(), w.RunID); err != nil {
		t.Fatal(err)
	}
	child, _ = store.Load(w.RunID)
	if child.Run.Status != StatusCompleted || len(child.Actions) != 1 || child.Actions[0].Result != nil || !child.Actions[0].FinishedAt.IsZero() {
		t.Fatal("explicit resume replayed or failed to return historical outcome")
	}
	child, _ = store.Load(w.RunID)
	parent, _ := store.Load(id)
	if len(child.Actions) != 1 || child.Actions[0].Status != "uncertain" || parent.Plan.Tasks[0].Status != "completed" || len(child.Evidence) != 1 || child.Evidence[0].Type != "runtime_observation" {
		t.Fatalf("runtime observation settlement failed: %+v %+v", child.Actions, parent.Plan)
	}
	for _, ev := range child.Evidence {
		if ev.Verification == "verified" {
			t.Fatal("manual observation promoted to verified")
		}
	}
}

func TestWorkerResumeInputBatchPreservesAnswerUntilToolsSettle(t *testing.T) {
	p := manualProvider(
		ToolCall{ID: "need-input", Name: "request_input", Arguments: `{"reason":"provide operator-answer"}`},
		ToolCall{ID: "external", Name: "read", Arguments: `{"path":"pending-tool-must-not-run"}`},
	)
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	waitTestRun(t, a, id)
	// Legacy archive: request_input was observed, the remaining read was not.
	if _, err := store.Update(id, func(s *Snapshot) error {
		appendToolResult(s, "need-input", `{"input_requested":true}`, "executed")
		s.Run.Status, s.Run.PauseReason = StatusPaused, "waiting_input: legacy fixture"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.QueueMessage(id, "follow_up", "operator-answer"); err != nil {
		t.Fatal(err)
	}
	s, err := a.Run(context.Background(), id)
	if err != nil || s.Run.Status != StatusWaitingTool || !s.Mailbox[0].ConsumedAt.IsZero() {
		t.Fatalf("legacy paused batch consumed the answer before settlement: %+v %v", s.Run, err)
	}
	if _, err := a.executePendingTool(context.Background(), id, ToolCall{ID: "external", Name: "read", Arguments: `{"path":"pending-tool-must-not-run"}`}); err != nil {
		t.Fatal(err)
	}
	s, err = a.Run(context.Background(), id)
	if err != nil || s.Run.Status != StatusCompleted || len(s.Requests) != 2 || len(s.Actions) != 1 || s.Mailbox[0].ConsumedAt.IsZero() || s.Run.PauseReason != "" {
		t.Fatalf("settled batch did not resume: %+v %v", s.Run, err)
	}
	toolIndex, answerIndex, answers := -1, -1, 0
	for i, m := range s.Requests[1].Messages {
		if m.Role == "tool" && m.ToolCallID == "external" {
			toolIndex = i
		}
		if m.Role == "user" && m.Content == "operator-answer" {
			answerIndex = i
			answers++
		}
	}
	if toolIndex < 0 || answerIndex <= toolIndex || answers != 1 {
		t.Fatalf("incorrect tool/answer delivery order: tool=%d answer=%d count=%d", toolIndex, answerIndex, answers)
	}
}

func TestWorkerInputRequestContinuesWithoutOperator(t *testing.T) {
	p := manualProvider(ToolCall{ID: "input", Name: "request_input", Arguments: `{"reason":"missing target detail"}`})
	a, _ := newTestAgent(t, p)
	s, err := a.Run(context.Background(), createTestRun(t, a))
	if err != nil || s.Run.Status != StatusCompleted || len(p.requests) != 2 || len(s.Actions) != 1 || len(pendingToolCalls(&s)) != 0 || s.Run.PauseReason != "" {
		t.Fatalf("input request stopped automatic continuation: %+v %v", s.Run, err)
	}
	if s.Actions[0].Result == nil || !strings.Contains(s.Actions[0].Result.Stdout, "input_unavailable") {
		t.Fatal("model did not receive missing-input observation")
	}
}
