package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSchedulerPersistedPredicateIsVisibleWithoutRequirements(t *testing.T) {
	a, store := newTestAgent(t, resultProvider{})
	a.SetSuccessContains("flag{verified}")
	id := createTestRun(t, a)
	s, err := a.Run(context.Background(), id)
	if err != nil || s.Result.Status != "success" {
		t.Fatalf("%+v %v", s.Result, err)
	}
	wire, _ := json.Marshal(s.Requests[0].Messages)
	if !strings.Contains(string(wire), "flag{verified}") || !strings.Contains(string(wire), "success_predicate") {
		t.Fatal("persisted predicate absent from actual model request")
	}
	v, handled, err := a.verificationBuiltin(context.Background(), id, ToolCall{Name: "verification_status", Arguments: `{}`})
	if err != nil || !handled {
		t.Fatal(err)
	}
	p := v.(map[string]any)["success_predicate"].(map[string]any)
	if p["matched"] != true || p["value"] != "flag{verified}" || p["requirement_id"] != nil {
		t.Fatalf("predicate confused with requirements: %+v", p)
	}
	// Changing the predicate must not make a stale match survive.
	s, err = store.Update(id, func(s *Snapshot) error { s.Run.SuccessContains = "not-observed"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if successPredicate(&s).(map[string]any)["matched"] != false {
		t.Fatal("stale predicate match")
	}
	s.Run.Status = StatusCompleted
	if !continueForEvidence(&s) {
		t.Fatal("unmet predicate ended run")
	}
	text := s.Messages[len(s.Messages)-1].Content
	if !strings.Contains(text, "not-observed") || !strings.Contains(text, "not evidence_check") {
		t.Fatal("completion audit hides actual contract")
	}
}

type incompleteThenInputProvider struct{ calls int }

func (p *incompleteThenInputProvider) Complete(_ context.Context, _ ModelRequest) (ModelResponse, error) {
	p.calls++
	if p.calls == 1 {
		return ModelResponse{Content: `{"trace_result":{"status":"incomplete","summary":"more work remains"}}`}, nil
	}
	return ModelResponse{ToolCalls: []ToolCall{{ID: "need-input", Name: "request_input", Arguments: `{"reason":"fixture requires operator value"}`}}}, nil
}

func TestSchedulerExplicitIncompleteDoesNotCompleteChildOrUnlockDependency(t *testing.T) {
	p := &incompleteThenInputProvider{}
	a, store := newTestAgent(t, p)
	two := 2
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &two})
	id := createTestRun(t, a)
	_, err := a.SetPlan(id, []PlanTask{{ID: "one", Goal: "inspect"}, {ID: "two", Goal: "dependent", Dependencies: []string{"one"}}})
	if err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), id, "one")
	if err != nil {
		t.Fatal(err)
	}
	defer a.StopWorkers(context.Background())
	s, err := waitAllTestWorkers(a, context.Background(), id, 10)
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Load(w.RunID)
	if err != nil || child.Run.Status != StatusPaused || child.Run.PauseReason != "model request budget exhausted" || len(child.Requests) != 2 || s.Plan.Tasks[0].Status != "paused" || len(s.Plan.Frontier) != 0 {
		t.Fatalf("incomplete child unlocked work: child=%+v plan=%+v err=%v", child.Run, s.Plan, err)
	}
}

func TestSchedulerExpiredPublicLeaseCannotBeReclaimed(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "inspect"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "work", "owner", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "work", "owner", 30); err == nil {
		t.Fatal("public claim took expired lease")
	}
}

func TestSchedulerActiveLeaseSurvivesExpiry(t *testing.T) {
	p := &resumeWorkerProvider{entered: make(chan struct{})}
	a, store, id, w := resumeWorkerFixture(t, p)
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child not started")
	}
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Second)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.RecoverWorkers(id)
	if err != nil || s.Plan.Tasks[0].Status != "running" {
		t.Fatalf("live child marked dead: %+v %v", s.Plan, err)
	}
	if err = a.renewActiveWorkerLease(context.Background(), id, w); err != nil {
		t.Fatal(err)
	}
	s, _ = store.Load(id)
	if !s.Plan.Tasks[0].LeaseUntil.After(time.Now()) {
		t.Fatal("live child not renewed")
	}
	// A renewal may enter before its deadline and wait behind another Store
	// transaction until after it; this is not loss of the executor's ownership.
	deadline := time.Now().Add(150 * time.Millisecond)
	if _, err = store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[0].LeaseUntil = deadline; return nil }); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	renewed := make(chan error, 1)
	go func() { renewed <- a.renewActiveWorkerLease(context.Background(), id, w) }()
	<-time.After(time.Until(deadline.Add(20 * time.Millisecond)))
	store.mu.Unlock()
	if err = <-renewed; err != nil {
		t.Fatalf("store wait revoked live lease: %v", err)
	}
	wrong := w
	wrong.ID = "another-owner"
	if err = a.renewActiveWorkerLease(context.Background(), id, wrong); err == nil {
		t.Fatal("wrong owner renewed")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err = a.renewActiveWorkerLease(cancelled, id, w); err == nil {
		t.Fatal("cancelled lifecycle renewed")
	}
}

