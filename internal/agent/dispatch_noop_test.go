package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDispatchUnchangedCapabilityBlockSkipsCommit(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			id := createTestRun(t, a)
			if _, err := a.SetPlan(id, []PlanTask{{ID: "blocked", Goal: "needs tool", RequiredToolIDs: []string{"missing"}}}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(id, func(s *Snapshot) error {
				s.Messages = append(s.Messages, Message{Role: "user", Content: strings.Repeat("retained history ", 4096)})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			writes := 0
			store.beforePersist = func(string) { writes++ }
			defer func() { store.beforePersist = nil }()
			before, err := a.ClaimTask(id, "blocked", "owner", 30)
			var blocked *TaskBlockedError
			if !errors.As(err, &blocked) || writes != 1 || before.Plan.Tasks[0].Status != "blocked_capability" {
				t.Fatalf("first block must commit once: writes=%d err=%v", writes, err)
			}
			writes = 0
			for i := 0; i < 3; i++ {
				workers, err := a.DispatchFrontier(context.Background(), id)
				if err != nil || len(workers) != 0 {
					t.Fatalf("unchanged block dispatched: workers=%v err=%v", workers, err)
				}
			}
			after, err := a.ClaimTask(id, "blocked", "another-owner", 30)
			if !errors.As(err, &blocked) || writes != 0 || valueHash(before) != valueHash(after) {
				t.Fatalf("unchanged block rewrote or lost full snapshot: writes=%d before=%s after=%s err=%v", writes, valueHash(before), valueHash(after), err)
			}
			if a.HasActiveWorkers() || len(after.Workers) != 0 || after.Plan.Tasks[0].WorkerID != "" {
				t.Fatal("blocked dispatch retained an executor or claim")
			}
			other, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			persisted, err := other.Load(id)
			if err != nil || valueHash(persisted) != valueHash(before) {
				t.Fatalf("no-op changed durable history, events or timestamp: %v", err)
			}
		})
	}
}

func TestDispatchCapabilityBlockChangesStillCommit(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			id := createTestRun(t, a)
			if _, err := a.SetPlan(id, []PlanTask{{ID: "blocked", Goal: "needs tools", RequiredToolIDs: []string{"missing-a", "missing-b"}}}); err != nil {
				t.Fatal(err)
			}
			before, err := a.ClaimTask(id, "blocked", "owner", 30)
			var blocked *TaskBlockedError
			if !errors.As(err, &blocked) {
				t.Fatal(err)
			}
			writes := 0
			store.beforePersist = func(string) { writes++ }
			defer func() { store.beforePersist = nil }()
			for i, name := range []string{"missing-a", "missing-b"} {
				definition := execToolDefinition()
				definition.Function.Name = name
				a.registry.tools[name] = RegisteredTool{Adapter: "builtin", Definition: definition}
				after, err := a.ClaimTask(id, "blocked", "owner", 30)
				if writes != i+1 {
					t.Fatalf("changed prerequisite did not commit once: writes=%d err=%v", writes, err)
				}
				if i == 0 {
					if !errors.As(err, &blocked) || !reflect.DeepEqual(blocked.MissingToolIDs, []string{"missing-b"}) || after.Run.EventSeq != before.Run.EventSeq+1 {
						t.Fatalf("changed blocker was suppressed: block=%+v err=%v", blocked, err)
					}
				} else if err != nil || after.Plan.Tasks[0].Status != "running" || after.Plan.Tasks[0].WorkerID != "owner" || after.Plan.Tasks[0].BlockedReason != nil || after.Run.EventSeq != before.Run.EventSeq+3 {
					t.Fatalf("restored capability did not claim: task=%+v err=%v", after.Plan.Tasks[0], err)
				}
			}
		})
	}
}

