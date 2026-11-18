package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// MarshalJSON is a deterministic interleave at candidate construction. It also
// proves the actual AppendEvents path has not taken either writer lock yet.
type optimisticEventData struct {
	calls  int
	before func(int) error
}

func (d *optimisticEventData) MarshalJSON() ([]byte, error) {
	d.calls++
	if d.before != nil {
		if err := d.before(d.calls); err != nil {
			return nil, err
		}
	}
	return []byte("{\"payload\":\"kept\"}"), nil
}

func optimisticFixture(t *testing.T) (*Store, string) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create("fixture", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	return store, snap.Run.ID
}

func assertOptimisticTempsClean(t *testing.T, store *Store) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(store.root, ".trace-*.tmp"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("staging leaked: %v %v", paths, err)
	}
}

func optimisticLocksFree(store *Store) error {
	if !store.mu.TryLock() {
		return errors.New("candidate holds Store.mu")
	}
	store.mu.Unlock()
	unlock, err := acquireFileLock(filepath.Join(store.root, ".store.lock"), false)
	if err != nil {
		return fmt.Errorf("candidate holds global file lock: %w", err)
	}
	unlock()
	return nil
}

func TestOptimisticTelemetryRetriesSemanticInterleave(t *testing.T) {
	for _, anotherStore := range []bool{false, true} {
		for _, conflicts := range []int{1, 2} {
			t.Run(fmt.Sprintf("other=%t/conflicts=%d", anotherStore, conflicts), func(t *testing.T) {
				store, id := optimisticFixture(t)
				writer := store
				if anotherStore {
					var err error
					writer, err = NewStore(store.root)
					if err != nil {
						t.Fatal(err)
					}
				}
				if _, _, err := store.EventBatch(id, 0); err != nil {
					t.Fatal(err)
				}
				data := &optimisticEventData{}
				data.before = func(call int) error {
					if call > conflicts {
						if conflicts == 2 {
							if store.mu.TryLock() {
								store.mu.Unlock()
								return errors.New("second conflict did not enter locked fallback")
							}
						}
						return nil
					}
					if err := optimisticLocksFree(store); err != nil {
						return err
					}
					_, err := writer.Update(id, func(s *Snapshot) error {
						s.Run.Goal = fmt.Sprintf("semantic-%d", call)
						s.Plan.Capacity = call + 1
						addEvent(s, "semantic", fmt.Sprint(call), nil)
						return nil
					})
					return err
				}
				input := []Event{{Type: "model_stream", Summary: "exactly once", Data: data}}
				if err := store.AppendEvents(id, input); err != nil {
					t.Fatal(err)
				}
				if data.calls != conflicts+1 {
					t.Fatalf("unexpected attempts: %d", data.calls)
				}
				if input[0].Sequence != 0 || !input[0].CreatedAt.IsZero() {
					t.Fatal("candidate mutated caller event")
				}
				got, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				if got.Run.Goal != fmt.Sprintf("semantic-%d", conflicts) || got.Plan.Capacity != conflicts+1 {
					t.Fatalf("semantic update lost: %+v %+v", got.Run, got.Plan)
				}
				if got.Run.EventSeq != conflicts+1 || len(got.Events) != conflicts+1 {
					t.Fatalf("duplicated retry: %+v", got.Events)
				}
				for i, event := range got.Events {
					want := "semantic"
					if i == conflicts {
						want = "model_stream"
					}
					if event.Sequence != i+1 || event.Type != want {
						t.Fatalf("incorrect order: %+v", got.Events)
					}
				}
				assertOptimisticTempsClean(t, store)
			})
		}
	}
}

func TestOptimisticTelemetryDeleteInterleaveNeverResurrects(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(fmt.Sprint(permanent), func(t *testing.T) {
			store, id := optimisticFixture(t)
			other, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			changed := store.EventChanges(id)
			scheduled, stop := store.SubscribeScheduling(id)
			defer stop()
			data := &optimisticEventData{before: func(call int) error {
				if err := optimisticLocksFree(store); err != nil {
					return err
				}
				if permanent {
					_, err := other.PurgeRun(id)
					return err
				}
				return other.Delete(id)
			}}
			err = store.AppendEvents(id, []Event{{Type: "worker_message", Data: data}})
			if err == nil {
				t.Fatal("deleted run accepted telemetry")
			}
			if permanent && !os.IsNotExist(err) {
				t.Fatalf("missing run error lost: %v", err)
			}
			assertSchedulingQuiet(t, changed)
			assertSchedulingQuiet(t, scheduled)
			if permanent {
				if _, err := os.Stat(filepath.Join(store.root, id+".json")); !os.IsNotExist(err) {
					t.Fatalf("purge resurrected: %v", err)
				}
			} else {
				got, err := store.Load(id)
				if err != nil || !got.Run.Deleted || got.Run.EventSeq != 0 || len(got.Events) != 0 {
					t.Fatalf("delete overwritten: %+v %v", got.Run, err)
				}
			}
			assertOptimisticTempsClean(t, store)
		})
	}
}

