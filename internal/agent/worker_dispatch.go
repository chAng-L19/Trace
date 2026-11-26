package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

var errDispatchInterrupted = errors.New("dispatcher stopped during preparation")

// DispatchTask reserves one child identity before publishing or executing it.
// Adapted from Muteki graph_intents.py owner-fenced admission and ARTEX goals.go.
func (a *Agent) DispatchTask(ctx context.Context, id, taskID string) (AgentWorker, error) {
	if err := ctx.Err(); err != nil {
		return AgentWorker{}, err
	}
	ctx, release, err := a.workers.begin(ctx)
	if err != nil {
		return AgentWorker{}, err
	}
	launched := false
	defer func() {
		if !launched {
			release()
		}
	}()
	wid, err := newID()
	if err != nil {
		return AgentWorker{}, err
	}
	childID, err := newID()
	if err != nil {
		return AgentWorker{}, err
	}
	unlock, err := a.store.AcquireRunLock(childID)
	if err != nil {
		return AgentWorker{}, err
	}
	defer func() {
		if !launched {
			unlock()
		}
	}()
	snap, err := a.claimTaskWithChild(ctx, id, taskID, wid, 30, childID)
	if err != nil {
		return AgentWorker{}, err
	}
	abort := func(cause error) (AgentWorker, error) {
		return AgentWorker{}, errors.Join(cause, a.abortDispatchPreparation(id, taskID, wid, childID, cause))
	}
	task, _ := planTask(&snap, taskID)
	workerContext, err := a.newWorkerContext(&snap, *task)
	if err != nil {
		return abort(err)
	}
	workerContext.WorkerID, workerContext.RunID = wid, childID
	for _, handoff := range workerContext.Handoffs {
		if handoff.Status != "available" {
			return abort(&HandoffNotReadyError{TaskID: taskID, Sources: []string{handoff.SourceTaskID}})
		}
	}
	workerContext.MethodCandidates = a.recallWorkerMethods(task.Goal)
	inputs, err := taskInputAttachments(&snap, task.InputAttachmentIDs)
	if err != nil {
		return abort(err)
	}
	stageModel, err := a.stageModelSelection(*task)
	if err != nil {
		return abort(err)
	}
	_, err = a.CreateConfigured(task.Goal, RunConfiguration{runID: childID, workerTask: task, workerContext: workerContext, Requirements: task.Requirements, ModelSelection: stageModel, inheritedRun: &snap.Run, TreeBudget: &TreeBudget{}, role: "worker"}, inputs...)
	if err != nil {
		return abort(err)
	}
	w := AgentWorker{ID: wid, TaskID: taskID, RunID: childID, Status: "running", CleanupStatus: "pending", StartedAt: time.Now().UTC(), Visit: 1}
	err = a.registerPreparedWorker(ctx, id, w, workerContext)
	if err != nil {
		return abort(err)
	}
	launched = true
	go func() { defer release(); defer unlock(); a.runChild(ctx, id, w) }()
	return w, nil
}

// Registration rechecks the child under the same Store fence as publication.
func (a *Agent) registerPreparedWorker(ctx context.Context, id string, w AgentWorker, workerContext *WorkerContext) error {
	_, err := a.store.Update(id, func(s *Snapshot) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.checkTaskAdmissionLocked(s); err != nil {
			return err
		}
		t, err := planTask(s, w.TaskID)
		if err != nil {
			return err
		}
		if t.WorkerID != w.ID || t.ChildRunID != w.RunID || t.Status != "running" || t.DispatchPhase != "preparing" {
			return fmt.Errorf("dispatch identity changed")
		}
		child, err := a.store.loadRawLocked(w.RunID)
		if err != nil {
			return err
		}
		if child.Run.Deleted || child.Run.ParentRunID != id || child.Run.ParentTaskID != w.TaskID || child.WorkerContext == nil || child.WorkerContext.WorkerID != w.ID || child.WorkerContext.RunID != w.RunID {
			return fmt.Errorf("dispatch child identity changed before registration")
		}
		if child.Run.Status != StatusCreated {
			return fmt.Errorf("dispatch child is no longer ready: %s", child.Run.Status)
		}
		if err := a.checkWorkerCapacityLocked(s, 0); err != nil {
			return err
		}
		if err := a.checkTaskDispatchWindowLocked(s, *t); err != nil {
			return err
		}
		if err := a.validateHandoffManifestLocked(s, *t, workerContext.Handoffs); err != nil {
			return err
		}
		t.DispatchPhase = ""
		t.LeaseUntil = time.Now().UTC().Add(30 * time.Second)
		s.Workers = append(s.Workers, w)
		addEvent(s, "task_dispatch_settled", "任务派发准备已结算", *t)
		addEvent(s, "worker_started", "子智能 worker 已启动", w)
		return nil
	})
	return err
}

