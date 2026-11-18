package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestSchedulingExplicitResourcesAndUnknownCleanup(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	tasks := []PlanTask{{ID: "a", Goal: "shared write", ResourceKeys: []string{"file:common"}}, {ID: "b", Goal: "same write", ResourceKeys: []string{"file:common"}}}
	for i := 0; i < 4; i++ {
		tasks = append(tasks, PlanTask{ID: fmt.Sprintf("independent-%d", i), Goal: "independent directory", ResourceKeys: []string{fmt.Sprintf("dir:%d", i)}})
	}
	if _, err := a.SetPlan(id, tasks); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "a", "owner", 30); err != nil {
		t.Fatal(err)
	}
	var busy *ResourceBusyError
	if _, err := a.ClaimTask(id, "b", "other", 30); !errors.As(err, &busy) {
		t.Fatalf("shared write admitted: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := a.ClaimTask(id, fmt.Sprintf("independent-%d", i), fmt.Sprintf("worker-%d", i), 30); err != nil {
			t.Fatal(err)
		}
	}
	child, err := a.Create("cleanup fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(child.Run.ID, func(s *Snapshot) error { s.Run.ParentRunID, s.Run.ParentTaskID = id, "a"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Plan.Tasks[0].Status = "failed"
		s.Workers = append(s.Workers, AgentWorker{ID: "owner", TaskID: "a", RunID: child.Run.ID, Status: "failed", CleanupStatus: "unknown"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompleteTask(id, "independent-0", "worker-0", "completed", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "b", "other", 30); !errors.As(err, &busy) {
		t.Fatalf("unknown cleanup released resource: %v", err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Workers[0].CleanupStatus = "completed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "b", "other", 30); err != nil {
		t.Fatal(err)
	}
}

func TestSchedulingPrerequisitesSkipHeadAndRestore(t *testing.T) {
	p := &blockingProvider{entered: make(chan struct{}, 2), release: make(chan struct{}, 2)}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.StopWorkers(ctx); err != nil {
			t.Error(err)
		}
	})
	tasks := []PlanTask{{ID: "missing", Goal: "requires explicit input", Priority: 10, RequiredAssetIDs: []string{"input"}, RequiredToolIDs: []string{"not-installed"}}, {ID: "ready", Goal: "optional input does not block", AssetIDs: []string{"absent-optional"}}}
	if _, err := a.SetPlan(id, tasks); err != nil {
		t.Fatal(err)
	}
	workers, err := a.DispatchFrontier(context.Background(), id)
	if err != nil || len(workers) != 1 || workers[0].TaskID != "ready" {
		t.Fatalf("frontier: %+v %v", workers, err)
	}
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("independent child did not start")
	}
	s, _ := store.Load(id)
	if s.Plan.Tasks[0].Status != "blocked_capability" || s.Plan.Tasks[0].WorkerID != "" || s.Plan.Tasks[0].ChildRunID != "" || len(s.Workers) != 1 {
		t.Fatalf("preflight claimed/created: %+v", s.Plan)
	}
	if _, err := a.UpsertAsset(id, Asset{ID: "input", Kind: "fixture", Value: "available"}); err != nil {
		t.Fatal(err)
	}
	// Register before the next dispatch, as configuration rebuilding does; no
	// network capability probe or model turn is needed for the blocked task.
	definition := execToolDefinition()
	definition.Function.Name = "not-installed"
	a.registry.tools["not-installed"] = RegisteredTool{Adapter: "builtin", Definition: definition}
	workers, err = a.DispatchFrontier(context.Background(), id)
	if err != nil || len(workers) != 1 || workers[0].TaskID != "missing" {
		t.Fatalf("restore: %+v %v", workers, err)
	}
	child, err := store.Load(workers[0].RunID)
	if err != nil || child.WorkerContext == nil || len(child.WorkerContext.Assets) != 1 || len(child.WorkerContext.RequiredToolIDs) != 1 {
		t.Fatalf("required context missing: %+v %v", child.WorkerContext, err)
	}
}

func TestSchedulingPrerequisiteStoreFailureStopsFrontier(t *testing.T) {
	a, store, id, p := dispatchFailureFixture(t)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "missing", RequiredToolIDs: []string{"missing"}}, {ID: "next", Goal: "must not start after store failure"}}); err != nil {
		t.Fatal(err)
	}
	var restore func()
	store.beforePersist = func(got string) {
		if got == id && restore == nil {
			restore = dispatchBlockRename(t, store, id)
		}
	}
	workers, err := a.DispatchFrontier(context.Background(), id)
	store.beforePersist = nil
	if restore != nil {
		restore()
	}
	var blocked *TaskBlockedError
	if err == nil || errors.As(err, &blocked) || len(workers) != 0 || p.calls.Load() != 0 {
		t.Fatalf("storage failure swallowed: %v %+v", err, workers)
	}
	s, _ := store.Load(id)
	if s.Plan.Tasks[0].Status != "pending" || len(s.Workers) != 0 {
		t.Fatalf("failed commit became blocker/child: %+v", s.Plan)
	}
}

func TestSchedulingConcurrentResourceClaimOnlyOneOwner(t *testing.T) {
	a, _ := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "a", Goal: "edit", ResourceKeys: []string{"write"}}, {ID: "b", Goal: "edit", ResourceKeys: []string{"write"}}}); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, task := range []string{"a", "b"} {
		wg.Add(1)
		go func(task string) { defer wg.Done(); _, err := a.ClaimTask(id, task, task, 30); results <- err }(task)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			var busy *ResourceBusyError
			if !errors.As(err, &busy) {
				t.Fatal(err)
			}
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d conflicting claims", accepted)
	}
}

func TestSchedulingResourceResumeAndNestedOwnership(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "a", Goal: "write", ResourceKeys: []string{"shared"}}, {ID: "b", Goal: "write", ResourceKeys: []string{"shared"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "a", "a-worker", 30); err != nil {
		t.Fatal(err)
	}
	child := registerBudgetChild(t, a, id, "a", "a-worker")
	if _, err := store.Update(child, func(s *Snapshot) error { s.Run.Status = StatusPaused; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "b", "b-worker", 30); err != nil {
		t.Fatal(err)
	}
	before, err := store.Load(child)
	if err != nil {
		t.Fatal(err)
	}
	_, release, _, err := a.prepareWorkerResume(context.Background(), before)
	if release != nil {
		release()
	}
	var busy *ResourceBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("resumed into owned resource: %v", err)
	}
	if _, err := a.SetPlan(child, []PlanTask{{ID: "nested", Goal: "write", ResourceKeys: []string{"shared"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(child, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(child, "nested", "nested-worker", 30); !errors.As(err, &busy) {
		t.Fatalf("nested write bypassed root owner: %v", err)
	}
	if _, err := a.CompleteTask(id, "b", "b-worker", "completed", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(child, func(s *Snapshot) error { s.Run.Status = StatusPaused; return nil }); err != nil {
		t.Fatal(err)
	}
	before, _ = store.Load(child)
	_, release, w, err := a.prepareWorkerResume(context.Background(), before)
	if release != nil {
		release()
	}
	if err != nil || w.RunID != child || w.ID != "a-worker" || w.Visit != 1 {
		t.Fatalf("resume identity: %+v %v", w, err)
	}
}
