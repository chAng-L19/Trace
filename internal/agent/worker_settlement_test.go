package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func settlementFixture(t *testing.T) (*Agent, *Store, string, AgentWorker) {
	t.Helper()
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "one execution"}, {ID: "next", Goal: "dependency", Dependencies: []string{"work"}}}); err != nil {
		t.Fatal(err)
	}
	wid, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ClaimTask(id, "work", wid, 30); err != nil {
		t.Fatal(err)
	}
	child, err := a.Create("one execution")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(child.Run.ID, func(s *Snapshot) error {
		s.RootContract = nil
		s.Run.ParentRunID = id
		s.Run.ParentTaskID = "work"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := AgentWorker{ID: wid, TaskID: "work", RunID: child.Run.ID, Status: "running", CleanupStatus: "pending", Visit: 1, StartedAt: time.Now().UTC()}
	if _, err = store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusRunning
		s.Plan.Capacity = 1
		s.Plan.Tasks[0].ChildRunID = w.RunID
		s.Workers = append(s.Workers, w)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return a, store, id, w
}

// The hook runs with Store.mu held. It changes only this test's TempDir and
// causes the stage/publish path's real os.Rename to fail after Sync.
func settlementRenameFailure(t *testing.T, store *Store, id string) (func(), *bool) {
	t.Helper()
	failed := false
	path := filepath.Join(store.root, id+".json")
	saved := path + ".settlement-saved"
	store.mu.Lock()
	store.beforePersist = func(got string) {
		if got != id || failed {
			return
		}
		if err := os.Rename(path, saved); err != nil {
			panic(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			panic(err)
		}
		failed = true
	}
	store.mu.Unlock()
	restore := func() {
		store.mu.Lock()
		defer store.mu.Unlock()
		store.beforePersist = nil
		if !failed {
			return
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(saved, path); err != nil {
			t.Fatal(err)
		}
	}
	return restore, &failed
}

func settlementOperation(t *testing.T, a *Agent, store *Store, id string, w AgentWorker, run func(*Snapshot)) error {
	t.Helper()
	unlock, err := store.AcquireRunLock(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	_, err = a.runChildOperation(context.Background(), id, w, func(_ *Agent, _ context.Context, runID string) (Snapshot, error) {
		return store.Update(runID, func(s *Snapshot) error { run(s); return nil })
	})
	return err
}

func settlementCounts(s Snapshot) (int, int) {
	n := 0
	for _, e := range s.Events {
		if e.Type == "worker_finished" {
			n++
		}
	}
	return n, len(s.WorkerMessages)
}

func TestWorkerSettlementParentRenameRecovery(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			a, store, id, w := settlementFixture(t)
			restore, failed := settlementRenameFailure(t, store, id)
			calls := 0
			err := settlementOperation(t, a, store, id, w, func(s *Snapshot) { calls++; s.Run.Status = StatusCompleted })
			restore()
			if err == nil || !strings.Contains(err.Error(), "persist run") || !*failed {
				t.Fatalf("missing real rename failure: %v", err)
			}
			if err = a.StopWorkers(context.Background()); err == nil {
				t.Fatal("shutdown hid settlement failure")
			}
			before, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			child, err := store.Load(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if before.Workers[0].CleanupStatus != "pending" || occupiedWorkerSlots(&before) != 1 || !child.WorkerSettlement.matches(id, w) || child.WorkerSettlement.CleanupStatus != "completed" {
				t.Fatal("failed write lost receipt or released capacity")
			}
			if restart {
				if _, err = store.Update(id, func(s *Snapshot) error {
					s.Plan.Tasks[0].Status = "uncertain"
					s.Workers[0].Status, s.Workers[0].CleanupStatus = "uncertain", "unknown"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				reopened, err := NewStore(store.root)
				if err != nil {
					t.Fatal(err)
				}
				fresh := *a
				fresh.store = reopened
				fresh.workers = newWorkerLifecycle()
				a = &fresh
				store = reopened
			}
			// Receipt is sufficient even before lease expiry; no execution is restarted.
			recovered, err := a.RecoverWorkers(id)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Plan.Tasks[0].Status != "completed" || recovered.Workers[0].CleanupStatus != "completed" || occupiedWorkerSlots(&recovered) != 0 || calls != 1 {
				t.Fatalf("recovery: %+v %v", recovered.Workers, calls)
			}
			if err = a.StopWorkers(context.Background()); err != nil {
				t.Fatal("successful recovery retained pending error", err)
			}
			_, err = store.Update(id, func(s *Snapshot) error { return a.applyWorkerSettlementLocked(s, w) })
			if err != nil {
				t.Fatal(err)
			}
			again, err := a.RecoverWorkers(id)
			if err != nil {
				t.Fatal(err)
			}
			finished, messages := settlementCounts(again)
			if finished != 1 || messages != 1 || calls != 1 {
				t.Fatalf("duplicate settlement: %d %d %d", finished, messages, calls)
			}
		})
	}
}

func TestWorkerSettlementReceiptRenameFailureStaysUnknown(t *testing.T) {
	a, store, id, w := settlementFixture(t)
	restore, failed := settlementRenameFailure(t, store, w.RunID)
	// First save commits the actual result; arm the failure only after it.
	store.mu.Lock()
	hook := store.beforePersist
	store.beforePersist = nil
	store.mu.Unlock()
	unlock, err := store.AcquireRunLock(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.runChildOperation(context.Background(), id, w, func(_ *Agent, _ context.Context, runID string) (Snapshot, error) {
		result, e := store.Update(runID, func(s *Snapshot) error { s.Run.Status = StatusCompleted; return nil })
		store.mu.Lock()
		store.beforePersist = hook
		store.mu.Unlock()
		return result, e
	})
	unlock()
	restore()
	if err == nil || !*failed {
		t.Fatal("missing receipt persistence failure")
	}
	if err = a.StopWorkers(context.Background()); err == nil {
		t.Fatal("receipt error hidden")
	}
	child, err := store.Load(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if child.WorkerSettlement != nil {
		t.Fatal("failed receipt was published")
	}
	if _, err = store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Second); return nil }); err != nil {
		t.Fatal(err)
	}
	recovered, err := a.RecoverWorkers(id)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Workers[0].CleanupStatus != "unknown" || occupiedWorkerSlots(&recovered) != 1 {
		t.Fatal("idle lock invented cleanup proof")
	}
	if _, err = a.ResumeWorker(context.Background(), id, w.ID); err == nil {
		t.Fatal("unknown receipt resumed")
	}
}

func TestWorkerSettlementRecoveryFences(t *testing.T) {
	for _, scenario := range []string{"old_visit", "wrong_worker", "wrong_parent", "cleanup_failure", "live_lock"} {
		t.Run(scenario, func(t *testing.T) {
			a, store, id, w := settlementFixture(t)
			receipt := WorkerSettlement{ParentRunID: id, TaskID: w.TaskID, WorkerID: w.ID, RunID: w.RunID, Visit: w.Visit, CleanupStatus: "completed", ObservedAt: time.Now().UTC()}
			switch scenario {
			case "old_visit":
				receipt.Visit--
			case "wrong_worker":
				receipt.WorkerID = "other"
			case "wrong_parent":
				receipt.ParentRunID = "other"
			case "cleanup_failure":
				receipt.CleanupStatus = "uncertain"
				receipt.CleanupError = "DELETE 500"
			}
			if _, err := store.Update(w.RunID, func(s *Snapshot) error { s.Run.Status = StatusCompleted; s.WorkerSettlement = &receipt; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Second); return nil }); err != nil {
				t.Fatal(err)
			}
			if scenario == "live_lock" {
				unlock, err := store.AcquireRunLock(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			recovered, err := a.RecoverWorkers(id)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Workers[0].CleanupStatus == "completed" || occupiedWorkerSlots(&recovered) != 1 {
				t.Fatalf("invalid proof released capacity: %s", scenario)
			}
			n, m := settlementCounts(recovered)
			if n != 0 || m != 0 {
				t.Fatal("invalid receipt emitted final result")
			}
		})
	}
}

func TestWorkerSettlementResumeInvalidatesPreviousVisit(t *testing.T) {
	a, store, id, w := settlementFixture(t)
	if err := settlementOperation(t, a, store, id, w, func(s *Snapshot) { s.Run.Status = StatusPaused; s.Run.PauseReason = "parent_pause" }); err != nil {
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
	_, release, next, err := a.prepareWorkerResume(context.Background(), child)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	release()
	unlock()
	if next.Visit != w.Visit+1 {
		t.Fatal("resume reused settlement visit")
	}
	if err = a.renewActiveWorkerLease(context.Background(), id, w); err == nil {
		t.Fatal("old visit renewed current lease")
	}
	if err = a.renewActiveWorkerLease(context.Background(), id, next); err != nil {
		t.Fatal("current visit could not renew", err)
	}
	if _, err = store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Second); return nil }); err != nil {
		t.Fatal(err)
	}
	recovered, err := a.RecoverWorkers(id)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Workers[0].CleanupStatus != "unknown" || occupiedWorkerSlots(&recovered) != 1 {
		t.Fatal("old receipt settled resumed visit")
	}
}

func TestWorkerSettlementControlAndResultRecovery(t *testing.T) {
	for _, scenario := range []string{"parent_cancel", "parent_pause", "incomplete", "waiting_tool", "predicate"} {
		t.Run(scenario, func(t *testing.T) {
			a, store, id, w := settlementFixture(t)
			if scenario == "predicate" {
				if _, err := store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[0].SuccessContains = "required-proof"; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			restore, _ := settlementRenameFailure(t, store, id)
			err := settlementOperation(t, a, store, id, w, func(s *Snapshot) {
				s.Run.Status = StatusCompleted
				switch scenario {
				case "parent_pause":
					s.Run.Status = StatusFailed
				case "incomplete":
					s.Run.RequireVerifiedCompletion = true
				case "waiting_tool":
					s.Run.Status = StatusWaitingTool
				}
			})
			restore()
			if err == nil {
				t.Fatal("missing parent failure")
			}
			if scenario == "parent_cancel" || scenario == "parent_pause" {
				_, err = store.Update(id, func(s *Snapshot) error {
					if scenario == "parent_cancel" {
						s.Run.Status = StatusCancelled
					} else {
						s.Run.Status = StatusPaused
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			recovered, err := a.RecoverWorkers(id)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"parent_cancel": "cancelled", "parent_pause": "paused", "incomplete": "uncertain", "waiting_tool": "uncertain", "predicate": "failed"}[scenario]
			if recovered.Plan.Tasks[0].Status != want || recovered.Workers[0].CleanupStatus != "completed" {
				t.Fatalf("control/result changed: %+v", recovered.Plan.Tasks[0])
			}
			if hasString(recovered.Plan.Frontier, "next") {
				t.Fatal("unverified result unlocked dependency")
			}
		})
	}
}

func TestWorkerSettlementBackgroundFailureIsObservable(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "background completion"}}); err != nil {
		t.Fatal(err)
	}
	failed := false
	path := filepath.Join(store.root, id+".json")
	saved := path + ".settlement-saved"
	store.mu.Lock()
	store.beforePersist = func(got string) {
		if got != id || failed {
			return
		}
		b, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		var parent Snapshot
		if err = json.Unmarshal(b, &parent); err != nil {
			panic(err)
		}
		if len(parent.Workers) != 1 {
			return
		}
		b, err = os.ReadFile(filepath.Join(store.root, parent.Workers[0].RunID+".json"))
		if err != nil {
			panic(err)
		}
		var child Snapshot
		if err = json.Unmarshal(b, &child); err != nil {
			panic(err)
		}
		if child.WorkerSettlement == nil {
			return
		}
		if err = os.Rename(path, saved); err != nil {
			panic(err)
		}
		if err = os.Mkdir(path, 0700); err != nil {
			panic(err)
		}
		failed = true
	}
	store.mu.Unlock()
	if _, err := a.DispatchTask(context.Background(), id, "work"); err != nil {
		t.Fatal(err)
	}
	a.workers.wg.Wait()
	store.mu.Lock()
	store.beforePersist = nil
	if failed {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(saved, path); err != nil {
			t.Fatal(err)
		}
	}
	store.mu.Unlock()
	if !failed {
		t.Fatal("background persistence fault did not fire")
	}
	if err := a.StopWorkers(context.Background()); err == nil {
		t.Fatal("background error swallowed")
	}
	if _, err := a.RecoverWorkers(id); err != nil {
		t.Fatal(err)
	}
	if err := a.StopWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
}
