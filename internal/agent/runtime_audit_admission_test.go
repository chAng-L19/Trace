package agent

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"testing"
	"time"
)

func TestRuntimeAuditDispatchRejectsTerminalParent(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, status := range []Status{StatusCompleted, StatusFailed, StatusPaused, StatusCancelled} {
			t.Run(backend+"/"+string(status), func(t *testing.T) {
				a, store := treeCapacityAgent(t, backend, MockProvider{})
				id := createTestRun(t, a)
				if _, err := a.SetPlan(id, capacityTasks(1)); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = status; return nil }); err != nil {
					t.Fatal(err)
				}
				before, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(func() {
					cancel()
					if err := a.StopWorkers(context.Background()); err != nil {
						t.Error(err)
					}
				})
				if _, err := a.DispatchTask(ctx, id, "0"); err == nil {
					t.Error("terminal parent admitted a new child without resume")
				}
				after, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				runs, err := store.List()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before.Plan, after.Plan) || len(after.Workers) != 0 || len(after.Events) != len(before.Events) || len(runs) != 1 {
					t.Fatalf("rejected dispatch mutated plan or created child: status=%s task=%s workers=%d runs=%d", after.Run.Status, after.Plan.Tasks[0].Status, len(after.Workers), len(runs))
				}
			})
		}
	}
}

func TestRuntimeAuditCancelledDispatchLeavesPendingWork(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, frontier := range []bool{false, true} {
			name := "task"
			if frontier {
				name = "frontier"
			}
			t.Run(backend+"/"+name, func(t *testing.T) {
				a, store := treeCapacityAgent(t, backend, MockProvider{})
				id := createTestRun(t, a)
				if _, err := a.SetPlan(id, capacityTasks(2)); err != nil {
					t.Fatal(err)
				}
				before, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if frontier {
					_, err = a.DispatchFrontier(ctx, id)
				} else {
					_, err = a.DispatchTask(ctx, id, "0")
				}
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled admission: %v", err)
				}
				after, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				runs, err := store.List()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before.Plan, after.Plan) || len(after.Workers) != 0 || len(after.Events) != len(before.Events) || len(runs) != 1 {
					t.Fatalf("pre-canceled dispatch consumed pending work: task=%s workers=%d runs=%d", after.Plan.Tasks[0].Status, len(after.Workers), len(runs))
				}
				if a.HasActiveWorkers() {
					t.Fatal("pre-canceled admission retained lifecycle registration")
				}
			})
		}
	}
}

func TestRuntimeAuditCancelledWhileWaitingForClaim(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			id := createTestRun(t, a)
			if _, err := a.SetPlan(id, capacityTasks(1)); err != nil {
				t.Fatal(err)
			}
			before, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			store.mu.Lock()
			go func() { _, err := a.DispatchTask(ctx, id, "0"); done <- err }()
			deadline := time.After(5 * time.Second)
			for !a.HasActiveWorkers() {
				select {
				case <-deadline:
					store.mu.Unlock()
					t.Fatal("dispatch did not reach blocked claim")
				default:
					runtime.Gosched()
				}
			}
			cancel()
			store.mu.Unlock()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("claim cancellation: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("dispatch did not return")
			}
			after, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			runs, err := store.List()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.Plan, after.Plan) || len(after.Workers) != 0 || len(after.Events) != len(before.Events) || len(runs) != 1 || a.HasActiveWorkers() {
				t.Fatalf("cancellation before claim commit consumed work: task=%s workers=%d runs=%d", after.Plan.Tasks[0].Status, len(after.Workers), len(runs))
			}
		})
	}
}
