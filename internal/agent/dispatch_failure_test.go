package agent

import (
	"context"
	"errors"
	"fmt"
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

// Called under Store.mu: the real atomic Rename fails, leaving the prior
// snapshot untouched. Restores run only after the dispatch has returned.
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
	if task.Status != "running" || task.WorkerID == "" || task.ChildRunID != "" || task.LeaseUntil.IsZero() || len(s.Workers) != 0 || occupiedWorkerSlots(&s) != 1 {
		t.Fatalf("failed dispatch released or registered its claim: %+v workers=%+v", task, s.Workers)
	}
	if a.HasActiveWorkers() || p.calls.Load() != 0 {
		t.Fatal("failed dispatch left an active lifecycle or invoked the model")
	}
}

func TestDispatchFailureCreateAndSettlementRename(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
			a, store, id, p := dispatchFailureFixture(t)
			var restores []func()
			var childID string
			parentWrites := 0
			store.mu.Lock()
			store.beforePersist = func(got string) {
				if got == id {
					parentWrites++
					if parentWrites == 2 {
						restores = append(restores, dispatchBlockRename(t, store, got))
					}
				} else {
					childID = got
					restores = append(restores, dispatchBlockRename(t, store, got))
				}
			}
			store.mu.Unlock()
			_, err := a.DispatchTask(context.Background(), id, "work")
			store.mu.Lock()
			store.beforePersist = nil
			for _, restore := range restores {
				restore()
			}
			store.mu.Unlock()
			causes := dispatchRenameCauses(err)
			if len(causes) != 2 || !causes[filepath.Join(store.root, id+".json")] || !causes[filepath.Join(store.root, childID+".json")] {
				t.Fatalf("create and settlement rename causes not preserved: %v", err)
			}
			dispatchAssertRetainedClaim(t, a, store, id, p)
			if _, err = store.Update(id, func(s *Snapshot) error {
				s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Second)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if restart {
				reopened, err := NewStore(store.root)
				if err != nil {
					t.Fatal(err)
				}
				fresh := *a
				fresh.store, fresh.workers = reopened, newWorkerLifecycle()
				a, store = &fresh, reopened
			}
			for n := 0; n < 2; n++ {
				s, err := a.RecoverWorkers(id)
				if err != nil {
					t.Fatal(err)
				}
				if s.Plan.Tasks[0].Status != "uncertain" || s.Plan.Tasks[0].ChildRunID != "" || occupiedWorkerSlots(&s) != 1 || hasString(s.Plan.Frontier, "work") || len(s.Workers) != 0 {
					t.Fatalf("recovery released or replayed unknown claim: %+v", s.Plan)
				}
			}
			if _, err = a.DispatchTask(context.Background(), id, "work"); err == nil {
				t.Fatal("uncertain claim was replayed")
			}
			if _, err = a.DispatchTask(context.Background(), id, "next"); !errors.Is(err, errWorkerCapacity) {
				t.Fatalf("unknown claim no longer occupies capacity: %v", err)
			}
			runs, err := store.List()
			if err != nil || len(runs) != 1 || p.calls.Load() != 0 || a.HasActiveWorkers() {
				t.Fatalf("recovery created work: runs=%d calls=%d err=%v", len(runs), p.calls.Load(), err)
			}
		})
	}
}

func TestDispatchFailureInputPreflightDoesNotClaim(t *testing.T) {
	a, store, id, p := dispatchFailureFixture(t)
	// Simulate a stored input reference that no longer resolves in the parent.
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Plan.Tasks[0].InputAttachmentIDs = []string{"missing-input"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	writes := 0
	store.mu.Lock()
	store.beforePersist = func(got string) {
		if got == id {
			writes++
		}
	}
	store.mu.Unlock()
	_, err := a.DispatchTask(context.Background(), id, "work")
	store.mu.Lock()
	store.beforePersist = nil
	store.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "missing-input") || writes != 0 {
		t.Fatalf("invalid input was not rejected before claiming: writes=%d err=%v", writes, err)
	}
	s, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	task := s.Plan.Tasks[0]
	if task.Status != "pending" || task.WorkerID != "" || task.ChildRunID != "" || !task.LeaseUntil.IsZero() || len(s.Workers) != 0 || occupiedWorkerSlots(&s) != 0 || p.calls.Load() != 0 || a.HasActiveWorkers() {
		t.Fatalf("invalid input consumed a worker or invoked the model: %+v workers=%+v", task, s.Workers)
	}
}

