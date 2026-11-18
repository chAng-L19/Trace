package agent

import (
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

func pausedDeletionFixture(t *testing.T, backend string) (*Agent, *Store, string, AgentWorker) {
	t.Helper()
	a, store, id, w := lockRecoveryFixture(t, backend)
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status, s.Run.PauseReason = StatusPaused, "operator_pause"
		s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Hour)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(w.RunID, func(s *Snapshot) error {
		s.Run.Status, s.Run.PauseReason = StatusPaused, "parent_pause"
		s.WorkerContext = &WorkerContext{ParentRunID: id, TaskID: w.TaskID, WorkerID: w.ID, RunID: w.RunID}
		// An uncertain business outcome with verified resource cleanup must
		// remain uncertain; deletion never replays or accepts this action.
		s.Actions = []Action{{Adapter: "exec", RequestID: "request", CallID: "call", Status: "uncertain", ResourceCleanupStatus: "completed"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return a, store, id, w
}

func deletePausedFixture(store *Store, id, operation string) (EvidenceDeleteResult, error) {
	if operation == "purge" {
		return store.PurgeRun(id)
	}
	return store.DeleteWithEvidence(id, true)
}

func TestPausedDeleteInterruptedWorkerTree(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, operation := range []string{"hide", "purge"} {
			for _, history := range []string{"missing_receipt", "later_visit_missing_receipt", "previous_visit_receipt", "unknown_worker_cleanup"} {
				t.Run(backend+"/"+operation+"/"+history, func(t *testing.T) {
					_, store, id, w := pausedDeletionFixture(t, backend)
					if _, err := store.Update(id, func(s *Snapshot) error {
						if history == "later_visit_missing_receipt" || history == "previous_visit_receipt" {
							s.Workers[0].Visit++
						}
						if history == "unknown_worker_cleanup" {
							s.Workers[0].Status, s.Workers[0].CleanupStatus = "uncertain", "unknown"
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if history == "previous_visit_receipt" {
						if _, err := store.Update(w.RunID, func(s *Snapshot) error {
							s.WorkerSettlement = &WorkerSettlement{ParentRunID: id, TaskID: w.TaskID, WorkerID: w.ID, RunID: w.RunID, Visit: w.Visit, CleanupStatus: "completed", ObservedAt: time.Now().UTC()}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
					before, err := store.Load(id)
					if err != nil {
						t.Fatal(err)
					}
					childBefore, err := store.Load(w.RunID)
					if err != nil {
						t.Fatal(err)
					}
					result, err := deletePausedFixture(store, id, operation)
					if err != nil || !result.Deleted {
						t.Fatalf("stopped tree retained: %+v %v", result, err)
					}
					if operation == "purge" {
						if len(result.RunIDs) != 2 {
							t.Fatalf("incomplete tree removal: %+v", result)
						}
						for _, run := range []string{id, w.RunID} {
							if _, err := store.Load(run); !errors.Is(err, os.ErrNotExist) {
								t.Fatalf("run remains: %s %v", run, err)
							}
						}
						return
					}
					after, err := store.Load(id)
					childAfter, childErr := store.Load(w.RunID)
					if err != nil || childErr != nil || !after.Run.Deleted || after.Run.Status != StatusPaused || after.Run.PauseReason != before.Run.PauseReason || !reflect.DeepEqual(before.Workers, after.Workers) || !reflect.DeepEqual(before.Plan, after.Plan) || !reflect.DeepEqual(before.Result, after.Result) || !reflect.DeepEqual(childBefore, childAfter) {
						t.Fatalf("hiding rewrote execution or cleanup history: %v %v", err, childErr)
					}
					if occupiedWorkerSlots(&after) != 1 {
						t.Fatal("deletion released the normal worker capacity gate")
					}
				})
			}
		}
	}
}

func TestPausedDeleteRetainsActiveOwnersAndCleanupFailures(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, operation := range []string{"hide", "purge"} {
			for _, scenario := range []string{"root_lock", "child_lock", "child_lock_error", "child_running", "wrong_parent", "wrong_task", "wrong_worker", "wrong_context", "wrong_receipt_owner", "future_receipt", "lifecycle_cleanup", "missing_receipt_cleanup_error", "tool_cleanup", "missing_tool_result", "local_task", "unregistered_descendant"} {
				t.Run(backend+"/"+operation+"/"+scenario, func(t *testing.T) {
					a, store, id, w := pausedDeletionFixture(t, backend)
					if _, err := store.Update(w.RunID, func(s *Snapshot) error {
						switch scenario {
						case "child_running":
							s.Run.Status = StatusRunning
						case "wrong_parent":
							s.Run.ParentRunID = "other"
						case "wrong_task":
							s.Run.ParentTaskID = "other"
						case "wrong_context":
							s.WorkerContext.WorkerID = "other"
						case "wrong_receipt_owner", "future_receipt", "lifecycle_cleanup":
							s.WorkerSettlement = &WorkerSettlement{ParentRunID: id, TaskID: w.TaskID, WorkerID: w.ID, RunID: w.RunID, Visit: w.Visit, CleanupStatus: "completed", ObservedAt: time.Now().UTC()}
							if scenario == "wrong_receipt_owner" {
								s.WorkerSettlement.WorkerID = "other"
							} else if scenario == "future_receipt" {
								s.WorkerSettlement.Visit++
							} else {
								s.WorkerSettlement.LifecycleCleanupError = "transport cleanup failed"
							}
						case "tool_cleanup":
							s.Actions[0].ResourceCleanupError = "owned process is still populated"
						case "missing_tool_result":
							s.Actions[0].ResourceCleanupStatus = "unknown"
						case "local_task":
							s.Plan.Tasks = []PlanTask{{ID: "local", Status: "running", LocalExecution: true}}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if scenario == "wrong_worker" || scenario == "missing_receipt_cleanup_error" {
						if _, err := store.Update(id, func(s *Snapshot) error {
							if scenario == "wrong_worker" {
								s.Plan.Tasks[0].WorkerID = "other"
							} else {
								s.Workers[0].CleanupStatus, s.Workers[0].Error = "uncertain", "MCP transport cleanup failed"
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
					if scenario == "unregistered_descendant" {
						orphan := createTestRun(t, a)
						if _, err := store.Update(orphan, func(s *Snapshot) error {
							s.Run.ParentRunID, s.Run.ParentTaskID, s.Run.Status = w.RunID, "orphan", StatusPaused
							s.Actions = []Action{{Adapter: "local", Status: "started"}}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
					if scenario == "root_lock" || scenario == "child_lock" {
						locked := id
						if scenario == "child_lock" {
							locked = w.RunID
						}
						release, err := store.AcquireRunLock(locked)
						if err != nil {
							t.Fatal(err)
						}
						defer release()
					} else if scenario == "child_lock_error" {
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
					result, err := deletePausedFixture(store, id, operation)
					if err == nil || result.Deleted {
						t.Fatalf("protected tree deleted: %+v %v", result, err)
					}
					if scenario == "root_lock" || scenario == "child_lock" {
						if !errors.Is(err, ErrRunLockBusy) {
							t.Fatalf("lost contention cause: %v", err)
						}
					} else if scenario == "child_lock_error" {
						var pathErr *os.PathError
						if !errors.As(err, &pathErr) || errors.Is(err, ErrRunLockBusy) {
							t.Fatalf("lost lock access cause: %v", err)
						}
					}
					after, loadErr := store.Load(id)
					childAfter, childErr := store.Load(w.RunID)
					if loadErr != nil || childErr != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(childBefore, childAfter) {
						t.Fatalf("rejected deletion changed history: %v %v", loadErr, childErr)
					}
				})
			}
		}
	}
}
