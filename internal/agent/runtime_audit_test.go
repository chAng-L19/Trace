package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRuntimeAuditNestedInboxDrainsBothDirections(t *testing.T) {
	a, parent, receiver := schedulingInbox(t, true, 0, 1)
	grandchild := createTestRun(t, a)
	if _, err := a.store.Update(receiver, func(s *Snapshot) error {
		s.Workers = []AgentWorker{{ID: "grandchild", TaskID: "nested", RunID: grandchild, Status: "completed", CleanupStatus: "completed"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SendWorkerMessage(receiver, "grandchild", "parent", "nested result"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := a.DeliverWorkerMessages(receiver); err != nil {
			t.Fatal(err)
		}
	}
	s, err := a.store.Load(receiver)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 || !s.WorkerMessages[0].Delivered {
		t.Fatalf("nested inbox lost or duplicated a message: messages=%+v queue=%+v", s.Messages, s.WorkerMessages)
	}
	for _, suffix := range []string{"\n0", "\nnested result"} {
		count := 0
		for _, m := range s.Messages {
			if strings.HasSuffix(m.Content, suffix) {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("message %q delivered %d times", suffix, count)
		}
	}
	root, err := a.store.Load(parent)
	if err != nil || !root.WorkerMessages[0].Delivered || root.WorkerMessages[1].Delivered {
		t.Fatalf("upstream acknowledgement changed wrong recipient: %+v %v", root.WorkerMessages, err)
	}
}

func TestRuntimeAuditNestedWorkerToolsUseLocalChildren(t *testing.T) {
	a, parent, receiver := schedulingInbox(t, true, 0, 0)
	child := createTestRun(t, a)
	if _, err := a.store.Update(receiver, func(s *Snapshot) error {
		s.Workers = []AgentWorker{{ID: "nested", TaskID: "task", RunID: child, Status: "completed", CleanupStatus: "completed"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	value, err := a.builtin(context.Background(), receiver, ToolCall{Name: "worker_status", Arguments: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	state := value.(map[string]any)
	workers := state["workers"].([]AgentWorker)
	if len(workers) != 1 || workers[0].ID != "nested" {
		t.Fatalf("worker_status returned upstream workers instead of local children: %+v", workers)
	}
	for _, to := range []string{"nested", "parent"} {
		if _, err := a.builtin(context.Background(), receiver, ToolCall{Name: "worker_message", Arguments: `{"to":"` + to + `","content":"routed"}`}); err != nil {
			t.Fatalf("message to %s: %v", to, err)
		}
	}
	local, _ := a.store.Load(receiver)
	upstream, _ := a.store.Load(parent)
	if len(local.WorkerMessages) != 1 || local.WorkerMessages[0].To != "nested" || len(upstream.WorkerMessages) != 2 || upstream.WorkerMessages[1].From != "worker" || upstream.WorkerMessages[1].To != "parent" {
		t.Fatalf("nested message scope mismatch: local=%+v upstream=%+v", local.WorkerMessages, upstream.WorkerMessages)
	}
}

func TestRuntimeAuditCleanupStillOccupiesCapacity(t *testing.T) {
	for _, status := range []string{"running", "failed", "uncertain"} {
		t.Run(status, func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id := createTestRun(t, a)
			if _, err := a.SetPlan(id, []PlanTask{{ID: "old", Goal: "old", Stage: true}, {ID: "new", Goal: "new"}}); err != nil {
				t.Fatal(err)
			}
			child, err := a.CreateConfigured("cleanup fixture", RunConfiguration{role: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(child.Run.ID, func(s *Snapshot) error {
				s.Run.ParentRunID, s.Run.ParentTaskID = id, "old"
				s.Run.Status = StatusPaused
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(id, func(s *Snapshot) error {
				s.Plan.Capacity = 1
				s.Plan.Tasks[0].Status = "uncertain"
				s.Plan.Tasks[0].WorkerID, s.Plan.Tasks[0].ChildRunID = "old-worker", child.Run.ID
				s.Workers = []AgentWorker{{ID: "old-worker", TaskID: "old", RunID: child.Run.ID, Status: status, CleanupStatus: "pending"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ClaimTask(id, "new", "new-worker", 30); !errors.Is(err, errWorkerCapacity) {
				t.Fatalf("unsettled %s worker released capacity: %v", status, err)
			}
			workers, err := a.DispatchFrontier(context.Background(), id)
			if err != nil || len(workers) != 0 {
				t.Fatalf("full frontier should not dispatch: %+v %v", workers, err)
			}
			if _, err := store.Update(id, func(s *Snapshot) error {
				s.Workers[0].Status, s.Workers[0].CleanupStatus = "uncertain", "completed"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ClaimTask(id, "new", "new-worker", 30); !errors.Is(err, errWorkerCapacity) {
				t.Fatalf("cleanup alone released an unresolved task: %v", err)
			}
			disposed, err := a.DisposeTask(id, "old", "abandoned", "inspected settled original attempt", "")
			if err != nil || disposed.Plan.Tasks[0].Status != "abandoned" || disposed.Workers[0].Status != "uncertain" {
				t.Fatalf("explicit disposition lost original attempt history: plan=%+v workers=%+v err=%v", disposed.Plan, disposed.Workers, err)
			}
			if _, err := a.ClaimTask(id, "new", "new-worker", 30); err != nil {
				t.Fatalf("explicitly disposed worker retained capacity: %v", err)
			}
		})
	}
}

func TestRuntimeAuditManualCompletionCannotSettleLiveChild(t *testing.T) {
	p := &blockingProvider{entered: make(chan struct{}, 2), release: make(chan struct{}, 2)}
	a, store := newTestAgent(t, p)
	t.Cleanup(func() { a.StopWorkers(context.Background()) })
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "live", Goal: "live"}}); err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), id, "live")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not start")
	}
	if _, err := a.CompleteTask(id, "live", w.ID, "failed", nil); err == nil {
		t.Fatal("task_complete overwrote a live child lifecycle")
	}
	s, err := store.Load(id)
	if err != nil || s.Plan.Tasks[0].Status != "running" || s.Workers[0].CleanupStatus != "pending" {
		t.Fatalf("rejected completion mutated lifecycle: %+v %v", s.Plan, err)
	}
}

func TestRuntimeAuditLateWorkerMessageRequiresAnotherModelTurn(t *testing.T) {
	var a *Agent
	var id string
	calls := 0
	p := settlementProviderFunc(func(_ context.Context, req ModelRequest) (ModelResponse, error) {
		calls++
		if calls == 1 {
			if _, err := a.SendWorkerMessage(id, "parent", "parent", "late worker receipt"); err != nil {
				return ModelResponse{}, err
			}
		} else {
			seen := false
			for _, m := range req.Messages {
				seen = seen || strings.Contains(m.Content, "late worker receipt")
			}
			if !seen {
				t.Error("continuation request omitted the late receipt")
			}
		}
		return ModelResponse{Content: "conversational final"}, nil
	})
	a, _ = newTestAgent(t, p)
	id = createTestRun(t, a)
	s, err := a.Run(context.Background(), id)
	if err != nil || calls != 2 || s.Run.Status != StatusCompleted || len(s.WorkerMessages) != 1 || !s.WorkerMessages[0].Delivered {
		t.Fatalf("run completed before consuming worker receipt: calls=%d run=%+v queue=%+v err=%v", calls, s.Run, s.WorkerMessages, err)
	}
}

func TestRuntimeAuditCancelledAncestorBlocksNewAndResumedWork(t *testing.T) {
	a, store, root, middle := resumeWorkerFixture(t, manualProvider(ToolCall{ID: "hold", Name: "external", Arguments: `{}`}))
	if _, err := waitAllTestWorkers(a, context.Background(), root, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(root, func(s *Snapshot) error {
		s.Plan.Capacity = 2 // The stopped middle worker retains one tree slot.
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlan(middle.RunID, []PlanTask{{ID: "leaf", Goal: "leaf"}, {ID: "new", Goal: "new"}}); err != nil {
		t.Fatal(err)
	}
	leaf, err := a.DispatchTask(context.Background(), middle.RunID, "leaf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitAllTestWorkers(a, context.Background(), middle.RunID, 5); err != nil {
		t.Fatal(err)
	}
	// Seed a legacy stopped leaf so resume reaches ancestor admission rather
	// than rejecting a completed run before checking the cancelled root.
	if _, err := store.Update(leaf.RunID, func(s *Snapshot) error {
		s.Run.Status, s.Run.PauseReason = StatusPaused, "operator_pause"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Cancel(root); err != nil {
		t.Fatal(err)
	}
	before, err := store.Load(leaf.RunID)
	if err != nil {
		t.Fatal(err)
	}
	want := "worker ancestor " + root + " is stopped"
	if _, err := a.ResumeWorker(context.Background(), middle.RunID, leaf.ID); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("resume did not enforce cancelled root: %v", err)
	}
	if _, err := a.ClaimTask(middle.RunID, "new", "manual", 30); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("claim did not enforce cancelled root: %v", err)
	}
	if _, err := a.DispatchTask(context.Background(), middle.RunID, "new"); err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("dispatch did not enforce cancelled root: %v", err)
	}
	after, err := store.Load(leaf.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Requests) != len(before.Requests) || len(after.Actions) != len(before.Actions) {
		t.Fatal("rejected continuation advanced child execution")
	}
}

func TestRuntimeAuditPausedParentCannotBeClearedByChildStartup(t *testing.T) {
	a, store, root, child := resumeWorkerFixture(t, manualProvider(ToolCall{ID: "hold", Name: "external", Arguments: `{}`}))
	if _, err := waitAllTestWorkers(a, context.Background(), root, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Pause(root); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(child.RunID, func(s *Snapshot) error {
		s.Run.Status, s.Run.PauseReason = StatusPaused, "parent_pause"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	workerAgent := *a
	workerAgent.workerRunID = child.RunID
	before, _ := store.Load(child.RunID)
	_, _ = workerAgent.runLocked(context.Background(), child.RunID)
	after, _ := store.Load(child.RunID)
	if after.Run.Status != StatusPaused || after.Run.PauseReason != "parent_pause" || len(after.Requests) != len(before.Requests) {
		t.Fatalf("startup erased persisted pause: before=%+v after=%+v", before.Run, after.Run)
	}
}

func TestRuntimeAuditDirectChildCancelSettlesParent(t *testing.T) {
	a, store, root, child := resumeWorkerFixture(t, manualProvider(ToolCall{ID: "input", Name: "request_input", Arguments: `{"reason":"value"}`}))
	if _, err := waitAllTestWorkers(a, context.Background(), root, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Cancel(child.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CancelWorker(root, child.ID); err != nil {
		t.Fatalf("already-cancelled child blocked parent reconciliation: %v", err)
	}
	s, err := store.Load(root)
	if err != nil || s.Plan.Tasks[0].Status != "cancelled" || s.Workers[0].Status != "cancelled" || !s.Plan.Tasks[0].LeaseUntil.IsZero() {
		t.Fatalf("cancel did not settle parent: plan=%+v workers=%+v err=%v", s.Plan, s.Workers, err)
	}
}

func TestRuntimeAuditBranchIsIndependentOfSourceControl(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	source := createTestRun(t, a)
	if _, err := store.Update(source, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "user", Content: "branch point"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Load(source)
	branch, err := a.Branch(source, snapshot.Messages[len(snapshot.Messages)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Cancel(source); err != nil {
		t.Fatal(err)
	}
	result, err := a.Run(context.Background(), branch.Run.ID)
	if err != nil || result.Run.Status != StatusCompleted {
		t.Fatalf("independent branch inherited source control: %+v %v", result.Run, err)
	}
}

func TestRuntimeAuditNestedWorkerMessageUnlocksExplicitResume(t *testing.T) {
	p := manualProvider(ToolCall{ID: "input", Name: "request_input", Arguments: `{"reason":"value"}`})
	p.responses[1].Content = fixtureReport("verified response")
	a, store, root, middle := resumeWorkerFixture(t, p)
	if _, err := waitAllTestWorkers(a, context.Background(), root, 5); err != nil {
		t.Fatal(err)
	}
	leaf, err := a.CreateConfigured("nested leaf", RunConfiguration{role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(leaf.Run.ID, func(s *Snapshot) error {
		s.Run.ParentRunID, s.Run.ParentTaskID, s.Run.Status = middle.RunID, "leaf", StatusCompleted
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(middle.RunID, func(s *Snapshot) error {
		s.Workers = []AgentWorker{{ID: "leaf-worker", TaskID: "leaf", RunID: leaf.Run.ID, Status: "completed", CleanupStatus: "completed"}}
		s.WorkerMessages = []WorkerMessage{{ID: "leaf-reply", From: "leaf-worker", To: "parent", Content: "resume input", CreatedAt: time.Now().UTC()}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ResumeWorker(context.Background(), root, middle.ID); err != nil {
		t.Fatalf("nested local reply did not unblock worker resume: %v", err)
	}
	if _, err := waitAllTestWorkers(a, context.Background(), root, 5); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.Load(middle.RunID)
	if err != nil || resumed.Run.Status != StatusCompleted || !resumed.WorkerMessages[0].Delivered {
		t.Fatalf("resumed worker did not consume nested reply: %+v %v", resumed.Run, err)
	}
}

func TestRuntimeAuditLateUpstreamMessageIsConsumedBeforeChildCompletion(t *testing.T) {
	var a *Agent
	parent, child := "", ""
	calls := 0
	p := settlementProviderFunc(func(_ context.Context, req ModelRequest) (ModelResponse, error) {
		calls++
		if calls > 2 {
			return ModelResponse{}, errors.New("unexpected request after upstream input was consumed")
		}
		if calls == 1 {
			_, err := a.SendWorkerMessage(parent, "parent", "worker", "late upstream input")
			return ModelResponse{Content: "first final"}, err
		}
		seen := false
		for _, m := range req.Messages {
			seen = seen || strings.Contains(m.Content, "late upstream input")
		}
		if !seen {
			return ModelResponse{}, errors.New("late upstream input missing from continuation")
		}
		return ModelResponse{Content: "second final"}, nil
	})
	a, _ = newTestAgent(t, p)
	parent = createTestRun(t, a)
	// Match DispatchTask: a child is created as a worker, never as a root
	// with a fixed root-only intent contract that becomes invalid on reparenting.
	created, err := a.CreateConfigured("upstream child", RunConfiguration{role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	child = created.Run.ID
	_, err = a.store.Update(parent, func(s *Snapshot) error {
		s.Run.Status = StatusRunning
		s.Plan.Tasks = []PlanTask{{ID: "work", Goal: "work", Status: "running", WorkerID: "worker", ChildRunID: child, LeaseUntil: time.Now().Add(time.Hour)}}
		s.Workers = []AgentWorker{{ID: "worker", TaskID: "work", RunID: child, Status: "running", CleanupStatus: "pending"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.store.Update(child, func(s *Snapshot) error {
		s.Run.ParentRunID, s.Run.ParentTaskID = parent, "work"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	worker := *a
	worker.workerRunID = child
	s, err := worker.runLocked(context.Background(), child)
	if err != nil || calls != 2 || s.Run.Status != StatusCompleted {
		t.Fatalf("child completed before consuming upstream input: calls=%d run=%+v err=%v", calls, s.Run, err)
	}
	root, _ := a.store.Load(parent)
	if len(root.WorkerMessages) != 1 || !root.WorkerMessages[0].Delivered {
		t.Fatalf("upstream input not acknowledged: %+v", root.WorkerMessages)
	}
}

func TestRuntimeAuditExplicitVerdictCannotBePromotedByPredicate(t *testing.T) {
	for _, status := range []string{"incomplete", "failed", "unknown"} {
		t.Run(status, func(t *testing.T) {
			_, store, snap, _ := catalogFixture(t)
			claim, _ := json.Marshal(map[string]Result{"trace_result": {Status: status, EvidenceIDs: []string{snap.Evidence[0].ID}}})
			s, err := store.Update(snap.Run.ID, func(s *Snapshot) error {
				s.Run.SuccessContains = "flag{verified}"
				reportBusinessMessage(t, s).Content = string(claim)
				return nil
			})
			want := "incomplete"
			if status == "failed" {
				want = "failed"
			}
			if err != nil || s.Result.Status != want {
				t.Fatalf("explicit %s promoted by an independent predicate: %+v %v", status, s.Result, err)
			}
			loaded, err := store.Load(snap.Run.ID)
			if err != nil || loaded.Result.Status != want {
				t.Fatalf("reload changed explicit verdict: %+v %v", loaded.Result, err)
			}
		})
	}
}

func TestRuntimeAuditMalformedResultCannotUseImplicitSuccess(t *testing.T) {
	for _, mode := range []string{"predicate", "requirements"} {
		t.Run(mode, func(t *testing.T) {
			a, store, snap, _ := catalogFixture(t)
			if _, err := store.Update(snap.Run.ID, func(s *Snapshot) error {
				if mode == "predicate" {
					s.Run.SuccessContains = "flag{verified}"
				} else {
					s.Requirements = []VerificationRequirement{{ID: "proof", Description: "observed proof", Category: "goal", Predicate: EvidencePredicate{Kind: "contains", Value: "flag{verified}"}}}
				}
				reportBusinessMessage(t, s).Content = "```json\n{\"trace_result\":null}\n```"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if mode == "requirements" {
				if _, err := a.CheckEvidence(context.Background(), snap.Run.ID, "proof", []string{snap.Evidence[0].ID}); err != nil {
					t.Fatal(err)
				}
			}
			s, err := store.Load(snap.Run.ID)
			if err != nil || s.Result.Status != "incomplete" {
				t.Fatalf("ambiguous explicit result accepted through %s: %+v %v", mode, s.Result, err)
			}
			if !continueForEvidence(&s) {
				t.Fatal("ambiguous result terminated instead of requesting concrete continuation")
			}
		})
	}
}
