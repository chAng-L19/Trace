package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func pauseTreeFixture(t *testing.T, backend string, status Status) (*Agent, *Store, Snapshot, Snapshot, string) {
	t.Helper()
	a, store := treeCapacityAgent(t, backend, MockProvider{})
	root, middle, leaf := createTestRun(t, a), createTestRun(t, a), createTestRun(t, a)
	if _, err := store.Update(leaf, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Actions = []Action{{CallID: "unknown", Adapter: "exec", Status: "uncertain", ResourceOwner: "retained"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	mid, err := store.Update(middle, func(s *Snapshot) error {
		s.Run.Status, s.Run.PauseReason = status, "operator_pause"
		s.Run.ElapsedMillis = 321
		s.Workers = []AgentWorker{{ID: "leaf-worker", RunID: leaf, Status: "running", CleanupStatus: "pending"}}
		addEvent(s, "run_paused", "middle pause", nil)
		addEvent(s, "model_stream", "later event is not the pause", nil)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Update(root, func(s *Snapshot) error {
		s.Run.Status, s.Run.PauseReason = StatusPaused, "operator_pause"
		s.Workers = []AgentWorker{{ID: "middle-worker", RunID: middle, Status: "running", CleanupStatus: "pending"}}
		addEvent(s, "run_paused", "root pause", nil)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, store, parent, mid, leaf
}

func TestPauseStoppedWorkerSkipsWriteAndTraversesDescendants(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, status := range []Status{StatusPaused, StatusCompleted, StatusCancelled} {
			t.Run(backend+"/"+string(status), func(t *testing.T) {
				a, store, root, middle, leaf := pauseTreeFixture(t, backend, status)
				writes := map[string]int{}
				store.beforePersist = func(id string) { writes[id]++ }
				defer func() { store.beforePersist = nil }()
				if err := a.pauseSnapshotWorkers(root); err != nil {
					t.Fatal(err)
				}
				if writes[middle.Run.ID] != 0 || writes[leaf] != 1 {
					t.Fatalf("unexpected pause writes: %v", writes)
				}
				if err := a.pauseSnapshotWorkers(root); err != nil {
					t.Fatal(err)
				}
				if writes[middle.Run.ID] != 0 || writes[leaf] != 1 {
					t.Fatalf("repeat pause rewrote stopped child: %v", writes)
				}
				current, err := store.Load(middle.Run.ID)
				if err != nil || !reflect.DeepEqual(current.Run, middle.Run) {
					t.Fatal("stopped child status, reason, budget or timestamp changed", err)
				}
				child, err := store.Load(leaf)
				if err != nil || child.Run.Status != StatusPaused || child.Run.PauseReason != "parent_pause" {
					t.Fatal("descendant pause was skipped", err)
				}
				if len(child.Actions) != 1 || child.Actions[0].Status != "uncertain" || toolCleanupError(child) == nil {
					t.Fatal("pause fabricated an outcome or cleanup")
				}
				last := child.Events[len(child.Events)-1]
				if status == StatusPaused {
					data := last.Data.(map[string]any)
					if data["parent_event_sequence"] != float64(middle.Run.EventSeq-1) {
						t.Fatal("pause source was replaced by the latest event", data)
					}
				}
			})
		}
	}
}

func TestPauseUnchangedWorkerStillPersistsSourceRevocation(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, root, middle, _ := pauseTreeFixture(t, backend, StatusPaused)
			// Seed a stale saved projection directly; the next verified load must revoke it.
			middle.Assets = []Asset{{ID: "stale", Coverage: "verified", Predicate: "proof", EvidenceIDs: []string{"missing"}}}
			unlock, err := store.acquireStoreLock()
			if err != nil {
				t.Fatal(err)
			}
			err = store.saveLocked(middle)
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			writes := 0
			store.beforePersist = func(id string) {
				if id == middle.Run.ID {
					writes++
				}
			}
			defer func() { store.beforePersist = nil }()
			if err := a.pauseSnapshotWorkers(root); err != nil {
				t.Fatal(err)
			}
			if writes != 1 {
				t.Fatalf("revocation needs one commit, got %d", writes)
			}
			other, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			current, err := other.Load(middle.Run.ID)
			if err != nil || current.Assets[0].Coverage != "uncertain" || current.Run.EventSeq <= middle.Run.EventSeq {
				t.Fatal("no-op suppressed durable source revocation", err)
			}
		})
	}
}

func TestPauseDescendantCommitFailureIsNotNoop(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, root, _, leaf := pauseTreeFixture(t, backend, StatusPaused)
			var restore func()
			if backend == "json" {
				store.beforePersist = func(id string) {
					if id == leaf && restore == nil {
						restore = dispatchBlockRename(t, store, id)
					}
				}
			} else {
				db, err := store.sqliteDB()
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec("CREATE TRIGGER reject_pause BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'pause fixture failure'); END"); err != nil {
					t.Fatal(err)
				}
			}
			err := a.pauseSnapshotWorkers(root)
			store.beforePersist = nil
			if restore != nil {
				restore()
			}
			if err == nil || errors.Is(err, errPauseWorkerUnchanged) {
				t.Fatal("failed commit was treated as a no-op", err)
			}
			current, loadErr := store.Load(leaf)
			if loadErr != nil || current.Run.Status != StatusWaitingTool {
				t.Fatal("failed commit changed descendant", loadErr)
			}
		})
	}
}

