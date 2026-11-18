package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func treeCapacityAgent(t *testing.T, backend string, p Provider) (*Agent, *Store) {
	t.Helper()
	a, store := newTestAgent(t, p)
	if backend == "sqlite" {
		if err := MigrateStoreToSQLite(store.root); err != nil {
			t.Fatal(err)
		}
		var err error
		store, err = NewStore(store.root)
		if err != nil {
			t.Fatal(err)
		}
		a.store = store
	}
	t.Cleanup(func() { store.Close() })
	return a, store
}

func capacityFixtureChild(t *testing.T, a *Agent, parent, taskID string) (*Agent, AgentWorker) {
	t.Helper()
	wid := "fixture-" + taskID
	if _, err := a.ClaimTask(parent, taskID, wid, 3600); err != nil {
		t.Fatal(err)
	}
	child, err := a.CreateConfigured("capacity child", RunConfiguration{role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	w := AgentWorker{ID: wid, TaskID: taskID, RunID: child.Run.ID, Visit: 1, Status: "running", CleanupStatus: "pending"}
	if _, err = a.store.Update(child.Run.ID, func(s *Snapshot) error {
		s.Run.ParentRunID, s.Run.ParentTaskID, s.Run.Status = parent, taskID, StatusRunning
		s.WorkerContext = &WorkerContext{RunID: s.Run.ID, ParentRunID: parent, TaskID: taskID, WorkerID: wid}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.Update(parent, func(s *Snapshot) error {
		s.Run.Status = StatusRunning
		task, err := planTask(s, taskID)
		if err != nil {
			return err
		}
		task.ChildRunID = child.Run.ID
		s.Workers = append(s.Workers, w)
		return a.checkWorkerCapacityLocked(s, 0)
	}); err != nil {
		t.Fatal(err)
	}
	local := *a
	local.workerRunID, local.workerVisit = w.RunID, w.Visit
	return &local, w
}

func capacityTasks(n int) []PlanTask {
	tasks := make([]PlanTask, n)
	for i := range tasks {
		tasks[i] = PlanTask{ID: fmt.Sprint(i), Goal: "capacity work"}
	}
	return tasks
}

func TestTreeWorkerCapacityConcurrentClaimsAcrossAgentsAndRoots(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			root := createTestRun(t, a)
			if _, err := a.SetPlan(root, capacityTasks(10)); err != nil {
				t.Fatal(err)
			}
			_, left := capacityFixtureChild(t, a, root, "0")
			_, right := capacityFixtureChild(t, a, root, "1")
			for _, id := range []string{left.RunID, right.RunID} {
				if _, err := a.SetPlan(id, capacityTasks(10)); err != nil {
					t.Fatal(err)
				}
			}
			other, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			b, err := New(other, MockProvider{}, "")
			if err != nil {
				t.Fatal(err)
			}
			results := make(chan error, 18)
			var wg sync.WaitGroup
			for i := 0; i < 18; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					agent := []*Agent{a, b}[i%2]
					id := []string{root, left.RunID, right.RunID}[i%3]
					_, err := agent.ClaimTask(id, fmt.Sprint(2+i/3), fmt.Sprintf("manual-%d", i), 3600)
					results <- err
				}(i)
			}
			wg.Wait()
			close(results)
			accepted := 0
			for err := range results {
				if err == nil {
					accepted++
					continue
				}
				var busy *WorkerCapacityError
				if !errors.Is(err, errWorkerCapacity) || !errors.As(err, &busy) || busy.RootRunID != root || busy.Active != 5 || busy.Capacity != 5 {
					t.Fatalf("unexpected admission error: %v", err)
				}
			}
			if accepted != 3 {
				t.Fatalf("two resident children plus %d claims exceeded or underfilled five", accepted)
			}
			state, err := b.WorkerCapacity(right.RunID)
			if err != nil || state.Active != 5 || state.Capacity != 5 || state.RootRunID != root {
				t.Fatalf("reopened tree projection: %+v %v", state, err)
			}
			second := createTestRun(t, b)
			if _, err := b.SetPlan(second, capacityTasks(6)); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 5; i++ {
				if _, err := b.ClaimTask(second, fmt.Sprint(i), fmt.Sprint(i), 3600); err != nil {
					t.Fatalf("independent root shared capacity: %v", err)
				}
			}
			if _, err := b.ClaimTask(second, "5", "overflow", 3600); !errors.Is(err, errWorkerCapacity) {
				t.Fatalf("second root overflow: %v", err)
			}
			if _, err := a.SetPlanCapacity(left.RunID, 32); err == nil {
				t.Fatal("child override escaped root capacity")
			}
			if _, err := a.SetPlanCapacity(root, 7); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{left.RunID, right.RunID} {
				if _, err := b.ClaimTask(id, "9", "extra-"+id, 3600); err != nil {
					t.Fatalf("root override not inherited: %v", err)
				}
			}
			state, err = a.WorkerCapacity(root)
			if err != nil || state.Active != 7 || state.Capacity != 7 {
				t.Fatalf("override projection: %+v %v", state, err)
			}
		})
	}
}

