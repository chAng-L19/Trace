package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func executorFixture(t *testing.T, backend string) (*Agent, *Agent, *Store, string, AgentWorker) {
	t.Helper()
	a, store := treeCapacityAgent(t, backend, MockProvider{})
	root := createTestRun(t, a)
	tasks := capacityTasks(2)
	tasks[1].ResourceKeys = []string{"fixture/object"}
	if _, err := a.SetPlan(root, tasks); err != nil {
		t.Fatal(err)
	}
	local, w := capacityFixtureChild(t, a, root, "0")
	if _, err := local.SetPlan(w.RunID, []PlanTask{
		{ID: "local", Goal: "operate object", ResourceKeys: []string{"fixture/object"}},
		{ID: "nested", Goal: "coordinate nested work"},
	}); err != nil {
		t.Fatal(err)
	}
	return a, local, store, root, w
}

func TestExecutorOwnershipAdmission(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, change := range []string{"visit", "manual_child", "task", "worker", "lease", "ancestor_worker", "ancestor_stopped", "ancestor_lease", "local_visit", "context"} {
			t.Run(backend+"/"+change, func(t *testing.T) {
				a, local, store, root, w := executorFixture(t, backend)
				if change == "ancestor_worker" || change == "ancestor_stopped" || change == "ancestor_lease" {
					local, w = capacityFixtureChild(t, local, w.RunID, "nested")
				}
				if change == "local_visit" {
					if _, err := local.ClaimTask(w.RunID, "local", w.ID, 3600); err != nil {
						t.Fatal(err)
					}
				}
				call := ToolCall{ID: "old", Name: "exec", Arguments: `{}`}
				responsibilityPending(t, local, w.RunID, call)
				if change == "manual_child" {
					local = a
				} else if change == "context" || change == "local_visit" {
					if _, err := store.Update(w.RunID, func(s *Snapshot) error {
						if change == "context" {
							s.WorkerContext.TaskID = "other"
						} else {
							s.Plan.Tasks[0].LocalWorkerVisit++
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				} else if _, err := store.Update(root, func(s *Snapshot) error {
					switch change {
					case "visit":
						s.Workers[0].Visit++
					case "task":
						s.Plan.Tasks[0].ChildRunID = "replacement"
					case "worker", "ancestor_worker":
						s.Workers[0].Status = "uncertain"
					case "lease", "ancestor_lease":
						s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Hour)
					case "ancestor_stopped":
						s.Run.Status = StatusPaused
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				ran := false
				_, err := local.executeAction(context.Background(), w.RunID, call.ID, "mcp", nil, func() ToolResult {
					ran = true
					return ToolResult{Stdout: "must not execute"}
				})
				if err == nil || ran {
					t.Fatalf("stale admission: executed=%v err=%v", ran, err)
				}
				child, err := store.Load(w.RunID)
				if err != nil || len(child.Actions) != 0 {
					t.Fatalf("stale intent registered: %+v %v", child.Actions, err)
				}
			})
		}
	}
}

func TestExecutorOwnershipBuiltinWriteAdmission(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			_, local, store, root, w := executorFixture(t, backend)
			path := filepath.Join(t.TempDir(), "stale.txt")
			raw, err := json.Marshal(FileRequest{Path: path, Content: "stale"})
			if err != nil {
				t.Fatal(err)
			}
			call := ToolCall{ID: "stale-write", Name: "write", Arguments: string(raw)}
			responsibilityPending(t, local, w.RunID, call)
			if _, err := store.Update(root, func(s *Snapshot) error { s.Workers[0].Visit++; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := local.executeBuiltin(context.Background(), w.RunID, call); err == nil {
				t.Fatal("stale builtin write admitted")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale builtin reached file operation: %v", err)
			}
		})
	}
}

func TestExecutorOwnershipReceipt(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, change := range []string{"visit", "ancestor_visit", "ancestor_stopped", "ancestor_lease"} {
			for _, deferred := range []bool{false, true} {
				name := backend + "/" + change
				if deferred {
					name += "/deferred"
				}
				t.Run(name, func(t *testing.T) {
					a, local, store, root, w := executorFixture(t, backend)
					if change != "visit" {
						local, w = capacityFixtureChild(t, local, w.RunID, "nested")
					} else if _, err := local.ClaimTask(w.RunID, "local", w.ID, 3600); err != nil {
						t.Fatal(err)
					}
					call := ToolCall{ID: "sent", Name: "exec", Arguments: `{}`}
					responsibilityPending(t, local, w.RunID, call)
					original := ToolResult{Stdout: "remote accepted", Stderr: "remote warning", ExitCode: 7, Error: "remote observation", MCPResult: json.RawMessage(`{"accepted":true}`)}
					ctx := context.Background()
					if deferred {
						ctx = context.WithValue(ctx, piDeferredResultKey{}, true)
					}
					calls := 0
					snap, err := local.executeAction(ctx, w.RunID, call.ID, "mcp", nil, func() ToolResult {
						calls++
						if _, err := store.Update(root, func(s *Snapshot) error {
							if change == "ancestor_stopped" {
								s.Run.Status = StatusPaused
							} else if change == "ancestor_lease" {
								s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Hour)
							} else {
								s.Workers[0].Visit++
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						return original
					})
					if err != nil || len(snap.Actions) != 1 {
						t.Fatalf("receipt lost: %+v %v", snap.Actions, err)
					}
					action := snap.Actions[0]
					if action.Status != "uncertain" || action.Result == nil || !action.Result.OutcomeUncertain || action.ExecutorOwnershipError == "" || toolCleanupError(snap) == nil {
						t.Fatalf("stale result promoted or occupancy released: %+v", action)
					}
					got := *action.Result
					got.OutcomeUncertain, got.EvidenceID = false, ""
					if !reflect.DeepEqual(got, original) {
						t.Fatalf("raw observation changed: got=%+v want=%+v", got, original)
					}
					if deferred {
						content, ready, err := readyPiResult(&snap, call.ID)
						if err != nil || !ready || content == "" {
							t.Fatalf("deferred uncertain receipt unavailable: %v %v", ready, err)
						}
					}
					if _, err := store.Update(w.RunID, func(s *Snapshot) error { s.Run.Status = StatusWaitingTool; return nil }); err != nil {
						t.Fatal(err)
					}
					if _, err := local.executeAction(ctx, w.RunID, call.ID, "mcp", nil, func() ToolResult { calls++; return ToolResult{} }); deferred && err != nil {
						t.Fatal(err)
					}
					if calls != 1 {
						t.Fatalf("stale request replayed %d times", calls)
					}
					if change == "visit" {
						var busy *ResourceBusyError
						if _, err := a.ClaimTask(root, "1", "other", 3600); !errors.As(err, &busy) {
							t.Fatalf("uncertain object released: %v", err)
						}
						cleaned, err := store.Update(w.RunID, func(s *Snapshot) error {
							// Completed local cleanup, even with a completed Action, does not reconcile external effects.
							s.Actions[0].ResourceCleanupError = ""
							s.Actions[0].ResourceCleanupStatus = "completed"
							s.Actions[0].Status = "completed"
							s.Run.Status = StatusCompleted
							s.WorkerSettlement = &WorkerSettlement{ParentRunID: root, TaskID: w.TaskID, WorkerID: w.ID, RunID: w.RunID, Visit: w.Visit + 1, CleanupStatus: "completed", ObservedAt: time.Now().UTC()}
							return nil
						})
						if err != nil {
							t.Fatal(err)
						}
						if cleaned.Actions[0].ExecutorOwnershipError != action.ExecutorOwnershipError || toolCleanupError(cleaned) == nil {
							t.Fatalf("process-only cleanup erased ownership hold: %+v", cleaned.Actions[0])
						}
						if !reflect.DeepEqual(cleaned.Actions[0].Result, action.Result) {
							t.Fatalf("process-only cleanup changed external observation: %+v", cleaned.Actions[0].Result)
						}
						parent, err := store.Update(root, func(s *Snapshot) error { return a.applyWorkerSettlementLocked(s, s.Workers[0]) })
						if err != nil || taskDone(parent.Plan.Tasks[0]) || parent.Workers[0].CleanupStatus != "uncertain" {
							t.Fatalf("settlement promoted stale receipt: %+v %v", parent.Plan, err)
						}
						if occupiedWorkerSlots(&parent) == 0 || len(parent.Plan.Tasks[0].ResourceKeys) != 0 {
							t.Fatalf("occupancy lost or coordinator took object lock: %+v %v", parent.Plan, err)
						}
						if _, err := a.ClaimTask(root, "1", "other", 3600); !errors.As(err, &busy) {
							t.Fatalf("process-only cleanup released shared object after settlement: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestExecutorOwnershipNormalAndCoordination(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, adapter := range []string{"exec", "local", "mcp", "builtin"} {
			t.Run(backend+"/"+adapter, func(t *testing.T) {
				a, local, store, root, w := executorFixture(t, backend)
				if _, err := local.ClaimTask(w.RunID, "local", w.ID, 3600); err != nil {
					t.Fatalf("coordinator blocked child resource: %v", err)
				}
				call := ToolCall{ID: "current", Name: "exec", Arguments: `{}`}
				if adapter == "builtin" {
					call.Name = "worker_status"
				}
				responsibilityPending(t, local, w.RunID, call)
				snap, err := local.executeAction(context.Background(), w.RunID, call.ID, adapter, nil, func() ToolResult {
					// Lease renewal and another Store must be possible while I/O is in flight.
					other, err := NewStore(store.root)
					if err != nil {
						t.Fatal(err)
					}
					defer other.Close()
					if _, err := other.Update(root, func(s *Snapshot) error { s.Plan.Tasks[0].LeaseUntil = time.Now().Add(2 * time.Hour); return nil }); err != nil {
						t.Fatal(err)
					}
					if _, err := store.Load(root); err != nil {
						t.Fatal(err)
					}
					return ToolResult{Stdout: "observed"}
				})
				if err != nil || len(snap.Actions) != 1 || snap.Actions[0].Status != "completed" || snap.Actions[0].Result.OutcomeUncertain || toolCleanupError(snap) != nil {
					t.Fatalf("current execution rejected: %+v %v", snap.Actions, err)
				}
				state, err := a.WorkerCapacity(root)
				if err != nil || state.Active != 1 {
					t.Fatalf("local executor counted twice: %+v %v", state, err)
				}
			})
		}
	}
}

func TestExecutorOwnershipAncestorLeaseRenewal(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			_, local, store, root, w := executorFixture(t, backend)
			local, w = capacityFixtureChild(t, local, w.RunID, "nested")
			call := ToolCall{ID: "renewed", Name: "exec", Arguments: `{}`}
			responsibilityPending(t, local, w.RunID, call)
			snap, err := local.executeAction(context.Background(), w.RunID, call.ID, "mcp", nil, func() ToolResult {
				if _, err := store.Update(root, func(s *Snapshot) error {
					s.Plan.Tasks[0].LeaseUntil = time.Now().Add(2 * time.Hour)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return ToolResult{Stdout: "accepted after ancestor renewal"}
			})
			if err != nil || len(snap.Actions) != 1 || snap.Actions[0].Status != "completed" || snap.Actions[0].Result.OutcomeUncertain {
				t.Fatalf("valid ancestor renewal invalidated execution: %+v %v", snap.Actions, err)
			}
		})
	}
}

func TestExecutorOwnershipManualRoot(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			s, err := a.CreateConfigured("manual root execution", RunConfiguration{role: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(s.Run.ID, func(s *Snapshot) error { s.Run.Responsibility = ""; return nil }); err != nil {
				t.Fatal(err)
			}
			call := ToolCall{ID: "manual", Name: "exec", Arguments: `{}`}
			responsibilityPending(t, a, s.Run.ID, call)
			s, err = a.executeAction(context.Background(), s.Run.ID, call.ID, "mcp", nil, func() ToolResult { return ToolResult{Stdout: "manual observation"} })
			if err != nil || len(s.Actions) != 1 || s.Actions[0].Status != "completed" {
				t.Fatalf("manual root compatibility: %+v %v", s.Actions, err)
			}
		})
	}
}

func TestExecutorOwnershipReportContinuation(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, parent, w, _ := reportWorkerFixture(t, backend, nil)
			child, err := store.Load(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			_, release, resumed, err := a.prepareWorkerResume(context.Background(), child)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			local := *a
			local.workerRunID, local.workerVisit = resumed.RunID, resumed.Visit
			if _, err := store.Update(w.RunID, func(s *Snapshot) error {
				s.Run.Status = StatusRunning
				if !local.reportWorkerVisit(s) {
					t.Fatal("fixture lost reportBusinessContinuing")
				}
				owners, err := local.executorOwnershipLocked(s)
				if err != nil || len(owners) != 1 || owners[0].Visit != resumed.Visit {
					t.Fatalf("completed report task rejected: %+v %v", owners, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			p, err := store.Load(parent)
			if err != nil || !taskDone(p.Plan.Tasks[0]) || occupiedWorkerSlots(&p) != 1 {
				t.Fatalf("report business completion or occupancy changed: %+v %v", p.Plan, err)
			}
		})
	}
}
