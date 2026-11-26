package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type dispatchFailureProvider struct{ calls atomic.Int32 }

func (p *dispatchFailureProvider) Complete(context.Context, ModelRequest) (ModelResponse, error) {
	p.calls.Add(1)
	return ModelResponse{Content: "unexpected dispatch"}, nil
}

// Inject a failed atomic publish without replacing the previous snapshot.
func dispatchBlockRename(t *testing.T, store *Store, id string) func() {
	t.Helper()
	path := filepath.Join(store.root, id+".json")
	saved := path + ".dispatch-saved"
	existed := true
	if err := os.Rename(path, saved); errors.Is(err, os.ErrNotExist) {
		existed = false
	} else if err != nil {
		panic(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		panic(err)
	}
	return func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if existed {
			if err := os.Rename(saved, path); err != nil {
				t.Fatal(err)
			}
		}
	}
}
func dispatchFailureFixture(t *testing.T) (*Agent, *Store, string, *dispatchFailureProvider) {
	t.Helper()
	p := &dispatchFailureProvider{}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "must not execute"}, {ID: "next", Goal: "independent"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlanCapacity(id, 1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.StopWorkers(ctx); err != nil {
			t.Error(err)
		}
	})
	return a, store, id, p
}
func dispatchRenameCauses(err error) map[string]bool {
	paths := map[string]bool{}
	var visit func(error)
	visit = func(err error) {
		if link, ok := err.(*os.LinkError); ok && link.Op == "rename" {
			paths[link.New] = true
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			for _, cause := range joined.Unwrap() {
				visit(cause)
			}
		} else if wrapped, ok := err.(interface{ Unwrap() error }); ok {
			visit(wrapped.Unwrap())
		}
	}
	visit(err)
	return paths
}
func dispatchAssertRetainedClaim(t *testing.T, a *Agent, store *Store, id string, p *dispatchFailureProvider) {
	t.Helper()
	s, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	task := s.Plan.Tasks[0]
	if task.Status != "running" || task.DispatchPhase != "preparing" || task.WorkerID == "" || task.ChildRunID == "" || len(s.Workers) != 0 || occupiedWorkerSlots(&s) != 1 {
		t.Fatalf("lost startup reservation: %+v", task)
	}
	if a.HasActiveWorkers() || p.calls.Load() != 0 {
		t.Fatal("failed dispatch retained a lifecycle or called provider")
	}
}

func TestDispatchFailureCreatedChildCleanup(t *testing.T) {
	for _, stage := range []string{"creation", "registration", "cancel", "stopped", "settlement"} {
		t.Run(stage, func(t *testing.T) {
			a, store, id, p := dispatchFailureFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var childID string
			var restore func()
			parentWrites, childWrites := 0, 0
			lockHeld := false
			store.beforePersist = func(got string) {
				if got == id {
					parentWrites++
					if stage == "registration" && parentWrites == 2 || stage == "settlement" && parentWrites == 2 {
						restore = dispatchBlockRename(t, store, id)
					}
					return
				}
				childID = got
				childWrites++
				if childWrites == 1 {
					unlock, err := store.AcquireRunLock(got)
					lockHeld = errors.Is(err, ErrRunLockBusy)
					if err == nil {
						unlock()
					}
					if stage == "creation" {
						restore = dispatchBlockRename(t, store, got)
					}
					if stage == "cancel" || stage == "stopped" || stage == "settlement" {
						cancel()
					}
				}
				if stage == "cancel" && childWrites == 2 {
					restore = dispatchBlockRename(t, store, got)
				}
			}
			_, err := a.DispatchTask(ctx, id, "work")
			store.beforePersist = nil
			if restore != nil {
				restore()
			}
			if err == nil || !lockHeld {
				t.Fatalf("failure or startup lock missing: %v held=%v", err, lockHeld)
			}
			if stage == "cancel" || stage == "stopped" || stage == "settlement" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			if stage != "stopped" {
				if len(dispatchRenameCauses(err)) == 0 {
					t.Fatalf("publish cause lost: %v", err)
				}
				dispatchAssertRetainedClaim(t, a, store, id, p)
			} else {
				s, loadErr := store.Load(id)
				if loadErr != nil || s.Plan.Tasks[0].Status != "pending" || occupiedWorkerSlots(&s) != 0 {
					t.Fatalf("zero-effect cancellation did not release reservation: %+v %v", s.Plan, loadErr)
				}
			}
			if p.calls.Load() != 0 || a.HasActiveWorkers() {
				t.Fatal("aborted preparation executed work")
			}
			unlock, lockErr := store.AcquireRunLock(childID)
			if lockErr != nil {
				t.Fatal(lockErr)
			}
			unlock()
			recovered, recoveryErr := a.RecoverWorkers(id)
			if recoveryErr != nil || recovered.Plan.Tasks[0].Status != "pending" || len(recovered.Workers) != 0 || occupiedWorkerSlots(&recovered) != 0 {
				t.Fatalf("startup recovery did not converge: %+v %v", recovered.Plan, recoveryErr)
			}
			if stage != "creation" {
				child, loadErr := store.Load(childID)
				if loadErr != nil || child.Run.Status != StatusCancelled || len(child.Actions) != 0 {
					t.Fatalf("child not safely retired: %+v %v", child.Run, loadErr)
				}
			}
		})
	}
}

