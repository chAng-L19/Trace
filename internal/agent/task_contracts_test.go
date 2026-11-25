package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func taskRequirement(id string) VerificationRequirement {
	return VerificationRequirement{ID: id, Description: "probe reports exact JSON success", Category: "goal", ToolID: "exec", Predicate: EvidencePredicate{Kind: "json_equals", Pointer: "/ok", Expected: true}}
}

func TestTaskContractValidationAndRetryImmutability(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	task := PlanTask{ID: "probe", Goal: "probe target", Requirements: []VerificationRequirement{taskRequirement("proof")}}
	invalid := task
	invalid.Requirements = []VerificationRequirement{{ID: "bad"}}
	if _, err := a.SetPlan(id, []PlanTask{invalid}); err == nil {
		t.Fatal("invalid structured contract accepted")
	}
	if _, err := a.SetPlan(id, []PlanTask{task}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, task.ID, "manual", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompleteTask(id, task.ID, "manual", "failed", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RetryTask(id, task.ID); err != nil {
		t.Fatal(err)
	}
	weakened := task
	weakened.Requirements = nil
	if _, err := a.SetPlan(id, []PlanTask{weakened}); err == nil {
		t.Fatal("retried contract weakened")
	}
	if _, err := a.SetPlan(id, nil); err == nil {
		t.Fatal("retried task removed")
	}
	// Legacy snapshots have no Started field; claim/retry events retain identity.
	if _, err := store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[0].Started = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlan(id, []PlanTask{weakened}); err == nil {
		t.Fatal("legacy retry contract weakened")
	}
	s, err := a.SetPlan(id, []PlanTask{task})
	if err != nil || !sameTaskRequirements(s.Plan.Tasks[0].Requirements, task.Requirements) {
		t.Fatalf("contract reload: %+v %v", s.Plan, err)
	}
}

// This provider exercises actual child exec -> evidence_check -> final text.
// It has no mutable counter, so independently dispatched children can share it.
type taskContractProvider struct{}

func (taskContractProvider) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	requirementID := "second-proof"
	for _, message := range request.Messages {
		if strings.Contains(message.Content, `"id":"first-proof"`) {
			requirementID = "first-proof"
		}
	}
	last := lastConversationMessage(request)
	if last.Role != "tool" {
		return ModelResponse{ToolCalls: []ToolCall{{ID: "probe", Name: "exec", Arguments: `{"command":"printf '{\"ok\":true}'"}`}}, StopReason: "tool_calls"}, nil
	}
	var result ToolResult
	if last.ToolCallID == "probe" && json.Unmarshal([]byte(last.Content), &result) == nil && result.EvidenceID != "" {
		arguments, _ := json.Marshal(map[string]any{"requirement_id": requirementID, "evidence_ids": []string{result.EvidenceID}})
		return ModelResponse{ToolCalls: []ToolCall{{ID: "check", Name: "evidence_check", Arguments: string(arguments)}}, StopReason: "tool_calls"}, nil
	}
	return ModelResponse{Content: fixtureReport("Probe and its declared evidence check finished."), StopReason: "stop"}, nil
}

func TestTaskContractsReachIsolatedChildrenAndRevalidate(t *testing.T) {
	a, store := newTestAgent(t, taskContractProvider{})
	defer a.CloseMCP()
	id := createTestRun(t, a)
	tasks := []PlanTask{
		{ID: "first", Goal: "first target", Requirements: []VerificationRequirement{taskRequirement("first-proof")}},
		{ID: "second", Goal: "second target", Requirements: []VerificationRequirement{taskRequirement("second-proof")}},
	}
	if _, err := a.SetPlan(id, tasks); err != nil {
		t.Fatal(err)
	}
	workers, err := a.DispatchFrontier(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	s, err := waitAllTestWorkers(a, context.Background(), id, 10)
	if err != nil || len(workers) != 2 {
		t.Fatalf("workers: %+v %v", workers, err)
	}
	for _, worker := range workers {
		child, err := store.Load(worker.RunID)
		if err != nil {
			t.Fatal(err)
		}
		task, _ := planTask(&s, worker.TaskID)
		if len(child.Requirements) != 1 || child.Requirements[0].ID != worker.TaskID+"-proof" || task.Status != "verified" || !taskDone(*task) {
			t.Fatalf("child contract leaked or failed: task=%+v child=%+v", task, child.Result)
		}
		foreign, _ := planTask(&s, map[string]string{"first": "second", "second": "first"}[worker.TaskID])
		if taskRequirementsSatisfied(&child, *foreign) {
			t.Fatal("sibling requirement accepted")
		}
		for _, mutation := range []string{"wrong_tool", "wrong_arguments", "string_coincidence"} {
			bad := *task
			bad.Requirements = append([]VerificationRequirement(nil), task.Requirements...)
			switch mutation {
			case "wrong_tool":
				bad.Requirements[0].ToolID = "read"
			case "wrong_arguments":
				bad.Requirements[0].ArgumentsHash = strings.Repeat("0", 64)
			case "string_coincidence":
				bad.Requirements[0].Predicate.Expected = "true"
			}
			if taskRequirementsSatisfied(&child, bad) {
				t.Fatalf("accepted %s", mutation)
			}
			check := latestCheck(&child, task.Requirements[0].ID)
			state, _ := checkPredicate(&child, bad.Requirements[0].Predicate, check.EvidenceIDs, bad.Requirements[0].ToolID, bad.Requirements[0].ArgumentsHash, 0)
			if state == "pass" {
				t.Fatalf("probe accepted %s", mutation)
			}
		}
	}
	if _, err := store.Update(workers[0].RunID, func(child *Snapshot) error { child.Evidence[0].Content += "tampered"; return nil }); err != nil {
		t.Fatal(err)
	}
	s, err = store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	task, _ := planTask(&s, workers[0].TaskID)
	if task.Status != "uncertain" || taskDone(*task) {
		t.Fatal("task survived invalid child proof")
	}
}

func TestTaskDispositionPersistsWithoutUnlockingFailedDependencies(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	tasks := []PlanTask{{ID: "failed", Goal: "effectful task"}, {ID: "dependent", Goal: "consume result", Dependencies: []string{"failed"}}}
	if _, err := a.SetPlan(id, tasks); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "failed", "manual", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompleteTask(id, "failed", "manual", "failed", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[0].Error = "original business failure"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.builtin(context.Background(), id, ToolCall{Name: "task_dispose", Arguments: `{"task_id":"failed","status":"blocked","reason":"target unavailable"}`}); err != nil {
		t.Fatal(err)
	}
	s, err := store.Load(id)
	if err != nil || taskDone(s.Plan.Tasks[0]) || !taskTerminal(s.Plan.Tasks[0]) || len(s.Plan.Frontier) != 0 {
		t.Fatalf("invalid disposed state: %+v %v", s.Plan, err)
	}
	if _, err := a.ClaimTask(id, "dependent", "other", 30); err == nil {
		t.Fatal("failed dependency unlocked")
	}
	if _, err := a.SetPlan(id, tasks[1:]); err == nil {
		t.Fatal("disposed task deleted")
	}
	if _, err := a.SetPlan(id, tasks); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RecoverWorkers(id); err != nil {
		t.Fatal(err)
	}
	s, _ = store.Load(id)
	if s.Plan.Tasks[0].Status != "blocked" || s.Plan.Tasks[0].PreviousStatus != "failed" || s.Plan.Tasks[0].Error != "original business failure" {
		t.Fatalf("history lost: %+v", s.Plan.Tasks[0])
	}
	s.Run.Status = StatusCompleted
	if planDispositionFailure(&s) != "" {
		t.Fatal("unresolved dependent silently skipped")
	}
	if _, err := a.DisposeTask(id, "dependent", "abandoned", "prerequisite closed", ""); err != nil {
		t.Fatal(err)
	}
	s, _ = store.Load(id)
	s.Run.Status, s.Run.RequireVerifiedCompletion = StatusCompleted, true
	s.Requirements = []VerificationRequirement{taskRequirement("unmet-root")}
	s.Messages = append(s.Messages, Message{Role: "assistant", Content: `{"trace_result":{"status":"success"}}`})
	messageCount := len(s.Messages)
	continueForEvidence(&s)
	if s.Run.Status != StatusFailed || s.Result.Status != "failed" || len(s.Messages) != messageCount || len(unmetRequirements(&s)) != 1 {
		t.Fatalf("disposed plan renewed or claimed success: %+v", s.Result)
	}
	if err := a.checkWorkerAncestors(&s); err == nil {
		t.Fatal("terminal disposed plan admitted another model run")
	}
}

func TestTaskReplacementPreservesContractAndRequiresDependencyReplan(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	tasks := []PlanTask{{ID: "old", Goal: "same goal"}, {ID: "replacement", Goal: "same goal"}, {ID: "next", Goal: "consume", Dependencies: []string{"old"}}}
	if _, err := a.SetPlan(id, tasks); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DisposeTask(id, "old", "superseded", "new approach", "replacement"); err != nil {
		t.Fatal(err)
	}
	weakened := append([]PlanTask(nil), tasks...)
	weakened[1].Goal = "unrelated easy goal"
	if _, err := a.SetPlan(id, weakened); err == nil {
		t.Fatal("replacement goal weakened")
	}
	cycle := append([]PlanTask(nil), tasks...)
	cycle[1].Dependencies = []string{"old"}
	if _, err := a.SetPlan(id, cycle); err == nil {
		t.Fatal("replacement dependency cycle accepted")
	}
	if _, err := a.DisposeTask(id, "replacement", "superseded", "cycle", "old"); err == nil {
		t.Fatal("replacement cycle accepted")
	}
	if _, err := a.ClaimTask(id, "replacement", "manual", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompleteTask(id, "replacement", "manual", "completed", nil); err != nil {
		t.Fatal(err)
	}
	s, _ := store.Load(id)
	if !planTaskSatisfied(&s.Plan, s.Plan.Tasks[0], map[string]bool{}) || taskDone(s.Plan.Tasks[0]) || hasString(s.Plan.Frontier, "next") {
		t.Fatal("replacement conflated with old dependency")
	}
	tasks[2].Dependencies = []string{"replacement"}
	s, err := a.SetPlan(id, tasks)
	if err != nil || !hasString(s.Plan.Frontier, "next") {
		t.Fatalf("explicit replan failed: %+v %v", s.Plan, err)
	}
	if _, err := a.DisposeTask(id, "old", "abandoned", "erase replacement", ""); err == nil {
		t.Fatal("disposition rewritten")
	}
	strict := PlanTask{Goal: "same goal", SuccessContains: "proof", Requirements: []VerificationRequirement{taskRequirement("proof")}}
	weak := strict
	weak.Requirements = nil
	if taskContractCovers(weak, strict) {
		t.Fatal("replacement removed structured requirements")
	}
	weak = strict
	weak.SuccessContains = ""
	if taskContractCovers(weak, strict) {
		t.Fatal("replacement removed success predicate")
	}
}

func TestTaskDispositionRejectsLiveUnknownAndPreservesFailedChild(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "task", Goal: "failed effect"}}); err != nil {
		t.Fatal(err)
	}
	child, err := a.Create("failed effect")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(child.Run.ID, func(s *Snapshot) error {
		s.Run.ParentRunID, s.Run.ParentTaskID, s.Run.Status = id, "task", StatusFailed
		s.Run.Error = "business failure after effect"
		s.Actions = []Action{{CallID: "committed-effect", Status: "completed"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Plan.Tasks[0].Status, s.Plan.Tasks[0].WorkerID, s.Plan.Tasks[0].ChildRunID = "failed", "worker", child.Run.ID
		s.Workers = []AgentWorker{{ID: "worker", TaskID: "task", RunID: child.Run.ID, Status: "failed", CleanupStatus: "unknown"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DisposeTask(id, "task", "blocked", "effect failed", ""); err == nil {
		t.Fatal("unknown cleanup disposed")
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Workers[0].CleanupStatus = "completed"; return nil }); err != nil {
		t.Fatal(err)
	}
	unlock, err := store.AcquireRunLock(child.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DisposeTask(id, "task", "blocked", "effect failed", ""); err == nil {
		t.Fatal("active child disposed")
	}
	unlock()
	if _, err := a.DisposeTask(id, "task", "blocked", "effect failed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ResumeWorker(context.Background(), id, "worker"); err == nil {
		t.Fatal("disposed child resumed")
	}
	if _, err := a.RetryTask(id, "task"); err == nil {
		t.Fatal("disposed effects replayed")
	}
	after, err := store.Load(child.Run.ID)
	if err != nil || len(after.Actions) != 1 || after.Run.Error != "business failure after effect" || after.Run.Status != StatusFailed {
		t.Fatalf("original child lost: %+v %v", after.Run, err)
	}
}

func TestUnregisteredUncertainTaskRetainsCapacity(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "old", Goal: "claim"}, {ID: "new", Goal: "next"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlanCapacity(id, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "old", "unregistered", 30); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Second); return nil }); err != nil {
		t.Fatal(err)
	}
	s, err := a.RecoverWorkers(id)
	if err != nil || occupiedWorkerSlots(&s) != 1 {
		t.Fatalf("unknown claim released slot: %+v %v", s.Plan, err)
	}
	if _, err := a.ClaimTask(id, "new", "other", 30); err == nil {
		t.Fatal("unknown claim allowed oversubscription")
	}
	if _, err := a.DisposeTask(id, "old", "abandoned", "unknown", ""); err == nil {
		t.Fatal("unknown claim disposed")
	}
}

func TestLegacyCompletedFailureAndCancelledTaskCanBeDisposed(t *testing.T) {
	for _, outcome := range []string{"failed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			a, store := newTestAgent(t, MockProvider{})
			id := createTestRun(t, a)
			if _, err := a.SetPlan(id, []PlanTask{{ID: "task", Goal: "legacy goal"}}); err != nil {
				t.Fatal(err)
			}
			child, err := a.Create("legacy goal")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(child.Run.ID, func(s *Snapshot) error {
				s.Run.ParentRunID, s.Run.ParentTaskID, s.Run.Status = id, "task", StatusCompleted
				s.Messages = append(s.Messages, Message{Role: "assistant", Content: `{"trace_result":{"status":"failed","summary":"original failure"}}`})
				if outcome == "cancelled" {
					s.Run.Status = StatusCancelled
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			s, err := store.Update(id, func(s *Snapshot) error {
				s.Plan.Tasks[0].Status, s.Plan.Tasks[0].WorkerID, s.Plan.Tasks[0].ChildRunID = "completed", "worker", child.Run.ID
				s.Workers = []AgentWorker{{ID: "worker", TaskID: "task", RunID: child.Run.ID, Status: "completed", CleanupStatus: "completed"}}
				if outcome == "cancelled" {
					s.Plan.Tasks[0].Status, s.Workers[0].Status = "cancelled", "cancelled"
				}
				return nil
			})
			if err != nil || taskDone(s.Plan.Tasks[0]) {
				t.Fatalf("old business failure accepted: %+v %v", s.Plan, err)
			}
			if _, err := a.DisposeTask(id, "task", "abandoned", "legacy attempt inspected", ""); err != nil {
				t.Fatal(err)
			}
			after, err := store.Load(child.Run.ID)
			if err != nil || after.Result.Status != outcome {
				t.Fatalf("child outcome changed: %+v %v", after.Result, err)
			}
		})
	}
}

func TestTaskDispositionFinishTurnIsFailureWithoutContinuation(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "task", Goal: "unmet goal"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DisposeTask(id, "task", "abandoned", "no viable path", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	end, err := a.finishPiTurn(context.Background(), id, ModelRequest{ID: "final"}, ModelResponse{Content: "finished"})
	if err != nil || !end {
		t.Fatalf("finish: end=%v %v", end, err)
	}
	s, err := store.Load(id)
	if err != nil || s.Run.Status != StatusFailed || s.Result.Status != "failed" {
		t.Fatalf("root not failed: %+v %v", s.Result, err)
	}
	for _, e := range s.Events {
		if e.Type == "run_completed" || e.Type == "completion_deferred" {
			t.Fatalf("false completion/continuation: %s", e.Type)
		}
	}
}

func TestTaskDispositionTerminalSurvivesAddedPendingTask(t *testing.T) {
	provider := &modelRetryProvider{}
	a, store := newTestAgent(t, provider)
	id := createTestRun(t, a)
	task := PlanTask{ID: "closed", Goal: "original unmet goal"}
	if _, err := a.SetPlan(id, []PlanTask{task}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DisposeTask(id, task.ID, "abandoned", "no viable path", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.finishPiTurn(context.Background(), id, ModelRequest{ID: "final"}, ModelResponse{Content: "finished"}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Load(id)
	if err != nil || before.Run.Status != StatusFailed {
		t.Fatalf("expected terminal failure: %+v %v", before.Run, err)
	}
	if _, err := a.SetPlan(id, []PlanTask{task, {ID: "new", Goal: "later work"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), id); err == nil {
		t.Fatal("pending addition reopened terminal disposition")
	}
	after, err := store.Load(id)
	if err != nil || after.Run.Status != StatusFailed || after.Run.Error != before.Run.Error || len(after.Requests) != len(before.Requests) || len(provider.requests) != 0 {
		t.Fatalf("terminal history changed or model requested: %+v requests=%d calls=%d err=%v", after.Run, len(after.Requests), len(provider.requests), err)
	}
	// Provider failures remain resumable: only an explicit plan_failed event seals a Run.
	failure := &Snapshot{Run: Run{Status: StatusFailed, Error: "provider temporarily unavailable"}}
	if err := a.checkWorkerAncestors(failure); err != nil {
		t.Fatalf("provider retry contract changed: %v", err)
	}
}
