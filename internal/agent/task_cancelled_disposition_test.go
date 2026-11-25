package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trace/internal/worker"
)

// Reproduce both incident outcomes through the existing child lifecycle:
// cancellation/timeout stays uncertain even after the current visit cleans up.
func cancelledDispositionChildren(t *testing.T, a *Agent, store *Store, parent string) []AgentWorker {
	t.Helper()
	var workers []AgentWorker
	for i, taskID := range []string{"t-a03", "t-f205"} {
		_, w := capacityFixtureChild(t, a, parent, taskID)
		if err := settlementOperation(t, a, store, parent, w, func(s *Snapshot) {
			s.Run.Status = StatusCancelled
			s.Run.Error = "cancelled after an uncertain external operation"
			status := worker.StatusCancelled
			if i == 1 {
				status = worker.StatusTimedOut
			}
			result := &ToolResult{ExitCode: -1, Error: "original interruption", Interrupted: true, OutcomeUncertain: true, WorkerOutcomeUncertain: true, WorkerStatus: status}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			requestID := w.RunID + "-old"
			s.Requests = []ModelRequest{{ID: requestID}, {ID: w.RunID + "-current"}}
			s.Messages = append(s.Messages,
				Message{Role: "assistant", RequestID: requestID, ToolCalls: []ToolCall{{ID: "old-call", Name: "exec", Arguments: `{"command":"original operation"}`}}},
				Message{Role: "tool", ToolCallID: "old-call", Content: string(data)})
			s.Actions = []Action{{RequestID: requestID, CallID: "old-call", Name: "exec", Adapter: "exec", Status: "uncertain", Result: result, ResourceCleanupStatus: "completed", FinishedAt: time.Now().UTC()}}
		}); err != nil {
			t.Fatal(err)
		}
		workers = append(workers, w)
	}
	return workers
}

func TestCancelledTaskExplicitFailureConvergesAfterRestart(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			provider := &dispatchFailureProvider{}
			a, store := treeCapacityAgent(t, backend, provider)
			parent := createTestRun(t, a)
			if _, err := a.SetPlan(parent, []PlanTask{{ID: "t-a03", Goal: "first unfinished goal"}, {ID: "t-f205", Goal: "second unfinished goal"}}); err != nil {
				t.Fatal(err)
			}
			workers := cancelledDispositionChildren(t, a, store, parent)
			if err := a.DeliverWorkerMessages(parent); err != nil {
				t.Fatal(err)
			}
			before := map[string]string{}
			for _, w := range workers {
				child, err := store.Load(w.RunID)
				if err != nil || !child.WorkerSettlement.matches(parent, w) || child.WorkerSettlement.CleanupStatus != "completed" {
					t.Fatalf("current lifecycle not settled: %+v %v", child.WorkerSettlement, err)
				}
				before[w.RunID] = valueHash(child)
			}
			root, err := store.Update(parent, func(s *Snapshot) error {
				s.Run.RequireVerifiedCompletion, s.Run.Budget = false, Budget{}
				// Neither the external stop receipt nor cancellation proves success.
				s.Messages = append(s.Messages, Message{Role: "assistant", Content: `{"trace_result":{"status":"success","summary":"invalid_state: task already finished"}}`})
				return nil
			})
			if err != nil || planDispositionFailure(&root) != "" || root.Result.Status == "success" {
				t.Fatalf("ordinary cancellation was promoted or auto-closed: %+v %v", root.Result, err)
			}
			for i, w := range workers {
				disposition := []string{"abandoned", "blocked"}[i]
				root, err = a.DisposeTask(parent, w.TaskID, disposition, "external task ended; goal remains unfinished and original effects unknown", "")
				if err != nil || taskDone(root.Plan.Tasks[i]) || root.Plan.Tasks[i].PreviousStatus != "cancelled" {
					t.Fatalf("explicit failure: %+v %v", root.Plan, err)
				}
				if i == 0 && planDispositionFailure(&root) != "" {
					t.Fatal("remaining cancelled attempt was automatically closed")
				}
			}
			if _, err := a.RecoverWorkers(parent); err != nil {
				t.Fatal(err)
			}
			end, err := a.finishPiTurn(context.Background(), parent, ModelRequest{ID: "final"}, ModelResponse{Content: "unfinished goals reported"})
			if err != nil || !end {
				t.Fatalf("root continued: end=%v %v", end, err)
			}
			root, err = store.Load(parent)
			if err != nil || root.Run.Status != StatusFailed || root.Result.Status != "failed" || root.Run.Budget != (Budget{}) || root.Run.RequireVerifiedCompletion {
				t.Fatalf("root failure or budget contract lost: %+v %v", root.Run, err)
			}
			failures := 0
			for _, event := range root.Events {
				if event.Type == "plan_failed" {
					failures++
				}
				if event.Type == "completion_deferred" || event.Type == "run_completed" {
					t.Fatalf("false continuation/completion: %s", event.Type)
				}
			}
			if failures != 1 {
				t.Fatalf("terminal failures=%d", failures)
			}
			a, store = reopenCapacityAgent(t, store)
			a.provider = provider
			if _, err := a.Run(context.Background(), parent); err == nil {
				t.Fatal("persisted failure resumed")
			}
			for _, w := range workers {
				if _, err := a.ResumeWorker(context.Background(), parent, w.ID); err == nil {
					t.Fatal("cancelled worker resumed")
				}
				if _, err := a.RetryTask(parent, w.TaskID); err == nil {
					t.Fatal("unknown historical operation retried")
				}
				child, err := store.Load(w.RunID)
				if err != nil || valueHash(child) != before[w.RunID] {
					t.Fatalf("child history changed: %s %v", w.TaskID, err)
				}
			}
			after, err := store.Load(parent)
			if err != nil || after.Run.Status != StatusFailed || len(after.Requests) != len(root.Requests) || provider.calls.Load() != 0 {
				t.Fatalf("restart generated work: requests=%d calls=%d %v", len(after.Requests), provider.calls.Load(), err)
			}
		})
	}
}

