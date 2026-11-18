package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func reopenCapacityAgent(t *testing.T, store *Store) (*Agent, *Store) {
	t.Helper()
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	a, err := New(reopened, MockProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	return a, reopened
}

func retainedCapacityFixture(t *testing.T, backend, workerStatus, taskStatus string) (*Agent, *Store, string, AgentWorker) {
	t.Helper()
	a, store := treeCapacityAgent(t, backend, MockProvider{})
	root := createTestRun(t, a)
	if _, err := a.SetPlan(root, capacityTasks(2)); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlanCapacity(root, 1); err != nil {
		t.Fatal(err)
	}
	_, w := capacityFixtureChild(t, a, root, "0")
	// Use the normal child lifecycle to commit both cleanup receipt and visit.
	if err := settlementOperation(t, a, store, root, w, func(s *Snapshot) {
		s.Run.Status, s.Run.PauseReason = StatusPaused, "parent_pause"
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(root, func(s *Snapshot) error {
		s.Workers[0].Status = workerStatus
		s.Plan.Tasks[0].Status = taskStatus
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, store = reopenCapacityAgent(t, store)
	return a, store, root, w
}

func TestTreeWorkerCapacityRetainedSlotResumeAfterRestart(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, scenario := range []struct{ name, worker, task string }{
			{"worker_uncertain", "uncertain", "paused"},
			{"task_uncertain", "completed", "uncertain"},
			{"both_uncertain", "uncertain", "uncertain"},
		} {
			t.Run(backend+"/"+scenario.name, func(t *testing.T) {
				a, store, root, w := retainedCapacityFixture(t, backend, scenario.worker, scenario.task)
				state, err := a.WorkerCapacity(root)
				if err != nil || state.Active != 1 {
					t.Fatalf("retained reservation: %+v %v", state, err)
				}
				if _, err := a.ClaimTask(root, "1", "overflow", 30); !errors.Is(err, errWorkerCapacity) {
					t.Fatalf("retained slot lent to another task: %v", err)
				}
				child, err := store.Load(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				unlock, err := store.AcquireRunLock(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
				_, release, next, err := a.prepareWorkerResume(context.Background(), child)
				if err != nil {
					t.Fatalf("same worker double-counted its reservation: %v", err)
				}
				defer release()
				if next.ID != w.ID || next.RunID != w.RunID || next.Visit != w.Visit+1 {
					t.Fatalf("identity changed: %+v", next)
				}
				state, err = a.WorkerCapacity(root)
				if err != nil || state.Active != 1 {
					t.Fatalf("resumed count: %+v %v", state, err)
				}
				if _, err := a.ClaimTask(root, "1", "after-resume", 30); !errors.Is(err, errWorkerCapacity) {
					t.Fatalf("resume subtracted an extra slot: %v", err)
				}
			})
		}
	}
}

func TestTreeWorkerCapacityRetainedSlotNeedsCurrentCleanup(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, scenario := range []string{"unknown_cleanup", "stale_visit", "missing_receipt", "active_worker"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				a, store, root, w := retainedCapacityFixture(t, backend, "uncertain", "uncertain")
				if scenario == "unknown_cleanup" || scenario == "missing_receipt" {
					if _, err := store.Update(w.RunID, func(s *Snapshot) error {
						if scenario == "missing_receipt" {
							s.WorkerSettlement = nil
						} else {
							s.WorkerSettlement.CleanupStatus = "unknown"
							s.WorkerSettlement.LifecycleCleanupError = "fixture unresolved lifecycle"
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := store.Update(root, func(s *Snapshot) error {
					switch scenario {
					case "unknown_cleanup":
						s.Workers[0].CleanupStatus = "unknown"
					case "stale_visit":
						s.Workers[0].Visit++
					case "active_worker":
						s.Workers[0].Status = "running"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				child, err := store.Load(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				unlock, err := store.AcquireRunLock(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				_, release, _, err := a.prepareWorkerResume(context.Background(), child)
				if release != nil {
					release()
				}
				unlock()
				if err == nil {
					t.Fatal("invalid cleanup proof resumed retained worker")
				}
				if _, err := a.DisposeTask(root, "0", "abandoned", "inspect unresolved work", ""); err == nil {
					t.Fatal("invalid cleanup proof released disposition")
				}
				state, err := a.WorkerCapacity(root)
				if err != nil || state.Active != 1 {
					t.Fatalf("invalid proof lost capacity: %+v %v", state, err)
				}
			})
		}
	}
}

func TestTreeWorkerCapacityDispositionReleasesOnlySettledIdentity(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, root, w := retainedCapacityFixture(t, backend, "uncertain", "uncertain")
			if _, err := a.DisposeTask(root, "0", "abandoned", "inspected original attempt; no viable continuation", ""); err != nil {
				t.Fatal(err)
			}
			a, store = reopenCapacityAgent(t, store)
			state, err := a.WorkerCapacity(root)
			if err != nil || state.Active != 0 {
				t.Fatalf("legal disposition leaked slot: %+v %v", state, err)
			}
			// Malformed/stale disposal history must not hide live or uncleared work.
			for _, scenario := range []string{"unknown_cleanup", "active", "old_identity", "stale_visit"} {
				if _, err := store.Update(root, func(s *Snapshot) error {
					current := &s.Workers[0]
					current.Status, current.CleanupStatus, current.Visit = "uncertain", "completed", w.Visit
					s.Plan.Tasks[0].WorkerID = w.ID
					switch scenario {
					case "unknown_cleanup":
						current.CleanupStatus = "unknown"
					case "active":
						current.Status = "running"
					case "old_identity":
						s.Plan.Tasks[0].WorkerID = "different-attempt"
					case "stale_visit":
						current.Visit++
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				state, err = a.WorkerCapacity(root)
				if err != nil || state.Active != 1 {
					t.Fatalf("%s hidden by disposition: %+v %v", scenario, state, err)
				}
				if _, err := a.ClaimTask(root, "1", scenario, 30); err == nil {
					t.Fatalf("%s lent occupied slot", scenario)
				}
			}
		})
	}
}

func TestTreeWorkerCapacitySourceRevocationRetainsCompletedWorkerSlot(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, taskContractProvider{})
			defer a.CloseMCP()
			root := createTestRun(t, a)
			if _, err := a.SetPlan(root, []PlanTask{{ID: "first", Goal: "source proof", Requirements: []VerificationRequirement{taskRequirement("first-proof")}}, {ID: "next", Goal: "independent next work"}}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.SetPlanCapacity(root, 1); err != nil {
				t.Fatal(err)
			}
			w, err := a.DispatchTask(context.Background(), root, "first")
			if err != nil {
				t.Fatal(err)
			}
			parent, err := waitAllTestWorkers(a, context.Background(), root, 10)
			if err != nil || parent.Plan.Tasks[0].Status != "verified" || parent.Workers[0].Status != "completed" {
				t.Fatalf("source fixture: %+v %v", parent.Plan, err)
			}
			state, err := a.WorkerCapacity(root)
			if err != nil || state.Active != 0 {
				t.Fatalf("valid source held slot: %+v %v", state, err)
			}
			if _, err := store.Update(w.RunID, func(s *Snapshot) error { s.Evidence[0].Content += "changed-source"; return nil }); err != nil {
				t.Fatal(err)
			}
			a, store = reopenCapacityAgent(t, store)
			parent, err = store.Load(root)
			if err != nil || parent.Plan.Tasks[0].Status != "uncertain" || parent.Workers[0].Status != "completed" {
				t.Fatalf("source not revoked: %+v %v", parent.Plan, err)
			}
			state, err = a.WorkerCapacity(root)
			if err != nil || state.Active != 1 {
				t.Fatalf("revoked source lost reservation: %+v %v", state, err)
			}
			if _, err := a.ClaimTask(root, "next", "overflow", 30); !errors.Is(err, errWorkerCapacity) {
				t.Fatalf("revoked source lent slot: %v", err)
			}
			if _, err := a.DisposeTask(root, "first", "abandoned", "source proof revoked; original effects inspected", ""); err != nil {
				t.Fatal(err)
			}
			state, err = a.WorkerCapacity(root)
			if err != nil || state.Active != 0 {
				t.Fatalf("inspected revoked source leaked slot: %+v %v", state, err)
			}
		})
	}
}

func TestTreeWorkerCapacityRetainedLocalContinuationCountsOnce(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, scenario := range []string{"same_executor", "unknown_action", "uncertain_local", "stale_local_visit", "wrong_manifest"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				a, store := treeCapacityAgent(t, backend, MockProvider{})
				root := createTestRun(t, a)
				if _, err := a.SetPlan(root, capacityTasks(2)); err != nil {
					t.Fatal(err)
				}
				if _, err := a.SetPlanCapacity(root, 1); err != nil {
					t.Fatal(err)
				}
				local, w := capacityFixtureChild(t, a, root, "0")
				if _, err := a.SetPlan(w.RunID, capacityTasks(1)); err != nil {
					t.Fatal(err)
				}
				if _, err := local.ClaimTask(w.RunID, "0", w.ID, 30); err != nil {
					t.Fatal(err)
				}
				if err := settlementOperation(t, a, store, root, w, func(s *Snapshot) { s.Run.Status, s.Run.PauseReason = StatusPaused, "parent_pause" }); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Update(root, func(s *Snapshot) error {
					s.Workers[0].Status, s.Plan.Tasks[0].Status = "uncertain", "uncertain"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Update(w.RunID, func(s *Snapshot) error {
					switch scenario {
					case "unknown_action":
						s.Actions = append(s.Actions, Action{CallID: "lost-reply", Name: "external", Adapter: "mcp", Status: "uncertain"})
					case "uncertain_local":
						s.Plan.Tasks[0].Status = "uncertain"
					case "stale_local_visit":
						s.Plan.Tasks[0].LocalWorkerVisit++
					case "wrong_manifest":
						s.WorkerContext.WorkerID = "different-executor"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				a, store = reopenCapacityAgent(t, store)
				want := 1
				if scenario != "same_executor" {
					want = 2
				}
				state, err := a.WorkerCapacity(root)
				if err != nil || state.Active != want {
					t.Fatalf("retained local %s: %+v %v", scenario, state, err)
				}
				child, err := store.Load(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				unlock, err := store.AcquireRunLock(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
				_, release, next, err := a.prepareWorkerResume(context.Background(), child)
				if release != nil {
					defer release()
				}
				if scenario != "same_executor" {
					if !errors.Is(err, errWorkerCapacity) || !strings.Contains(err.Error(), "separate reservation") {
						t.Fatalf("uncertain local reservation relaxed: %v", err)
					}
					after, loadErr := store.Load(w.RunID)
					if loadErr != nil || valueHash(after.Actions) != valueHash(child.Actions) {
						t.Fatal("blocked continuation changed actions")
					}
					return
				}
				if err != nil {
					t.Fatalf("same settled executor self-locked at capacity 1: %v", err)
				}
				state, err = a.WorkerCapacity(root)
				if err != nil || state.Active != 1 {
					t.Fatalf("parent/child handoff transiently double-counted: %+v %v", state, err)
				}
				if err = a.resumeLocalTaskVisit(w.RunID, next); err != nil {
					t.Fatal(err)
				}
				state, err = a.WorkerCapacity(root)
				if err != nil || state.Active != 1 {
					t.Fatalf("local handoff subtracted twice: %+v %v", state, err)
				}
				if _, err = a.ClaimTask(root, "1", "extra", 30); !errors.Is(err, errWorkerCapacity) {
					t.Fatalf("duplicate slot exemption lent capacity: %v", err)
				}
			})
		}
	}
}
