package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"trace/internal/mcp"
)

func nestedSettlementWorker(t *testing.T, a *Agent, store *Store, parentID string) AgentWorker {
	t.Helper()
	// These recovery fixtures deliberately run a middle and a leaf together.
	// The root's former per-plan capacity=1 now needs an explicit tree override.
	capacity, err := a.WorkerCapacity(parentID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPlanCapacity(capacity.RootRunID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlan(parentID, []PlanTask{{ID: "leaf", Goal: "grandchild once"}}); err != nil {
		t.Fatal(err)
	}
	wid, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ClaimTask(parentID, "leaf", wid, 30); err != nil {
		t.Fatal(err)
	}
	child, err := a.Create("grandchild once")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(child.Run.ID, func(s *Snapshot) error {
		s.RootContract = nil
		s.Run.ParentRunID = parentID
		s.Run.ParentTaskID = "leaf"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	w := AgentWorker{ID: wid, TaskID: "leaf", RunID: child.Run.ID, Status: "running", CleanupStatus: "pending", Visit: 1, StartedAt: time.Now().UTC()}
	if _, err = store.Update(parentID, func(s *Snapshot) error {
		s.Plan.Tasks[0].ChildRunID = w.RunID
		s.Workers = append(s.Workers, w)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

func settlementHTTPMCP(t *testing.T, cleanupCode int) (string, *atomic.Int32) {
	t.Helper()
	var deletes atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
			w.WriteHeader(cleanupCode)
			return
		}
		var rpc integrationRPC
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			return
		}
		if rpc.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var result any
		switch rpc.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "cleanup-fixture")
			result = map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	t.Cleanup(remote.Close)
	return remote.URL, &deletes
}

func TestWorkerSettlementNestedRenameRecovery(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprint(restart), func(t *testing.T) {
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
				restore, failed := settlementRenameFailure(t, store, runID)
				leafErr := settlementOperation(t, childAgent, store, runID, leaf, func(s *Snapshot) { calls++; s.Run.Status = StatusCompleted })
				restore()
				if leafErr == nil || !*failed {
					t.Fatal("grandchild parent rename fault did not occur")
				}
				// The middle model already returned; the retirement boundary, not execution,
				// must reconcile the descendant before this middle worker can be published.
				return store.Update(runID, func(s *Snapshot) error { s.Run.Status = StatusCompleted; return nil })
			})
			unlock()
			var pending *workerSettlementPendingError
			if !errors.As(err, &pending) {
				t.Fatalf("nested settlement lost typed pending: %v", err)
			}
			child, err := store.Load(middle.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if child.WorkerSettlement == nil || child.WorkerSettlement.CleanupStatus != "completed" || !child.WorkerSettlement.DescendantSettlementPending {
				t.Fatal("settlement pending contaminated actual cleanup receipt")
			}
			if restart {
				freshStore, err := NewStore(store.root)
				if err != nil {
					t.Fatal(err)
				}
				fresh := *a
				fresh.store = freshStore
				fresh.workers = newWorkerLifecycle()
				a = &fresh
				store = freshStore
			}
			root, err := a.RecoverWorkers(id)
			if err != nil {
				t.Fatal(err)
			}
			mid, err := store.Load(middle.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if root.Plan.Tasks[0].Status != "completed" || mid.Plan.Tasks[0].Status != "completed" || occupiedWorkerSlots(&root) != 0 || occupiedWorkerSlots(&mid) != 0 || calls != 1 {
				t.Fatalf("nested recovery did not converge: root=%+v mid=%+v calls=%d", root.Workers, mid.Workers, calls)
			}
			if _, err = a.RecoverWorkers(id); err != nil {
				t.Fatal(err)
			}
			root, err = store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			for _, snapshot := range []Snapshot{root, mid} {
				n, m := settlementCounts(snapshot)
				if n != 1 || m != 1 {
					t.Fatalf("nested duplicate summary: %d/%d", n, m)
				}
			}
			if err = a.StopWorkers(context.Background()); err != nil {
				t.Fatal("resolved nested pending remained", err)
			}
		})
	}
}

func TestWorkerSettlementNestedRealCleanupFailureStaysBlocked(t *testing.T) {
	a, store, id, middle := settlementFixture(t)
	url, deletes := settlementHTTPMCP(t, http.StatusInternalServerError)
	unlock, err := store.AcquireRunLock(middle.RunID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.runChildOperation(context.Background(), id, middle, func(childAgent *Agent, _ context.Context, runID string) (Snapshot, error) {
		if _, err := store.Update(runID, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
			return Snapshot{}, err
		}
		leaf := nestedSettlementWorker(t, childAgent, store, runID)
		leafUnlock, e := store.AcquireRunLock(leaf.RunID)
		if e != nil {
			return Snapshot{}, e
		}
		_, leafErr := childAgent.runChildOperation(context.Background(), runID, leaf, func(grandchildAgent *Agent, _ context.Context, leafID string) (Snapshot, error) {
			if _, e := grandchildAgent.AddMCP(context.Background(), mcp.Config{Name: "bad-cleanup", URL: url}); e != nil {
				return Snapshot{}, e
			}
			return store.Update(leafID, func(s *Snapshot) error { s.Run.Status = StatusCompleted; return nil })
		})
		leafUnlock()
		var cleanup *mcp.HTTPError
		if !errors.As(leafErr, &cleanup) {
			t.Fatalf("missing real cleanup failure: %v", leafErr)
		}
		return store.Update(runID, func(s *Snapshot) error { s.Run.Status = StatusCompleted; return nil })
	})
	unlock()
	if err == nil || deletes.Load() != 1 {
		t.Fatalf("missing grandchild cleanup error: %v %d", err, deletes.Load())
	}
	if _, err = a.RecoverWorkers(id); err != nil {
		t.Fatal(err)
	}
	root, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	mid, err := store.Load(middle.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if root.Workers[0].CleanupStatus != "uncertain" || mid.Workers[0].CleanupStatus != "uncertain" || occupiedWorkerSlots(&root) != 1 || occupiedWorkerSlots(&mid) != 1 {
		t.Fatal("real cleanup failure released nested capacity")
	}
}

func TestWorkerSettlementCloseMCPSeparatesPendingFromCleanup(t *testing.T) {
	for _, code := range []int{http.StatusNoContent, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			a, _ := newTestAgent(t, successProvider{})
			url, deletes := settlementHTTPMCP(t, code)
			if _, err := a.AddMCP(context.Background(), mcp.Config{Name: "close-fixture", URL: url}); err != nil {
				t.Fatal(err)
			}
			original := &os.PathError{Op: "rename", Path: filepath.Join(t.TempDir(), "parent.json"), Err: os.ErrPermission}
			worker := AgentWorker{ID: "worker", Visit: 1}
			a.recordWorkerSettlementError("parent", worker, original)
			err := a.CloseMCP()
			var pending *workerSettlementPendingError
			if !errors.As(err, &pending) || !errors.Is(err, os.ErrPermission) || deletes.Load() != 1 {
				t.Fatalf("pending lost identity or stopped actual close: %v", err)
			}
			a.recordWorkerSettlementError("parent", worker, nil)
			err = a.CloseMCP()
			if errors.As(err, &pending) || errors.Is(err, os.ErrPermission) {
				t.Fatalf("cleared pending persisted: %v", err)
			}
			var cleanup *mcp.HTTPError
			if code == http.StatusNoContent {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.As(err, &cleanup) || cleanup.StatusCode != code {
				t.Fatalf("actual cleanup error discarded: %v", err)
			}
			if deletes.Load() != 1 {
				t.Fatal("close was repeated instead of reporting original resource result")
			}
		})
	}
}

func TestWorkerSettlementNestedRechecksAfterSecondParentFailure(t *testing.T) {
	a, store, id, middle := settlementFixture(t)
	var leaf AgentWorker
	unlock, err := store.AcquireRunLock(middle.RunID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.runChildOperation(context.Background(), id, middle, func(childAgent *Agent, _ context.Context, runID string) (Snapshot, error) {
		if _, e := store.Update(runID, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); e != nil {
			return Snapshot{}, e
		}
		leaf = nestedSettlementWorker(t, childAgent, store, runID)
		restore, _ := settlementRenameFailure(t, store, runID)
		leafErr := settlementOperation(t, childAgent, store, runID, leaf, func(s *Snapshot) { s.Run.Status = StatusPaused; s.Run.PauseReason = "parent_pause" })
		restore()
		if leafErr == nil {
			t.Fatal("missing initial descendant persistence failure")
		}
		return store.Update(runID, func(s *Snapshot) error { s.Run.Status = StatusCompleted; return nil })
	})
	unlock()
	if err == nil {
		t.Fatal("middle lost descendant pending")
	}
	restore, failed := settlementRenameFailure(t, store, id)
	_, err = a.RecoverWorkers(id)
	restore()
	if err == nil || !*failed {
		t.Fatal("root commit retry did not fail")
	}
	mid, err := store.Load(middle.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if mid.Workers[0].Status != "paused" || mid.WorkerSettlement == nil || !mid.WorkerSettlement.DescendantSettlementPending {
		t.Fatal("descendant recovery erased unfinished root proof requirement")
	}
	child, err := store.Load(leaf.RunID)
	if err != nil {
		t.Fatal(err)
	}
	leafUnlock, err := store.AcquireRunLock(leaf.RunID)
	if err != nil {
		t.Fatal(err)
	}
	_, release, next, err := a.prepareWorkerResume(context.Background(), child)
	if err != nil {
		leafUnlock()
		t.Fatal(err)
	}
	release()
	leafUnlock()
	if next.Visit != leaf.Visit+1 {
		t.Fatal("new leaf visit not established")
	}
	if _, err = a.RecoverWorkers(id); err == nil {
		t.Fatal("old middle receipt ignored newer live descendant visit")
	}
	root, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	mid, err = store.Load(middle.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if occupiedWorkerSlots(&root) != 1 || occupiedWorkerSlots(&mid) != 1 {
		t.Fatal("new descendant visit released root capacity")
	}
	n, m := settlementCounts(root)
	if n != 0 || m != 0 {
		t.Fatal("uncommitted root emitted a successful summary")
	}
}

func TestWorkerSettlementNestedLegacyCleanupCompatibility(t *testing.T) {
	for _, cleanup := range []string{"completed", "unknown"} {
		t.Run(cleanup, func(t *testing.T) {
			a, store, id, middle := settlementFixture(t)
			if _, err := store.Update(middle.RunID, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
				t.Fatal(err)
			}
			leaf := nestedSettlementWorker(t, a, store, middle.RunID)
			if _, err := store.Update(leaf.RunID, func(s *Snapshot) error { s.Run.Status = StatusCompleted; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(middle.RunID, func(s *Snapshot) error {
				s.Run.Status = StatusCompleted
				s.Plan.Tasks[0].Status = "completed"
				s.Plan.Tasks[0].LeaseUntil = time.Time{}
				s.Workers[0].Status = "completed"
				s.Workers[0].CleanupStatus = cleanup
				s.Workers[0].Visit = 0
				s.Workers[0].SettledVisit = nil
				s.WorkerSettlement = &WorkerSettlement{ParentRunID: id, TaskID: middle.TaskID, WorkerID: middle.ID, RunID: middle.RunID, Visit: middle.Visit, CleanupStatus: "completed", DescendantSettlementPending: true, ObservedAt: time.Now().UTC()}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_, err := a.RecoverWorkers(id)
			if cleanup == "completed" && err != nil {
				t.Fatal("legacy committed cleanup became unknown", err)
			}
			if cleanup == "unknown" && err == nil {
				t.Fatal("legacy unknown was promoted")
			}
			root, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if (occupiedWorkerSlots(&root) == 0) != (cleanup == "completed") {
				t.Fatal("legacy cleanup gate changed")
			}
			oldChild, err := store.Load(leaf.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if oldChild.WorkerSettlement != nil {
				t.Fatal("recovery fabricated a legacy cleanup receipt")
			}
		})
	}
}

func TestWorkerSettlementPendingWrappedClassification(t *testing.T) {
	cause := &os.PathError{Op: "rename", Path: "fixture", Err: os.ErrPermission}
	wrapped := fmt.Errorf("nested owner: %w", &workerSettlementPendingError{err: cause})
	cleanup, pending := splitWorkerSettlementErrors(wrapped)
	var original *os.PathError
	if cleanup != nil || !errors.Is(pending, os.ErrPermission) || !errors.As(pending, &original) {
		t.Fatal("pending wrapper lost original persistence error")
	}
	genuine := errors.New("actual transport cleanup failed")
	cleanup, pending = splitWorkerSettlementErrors(errors.Join(wrapped, genuine))
	if !errors.Is(cleanup, genuine) || !errors.Is(pending, os.ErrPermission) {
		t.Fatal("pending classification lost real cleanup")
	}
}
