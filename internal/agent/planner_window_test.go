package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestTaskWindowAdmissionBoundaryAndUnknown(t *testing.T) {
	for _, estimate := range []int64{0, 79, 80, 81} {
		a, store := newTestAgent(t, MockProvider{})
		id := createTestRun(t, a)
		if _, err := a.SetRunBudget(id, Budget{MaxSeconds: 100, CheckpointSeconds: 20}); err != nil {
			t.Fatal(err)
		}
		if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "observable stage", EstimatedSeconds: estimate}}); err != nil {
			t.Fatal(err)
		}
		before, _ := store.Load(id)
		_, err := a.ClaimTask(id, "work", "w", 30)
		var window *TaskWindowError
		if estimate > 80 {
			if !errors.As(err, &window) || window.AvailableMillis != 80000 {
				t.Fatalf("estimate %d: %v", estimate, err)
			}
			after, _ := store.Load(id)
			if valueHash(before) != valueHash(after) {
				t.Fatal("rejected admission mutated plan, events or budget")
			}
		} else if err != nil {
			t.Fatalf("estimate %d: %v", estimate, err)
		}
	}
}

func TestTaskWindowChecksTightestAncestorAndBranchBoundary(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	root := createTestRun(t, a)
	if _, err := a.SetRunBudget(root, Budget{MaxSeconds: 100, CheckpointSeconds: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlan(root, []PlanTask{{ID: "parent", Goal: "parent"}}); err != nil {
		t.Fatal(err)
	}
	child := registerBudgetChild(t, a, root, "parent", "w")
	if _, err := a.SetRunBudget(child, Budget{MaxSeconds: 200, CheckpointSeconds: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlan(child, []PlanTask{{ID: "work", Goal: "stage", EstimatedSeconds: 81}}); err != nil {
		t.Fatal(err)
	}
	_, err := a.ClaimTask(child, "work", "nested", 30)
	var window *TaskWindowError
	if !errors.As(err, &window) || window.BudgetRunID != root {
		t.Fatalf("ancestor deadline bypassed: %v", err)
	}
	if _, err := store.Update(child, func(s *Snapshot) error { s.Run.ParentTaskID = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(child, "work", "branch", 30); err != nil {
		t.Fatalf("ordinary branch inherited worker deadline: %v", err)
	}
}

func TestTaskWindowFrontierSkipsLongTaskAndKeepsPriority(t *testing.T) {
	for _, restored := range []bool{false, true} {
		a, store := newTestAgent(t, MockProvider{})
		id := createTestRun(t, a)
		if _, err := a.SetRunBudget(id, Budget{MaxSeconds: 100, CheckpointSeconds: 20}); err != nil {
			t.Fatal(err)
		}
		if _, err := a.SetPlanCapacity(id, 1); err != nil {
			t.Fatal(err)
		}
		if _, err := a.SetPlan(id, []PlanTask{
			{ID: "low", Goal: "low", Priority: 1, EstimatedSeconds: 5},
			{ID: "long", Goal: "long", Priority: 30, EstimatedSeconds: 81},
			{ID: "valuable", Goal: "valuable", Priority: 20, EstimatedSeconds: 30},
		}); err != nil {
			t.Fatal(err)
		}
		if restored {
			if _, err := store.Update(id, func(s *Snapshot) error {
				task, err := planTask(s, "valuable")
				if err != nil {
					return err
				}
				task.Status = "blocked_capability"
				refreshFrontier(&s.Plan)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		workers, err := a.DispatchFrontier(context.Background(), id)
		if err != nil || len(workers) != 1 || workers[0].TaskID != "valuable" {
			t.Fatalf("priority/window selection: %+v %v", workers, err)
		}
		if _, err := a.WaitWorkers(context.Background(), id, 5); err != nil {
			t.Fatal(err)
		}
		s, err := store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		long, _ := planTask(&s, "long")
		if long.Status != "pending" || long.Started || long.WorkerID != "" {
			t.Fatalf("skipped long task mutated: %+v", long)
		}
	}
}

func TestTaskWindowRegistrationRechecksRemainingBudget(t *testing.T) {
	p := &dispatchFailureProvider{}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	if _, err := a.SetRunBudget(id, Budget{MaxSeconds: 100, CheckpointSeconds: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "stage", EstimatedSeconds: 60}}); err != nil {
		t.Fatal(err)
	}
	var childID string
	store.mu.Lock()
	store.beforePersist = func(got string) {
		if got == id || childID != "" {
			return
		}
		childID = got
		hook := store.beforePersist
		store.beforePersist = nil
		defer func() { store.beforePersist = hook }()
		s, err := store.loadRawLocked(id)
		if err != nil {
			panic(err)
		}
		s.Run.ElapsedMillis = 30000
		if err := store.saveLocked(s); err != nil {
			panic(err)
		}
	}
	store.mu.Unlock()
	_, err := a.DispatchTask(context.Background(), id, "work")
	store.mu.Lock()
	store.beforePersist = nil
	store.mu.Unlock()
	var window *TaskWindowError
	if childID == "" || !errors.As(err, &window) {
		t.Fatalf("registration admitted insufficient window: %v", err)
	}
	s, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Workers) != 0 || occupiedWorkerSlots(&s) != 0 || p.calls.Load() != 0 {
		t.Fatal("registration rejection launched or retained worker")
	}
	child, err := store.Load(childID)
	if err != nil || child.Run.Status != StatusCancelled || len(child.Actions) != 0 || len(child.Requests) != 0 {
		t.Fatalf("unregistered child was not cancelled: %+v %v", child.Run, err)
	}
}

func TestTaskWindowEstimateValidationAndContract(t *testing.T) {
	a, _ := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	for _, estimate := range []int64{-1, maxBudgetSeconds + 1} {
		if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "stage", EstimatedSeconds: estimate}}); err == nil {
			t.Fatal("invalid estimate accepted")
		}
	}
	task := PlanTask{ID: "work", Goal: "stage", EstimatedSeconds: 60, ExpectedProgress: "recover access code; final output still required", SuccessContains: "final-output"}
	if _, err := a.SetPlan(id, []PlanTask{task}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "work", "w", 30); err != nil {
		t.Fatal(err)
	}
	changed := task
	changed.EstimatedSeconds = 1
	if _, err := a.SetPlan(id, []PlanTask{changed}); err == nil {
		t.Fatal("started estimate changed")
	}
	changed = task
	changed.ExpectedProgress = "different stage"
	if sameTaskSchedulingContract(task, changed) {
		t.Fatal("replacement lost stage contract")
	}
	if _, err := a.SetPlan(id, []PlanTask{changed}); err == nil {
		t.Fatal("started stage progress changed")
	}
	s, err := a.CompleteTask(id, "work", "w", "completed", nil)
	if err != nil || taskDone(s.Plan.Tasks[0]) {
		t.Fatalf("stage completion promoted business success: %v", err)
	}
	view, _, err := a.plannerContextAt(id)
	if err != nil || !strings.Contains(view, "scheduling_window") || !strings.Contains(view, "business success") {
		t.Fatalf("missing scheduling guidance: %s %v", view, err)
	}
}