// The caller owns the child's execution lock. Parent and child commits are
// recoverable stages, not a cross-snapshot atomic transaction.
func (a *Agent) abortDispatchPreparation(id, taskID, workerID, childID string, cause error) error {
	_, err := a.store.Update(id, func(s *Snapshot) error {
		task, err := planTask(s, taskID)
		if err != nil {
			return err
		}
		if task.WorkerID != workerID || task.ChildRunID != childID || task.DispatchPhase != "preparing" || task.Status != "running" {
			return fmt.Errorf("dispatch identity changed before abort")
		}
		for _, w := range s.Workers {
			if w.ID == workerID || w.RunID == childID {
				return fmt.Errorf("dispatch already registered; recover its worker")
			}
		}
		explicitCancellation := false
		child, loadErr := a.store.loadRawLocked(childID)
		if loadErr != nil && !errors.Is(loadErr, os.ErrNotExist) {
			return loadErr
		}
		if loadErr == nil {
			if child.Run.Deleted || child.Run.ParentRunID != id || child.Run.ParentTaskID != taskID || child.WorkerContext == nil || child.WorkerContext.WorkerID != workerID || child.WorkerContext.RunID != childID {
				return fmt.Errorf("dispatch child identity changed")
			}
			if len(child.Requests) > 0 || len(child.Actions) > 0 || len(child.Workers) > 0 || len(child.Evidence) > 0 || len(pendingToolCalls(&child)) > 0 || toolCleanupError(child) != nil || child.Run.Status != StatusCreated && child.Run.Status != StatusCancelled {
				task.Status, task.Error = "uncertain", "dispatch preparation contains execution state; inspect before recovery"
				task.DispatchPhase = ""
				addEvent(s, "task_dispatch_settled", "任务派发准备需要人工核查", *task)
				addEvent(s, "worker_dispatch_uncertain", task.Error, *task)
				return nil
			}
			cancellation := map[string]string{"reason": "dispatch_preparation_aborted", "parent_run_id": id, "task_id": taskID, "worker_id": workerID, "child_run_id": childID}
			if child.Run.Status == StatusCancelled {
				explicitCancellation = true
				for i := len(child.Events) - 1; i >= 0; i-- {
					if child.Events[i].Type == "run_cancelled" {
						explicitCancellation = valueHash(child.Events[i].Data) != valueHash(cancellation)
						break
					}
				}
			}
			if child.Run.Status != StatusCancelled {
				child.Run.Status = StatusCancelled
				child.Run.UpdatedAt = time.Now().UTC()
				addEvent(&child, "run_cancelled", "未启动的派发已中止", cancellation)
				if err := a.store.saveLocked(child); err != nil {
					return err
				}
			}
		}
		previousWorker, previousRun := task.WorkerID, task.ChildRunID
		task.Status = "failed"
		if errors.Is(cause, errDispatchInterrupted) || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || errors.Is(cause, errWorkerCapacity) || dispatchPrerequisiteWait(cause) || isStopped(s.Run.Status) {
			task.Status = "pending"
		}
		if explicitCancellation {
			task.Status = "cancelled"
		}
		task.WorkerID, task.ChildRunID, task.DispatchPhase = "", "", ""
		task.LeaseUntil = time.Time{}
		task.Error = cause.Error()
		refreshFrontier(&s.Plan)
		addEvent(s, "task_dispatch_settled", "任务派发准备已收口", *task)
		addEvent(s, "worker_dispatch_aborted", "未启动的派发已收口", map[string]string{"task_id": taskID, "worker_id": previousWorker, "run_id": previousRun, "error": cause.Error()})
		return nil
	})
	return err
}