func cancelledDispositionFixture(t *testing.T, backend string) (*Agent, *Store, string, AgentWorker) {
	t.Helper()
	a, store, parent, w := dispositionFixFixture(t, backend)
	if _, err := store.Update(w.RunID, func(s *Snapshot) error {
		s.Run.Status = StatusCancelled
		s.Actions[0].Status = "uncertain"
		s.Actions[0].ResourceCleanupStatus = "completed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(parent, func(s *Snapshot) error {
		s.Plan.Tasks[0].Status, s.Workers[0].Status = "cancelled", "cancelled"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return a, store, parent, w
}

func TestCancelledTaskFailureDispositionKeepsSettlementGates(t *testing.T) {
	cases := []struct {
		name, code string
		parent     func(*Snapshot)
		child      func(*Snapshot)
	}{
		{name: "active", code: "child_active"},
		{name: "worker_cleanup", code: "worker_unsettled", parent: func(s *Snapshot) { s.Workers[0].CleanupStatus = "uncertain" }},
		{name: "wrong_worker", code: "child_identity_mismatch", parent: func(s *Snapshot) { s.Workers[0].ID = "other" }},
		{name: "wrong_child", code: "child_identity_mismatch", child: func(s *Snapshot) { s.Run.ParentTaskID = "other" }},
		{name: "stale_visit", code: "child_settlement_uncommitted", parent: func(s *Snapshot) { s.Workers[0].Visit++ }},
		{name: "missing_settled_visit", code: "child_settlement_uncommitted", parent: func(s *Snapshot) { s.Workers[0].SettledVisit = nil }},
		{name: "missing_receipt", code: "child_settlement_uncommitted", child: func(s *Snapshot) { s.WorkerSettlement = nil }},
		{name: "missing_observation", code: "child_settlement_uncommitted", child: func(s *Snapshot) { s.WorkerSettlement.ObservedAt = time.Time{} }},
		{name: "wrong_receipt", code: "child_settlement_uncommitted", child: func(s *Snapshot) { s.WorkerSettlement.WorkerID = "other" }},
		{name: "receipt_cleanup", code: "child_settlement_uncommitted", child: func(s *Snapshot) { s.WorkerSettlement.CleanupStatus = "uncertain" }},
		{name: "legacy", code: "child_action_uncertain", parent: func(s *Snapshot) { s.Workers[0].Visit, s.Workers[0].SettledVisit = 0, nil }, child: func(s *Snapshot) { s.WorkerSettlement = nil }},
		{name: "started", code: "child_action_uncertain", child: func(s *Snapshot) { s.Actions[0].Status = "started" }},
		{name: "tool_cleanup", code: "descendants_unsettled", child: func(s *Snapshot) { s.Actions[0].ResourceCleanupError = "unresolved cleanup" }},
		{name: "missing_process_cleanup", code: "descendants_unsettled", child: func(s *Snapshot) {
			s.Actions[0].Adapter, s.Actions[0].Result, s.Actions[0].ResourceCleanupStatus = "exec", nil, "pending"
		}},
		{name: "cleanup_requirement", code: "child_cleanup_unresolved", child: func(s *Snapshot) { s.Requirements = []VerificationRequirement{{ID: "cleanup", Category: "cleanup"}} }},
		{name: "descendant_task", code: "descendants_unsettled", child: func(s *Snapshot) { s.Plan.Tasks = []PlanTask{{ID: "nested", Status: "running"}} }},
		{name: "descendant_worker", code: "descendants_unsettled", child: func(s *Snapshot) {
			s.Workers = []AgentWorker{{ID: "nested", Status: "cancelled", CleanupStatus: "unknown"}}
		}},
		{name: "failed", code: "child_action_uncertain", child: func(s *Snapshot) { s.Run.Status = StatusFailed }},
		{name: "paused", code: "child_action_uncertain", child: func(s *Snapshot) { s.Run.Status, s.Run.PauseReason = StatusPaused, "operator_pause" }},
	}
	for _, backend := range []string{"json", "sqlite"} {
		for _, tc := range cases {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				a, store, parent, w := cancelledDispositionFixture(t, backend)
				for _, update := range []struct {
					id string
					fn func(*Snapshot)
				}{{parent, tc.parent}, {w.RunID, tc.child}} {
					if update.fn != nil {
						if _, err := store.Update(update.id, func(s *Snapshot) error { update.fn(s); return nil }); err != nil {
							t.Fatal(err)
						}
					}
				}
				if tc.name == "active" {
					release, err := store.AcquireRunLock(w.RunID)
					if err != nil {
						t.Fatal(err)
					}
					defer release()
				}
				beforeParent, err := store.Load(parent)
				if err != nil {
					t.Fatal(err)
				}
				beforeChild, err := store.Load(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				_, err = a.DisposeTask(parent, w.TaskID, "abandoned", "goal remains unfinished", "")
				var blocked *TaskDispositionError
				if !errors.As(err, &blocked) || blocked.Code != tc.code {
					t.Fatalf("wrong blocker: %+v %v", blocked, err)
				}
				if beforeChild.Run.Status == StatusCancelled && strings.Contains(blocked.NextAction, "worker_resume") {
					t.Fatalf("cancelled diagnostic recommends resume: %s", blocked.NextAction)
				}
				afterParent, parentErr := store.Load(parent)
				afterChild, childErr := store.Load(w.RunID)
				if parentErr != nil || childErr != nil || valueHash(beforeParent) != valueHash(afterParent) || valueHash(beforeChild) != valueHash(afterChild) {
					t.Fatalf("rejection changed original state: %v %v", parentErr, childErr)
				}
			})
		}
	}
}

func TestCancelledTaskUncertaintyDoesNotUnlockReplacementOrDependency(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, parent, w := cancelledDispositionFixture(t, backend)
			root, err := store.Load(parent)
			if err != nil {
				t.Fatal(err)
			}
			tasks := append(root.Plan.Tasks, PlanTask{ID: "replacement", Goal: root.Plan.Tasks[0].Goal}, PlanTask{ID: "dependent", Goal: "consume original success", Dependencies: []string{w.TaskID}})
			if _, err := a.SetPlan(parent, tasks); err != nil {
				t.Fatal(err)
			}
			_, err = a.DisposeTask(parent, w.TaskID, "superseded", "new approach", "replacement")
			var blocked *TaskDispositionError
			if !errors.As(err, &blocked) || blocked.Code != "child_action_uncertain" || strings.Contains(blocked.NextAction, "worker_resume") {
				t.Fatalf("unknown effects allowed replacement or impossible resume: %+v %v", blocked, err)
			}
			if _, err := a.DisposeTask(parent, w.TaskID, "abandoned", "preserve unfinished goal", ""); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ClaimTask(parent, "dependent", "other-worker", 30); err == nil {
				t.Fatal("failure unlocked a success dependency")
			}
			root, err = store.Load(parent)
			if err != nil || taskDone(root.Plan.Tasks[0]) || planTaskSatisfied(&root.Plan, root.Plan.Tasks[0], map[string]bool{}) || hasString(root.Plan.Frontier, "dependent") || planDispositionFailure(&root) != "" {
				t.Fatalf("failure skipped unresolved work: %+v %v", root.Plan, err)
			}
		})
	}
}