func TestOptimisticTelemetryRenameFailurePublishesNothing(t *testing.T) {
	store, id := optimisticFixture(t)
	release := store.HoldEventView(id)
	defer release()
	if _, err := store.viewFields(id, true); err != nil {
		t.Fatal(err)
	}
	old, _ := store.eventViews.Load(id)
	changed := store.EventChanges(id)
	scheduled, stop := store.SubscribeScheduling(id)
	defer stop()
	var restore func()
	defer func() {
		store.beforePersist = nil
		if restore != nil {
			restore()
		}
	}()
	hookCalls := 0
	store.beforePersist = func(got string) {
		if got != id {
			t.Fatalf("wrong run: %s", got)
		}
		hookCalls++
		if store.mu.TryLock() {
			store.mu.Unlock()
			t.Fatal("beforePersist outside Store.mu")
		}
		if unlock, err := acquireFileLock(filepath.Join(store.root, ".store.lock"), false); err == nil {
			unlock()
			t.Fatal("beforePersist outside cross-process lock")
		}
		paths, err := filepath.Glob(filepath.Join(store.root, ".trace-*.tmp"))
		if err != nil || len(paths) != 1 {
			t.Fatalf("candidate not staged before lock: %v %v", paths, err)
		}
		raw, err := os.ReadFile(paths[0])
		var pending Snapshot
		if err != nil || json.Unmarshal(raw, &pending) != nil || pending.Run.EventSeq != 1 {
			t.Fatalf("incomplete staged snapshot: %v", err)
		}
		run, batch, err := store.EventBatch(id, 0)
		if err != nil || run.EventSeq != 0 || len(batch) != 0 {
			t.Fatal("candidate sequence visible before rename")
		}
		assertSchedulingQuiet(t, changed)
		assertSchedulingQuiet(t, scheduled)
		restore = dispatchBlockRename(t, store, id)
	}
	err := store.AppendEvents(id, []Event{{Type: "worker_message"}})
	var rename *os.LinkError
	if !errors.As(err, &rename) || rename.Op != "rename" || hookCalls != 1 {
		t.Fatalf("rename injection lost: %v hooks=%d", err, hookCalls)
	}
	assertSchedulingQuiet(t, changed)
	assertSchedulingQuiet(t, scheduled)
	current, _ := store.eventViews.Load(id)
	if current != old {
		t.Fatal("failed candidate replaced committed cache")
	}
	assertOptimisticTempsClean(t, store)
	store.beforePersist = nil
	restore()
	restore = nil
	if err := store.AppendEvents(id, []Event{{Type: "worker_message"}}); err != nil {
		t.Fatal(err)
	}
	run, batch, err := store.EventBatch(id, 0)
	if err != nil || run.EventSeq != 1 || len(batch) != 1 {
		t.Fatalf("failed attempt consumed sequence: %d %d %v", run.EventSeq, len(batch), err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("successful rename did not notify event reader")
	}
	select {
	case <-scheduled:
	default:
		t.Fatal("successful rename did not notify scheduler")
	}
	assertOptimisticTempsClean(t, store)
}

func TestOptimisticTelemetryReleaseDuringBuildDoesNotResurrectCache(t *testing.T) {
	store, id := optimisticFixture(t)
	if _, err := store.viewFields(id, true); err != nil {
		t.Fatal(err)
	}
	data := &optimisticEventData{before: func(int) error { store.ReleaseEventView(id); return nil }}
	if err := store.AppendEvents(id, []Event{{Type: "model_stream", Data: data}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.eventViews.Load(id); ok {
		t.Fatal("released cache resurrected by candidate")
	}
	got, err := store.Load(id)
	if err != nil || got.Run.EventSeq != 1 {
		t.Fatalf("released cache lost durable append: %+v %v", got.Run, err)
	}
	assertOptimisticTempsClean(t, store)
}

func TestOptimisticTelemetryRejectsSameSizeAndTimeDifferentInode(t *testing.T) {
	store, id := optimisticFixture(t)
	previous, err := store.viewFields(id, true)
	if err != nil {
		t.Fatal(err)
	}
	events := []Event{{Type: "worker_message"}}
	v, through, err := buildAppendedEvents(id, previous, events)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := v.snapshotParts()
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.stageSnapshot(parts...)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(staged.path)
	path := filepath.Join(store.root, id+".json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := store.stageSnapshot(original)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(replacement.path)
	store.mu.Lock()
	unlock, err := store.acquireStoreLock()
	if err != nil {
		store.mu.Unlock()
		t.Fatal(err)
	}
	_, err = store.publishSnapshotLocked(id, replacement)
	if err == nil {
		err = os.Chtimes(path, previous.info.ModTime(), previous.info.ModTime())
	}
	unlock()
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	now, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if now.Size() != previous.info.Size() || !now.ModTime().Equal(previous.info.ModTime()) || os.SameFile(now, previous.info) {
		t.Fatal("inode fixture invalid")
	}
	changed := store.EventChanges(id)
	scheduled, stop := store.SubscribeScheduling(id)
	defer stop()
	committed, err := store.commitAppendedEvents(id, previous, v, staged, events, through)
	if err != nil || committed {
		t.Fatalf("stale inode candidate committed: %t %v", committed, err)
	}
	assertSchedulingQuiet(t, changed)
	assertSchedulingQuiet(t, scheduled)
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("stale candidate replaced generation: %v", err)
	}
	if err := os.Remove(staged.path); err != nil {
		t.Fatal(err)
	}
	assertOptimisticTempsClean(t, store)
}

func TestOptimisticTelemetryRecoversInterruptedPurgeBeforeFallback(t *testing.T) {
	store, id := optimisticFixture(t)
	stage, err := os.MkdirTemp(store.root, ".run-delete-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(store.root, id+".json"), filepath.Join(stage, "run-"+id+".json")); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvents(id, []Event{{Type: "model_stream"}}); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load(id)
	if err != nil || got.Run.EventSeq != 1 || len(got.Events) != 1 {
		t.Fatalf("purge recovery lost append: %+v %v", got.Run, err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatalf("purge stage remains: %v", err)
	}
	assertOptimisticTempsClean(t, store)
}

func TestOptimisticTelemetryLockFailureDiscardsStagedCandidate(t *testing.T) {
	store, id := optimisticFixture(t)
	changed := store.EventChanges(id)
	scheduled, stop := store.SubscribeScheduling(id)
	defer stop()
	path := filepath.Join(store.root, id+".json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(store.root, ".store.lock")
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvents(id, []Event{{Type: "worker_message"}}); err == nil {
		t.Fatal("lock failure ignored")
	}
	assertSchedulingQuiet(t, changed)
	assertSchedulingQuiet(t, scheduled)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("lock failure changed durable generation: %v", err)
	}
	assertOptimisticTempsClean(t, store)
}

func TestOptimisticTelemetryConcurrentStoresKeepEveryEvent(t *testing.T) {
	store, id := optimisticFixture(t)
	other, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errors := make(chan error, 3)
	var group sync.WaitGroup
	for writer, current := range []*Store{store, other, store} {
		group.Add(1)
		go func(writer int, current *Store) {
			defer group.Done()
			<-start
			for i := 0; i < 12; i++ {
				label := fmt.Sprintf("%d/%d", writer, i)
				var err error
				if writer == 2 {
					_, err = current.Update(id, func(s *Snapshot) error { s.Run.Goal = label; addEvent(s, "semantic", label, nil); return nil })
				} else {
					err = current.AppendEvents(id, []Event{{Type: "model_stream", Summary: label}})
				}
				if err != nil {
					errors <- err
					return
				}
			}
		}(writer, current)
	}
	close(start)
	group.Wait()
	close(errors)
	for err := range errors {
		t.Fatal(err)
	}
	got, err := store.Load(id)
	if err != nil || got.Run.EventSeq != 36 || len(got.Events) != 36 || got.Run.Goal != "2/11" {
		t.Fatalf("concurrent writes lost: %+v events=%d %v", got.Run, len(got.Events), err)
	}
	seen := map[string]bool{}
	for i, event := range got.Events {
		if event.Sequence != i+1 || seen[event.Summary] {
			t.Fatalf("duplicate or invalid sequence: %+v", event)
		}
		seen[event.Summary] = true
	}
	assertOptimisticTempsClean(t, store)
}
