package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func lockRecoveryFixture(t *testing.T, backend string) (*Agent, *Store, string, AgentWorker) {
	t.Helper()
	a, store, id, w := settlementFixture(t)
	if backend == "sqlite" {
		if err := MigrateStoreToSQLite(store.root); err != nil {
			t.Fatal(err)
		}
		var err error
		store, err = NewStore(store.root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		a.store = store
	}
	return a, store, id, w
}

func failLockFile(t *testing.T, store *Store, id string) {
	t.Helper()
	path := filepath.Join(store.root, id+".lock")
	// Migration may have created this unlocked file in the test's TempDir.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
}

func TestRunLockClassifiesContentionOnly(t *testing.T) {
	_, store := newTestAgent(t, MockProvider{})
	unlock, err := store.AcquireRunLock("held")
	if err != nil {
		t.Fatal(err)
	}
	if release, err := store.AcquireRunLock("held"); !errors.Is(err, ErrRunLockBusy) || release != nil {
		t.Fatalf("contention not classified: %v", err)
	}
	unlock()
	release, err := store.AcquireRunLock("held")
	if err != nil {
		t.Fatal("lock did not release", err)
	}
	release()
	if err := os.Mkdir(filepath.Join(store.root, "broken.lock"), 0700); err != nil {
		t.Fatal(err)
	}
	_, err = store.AcquireRunLock("broken")
	var access *os.PathError
	if errors.Is(err, ErrRunLockBusy) || !errors.As(err, &access) {
		t.Fatalf("lock-file access failure hidden as contention: %v", err)
	}
	for _, invalid := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := store.AcquireRunLock(invalid); !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrRunLockBusy) {
			t.Fatalf("invalid ID classified as busy: %q %v", invalid, err)
		}
	}
}

func TestWorkerLockRecoveryPropagatesAccessFailureAndPreservesBusy(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, operation := range []string{"recover", "resume", "settlement", "wait"} {
			for _, busy := range []bool{false, true} {
				name := backend + "/" + operation + map[bool]string{false: "/access", true: "/busy"}[busy]
				t.Run(name, func(t *testing.T) {
					a, store, id, w := lockRecoveryFixture(t, backend)
					_, err := store.Update(id, func(s *Snapshot) error {
						if operation == "resume" || operation == "wait" {
							s.Workers[0].Status, s.Workers[0].CleanupStatus = "paused", "completed"
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					if busy {
						release, err := store.AcquireRunLock(w.RunID)
						if err != nil {
							t.Fatal(err)
						}
						defer release()
					} else {
						failLockFile(t, store, w.RunID)
					}
					before, err := store.Load(id)
					if err != nil {
						t.Fatal(err)
					}
					childBefore, err := store.Load(w.RunID)
					if err != nil {
						t.Fatal(err)
					}
					switch operation {
					case "recover":
						_, err = a.RecoverWorkers(id)
					case "resume":
						err = a.resumePausedWorkers(context.Background(), id)
					case "settlement":
						err = a.reconcileWorkerSettlements(id)
					case "wait":
						ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
						defer cancel()
						_, err = a.WaitWorkers(ctx, id, 1)
					}
					if busy {
						if operation == "wait" {
							if !errors.Is(err, context.DeadlineExceeded) {
								t.Fatalf("busy worker did not keep waiting: %v", err)
							}
						} else if err != nil {
							t.Fatalf("normal contention rejected: %v", err)
						}
					} else {
						var access *os.PathError
						if !errors.As(err, &access) || errors.Is(err, ErrRunLockBusy) || !strings.Contains(err.Error(), "acquire run lock") {
							t.Fatalf("access failure lost context or cause: %v", err)
						}
					}
					after, loadErr := store.Load(id)
					childAfter, childErr := store.Load(w.RunID)
					if loadErr != nil || childErr != nil || !reflect.DeepEqual(before.Plan, after.Plan) || !reflect.DeepEqual(before.Workers, after.Workers) || !reflect.DeepEqual(before.Result, after.Result) || !reflect.DeepEqual(childBefore, childAfter) {
						t.Fatalf("skipped/failed lock changed execution state: %v %v", loadErr, childErr)
					}
					if operation == "settlement" && before.Run.EventSeq != after.Run.EventSeq {
						t.Fatal("active settlement checkpoint wrote an event")
					}
				})
			}
		}
	}
}

func TestExecResourceRecoveryAggregatesLockFailures(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, id, w := lockRecoveryFixture(t, backend)
			for _, run := range []string{id, w.RunID} {
				failLockFile(t, store, run)
			}
			err := a.RecoverExecResources()
			var access *os.PathError
			if !errors.As(err, &access) || !strings.Contains(err.Error(), "recover exec resources "+id) || !strings.Contains(err.Error(), "recover exec resources "+w.RunID) {
				t.Fatalf("resource lock failures not aggregated: %v", err)
			}
			for _, run := range []string{id, w.RunID} {
				if err := os.Remove(filepath.Join(store.root, run+".lock")); err != nil {
					t.Fatal(err)
				}
				release, err := store.AcquireRunLock(run)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			if err := a.RecoverExecResources(); err != nil {
				t.Fatalf("live owners should retain recovery responsibility: %v", err)
			}
		})
	}
}

func TestWorkerLockPiPreparationPreservesPersistenceFailure(t *testing.T) {
	for _, operation := range []string{"settlement", "resume"} {
		t.Run(operation, func(t *testing.T) {
			a, store, id, w := lockRecoveryFixture(t, "json")
			before, err := store.Update(id, func(s *Snapshot) error {
				if operation == "resume" {
					s.Workers[0].Status, s.Workers[0].CleanupStatus = "paused", "completed"
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			failLockFile(t, store, w.RunID)
			restore, failed := settlementRenameFailure(t, store, id)
			_, err = a.preparePiRequest(context.Background(), id)
			restore()
			var access *os.PathError
			var rename *os.LinkError
			if !*failed || !errors.As(err, &access) || !errors.As(err, &rename) || !strings.Contains(err.Error(), w.RunID) {
				t.Fatalf("pi preparation lost recovery or persistence cause: %v", err)
			}
			after, loadErr := store.Load(id)
			if loadErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("failed preparation changed durable state: %v", loadErr)
			}
		})
	}
}