func TestDispatchFailureInputPreflightDoesNotClaim(t *testing.T) {
	a, store, id, p := dispatchFailureFixture(t)
	if _, err := store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[0].InputAttachmentIDs = []string{"missing-input"}; return nil }); err != nil {
		t.Fatal(err)
	}
	writes := 0
	store.beforePersist = func(got string) {
		if got == id {
			writes++
		}
	}
	_, err := a.DispatchTask(context.Background(), id, "work")
	store.beforePersist = nil
	if err == nil || !strings.Contains(err.Error(), "missing-input") || writes != 0 {
		t.Fatalf("invalid input claimed work: writes=%d err=%v", writes, err)
	}
	s, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Plan.Tasks[0].Status != "pending" || occupiedWorkerSlots(&s) != 0 || p.calls.Load() != 0 || a.HasActiveWorkers() {
		t.Fatal("invalid input consumed execution")
	}
}

func TestDispatchPreparationRecovery(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, stage := range []string{"missing", "created", "busy", "effects", "cancelled"} {
			t.Run(backend+"/"+stage, func(t *testing.T) {
				a, store, id, p := dispatchFailureFixture(t)
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
					t.Cleanup(func() { store.Close() })
				}
				childID, err := newID()
				if err != nil {
					t.Fatal(err)
				}
				unlock, err := store.AcquireRunLock(childID)
				if err != nil {
					t.Fatal(err)
				}
				held := true
				defer func() {
					if held {
						unlock()
					}
				}()
				s, err := a.claimTaskWithChild(context.Background(), id, "work", "preparing-worker", 30, childID)
				if err != nil {
					t.Fatal(err)
				}
				if stage != "missing" && stage != "busy" {
					task := s.Plan.Tasks[0]
					c, err := a.newWorkerContext(&s, task)
					if err != nil {
						t.Fatal(err)
					}
					c.RunID, c.WorkerID = childID, task.WorkerID
					if _, err = a.CreateConfigured(task.Goal, RunConfiguration{runID: childID, workerTask: &task, workerContext: c, inheritedRun: &s.Run, role: "worker"}); err != nil {
						t.Fatal(err)
					}
					if stage == "cancelled" {
						if _, err = a.Cancel(childID); err != nil {
							t.Fatal(err)
						}
					}
					if stage == "effects" {
						if _, err = store.Update(childID, func(c *Snapshot) error {
							c.Requests = append(c.Requests, ModelRequest{ID: "started-request"})
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
				}
				if _, err = store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Minute); return nil }); err != nil {
					t.Fatal(err)
				}
				if stage != "busy" {
					unlock()
					held = false
				}
				got, err := a.RecoverWorkers(id)
				if err != nil {
					t.Fatal(err)
				}
				want := "pending"
				if stage == "busy" {
					want = "running"
				}
				if stage == "effects" {
					want = "uncertain"
				}
				if stage == "cancelled" {
					want = "cancelled"
				}
				if got.Plan.Tasks[0].Status != want || p.calls.Load() != 0 || len(got.Workers) != 0 {
					t.Fatalf("recovery %+v want=%s", got.Plan, want)
				}
				occupied := 0
				if want == "running" || want == "uncertain" {
					occupied = 1
				}
				if occupiedWorkerSlots(&got) != occupied {
					t.Fatal("startup capacity changed incorrectly")
				}
				if stage == "busy" && got.Plan.Tasks[0].DispatchPhase != "preparing" {
					t.Fatal("busy startup was changed")
				}
				if stage == "cancelled" {
					for _, ready := range got.Plan.Frontier {
						if ready == "work" {
							t.Fatal("explicitly cancelled preparation returned to frontier")
						}
					}
					if _, err = a.DispatchTask(context.Background(), id, "work"); err == nil {
						t.Fatal("cancelled preparation was dispatched again")
					}
				}
				if stage != "busy" {
					matched := false
					for _, event := range got.Events {
						if event.Type == "task_dispatch_settled" && valueHash(event.Data) == valueHash(got.Plan.Tasks[0]) {
							matched = true
						}
					}
					if !matched {
						t.Fatal("dispatch settlement omitted its committed historical task snapshot")
					}
				}
			})
		}
	}
}