func TestDispatchFailureCreatedChildCleanup(t *testing.T) {
	for _, stage := range []string{"initialization", "registration", "cancel", "lock", "lock_cancel", "stopped", "settlement"} {
		t.Run(stage, func(t *testing.T) {
			a, store, id, p := dispatchFailureFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var childID string
			var restore, heldLock func()
			initializationLocked := false
			parentWrites, childWrites := 0, 0
			store.mu.Lock()
			store.beforePersist = func(got string) {
				if got == id {
					parentWrites++
					if stage == "registration" && parentWrites == 2 || stage == "settlement" && parentWrites == 3 {
						restore = dispatchBlockRename(t, store, id)
					}
					return
				}
				childID = got
				childWrites++
				if !strings.HasPrefix(stage, "lock") && childWrites == 2 {
					unlock, lockErr := store.AcquireRunLock(got)
					initializationLocked = lockErr != nil
					if lockErr == nil {
						unlock()
					}
				}
				if strings.HasPrefix(stage, "lock") && childWrites == 1 {
					var err error
					heldLock, err = store.AcquireRunLock(got)
					if err != nil {
						panic(err)
					}
				}
				if (stage == "initialization" || stage == "lock_cancel") && childWrites == 2 || stage == "cancel" && childWrites == 3 {
					restore = dispatchBlockRename(t, store, got)
				}
				if (stage == "cancel" || stage == "stopped" || stage == "settlement") && childWrites == 2 {
					cancel()
				}
			}
			store.mu.Unlock()
			_, err := a.DispatchTask(ctx, id, "work")
			store.mu.Lock()
			store.beforePersist = nil
			if restore != nil {
				restore()
			}
			store.mu.Unlock()
			if heldLock != nil {
				defer heldLock()
			}
			if err == nil {
				t.Fatal("dispatch failure was swallowed")
			}
			if !strings.HasPrefix(stage, "lock") && !initializationLocked {
				t.Fatal("child initialization ran without its execution lock")
			}
			childPath := filepath.Join(store.root, childID+".json")
			switch stage {
			case "initialization", "registration":
				var cancelCause *os.PathError
				path := childPath
				if stage == "registration" {
					path = filepath.Join(store.root, id+".json")
				}
				if !dispatchRenameCauses(err)[path] || !errors.As(err, &cancelCause) {
					t.Fatalf("original rename and cancellation failures missing: %v", err)
				}
			case "cancel", "stopped", "settlement":
				if !errors.Is(err, context.Canceled) || stage == "cancel" && !dispatchRenameCauses(err)[childPath] || stage == "settlement" && !dispatchRenameCauses(err)[filepath.Join(store.root, id+".json")] {
					t.Fatalf("context or cancellation cause missing: %v", err)
				}
			case "lock", "lock_cancel":
				if !errors.Is(err, ErrRunLockBusy) || stage == "lock_cancel" && !dispatchRenameCauses(err)[childPath] {
					t.Fatalf("lock or cancellation cause missing: %v", err)
				}
			}
			child, loadErr := store.Load(childID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			want := StatusCreated
			if stage == "registration" || stage == "lock" || stage == "stopped" || stage == "settlement" {
				want = StatusCancelled
			}
			if child.Run.Status != want || len(child.Requests) != 0 || len(child.Actions) != 0 || a.HasActiveWorkers() || p.calls.Load() != 0 {
				t.Fatalf("child executed or cleanup state was invented: status=%s want=%s", child.Run.Status, want)
			}
			if heldLock != nil {
				if unlock, err := store.AcquireRunLock(childID); err == nil {
					unlock()
					t.Fatal("dispatch released another executor's lock")
				}
			} else {
				unlock, err := store.AcquireRunLock(childID)
				if err != nil {
					t.Fatal("failed dispatch leaked its child lock", err)
				}
				unlock()
			}
			if stage == "stopped" {
				parent, err := store.Load(id)
				if err != nil || parent.Plan.Tasks[0].Status != "failed" || occupiedWorkerSlots(&parent) != 0 {
					t.Fatalf("settled non-started child did not release claim: %v", err)
				}
			} else {
				dispatchAssertRetainedClaim(t, a, store, id, p)
			}
		})
	}
}
