package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"trace/internal/worker"
)

func TestExecRecoveryRetainsUncertainAndOnlyCleansPersistedOwner(t *testing.T) {
	for _, failed := range []bool{false, true} {
		a, store := newTestAgent(t, MockProvider{})
		id := createTestRun(t, a)
		_, err := store.Update(id, func(s *Snapshot) error {
			s.Run.Status = StatusWaitingTool
			s.Actions = []Action{
				{RequestID: "r1", CallID: "same", Adapter: "exec", ResourceOwner: worker.ResourceOwner(id, "r1", "same"), Status: "completed", Result: &ToolResult{}},
				{RequestID: "r2", CallID: "same", Adapter: "exec", ResourceOwner: worker.ResourceOwner(id, "r2", "same"), ResourceRoot: "/original/exec", ResourceRootIdentity: "identity-1", Status: "started"},
				{RequestID: "r3", CallID: "other", Adapter: "local", Status: "started"},
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		clean := func(root, run, request, call string) error {
			calls++
			if run != id || request != "r2" || call != "same" {
				t.Fatal("wrong owner")
			}
			if failed {
				return errors.New("not empty")
			}
			return nil
		}
		release, err := store.AcquireRunLock(id)
		if err != nil {
			t.Fatal(err)
		}
		identity := func(string) (string, error) { return "identity-1", nil }
		s, err := a.reconcileExecResourcesWithIdentityLocked(id, identity, clean)
		release()
		if err != nil || calls != 1 || s.Run.Status != StatusWaitingTool || s.Actions[1].Status != "uncertain" || s.Actions[1].Result != nil || !s.Actions[1].FinishedAt.IsZero() || len(s.Evidence) != 0 {
			t.Fatalf("recovery forged outcome: %+v %v", s.Actions, err)
		}
		missing := 0
		for _, event := range s.Events {
			if event.Type == "action_result_missing" {
				missing++
			}
		}
		if missing != 1 {
			t.Fatalf("missing-result observation count = %d", missing)
		}
		if failed && s.Actions[1].ResourceCleanupStatus != "uncertain" {
			t.Fatal("cleanup failure released owner")
		}
		if !failed && s.Actions[1].ResourceCleanupStatus != "completed" {
			t.Fatal("cleanup not recorded")
		}
		if !failed {
			release, _ = store.AcquireRunLock(id)
			_, err = a.reconcileExecResourcesWithIdentityLocked(id, identity, clean)
			release()
			if err != nil || calls != 1 {
				t.Fatal("completed cleanup repeated")
			}
		}
	}
}

func TestExecRecoveryRejectsChangedProcessGroupIdentityAndUnknownOwner(t *testing.T) {
	for _, reason := range []string{"identity", "owner"} {
		t.Run(reason, func(t *testing.T) {
			t.Setenv("TRACE_EXEC_RESOURCE_MODE", "cgroup")
			a, store := newTestAgent(t, MockProvider{})
			id := createTestRun(t, a)
			_, err := store.Update(id, func(s *Snapshot) error {
				s.Run.Status = StatusWaitingTool
				owner := worker.ResourceOwner(id, "r", "c")
				if reason == "owner" {
					owner = worker.ResourceOwner("another-run", "r", "c")
				}
				s.Actions = []Action{{Adapter: "exec", RequestID: "r", CallID: "c", Status: "started", ResourceRoot: worker.ProcessGroupResourceRoot, ResourceRootIdentity: "original-namespace", ResourceOwner: owner}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			release, err := store.AcquireRunLock(id)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			called := false
			snap, err := a.reconcileExecResourcesWithIdentityLocked(id, func(root string) (string, error) {
				if root != worker.ProcessGroupResourceRoot {
					t.Fatal("recovery substituted the current resource mode")
				}
				if reason == "identity" {
					return "replacement-namespace", nil
				}
				return "original-namespace", nil
			}, func(string, string, string, string) error { called = true; return nil })
			if err != nil || called || snap.Run.Status != StatusWaitingTool || snap.Actions[0].Status != "uncertain" || snap.Actions[0].ResourceCleanupStatus != "uncertain" || snap.Actions[0].Result != nil || len(snap.Evidence) != 0 {
				t.Fatalf("changed identity or unknown owner accepted: %+v %v", snap.Actions, err)
			}
		})
	}
}

func TestExecIntentProcessGroupIdentityFailureDoesNotStart(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, mode := range []string{"process-group", "cgroup"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				t.Setenv("TRACE_EXEC_RESOURCE_MODE", mode)
				t.Setenv("TRACE_EXEC_CGROUP_ROOT", "/unavailable/exec")
				a, store := newTestAgent(t, MockProvider{})
				if backend == "sqlite" {
					store = sqliteTestStore(t)
					var err error
					a, err = New(store, MockProvider{}, "")
					if err != nil {
						t.Fatal(err)
					}
				}
				id := createTestRun(t, a)
				before, err := store.Update(id, func(s *Snapshot) error {
					s.Run.Status = StatusWaitingTool
					s.Requests = []ModelRequest{{ID: "request"}}
					s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "call", Name: "exec", Arguments: `{}`}}})
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				identityErr := errors.New("identity temporarily unavailable")
				started := false
				_, err = a.executeActionWithResourceIdentity(context.Background(), id, "call", "exec", nil, func() ToolResult {
					started = true
					return ToolResult{Stdout: "observed"}
				}, func(string) (string, error) { return "", identityErr })
				after, loadErr := store.Load(id)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if mode == "process-group" {
					if !errors.Is(err, identityErr) || started || len(after.Actions) != 0 || len(after.Events) != len(before.Events) || after.Run.Status != StatusWaitingTool {
						t.Fatalf("identity failure started or persisted an action: started=%v actions=%+v err=%v", started, after.Actions, err)
					}
				} else if err != nil || !started || len(after.Actions) != 1 || after.Actions[0].ResourceRootIdentity != "" {
					t.Fatalf("legacy cgroup intent behavior changed: started=%v actions=%+v err=%v", started, after.Actions, err)
				}
			})
		}
	}
}

func TestExecRecoveryPreservesManualControlState(t *testing.T) {
	for _, status := range []Status{StatusCreated, StatusPaused, StatusCancelled, StatusFailed, StatusCompleted} {
		t.Run(string(status), func(t *testing.T) {
			a, store := newTestAgent(t, MockProvider{})
			id := createTestRun(t, a)
			_, err := store.Update(id, func(s *Snapshot) error {
				s.Run.Status = status
				s.Run.PauseReason = "waiting_input: original question"
				s.Actions = []Action{{Adapter: "exec", RequestID: "r", CallID: "c", Status: "resolved_manually", ResourceRoot: "/original", ResourceRootIdentity: "identity", ResourceOwner: worker.ResourceOwner(id, "r", "c")}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			release, err := store.AcquireRunLock(id)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			snap, err := a.reconcileExecResourcesWithIdentityLocked(id, func(string) (string, error) { return "identity", nil }, func(string, string, string, string) error { return nil })
			if err != nil || snap.Run.Status != status || snap.Run.PauseReason != "waiting_input: original question" || snap.Actions[0].Status != "resolved_manually" || snap.Actions[0].ResourceCleanupStatus != "completed" {
				t.Fatalf("manual cleanup rewrote control state: %+v %v", snap.Run, err)
			}
		})
	}
}

func TestExecRecoveryHiddenRunKeepsDeletedAndGenericUpdateGuard(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := newTestAgent(t, MockProvider{})
			if backend == "sqlite" {
				store = sqliteTestStore(t)
				var err error
				a, err = New(store, MockProvider{}, "")
				if err != nil {
					t.Fatal(err)
				}
			}
			id := createTestRun(t, a)
			_, err := store.Update(id, func(s *Snapshot) error {
				s.Run.Status = StatusPaused
				s.Run.PauseReason = "operator_pause"
				s.Run.Deleted = true
				s.Actions = []Action{{Adapter: "exec", RequestID: "r", CallID: "c", Status: "started", ResourceRoot: "/missing", ResourceRootIdentity: "identity", ResourceOwner: worker.ResourceOwner(id, "r", "c")}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = a.RecoverExecResources(); err != nil {
				t.Fatal(err)
			}
			snap, err := store.Load(id)
			if err != nil || snap.Actions[0].ResourceCleanupStatus != "uncertain" || !snap.Run.Deleted {
				t.Fatalf("hidden owner was skipped: %+v %v", snap.Actions, err)
			}
			release, err := store.AcquireRunLock(id)
			if err != nil {
				t.Fatal(err)
			}
			snap, err = a.reconcileExecResourcesWithIdentityLocked(id, func(string) (string, error) { return "identity", nil }, func(string, string, string, string) error { return nil })
			release()
			if err != nil || !snap.Run.Deleted || snap.Run.Status != StatusPaused || snap.Run.PauseReason != "operator_pause" || snap.Actions[0].ResourceCleanupStatus != "completed" || snap.Actions[0].Result != nil || len(snap.Evidence) != 0 {
				t.Fatalf("hidden cleanup forged business state: %+v %v", snap.Run, err)
			}
			if _, err = store.Update(id, func(s *Snapshot) error { s.Run.Deleted = false; return nil }); err == nil {
				t.Fatal("generic deleted update became writable")
			}
		})
	}
}

func TestExecRecoverySameVisitSettlementConvergesOnlyKnownResources(t *testing.T) {
	for _, scenario := range []string{"resource", "mcp", "legacy"} {
		t.Run(scenario, func(t *testing.T) {
			a, store, id, w := settlementFixture(t)
			calls := 0
			err := settlementOperation(t, a, store, id, w, func(s *Snapshot) {
				calls++
				s.Run.Status = StatusPaused
				s.Run.PauseReason = "operator_pause"
				s.Actions = []Action{{Adapter: "exec", RequestID: "r", CallID: "c", Status: "resolved_manually", ResourceRoot: "/original", ResourceRootIdentity: "identity", ResourceOwner: worker.ResourceOwner(w.RunID, "r", "c"), Result: &ToolResult{OutcomeUncertain: true, WorkerCleanupError: "owned process live"}}}
			})
			if err == nil {
				t.Fatal("missing original cleanup failure")
			}
			parent, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(parent.ChildReports) != 1 || len(parent.WorkerMessages) != 1 || parent.Workers[0].SettledVisit == nil || occupiedWorkerSlots(&parent) != 1 {
				t.Fatal("unclean settlement did not retain slot")
			}
			oldHash := parent.ChildReports[0].SourceHash
			oldMessage := parent.WorkerMessages[0].ID
			if scenario != "resource" {
				_, err = store.Update(w.RunID, func(s *Snapshot) error {
					if scenario == "mcp" {
						s.WorkerSettlement.LifecycleCleanupError = "MCP DELETE 500"
					} else {
						s.WorkerSettlement.CleanupVersion = 0
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			unlock, err := store.AcquireRunLock(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			child, err := a.reconcileExecResourcesWithIdentityLocked(w.RunID, func(string) (string, error) { return "identity", nil }, func(string, string, string, string) error { return nil })
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			if child.Actions[0].ResourceCleanupStatus != "completed" || child.Actions[0].Status != "resolved_manually" || len(child.Evidence) != 0 || child.Run.PauseReason != "operator_pause" {
				t.Fatal("resource observation changed manual outcome")
			}
			parent, err = a.RecoverWorkers(id)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "resource" {
				if parent.Workers[0].CleanupStatus == "completed" || occupiedWorkerSlots(&parent) != 1 || len(parent.ChildReports) != 1 {
					t.Fatal("resource proof released unrelated cleanup")
				}
				return
			}
			if parent.Workers[0].CleanupStatus != "completed" || occupiedWorkerSlots(&parent) != 0 || parent.Plan.Tasks[0].Status != "paused" || hasString(parent.Plan.Frontier, "next") || calls != 1 {
				t.Fatalf("same visit cleanup failed or claimed success: %+v", parent.Workers)
			}
			if len(parent.ChildReports) != 2 || parent.ChildReports[1].SourceHash == oldHash || len(parent.WorkerMessages) != 2 || parent.WorkerMessages[1].ID == oldMessage || parent.ChildReports[1].Visit != w.Visit {
				t.Fatal("cleanup source version not published exactly once")
			}
			reports, err := store.ChildReports(id)
			if err != nil || reports[0].SourceStatus != "changed" || reports[1].SourceStatus != "available" || !reports[1].Current {
				t.Fatalf("wrong fixed source status: %+v %v", reports, err)
			}
			if _, err = store.ReportVersion(w.RunID, oldHash); err == nil {
				t.Fatal("old hash accepted new cleanup body")
			}
			hash := parent.ChildReports[1].SourceHash
			body, err := store.ReportVersion(w.RunID, hash)
			if err != nil {
				t.Fatal(err)
			}
			parent, err = a.RecoverWorkers(id)
			if err != nil || len(parent.ChildReports) != 2 || len(parent.WorkerMessages) != 2 {
				t.Fatal("repeat recovery duplicated report")
			}
			repeated, err := store.ReportVersion(w.RunID, hash)
			if err != nil || repeated != body {
				t.Fatal("fixed report changed on repeat recovery")
			}
			if err = a.StopWorkers(context.Background()); err != nil {
				t.Fatalf("historical tool-only error remained after durable cleanup: %v", err)
			}
		})
	}
}

func TestRecoverWorkersReconcilesResourcesWithoutRun(t *testing.T) {
	a, store, id, w := settlementFixture(t)
	_, err := store.Update(w.RunID, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Actions = []Action{{Adapter: "exec", RequestID: "r", CallID: "c", Status: "started", ResourceRoot: "/missing", ResourceRootIdentity: "identity", ResourceOwner: worker.ResourceOwner(w.RunID, "r", "c")}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.RecoverWorkers(id); err != nil {
		t.Fatal(err)
	}
	child, err := store.Load(w.RunID)
	if err != nil || child.Actions[0].ResourceCleanupStatus != "uncertain" || !strings.Contains(child.Actions[0].ResourceCleanupError, "root") || child.Actions[0].Result != nil || len(child.Requests) != 0 || len(child.Evidence) != 0 {
		t.Fatalf("recover bypassed resource check or replayed: %+v %v", child.Actions, err)
	}
}

func TestExecRecoveryNestedResourceSettlementConverges(t *testing.T) {
	a, store, id, middle := settlementFixture(t)
	var leaf AgentWorker
	calls := 0
	unlock, err := store.AcquireRunLock(middle.RunID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.runChildOperation(context.Background(), id, middle, func(childAgent *Agent, _ context.Context, runID string) (Snapshot, error) {
		if _, err := store.Update(runID, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
			return Snapshot{}, err
		}
		leaf = nestedSettlementWorker(t, childAgent, store, runID)
		childErr := settlementOperation(t, childAgent, store, runID, leaf, func(s *Snapshot) {
			calls++
			s.Run.Status = StatusPaused
			s.Run.PauseReason = "operator_pause"
			s.Actions = []Action{{Adapter: "exec", RequestID: "r", CallID: "c", Status: "resolved_manually", ResourceRoot: "/original", ResourceRootIdentity: "identity", ResourceOwner: worker.ResourceOwner(leaf.RunID, "r", "c"), Result: &ToolResult{WorkerCleanupError: "leaf process still live"}}}
		})
		if childErr == nil {
			t.Fatal("leaf cleanup failure missing")
		}
		return store.Update(runID, func(s *Snapshot) error { s.Run.Status = StatusPaused; s.Run.PauseReason = "operator_pause"; return nil })
	})
	unlock()
	if err == nil {
		t.Fatal("nested cleanup failure missing")
	}
	mid, err := store.Load(middle.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if mid.WorkerSettlement == nil || !mid.WorkerSettlement.DescendantCleanupPending || mid.WorkerSettlement.LifecycleCleanupError != "" {
		t.Fatalf("typed leaf cleanup became opaque lifecycle failure: %+v", mid.WorkerSettlement)
	}
	unlock, err = store.AcquireRunLock(leaf.RunID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.reconcileExecResourcesWithIdentityLocked(leaf.RunID, func(string) (string, error) { return "identity", nil }, func(string, string, string, string) error { return nil })
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	root, err := a.RecoverWorkers(id)
	if err != nil {
		t.Fatal(err)
	}
	mid, err = store.Load(middle.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if occupiedWorkerSlots(&root) != 0 || occupiedWorkerSlots(&mid) != 0 || mid.WorkerSettlement.DescendantCleanupPending || root.Plan.Tasks[0].Status != "paused" || mid.Plan.Tasks[0].Status != "paused" || calls != 1 {
		t.Fatalf("nested cleanup did not converge without replay: root=%+v middle=%+v", root.Workers, mid.Workers)
	}
	if err = a.StopWorkers(context.Background()); err != nil {
		t.Fatalf("cleared leaf retained historical cleanup: %v", err)
	}
}

func TestWorkerToolCleanupFilterRetainsUnrelatedCauses(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Actions = []Action{{Adapter: "exec", RequestID: "r", CallID: "c", Status: "resolved_manually", ResourceCleanupStatus: "completed"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mcpErr := errors.New("MCP DELETE 500")
	combined := errors.Join(&workerToolCleanupError{runID: id, err: errors.New("historical tool cleanup")}, mcpErr)
	if current := a.currentWorkerToolCleanupError(combined); !errors.Is(current, mcpErr) || strings.Contains(current.Error(), "historical tool cleanup") {
		t.Fatalf("cleanup source filter lost distinction: %v", current)
	}
	if current := a.currentWorkerToolCleanupError(&workerToolCleanupError{runID: "missing", err: errors.New("unobserved")}); current == nil {
		t.Fatal("missing source forged cleanup")
	}
}

func TestExecRecoveryDoesNotRetainStaleCompletion(t *testing.T) {
	a, store, id, w := settlementFixture(t)
	err := settlementOperation(t, a, store, id, w, func(s *Snapshot) {
		s.Run.Status = StatusCompleted
		s.Actions = []Action{{Adapter: "exec", RequestID: "r", CallID: "c", Status: "started", ResourceRoot: "/original", ResourceRootIdentity: "identity", ResourceOwner: worker.ResourceOwner(w.RunID, "r", "c")}}
	})
	if err == nil {
		t.Fatal("missing unresolved action cleanup")
	}
	release, err := store.AcquireRunLock(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	child, err := a.reconcileExecResourcesWithIdentityLocked(w.RunID, func(string) (string, error) { return "identity", nil }, func(string, string, string, string) error { return nil })
	release()
	if err != nil || child.Run.Status != StatusWaitingTool || child.Actions[0].Status != "uncertain" || child.Actions[0].Result != nil || len(child.Evidence) != 0 {
		t.Fatalf("resource cleanup retained stale business completion: %+v %v", child.Run, err)
	}
	parent, err := a.RecoverWorkers(id)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Plan.Tasks[0].Status != "uncertain" || hasString(parent.Plan.Frontier, "next") || parent.Workers[0].CleanupStatus != "completed" || occupiedWorkerSlots(&parent) != 1 {
		t.Fatalf("resource proof unlocked completed business dependency: %+v", parent.Plan)
	}
}

func TestExecRecoveryManualOutcomeAndChangedResourceRoot(t *testing.T) {
	for _, changed := range []bool{false, true} {
		a, store := newTestAgent(t, MockProvider{})
		id := createTestRun(t, a)
		_, err := store.Update(id, func(s *Snapshot) error {
			s.Run.Status = StatusPaused
			s.Actions = []Action{{RequestID: "r", CallID: "c", Adapter: "exec", ResourceOwner: worker.ResourceOwner(id, "r", "c"), ResourceRoot: "/original/exec", ResourceRootIdentity: "old", Status: "resolved_manually"}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		identity := func(root string) (string, error) {
			if root != "/original/exec" {
				t.Fatal("used mutable environment root")
			}
			if changed {
				return "new", nil
			}
			return "old", nil
		}
		calls := 0
		cleanup := func(string, string, string, string) error { calls++; return nil }
		release, err := store.AcquireRunLock(id)
		if err != nil {
			t.Fatal(err)
		}
		s, err := a.reconcileExecResourcesWithIdentityLocked(id, identity, cleanup)
		release()
		if err != nil || s.Actions[0].Status != "resolved_manually" || len(s.Evidence) != 0 {
			t.Fatal("cleanup changed business observation")
		}
		if changed && (calls != 0 || s.Actions[0].ResourceCleanupStatus != "uncertain") {
			t.Fatal("changed root forged cleanup")
		}
		if !changed && (calls != 1 || s.Actions[0].ResourceCleanupStatus != "completed") {
			t.Fatal("manual outcome blocked real cleanup")
		}
	}
}
