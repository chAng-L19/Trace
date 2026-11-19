package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRuntimeAuditLifecycleResumeRejectsTerminalParentOrAncestor(t *testing.T) {
	for _, status := range []Status{StatusCompleted, StatusFailed} {
		for _, relation := range []string{"parent", "grandparent"} {
			t.Run(string(status)+"/"+relation, func(t *testing.T) {
				a, store, root, middle := settlementFixture(t)
				leaf := nestedSettlementWorker(t, a, store, middle.RunID)
				if err := settlementOperation(t, a, store, middle.RunID, leaf, func(s *Snapshot) {
					s.Run.Status, s.Run.PauseReason = StatusPaused, "parent_pause"
				}); err != nil {
					t.Fatal(err)
				}
				stopped := middle.RunID
				if relation == "grandparent" {
					stopped = root
				}
				if _, err := store.Update(stopped, func(s *Snapshot) error { s.Run.Status = status; return nil }); err != nil {
					t.Fatal(err)
				}
				if _, err := a.Run(context.Background(), leaf.RunID); err == nil || !strings.Contains(err.Error(), string(status)) {
					t.Fatalf("expected resume denial for %s %s, got %v", relation, status, err)
				}
				parent, err := store.Load(middle.RunID)
				if err != nil {
					t.Fatal(err)
				}
				w := parent.Workers[0]
				if w.Visit != leaf.Visit || w.Status != "paused" || w.CleanupStatus != "completed" || a.HasActiveWorkers() {
					t.Fatalf("rejected resume changed registration: %+v", w)
				}
				child, err := store.Load(leaf.RunID)
				if err != nil || child.Run.Status != StatusPaused || len(child.Requests)+len(child.Actions) != 0 {
					t.Fatalf("rejected resume advanced child: %+v, err=%v", child.Run, err)
				}
			})
		}
	}
}

func TestRuntimeAuditLifecycleResumeReconcilesOldVisitOnlyOnCommit(t *testing.T) {
	for _, mode := range []string{"commit", "rollback"} {
		t.Run(mode, func(t *testing.T) {
			a, store, id, w := settlementFixture(t)
			restore, failed := settlementRenameFailure(t, store, id)
			err := settlementOperation(t, a, store, id, w, func(s *Snapshot) {
				s.Run.Status, s.Run.PauseReason = StatusPaused, "operator_pause"
			})
			restore()
			var pending *workerSettlementPendingError
			if !*failed || !errors.As(err, &pending) {
				t.Fatalf("missing initial parent write failure: %v", err)
			}
			child, err := store.Load(w.RunID)
			if err != nil || !child.WorkerSettlement.matches(id, w) || child.WorkerSettlement.CleanupStatus != "completed" {
				t.Fatalf("missing child cleanup receipt: %+v, err=%v", child.WorkerSettlement, err)
			}
			if mode == "rollback" {
				restore, failed = settlementRenameFailure(t, store, id)
			}
			_, err = a.Run(context.Background(), w.RunID)
			if mode == "rollback" {
				restore()
			}
			parent, loadErr := store.Load(id)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			stopErr := a.StopWorkers(context.Background())
			current := parent.Workers[0]
			if mode == "rollback" {
				if !*failed || err == nil || !errors.As(stopErr, &pending) || current.Visit != w.Visit || current.CleanupStatus != "pending" || occupiedWorkerSlots(&parent) != 1 {
					t.Fatalf("failed parent commit lost pending visit: %+v resume=%v stop=%v", current, err, stopErr)
				}
				return
			}
			if err != nil || stopErr != nil || current.Visit != w.Visit+1 || current.SettledVisit == nil || *current.SettledVisit != current.Visit || current.CleanupStatus != "completed" || occupiedWorkerSlots(&parent) != 0 {
				t.Fatalf("old visit survived committed resume: %+v resume=%v stop=%v", current, err, stopErr)
			}
		})
	}
}

func TestRuntimeAuditLifecycleTerminalAncestorCancelsActiveVisit(t *testing.T) {
	for _, status := range []Status{StatusFailed, StatusCompleted} {
		for _, relation := range []string{"parent", "grandparent"} {
			t.Run(string(status)+"/"+relation, func(t *testing.T) {
				a, store, root, w := settlementFixture(t)
				parentID := root
				if relation == "grandparent" {
					parentID = w.RunID
					w = nestedSettlementWorker(t, a, store, parentID)
				}
				unlock, err := store.AcquireRunLock(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
				ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				defer cancel()
				_, err = a.runChildOperation(ctx, parentID, w, func(child *Agent, visit context.Context, runID string) (Snapshot, error) {
					if _, err := store.Update(root, func(s *Snapshot) error { s.Run.Status = status; return nil }); err != nil {
						return Snapshot{}, err
					}
					<-visit.Done()
					// The callback still owns the visit: cancellation itself is not an exit receipt.
					parent, err := store.Load(parentID)
					if err != nil {
						return Snapshot{}, err
					}
					if parent.Workers[0].CleanupStatus != "pending" || occupiedWorkerSlots(&parent) != 1 {
						t.Errorf("cancellation released a live visit: %+v", parent.Workers)
					}
					return child.fail(runID, context.Cause(visit))
				})
				if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), string(status)) {
					t.Fatalf("watcher did not cancel on %s %s: %v", relation, status, err)
				}
				parent, err := store.Load(parentID)
				if err != nil {
					t.Fatal(err)
				}
				current := parent.Workers[0]
				if current.Status != "failed" || current.Visit != w.Visit || current.CleanupStatus != "completed" || occupiedWorkerSlots(&parent) != 0 {
					t.Fatalf("actual exit did not settle the original visit: %+v", current)
				}
			})
		}
	}
}