func TestDispatchPreparationOutlivesLease(t *testing.T) {
	a, store, id, _ := dispatchFailureFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initialized := false
	store.beforePersist = func(got string) {
		if got == id || initialized {
			return
		}
		initialized = true
		parent, err := store.loadRawLocked(id)
		if err != nil {
			panic(err)
		}
		parent.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Minute)
		if err = store.saveLocked(parent); err != nil {
			panic(err)
		}
	}
	w, err := a.DispatchTask(ctx, id, "work")
	store.mu.Lock()
	store.beforePersist = nil
	store.mu.Unlock()
	if err != nil || w.RunID == "" || !initialized {
		t.Fatalf("live startup rejected expired TTL: %+v %v", w, err)
	}
	cancel()
}

func TestReservedRunCreationDoesNotOverwrite(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.CreateConfigured("original", RunConfiguration{runID: id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.CreateConfigured("replacement", RunConfiguration{runID: id}); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing reserved ID overwritten: %v", err)
	}
	got, err := store.Load(id)
	if err != nil || got.Run.Goal != first.Run.Goal {
		t.Fatalf("original changed: %+v %v", got.Run, err)
	}
}

func TestDispatchRegistrationRejectsTerminalChild(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, status := range []Status{StatusCancelled, StatusCompleted, StatusPaused} {
			t.Run(backend+"/"+string(status), func(t *testing.T) {
				a, store, id, p := dispatchFailureFixture(t)
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
					t.Cleanup(func() { store.Close() })
				}
				childID, err := newID()
				if err != nil {
					t.Fatal(err)
				}
				unlock, err := store.AcquireRunLock(childID)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
				parent, err := a.claimTaskWithChild(context.Background(), id, "work", "preparing-worker", 30, childID)
				if err != nil {
					t.Fatal(err)
				}
				task := parent.Plan.Tasks[0]
				workerContext, err := a.newWorkerContext(&parent, task)
				if err != nil {
					t.Fatal(err)
				}
				workerContext.RunID, workerContext.WorkerID = childID, task.WorkerID
				if _, err = a.CreateConfigured(task.Goal, RunConfiguration{runID: childID, workerTask: &task, workerContext: workerContext, inheritedRun: &parent.Run, role: "worker"}); err != nil {
					t.Fatal(err)
				}
				if status == StatusCancelled {
					if _, err = a.Cancel(childID); err != nil {
						t.Fatal(err)
					}
				} else if _, err = store.Update(childID, func(child *Snapshot) error { child.Run.Status = status; return nil }); err != nil {
					t.Fatal(err)
				}
				w := AgentWorker{ID: task.WorkerID, TaskID: task.ID, RunID: childID, Status: "running", Visit: 1}
				if err = a.registerPreparedWorker(context.Background(), id, w, workerContext); err == nil {
					t.Fatal("terminal child registered")
				}
				if err = a.abortDispatchPreparation(id, task.ID, w.ID, childID, err); err != nil {
					t.Fatal(err)
				}
				got, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				want := "uncertain"
				if status == StatusCancelled {
					want = "cancelled"
				}
				if got.Plan.Tasks[0].Status != want || len(got.Workers) != 0 || p.calls.Load() != 0 || a.HasActiveWorkers() {
					t.Fatalf("terminal preparation lost: %+v", got.Plan)
				}
			})
		}
	}
}