type schedulerResumeProvider struct {
	entered chan context.Context
	release chan struct{}
}

func (p *schedulerResumeProvider) Complete(ctx context.Context, _ ModelRequest) (ModelResponse, error) {
	p.entered <- ctx
	select {
	case <-p.release:
		return ModelResponse{Content: "continued original child"}, nil
	case <-ctx.Done():
		return ModelResponse{}, ctx.Err()
	}
}

func TestSchedulerBuiltinResumeOutlivesToolCallAndHonorsParentPause(t *testing.T) {
	for _, pause := range []bool{false, true} {
		t.Run(map[bool]string{false: "continue", true: "parent_pause"}[pause], func(t *testing.T) {
			a, store, id, w := resumeWorkerFixture(t, &retryProvider{})
			if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
				t.Fatal(err)
			}
			p := &schedulerResumeProvider{entered: make(chan context.Context, 1), release: make(chan struct{})}
			a.provider = p
			callCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if _, err := a.builtin(callCtx, id, ToolCall{Name: "worker_resume", Arguments: `{"worker_id":"` + w.ID + `"}`}); err != nil {
				t.Fatal(err)
			}
			cancel()
			select {
			case childCtx := <-p.entered:
				if childCtx.Err() != nil {
					t.Fatal("tool call cancellation killed restored child")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("restored child did not enter provider")
			}
			if pause {
				if _, err := a.Pause(id); err != nil {
					t.Fatal(err)
				}
			} else {
				close(p.release)
			}
			if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
				t.Fatal(err)
			}
			child, err := store.Load(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if pause && (child.Run.Status != StatusPaused || child.Run.PauseReason != "parent_pause") {
				t.Fatalf("parent control lost: %+v", child.Run)
			}
			if !pause && child.Run.Status != StatusCompleted {
				t.Fatalf("restored child stopped: %+v", child.Run)
			}
		})
	}
}