func controlSubscriberCount(store *Store, id string) int {
	store.eventViewMu.Lock()
	defer store.eventViewMu.Unlock()
	if entry := store.scheduling[id]; entry != nil {
		return len(entry.readers)
	}
	return 0
}

func TestControlledContextUsesCommittedHintsAndReleasesSubscription(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, external := range []bool{false, true} {
			name := backend + "/local"
			if external {
				name = backend + "/other-store"
			}
			t.Run(name, func(t *testing.T) {
				a, store := treeCapacityAgent(t, backend, MockProvider{})
				id := createTestRun(t, a)
				if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
					t.Fatal(err)
				}
				ctx, stop := a.controlledContext(context.Background(), id)
				defer stop()
				if controlSubscriberCount(store, id) != 1 {
					t.Fatal("control did not subscribe")
				}
				writer := store
				if external {
					var err error
					writer, err = NewStore(store.root)
					if err != nil {
						t.Fatal(err)
					}
					defer writer.Close()
				}
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				defer unblock()
				writer.beforePersist = func(string) { close(entered); <-release }
				done := make(chan error, 1)
				go func() {
					_, err := writer.Update(id, func(s *Snapshot) error {
						s.Run.Status, s.Run.PauseReason = StatusPaused, "operator_pause"
						addEvent(s, "run_paused", "committed control", nil)
						return nil
					})
					done <- err
				}()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("commit barrier not reached")
				}
				// Even a coalesced hint cannot turn an uncommitted candidate into a pause.
				store.eventViewMu.Lock()
				for ch := range store.scheduling[id].readers {
					select {
					case ch <- struct{}{}:
					default:
					}
				}
				store.eventViewMu.Unlock()
				select {
				case <-ctx.Done():
					t.Fatal("cancelled before commit")
				case <-time.After(150 * time.Millisecond):
				}
				unblock()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("commit did not finish")
				}
				writer.beforePersist = nil
				select {
				case <-ctx.Done():
				case <-time.After(2 * time.Second):
					t.Fatal("committed stop was not observed")
				}
				stop()
				if controlSubscriberCount(store, id) != 0 {
					t.Fatal("control subscription leaked")
				}
			})
		}
	}
}

func TestControlledContextInitiallyStoppedAndParentSubscriptionLifecycle(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			root := createTestRun(t, a)
			if _, err := a.SetPlan(root, capacityTasks(1)); err != nil {
				t.Fatal(err)
			}
			_, w := capacityFixtureChild(t, a, root, "0")
			parentCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			done := make(chan error, 1)
			go func() {
				_, err := a.runChildOperation(parentCtx, root, w, func(a *Agent, ctx context.Context, id string) (Snapshot, error) {
					close(entered)
					<-ctx.Done()
					snap, err := a.store.Update(id, func(s *Snapshot) error { s.Run.Status, s.Run.PauseReason = StatusPaused, "parent_pause"; return nil })
					return snap, errors.Join(ctx.Err(), err)
				})
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("child operation not entered")
			}
			if controlSubscriberCount(store, root) != 1 {
				t.Fatal("parent observer did not subscribe")
			}
			if _, err := store.Update(root, func(s *Snapshot) error {
				s.Run.Status, s.Run.PauseReason = StatusPaused, "operator_pause"
				addEvent(s, "run_paused", "parent commit", nil)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("parent stop did not settle child")
			}
			if controlSubscriberCount(store, root) != 0 {
				t.Fatal("parent subscription leaked")
			}
			child, err := store.Load(w.RunID)
			if err != nil || child.WorkerSettlement == nil || !child.WorkerSettlement.matches(root, w) || child.WorkerSettlement.CleanupStatus != "completed" {
				t.Fatal("control notification bypassed durable child settlement", err)
			}
			ctx, stop := a.controlledContext(context.Background(), root)
			defer stop()
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("initial persisted pause missed")
			}
			stop()
			if controlSubscriberCount(store, root) != 0 {
				t.Fatal("initial pause leaked subscription")
			}
		})
	}
}
