package agent

import (
	"errors"
	"fmt"
	"time"
)

// ErrStaleWorkerVisit means the observed worker execution no longer owns the request.
var ErrStaleWorkerVisit = errors.New("stale_visit")

var errWorkerCancelUnchanged = errors.New("worker cancellation already persisted")

// CancelWorkerVisit fences cancellation to an observed visit when supplied.
// A nil expectedVisit preserves the legacy cancel-current-worker operation.
func (a *Agent) CancelWorkerVisit(id, workerID string, expectedVisit *uint64) (Snapshot, error) {
	snap, err := a.store.Load(id)
	if err != nil {
		return snap, err
	}
	if workerID == "parent" {
		return snap, fmt.Errorf("use parent cancel")
	}
	var observed AgentWorker
	for _, w := range snap.Workers {
		if w.ID == workerID {
			observed = w
			break
		}
	}
	if observed.ID == "" {
		return snap, fmt.Errorf("unknown worker: %s", workerID)
	}
	var cancelled AgentWorker
	_, err = a.store.Update(observed.RunID, func(child *Snapshot) error {
		// Update owns both Store locks. Re-read the parent here, at the child
		// commit boundary; public Load/Update would re-enter the same lock.
		parent, err := a.store.loadRawLocked(id)
		if err != nil {
			return err
		}
		if parent.Run.Deleted || child.Run.ParentRunID != id || child.Run.ParentTaskID != observed.TaskID {
			return fmt.Errorf("%w: worker %s parent identity changed", ErrStaleWorkerVisit, workerID)
		}
		for _, current := range parent.Workers {
			if current.ID != workerID {
				continue
			}
			task, taskErr := planTask(&parent, current.TaskID)
			if current.RunID != observed.RunID || current.TaskID != observed.TaskID || taskErr != nil || task.WorkerID != workerID || task.ChildRunID != observed.RunID {
				return fmt.Errorf("%w: worker %s execution identity changed", ErrStaleWorkerVisit, workerID)
			}
			if expectedVisit != nil && current.Visit != *expectedVisit {
				return fmt.Errorf("%w: worker %s expected visit %d, current visit %d", ErrStaleWorkerVisit, workerID, *expectedVisit, current.Visit)
			}
			cancelled = current
			if child.Run.Status == StatusCancelled {
				return errWorkerCancelUnchanged
			}
			return cancelRunSnapshot(child)
		}
		return fmt.Errorf("%w: worker %s is no longer registered", ErrStaleWorkerVisit, workerID)
	})
	if err != nil && !errors.Is(err, errWorkerCancelUnchanged) {
		return snap, err
	}
	return a.settleCancelledWorker(id, observed.RunID, &cancelled)
}

// Mirrors Agent.Cancel's state transition; both paths use Store.Update to
// retain the mailbox and apply the ordinary derived-state gates.
func cancelRunSnapshot(s *Snapshot) error {
	if s.Run.Status == StatusCompleted || s.Run.Status == StatusCancelled {
		return fmt.Errorf("run is already terminal")
	}
	s.Run.Status = StatusCancelled
	addEvent(s, "run_cancelled", "已请求取消运行", nil)
	return nil
}

// Paused workers have no goroutine left to settle their task. Active workers
// retain capacity until the ordinary cleanup receipt settles their execution.
func (a *Agent) settleCancelledWorker(id, childID string, expected *AgentWorker) (Snapshot, error) {
	return a.store.Update(id, func(s *Snapshot) error {
		for i := range s.Workers {
			w := &s.Workers[i]
			if w.RunID != childID {
				continue
			}
			if expected != nil && (w.ID != expected.ID || w.TaskID != expected.TaskID || w.Visit != expected.Visit) {
				return fmt.Errorf("%w: worker %s changed before cancellation settlement", ErrStaleWorkerVisit, expected.ID)
			}
			if w.Status == "running" || w.CleanupStatus != "completed" {
				continue
			}
			task, err := planTask(s, w.TaskID)
			if err != nil {
				return err
			}
			if taskDisposed(*task) {
				continue
			}
			if task.WorkerID == w.ID && task.ChildRunID == childID && (w.Status != "cancelled" || task.Status != "cancelled") {
				w.Status, task.Status = "cancelled", "cancelled"
				task.LeaseUntil = time.Time{}
				refreshFrontier(&s.Plan)
				addEvent(s, "worker_cancel_requested", "子 worker 取消已结算", map[string]any{"worker_id": w.ID, "task_id": task.ID, "child_run_id": childID, "visit": w.Visit, "worker": *w, "task": *task})
			}
		}
		return nil
	})
}