// Encode one ledger entry at a time: the real root has 229 MiB of history.
func incidentDispositionFactsHash(t *testing.T, s *Snapshot) string {
	t.Helper()
	h := sha256.New()
	encoder := json.NewEncoder(h)
	write := func(value any) {
		if err := encoder.Encode(value); err != nil {
			t.Fatal(err)
		}
	}
	write([]int{len(s.Actions), len(s.Requests), len(s.Evidence)})
	for _, action := range s.Actions {
		write(action)
	}
	for _, request := range s.Requests {
		write(request)
	}
	for _, evidence := range s.Evidence {
		write(evidence)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// The caller supplies a DISPOSABLE, complete Store copy, including artifacts.
// As in the Web incident fixture, set only the copy's root header to running
// before starting this test. Never rewrite receipts, source facts or models.
func TestIncidentCancelledTaskExplicitFailure(t *testing.T) {
	fixture := os.Getenv("TRACE_INCIDENT_FIXTURE_DIR")
	if fixture == "" {
		t.Skip("requires isolated complete incident Store copy with running root")
	}
	resolved, err := filepath.EvalSymlinks(fixture)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil || filepath.ToSlash(resolved) == "/home/ubuntu/trace/state" {
		t.Fatal("fixture must be a disposable Store copy")
	}
	store, err := NewStore(resolved)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	// Read the actual receipts before Load can revalidate any derived state.
	readRaw := func(id string) Snapshot {
		t.Helper()
		store.mu.Lock()
		defer store.mu.Unlock()
		unlock, err := store.acquireStoreLock()
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()
		s, err := store.loadRawLocked(id)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	const parent = "21747a3b09678fd8d7e854dc82969267"
	root := readRaw(parent)
	if root.Run.Status != StatusRunning || root.Run.RequireVerifiedCompletion || root.Run.Budget != (Budget{}) || len(root.Requests) != 422 {
		t.Fatalf("expected original 422-request unlimited fixture prepared as running; status=%s requests=%d", root.Run.Status, len(root.Requests))
	}
	facts := map[string]string{parent: incidentDispositionFactsHash(t, &root)}
	var workers []AgentWorker
	for _, expected := range []struct{ task, worker, child string }{
		{"t-a03", "dbd13921c3f845f84c8c8a5349a17563", "88920aa46ecfd1feed3e22195eb95d2e"},
		{"t-f205", "9eab8e0bb39cbac9125b59f3827af5a3", "8df1022a3852f32a49df07aeb80894f3"},
	} {
		task, err := planTask(&root, expected.task)
		if err != nil || task.Status != "cancelled" || task.WorkerID != expected.worker || task.ChildRunID != expected.child {
			t.Fatalf("original cancelled task identity changed: %s %v", expected.task, err)
		}
		child := readRaw(expected.child)
		found := false
		for _, w := range root.Workers {
			if w.ID != expected.worker || w.TaskID != expected.task || w.RunID != expected.child {
				continue
			}
			if w.Status != "cancelled" || w.CleanupStatus != "completed" || w.Visit != 1 || w.SettledVisit == nil || *w.SettledVisit != w.Visit || !child.WorkerSettlement.matches(parent, w) || child.WorkerSettlement.CleanupStatus != "completed" {
				t.Fatalf("original current settlement gate rejected %s; no receipts synthesized", expected.task)
			}
			workers, found = append(workers, w), true
		}
		unknown := 0
		for _, action := range child.Actions {
			if action.Status == "uncertain" {
				unknown++
			}
		}
		if !found || child.Run.ParentRunID != parent || child.Run.ParentTaskID != expected.task || child.Run.Status != StatusCancelled || unknown == 0 {
			t.Fatalf("original child identity/cancellation/unknown history missing: %s", expected.task)
		}
		facts[expected.child] = incidentDispositionFactsHash(t, &child)
		t.Logf("original task=%s visit=1 settled_visit=1 cleanup=completed uncertain=%d observed=%s", expected.task, unknown, child.WorkerSettlement.ObservedAt.Format(time.RFC3339Nano))
	}
	request := ModelRequest{ID: currentRequestID(&root)}
	eventSeq := root.Run.EventSeq
	root = Snapshot{} // Do not retain the large pre-mutation snapshot.
	provider := &dispatchFailureProvider{}
	a, err := New(store, provider, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range workers {
		if _, err := a.DisposeTask(parent, w.TaskID, "abandoned", "incident fixture: external task ended; original effects remain unknown and goal unfinished", ""); err != nil {
			t.Fatalf("original disposition gate rejected %s: %v", w.TaskID, err)
		}
	}
	if err := a.DeliverWorkerMessages(parent); err != nil {
		t.Fatal(err)
	}
	end, err := a.finishPiTurn(context.Background(), parent, request, ModelResponse{Content: "incident fixture failure settlement"})
	if err != nil || !end {
		t.Fatalf("original finish gate did not converge: end=%v %v", end, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	a, store = reopenCapacityAgent(t, store)
	a.provider = provider
	root, err = store.Load(parent)
	if err != nil || root.Run.Status != StatusFailed || root.Result.Status != "failed" {
		t.Fatalf("reopened incident root is not failed: %s %v", root.Run.Status, err)
	}
	// Exercise the shared Run admission gate without replacing the incident's
	// persisted model binding with a fixture provider or contacting its endpoint.
	if err := a.checkWorkerAncestors(&root); err == nil || !strings.Contains(err.Error(), "persisted terminal disposition") {
		t.Fatalf("reopened terminal plan admitted work: %v", err)
	}
	failed := 0
	for _, event := range root.Events {
		if event.Sequence <= eventSeq {
			continue
		}
		if event.Type == "plan_failed" {
			failed++
		}
		if event.Type == "completion_deferred" || event.Type == "run_completed" || event.Type == "model_request" {
			t.Fatalf("incident failure produced %s", event.Type)
		}
	}
	if failed != 1 {
		t.Fatalf("new plan_failed events=%d", failed)
	}
	root = Snapshot{}
	end, err = a.finishPiTurn(context.Background(), parent, request, ModelResponse{})
	if err != nil || !end || provider.calls.Load() != 0 {
		t.Fatalf("reopened failure generated work: end=%v calls=%d %v", end, provider.calls.Load(), err)
	}
	for id, before := range facts {
		after := readRaw(id)
		if incidentDispositionFactsHash(t, &after) != before {
			t.Fatalf("historical Action/Request/Evidence facts changed: %s", id)
		}
	}
}