func TestSchedulerBuiltinResumePreservesUnresolvedBoundaries(t *testing.T) {
	for _, scenario := range []string{"pending_tool", "uncertain_action", "cleanup_uncertain", "waiting_input", "parent_paused", "parent_cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			var provider Provider = manualProvider(ToolCall{ID: "pending", Name: "external", Arguments: `{}`})
			if scenario == "waiting_input" {
				provider = manualProvider(ToolCall{ID: "need-input", Name: "request_input", Arguments: `{"reason":"provide operator-answer"}`})
			}
			a, store, id, w := resumeWorkerFixture(t, provider)
			if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
				t.Fatal(err)
			}
			if scenario == "pending_tool" {
				// Preserve the legacy operator-paused batch; fresh unknown calls
				// now return an observation and continue the model loop.
				if _, err := store.Update(w.RunID, func(s *Snapshot) error {
					s.Run.Status, s.Run.PauseReason = StatusPaused, "operator_pause"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "uncertain_action" {
				if _, err := store.Update(w.RunID, func(s *Snapshot) error {
					s.Actions = append(s.Actions, Action{RequestID: currentRequestID(s), CallID: "pending", Name: "external", Adapter: "mcp", Arguments: `{}`, Status: "uncertain"})
					s.Run.Status, s.Run.PauseReason = StatusPaused, "parent_pause"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "cleanup_uncertain" {
				// Persist a real unresolved lifecycle receipt. A stale parent-only
				// projection is correctly repaired from a completed child receipt.
				if _, err := store.Update(w.RunID, func(s *Snapshot) error {
					s.WorkerSettlement.CleanupVersion = 1
					s.WorkerSettlement.CleanupStatus = "uncertain"
					s.WorkerSettlement.LifecycleCleanupError = "MCP transport cleanup unresolved"
					s.WorkerSettlement.CleanupError = s.WorkerSettlement.LifecycleCleanupError
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Update(id, func(s *Snapshot) error { s.Workers[0].CleanupStatus = "uncertain"; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "parent_paused" {
				if _, err := a.Pause(id); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "parent_cancelled" {
				if _, err := a.Cancel(id); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.Load(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "pending_tool" && len(pendingToolCalls(&before)) != 1 {
				t.Fatal("fixture lost its pending tool call")
			}
			if scenario == "waiting_input" && (before.Run.Status != StatusPaused || !strings.HasPrefix(before.Run.PauseReason, "waiting_input:")) {
				t.Fatal("fixture did not preserve the legacy input wait")
			}
			_, err = a.builtin(context.Background(), id, ToolCall{Name: "worker_resume", Arguments: `{"worker_id":"` + w.ID + `"}`})
			blocked := scenario != "pending_tool" && scenario != "uncertain_action"
			if blocked && err == nil {
				t.Fatal("resume bypassed control boundary")
			}
			if !blocked && err != nil {
				t.Fatal(err)
			}
			if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
				t.Fatal(err)
			}
			after, err := store.Load(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if len(after.Actions) != len(before.Actions) || len(after.Requests) != len(before.Requests) || len(pendingToolCalls(&after)) != len(pendingToolCalls(&before)) {
				t.Fatal("resume replayed an unresolved operation or requested new model work")
			}
			if scenario == "waiting_input" && (after.Run.Status != StatusPaused || after.Run.PauseReason != before.Run.PauseReason) {
				t.Fatal("input wait was lost")
			}
		})
	}
}

func TestSchedulerMalformedStructuredResultDoesNotFinishTurn(t *testing.T) {
	for _, body := range []string{`{"trace_result":null}`, `{"trace_result":"invalid"}`, `{"trace_result":{},"trace_result":{}}`, "```json\n{\"trace_result\":null}\n```"} {
		t.Run(body, func(t *testing.T) {
			p := &scriptedProvider{responses: []ModelResponse{{Content: body}, {ToolCalls: []ToolCall{{ID: "input", Name: "request_input", Arguments: `{"reason":"need an operator value"}`}}}}}
			a, _ := newTestAgent(t, p)
			two := 2
			a.ConfigureBudget(BudgetOverride{MaxModelRequests: &two})
			s, err := a.Run(context.Background(), createTestRun(t, a))
			if err != nil || s.Run.Status != StatusPaused || s.Run.PauseReason != "model request budget exhausted" || len(s.Requests) != 2 {
				t.Fatalf("invalid envelope ended work: %+v %v", s.Run, err)
			}
		})
	}
}

func TestSchedulerWorkerResumePreservesExecutedAction(t *testing.T) {
	p := &retryProvider{afterAction: true}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "inspect"}}); err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), id, "work")
	if err != nil {
		t.Fatal(err)
	}
	defer a.StopWorkers(context.Background())
	s, err := waitAllTestWorkers(a, context.Background(), id, 10)
	if err != nil || s.Workers[0].Status != "failed" {
		t.Fatalf("missing failed fixture: %+v %v", s.Workers, err)
	}
	child, _ := store.Load(w.RunID)
	if len(child.Actions) != 1 {
		t.Fatal("fixture did not execute one action")
	}
	if _, err = a.RetryTask(id, "work"); err == nil {
		t.Fatal("executed task recreated")
	}
	a.provider = resultProvider{}
	v, err := a.builtin(context.Background(), id, ToolCall{Name: "worker_resume", Arguments: `{"worker_id":"` + w.ID + `"}`})
	if err != nil {
		t.Fatal(err)
	}
	resumed := v.(AgentWorker)
	if resumed.ID != w.ID || resumed.RunID != w.RunID {
		t.Fatal("resume created another attempt")
	}
	s, err = waitAllTestWorkers(a, context.Background(), id, 10)
	if err != nil {
		t.Fatal(err)
	}
	child, _ = store.Load(w.RunID)
	if child.Result.Status != "success" || len(child.Actions) != 1 || len(s.Workers) != 1 {
		t.Fatalf("resume replayed action or lost result: %+v actions=%d workers=%d", child.Result, len(child.Actions), len(s.Workers))
	}
	if _, err = a.ResumeWorker(context.Background(), id, w.ID); err == nil {
		t.Fatal("completed worker resumed")
	}
	if _, err = a.ResumeWorker(context.Background(), id, "parent"); err == nil {
		t.Fatal("parent accepted as child")
	}
}

func TestSchedulerHeartbeatFailureRecordsCause(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	_, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "inspect"}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := a.Create("inspect")
	if err != nil {
		t.Fatal(err)
	}
	w := AgentWorker{ID: "live-owner", TaskID: "work", RunID: child.Run.ID, Status: "running"}
	_, err = store.Update(id, func(s *Snapshot) error {
		s.Plan.Tasks[0].Status = "running"
		s.Plan.Tasks[0].WorkerID = "lost-owner"
		s.Plan.Tasks[0].ChildRunID = child.Run.ID
		s.Workers = []AgentWorker{w}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(child.Run.ID, func(s *Snapshot) error { s.Run.ParentRunID = id; s.Run.ParentTaskID = "work"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_, err = a.runChildOperation(ctx, id, w, func(childAgent *Agent, ctx context.Context, runID string) (Snapshot, error) {
		<-ctx.Done()
		return childAgent.fail(runID, ctx.Err())
	})
	if err == nil || !strings.Contains(err.Error(), "task ownership lost") {
		t.Fatalf("lost cause: %v", err)
	}
	s, err := store.Load(child.Run.ID)
	if err != nil || !strings.Contains(s.Run.Error, "task ownership lost") {
		t.Fatalf("cause not durable: %s %v", s.Run.Error, err)
	}
}