func TestDispatchUnchangedCapabilityBlockPersistsRevocation(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, failCommit := range []bool{false, true} {
			name := backend
			if failCommit {
				name += "/commit-failure"
			}
			t.Run(name, func(t *testing.T) {
				a, store := treeCapacityAgent(t, backend, MockProvider{})
				id := createTestRun(t, a)
				if _, err := a.SetPlan(id, []PlanTask{{ID: "blocked", Goal: "needs tool", RequiredToolIDs: []string{"missing"}}}); err != nil {
					t.Fatal(err)
				}
				before, err := a.ClaimTask(id, "blocked", "owner", 30)
				var blocked *TaskBlockedError
				if !errors.As(err, &blocked) {
					t.Fatal(err)
				}
				// A stale saved verification must be revoked before the no-op callback.
				before.Assets = []Asset{{ID: "stale", Coverage: "verified", Predicate: "proof", EvidenceIDs: []string{"missing-proof"}}}
				unlock, err := store.acquireStoreLock()
				if err != nil {
					t.Fatal(err)
				}
				err = store.saveLocked(before)
				unlock()
				if err != nil {
					t.Fatal(err)
				}
				var restore func()
				if failCommit && backend == "sqlite" {
					db, err := store.sqliteDB()
					if err != nil {
						t.Fatal(err)
					}
					if _, err = db.Exec("CREATE TRIGGER reject_dispatch_revocation BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'dispatch revocation fixture failure'); END"); err != nil {
						t.Fatal(err)
					}
				}
				writes := 0
				store.beforePersist = func(string) {
					writes++
					if failCommit && backend == "json" && restore == nil {
						restore = dispatchBlockRename(t, store, id)
					}
				}
				defer func() { store.beforePersist = nil }()
				after, err := a.ClaimTask(id, "blocked", "owner", 30)
				if restore != nil {
					restore()
				}
				if failCommit {
					if err == nil || errors.As(err, &blocked) || writes != 1 {
						t.Fatalf("source commit failure was hidden by blocker: writes=%d err=%v", writes, err)
					}
					unlock, loadErr := store.acquireStoreLock()
					if loadErr != nil {
						t.Fatal(loadErr)
					}
					persisted, loadErr := store.loadRawLocked(id)
					unlock()
					if loadErr != nil || valueHash(persisted) != valueHash(before) {
						t.Fatalf("failed revocation changed durable state: %v", loadErr)
					}
					return
				}
				if !errors.As(err, &blocked) || writes != 1 || after.Assets[0].Coverage != "uncertain" || after.Run.EventSeq <= before.Run.EventSeq {
					t.Fatalf("source revocation skipped or no-op rewrote it: writes=%d err=%v", writes, err)
				}
				other, err := NewStore(store.root)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				unlock, err = other.acquireStoreLock()
				if err != nil {
					t.Fatal(err)
				}
				persisted, err := other.loadRawLocked(id)
				unlock()
				if err != nil || valueHash(persisted) != valueHash(after) || persisted.Assets[0].Coverage != "uncertain" {
					t.Fatalf("returned revocation was not durable: %v", err)
				}
			})
		}
	}
}

func TestDispatchUnchangedCapabilityBlockPersistsFrontierRepair(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			id := createTestRun(t, a)
			if _, err := a.SetPlan(id, []PlanTask{{ID: "blocked", Goal: "needs tool", RequiredToolIDs: []string{"missing"}}, {ID: "pending", Goal: "ready"}}); err != nil {
				t.Fatal(err)
			}
			before, err := a.ClaimTask(id, "blocked", "owner", 30)
			var blocked *TaskBlockedError
			if !errors.As(err, &blocked) {
				t.Fatal(err)
			}
			before.Plan.Frontier = []string{"blocked"}
			unlock, err := store.acquireStoreLock()
			if err != nil {
				t.Fatal(err)
			}
			err = store.saveLocked(before)
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			writes := 0
			store.beforePersist = func(string) { writes++ }
			defer func() { store.beforePersist = nil }()
			after, err := a.ClaimTask(id, "blocked", "owner", 30)
			if !errors.As(err, &blocked) || writes != 1 || !reflect.DeepEqual(after.Plan.Frontier, []string{"pending"}) {
				t.Fatalf("frontier repair lost or unnecessarily rewritten: writes=%d plan=%+v err=%v", writes, after.Plan, err)
			}
			other, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			unlock, err = other.acquireStoreLock()
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := other.loadRawLocked(id)
			unlock()
			if err != nil || valueHash(persisted) != valueHash(after) {
				t.Fatalf("repaired frontier was not durable: %v", err)
			}
		})
	}
}