func TestTreeWorkerCapacityResumeAndUnknownCleanup(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			root := createTestRun(t, a)
			if _, err := a.SetPlan(root, capacityTasks(3)); err != nil {
				t.Fatal(err)
			}
			_, paused := capacityFixtureChild(t, a, root, "0")
			_, sibling := capacityFixtureChild(t, a, root, "1")
			if _, err := a.SetPlan(sibling.RunID, capacityTasks(5)); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(paused.RunID, func(s *Snapshot) error {
				s.Run.Status, s.Run.PauseReason = StatusPaused, "parent_pause"
				s.WorkerSettlement = &WorkerSettlement{ParentRunID: root, TaskID: paused.TaskID, WorkerID: paused.ID, RunID: paused.RunID, Visit: paused.Visit, CleanupStatus: "completed", ObservedAt: time.Now().UTC()}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.RecoverWorkers(root); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 4; i++ {
				if _, err := a.ClaimTask(sibling.RunID, fmt.Sprint(i), fmt.Sprint(i), 3600); err != nil {
					t.Fatal(err)
				}
			}
			child, err := store.Load(paused.RunID)
			if err != nil {
				t.Fatal(err)
			}
			unlock, err := store.AcquireRunLock(child.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			if _, _, _, err := a.prepareWorkerResume(context.Background(), child); !errors.Is(err, errWorkerCapacity) {
				t.Fatalf("resume bypassed occupied cousin claims: %v", err)
			}
			if _, err := store.Update(sibling.RunID, func(s *Snapshot) error {
				s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Hour)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.RecoverWorkers(sibling.RunID); err != nil {
				t.Fatal(err)
			}
			other, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			b, err := New(other, MockProvider{}, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := b.ClaimTask(root, "2", "after-restart", 3600); !errors.Is(err, errWorkerCapacity) {
				t.Fatalf("unknown claim released capacity on restart: %v", err)
			}
			if _, err := b.CompleteTask(sibling.RunID, "1", "1", "completed", nil); err != nil {
				t.Fatal(err)
			}
			_, release, resumed, err := a.prepareWorkerResume(context.Background(), child)
			if err != nil {
				t.Fatal(err)
			}
			release()
			if resumed.ID != paused.ID || resumed.RunID != paused.RunID || resumed.Visit != paused.Visit+1 {
				t.Fatalf("resume replaced identity: %+v", resumed)
			}
			state, err := b.WorkerCapacity(root)
			if err != nil || state.Active != 5 {
				t.Fatalf("resume did not atomically consume released slot: %+v %v", state, err)
			}
			if _, err := store.Update(root, func(s *Snapshot) error {
				s.Workers[1].Status, s.Workers[1].CleanupStatus = "uncertain", "unknown"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := b.ClaimTask(root, "2", "cleanup-overflow", 3600); !errors.Is(err, errWorkerCapacity) {
				t.Fatalf("unknown parent cleanup released slot: %v", err)
			}
		})
	}
}

func TestTreeWorkerCapacityRegistrationRechecksRootOverride(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			p := &dispatchFailureProvider{}
			a, store := treeCapacityAgent(t, backend, p)
			root := createTestRun(t, a)
			if _, err := a.SetPlan(root, capacityTasks(2)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.SetPlanCapacity(root, 2); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ClaimTask(root, "0", "existing-reservation", 3600); err != nil {
				t.Fatal(err)
			}
			changed := false
			store.beforePersist = func(id string) {
				if id == root || changed {
					return
				}
				changed = true
				// Simulate the committed capacity edit between claim and child
				// registration, already holding the normal Store transaction.
				s, err := store.loadRawLocked(root)
				if err != nil {
					panic(err)
				}
				s.Plan.Capacity = 1
				if err := store.saveLocked(s); err != nil {
					panic(err)
				}
			}
			_, err := a.DispatchTask(context.Background(), root, "1")
			store.beforePersist = nil
			if !errors.Is(err, errWorkerCapacity) || !changed || p.calls.Load() != 0 || a.HasActiveWorkers() {
				t.Fatalf("registration ignored changed tree capacity or ran child: %v", err)
			}
			parent, err := store.Load(root)
			if err != nil || len(parent.Workers) != 0 || parent.Plan.Tasks[1].Status != "failed" {
				t.Fatalf("unstarted child claim was not settled: %+v %v", parent.Plan, err)
			}
			state, err := a.WorkerCapacity(root)
			if err != nil || state.Active != 1 || state.Capacity != 1 {
				t.Fatalf("startup rollback lost original reservation: %+v %v", state, err)
			}
		})
	}
}
