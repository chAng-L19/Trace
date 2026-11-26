package agent

// Contract reference: Muteki 144df766, swarm/worker_session.py:287-333
// (AGPL-3.0): preserve session identity and classify interruption separately
// from completion. Trace resumes its existing Run ledger, not an external
// engine session; uncertain actions retain the existing manual-resolution gate.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"trace/internal/worker"
)

// A failed Run may itself be resumed, but cannot host child execution until then.
// A successful business result is not a lifecycle state.
func workerParentStopped(run Run) bool {
	return run.Deleted || isStopped(run.Status) || run.Status == StatusFailed || run.Status == StatusCompleted
}

// Admission follows the persisted worker tree, not ordinary conversation branches.
func (a *Agent) checkWorkerAncestors(s *Snapshot) error {
	for _, event := range s.Events {
		if event.Type == "plan_failed" {
			return fmt.Errorf("plan has a persisted terminal disposition; create a new run for further work")
		}
	}
	if reason := planDispositionFailure(s); reason != "" {
		return fmt.Errorf("plan has a terminal disposition: %s", reason)
	}
	if s.Run.ParentRunID == "" || s.Run.ParentTaskID == "" {
		return nil
	}
	seen := map[string]bool{s.Run.ID: true}
	for parentID := s.Run.ParentRunID; parentID != ""; {
		if seen[parentID] {
			return fmt.Errorf("worker ancestry contains a cycle")
		}
		seen[parentID] = true
		parent, err := a.store.Observe(parentID)
		if err != nil {
			return err
		}
		if workerParentStopped(parent.Run) {
			return fmt.Errorf("worker ancestor %s is stopped (%s)", parentID, parent.Run.Status)
		}
		if parent.Run.ParentTaskID == "" {
			break // Ordinary conversation branches are independent, not workers.
		}
		parentID = parent.Run.ParentRunID
	}
	return nil
}

var errPauseWorkerUnchanged = errors.New("worker pause already persisted")

// Persist child pauses even if cancellation has already reached their request.
// An already stopped child still needs provenance validation and subtree traversal.
func (a *Agent) pauseSnapshotWorkers(s Snapshot) error {
	for _, w := range s.Workers {
		if w.Status != "running" {
			continue
		}
		var unchanged Snapshot
		child, err := a.store.updatePause(w.RunID, func(child *Snapshot) error {
			if child.Run.Status == StatusCompleted || child.Run.Status == StatusCancelled || child.Run.Status == StatusPaused {
				// Update has already verified sources and persisted any revocation.
				// Retain that snapshot without another full save for this no-op.
				unchanged = *child
				return errPauseWorkerUnchanged
			}
			child.Run.Status = StatusPaused
			child.Run.PauseReason = "parent_pause"
			addEvent(child, "run_paused", "父任务暂停，保留原 Worker 运行等待恢复", parentPauseEventData(s))
			return nil
		})
		if err == errPauseWorkerUnchanged {
			child, err = unchanged, nil
		}
		if err != nil {
			return err
		}
		if err := a.pauseSnapshotWorkers(child); err != nil {
			return err
		}
	}
	return nil
}

// Caller holds the existing child execution lock. Reclaim the same worker and
// task only after its previous lifecycle finished cleanup; never create a Run.
func (a *Agent) prepareWorkerResume(ctx context.Context, child Snapshot) (context.Context, func(), AgentWorker, error) {
	var w, reconciled AgentWorker
	var blocked *TaskBlockedError
	var err error
	child, err = a.reconcileExecResourcesLocked(child.Run.ID, worker.ReconcileResourceOwner)
	if err != nil {
		return nil, nil, w, err
	}
	if child.Run.Status == StatusCompleted || child.Run.Status == StatusCancelled {
		return nil, nil, w, fmt.Errorf("run is %s", child.Run.Status)
	}
	ctx, release, err := a.workers.begin(ctx)
	if err != nil {
		return nil, nil, w, err
	}
	_, err = a.store.Update(child.Run.ParentRunID, func(parent *Snapshot) error {
		if err := a.checkTaskAdmissionLocked(parent); err != nil {
			return err
		}
		task, err := planTask(parent, child.Run.ParentTaskID)
		if err != nil {
			return err
		}
		if task.ChildRunID != child.Run.ID {
			return fmt.Errorf("worker attempt has been replaced")
		}
		if taskDisposed(*task) {
			return fmt.Errorf("task is explicitly %s; its child history is retired", task.Status)
		}
		for _, dep := range task.Dependencies {
			source, err := planTask(parent, dep)
			if err != nil || !taskDone(*source) {
				return fmt.Errorf("worker dependency %s is not complete", dep)
			}
		}
		for i := range parent.Workers {
			current := &parent.Workers[i]
			if current.ID != task.WorkerID || current.RunID != child.Run.ID || current.TaskID != task.ID {
				continue
			}
			if current.CleanupStatus != "completed" && child.WorkerSettlement.matches(parent.Run.ID, *current) && child.WorkerSettlement.CleanupStatus == "completed" {
				if err := a.applyWorkerSettlementLocked(parent, *current); err != nil {
					return err
				}
				reconciled = *current // Keep the receipt identity before advancing Visit.
			}
			if current.Status == "running" || current.Status == "starting" || current.Status == "cancelling" || current.CleanupStatus != "completed" {
				return fmt.Errorf("worker execution and cleanup have not settled")
			}
			latestChild, err := a.store.loadRawLocked(child.Run.ID)
			if err != nil {
				return err
			}
			if latestChild.Run.Deleted || latestChild.Run.Status == StatusCompleted || latestChild.Run.Status == StatusCancelled {
				return fmt.Errorf("run is %s or deleted", latestChild.Run.Status)
			}
			if latestChild.Run.ParentRunID != parent.Run.ID || latestChild.Run.ParentTaskID != task.ID {
				return fmt.Errorf("worker attempt has been replaced")
			}
			// Reuse one retained slot only after this exact visit's cleanup was
			// committed. This admits the original continuation/manual resolution;
			// it does not resolve or replay any uncertain action in the child.
			additional := 1
			if current.Status == "uncertain" || task.Status == "uncertain" {
				if latestChild.Run.Deleted || !latestChild.WorkerSettlement.matches(parent.Run.ID, *current) || latestChild.WorkerSettlement.CleanupStatus != "completed" || current.SettledVisit == nil || *current.SettledVisit != current.Visit || toolCleanupError(latestChild) != nil {
					return fmt.Errorf("retained worker slot requires the current cleanup settlement")
				}
				additional = 0
			}
			if resumableLocalTask(&latestChild, parent.Run.ID, *current) != nil {
				additional = 0 // Reuse the unfinished local task's retained slot.
			}
			if err := a.checkWorkerCapacityLocked(parent, additional); err != nil {
				for _, local := range latestChild.Plan.Tasks {
					if local.LocalExecution && (local.Status == "running" || local.Status == "uncertain") {
						return fmt.Errorf("local task %s retains a separate reservation until its current identity, outcome and cleanup are reconciled: %w", local.ID, err)
					}
				}
				return err
			}
			if err := a.taskResourcesLocked(parent, *task); err != nil {
				return err
			}
			if err := a.checkDispatchWindowLocked(parent); err != nil {
				return err
			}
			if err := a.checkTreeBudgetLocked(parent, "resume"); err != nil {
				return err
			}
			blocked, err = a.taskPrerequisitesLocked(parent, *task)
			if err != nil {
				return err
			}
			if blocked != nil {
				if valueHash(task.BlockedReason) != valueHash(blocked) {
					task.BlockedReason = blocked
					addEvent(parent, "task_blocked_capability", "原 Worker 缺少恢复所需能力，身份和清理状态不变", blocked)
				}
				return nil
			}
			task.BlockedReason = nil
			reportOnly := taskDone(*task) && reportDeliveryPending(&latestChild) && reportBusinessSettled(parent.Run.ID, *current, &latestChild)
			current.Status, current.CleanupStatus, current.Error = "running", "pending", ""
			current.Visit++
			sourceFloor := latestChild.Run.EventSeq
			current.SourceEventFloor = &sourceFloor
			current.FinishedAt = time.Time{}
			if !reportOnly {
				task.Status, task.Error = "running", ""
			}
			task.LeaseUntil = time.Now().UTC().Add(30 * time.Second)
			w = *current
			refreshFrontier(&parent.Plan)
			addEvent(parent, "worker_resumed", "原 Worker 运行已重新接入执行与结算", w)
			return nil
		}
		return fmt.Errorf("child has no current registered worker")
	})
	if err == nil && reconciled.ID != "" {
		a.recordWorkerSettlementError(child.Run.ParentRunID, reconciled, nil)
	}
	if err == nil && blocked != nil {
		err = blocked
	}
	if err != nil {
		release()
		return nil, nil, w, err
	}
	return ctx, release, w, nil
}

func (a *Agent) resumeWorkerLocked(ctx context.Context, child Snapshot) (Snapshot, error) {
	return a.resumeWorkerOperation(ctx, child, (*Agent).runLocked)
}

// ResumeWorker exposes the same child continuation used by CLI/Web, rather
// than retrying a task that may already have committed external effects.
func (a *Agent) ResumeWorker(ctx context.Context, id, workerID string) (AgentWorker, error) {
	parent, err := a.store.Load(id)
	if err != nil {
		return AgentWorker{}, err
	}
	runID, err := workerRun(&parent, workerID)
	if err != nil {
		return AgentWorker{}, err
	}
	unlock, err := a.store.AcquireRunLock(runID)
	if err != nil {
		return AgentWorker{}, err
	}
	child, err := a.store.Load(runID)
	if err != nil {
		unlock()
		return AgentWorker{}, err
	}
	if child.Run.ParentRunID != id || child.Run.ParentTaskID == "" {
		unlock()
		return AgentWorker{}, fmt.Errorf("worker is not a registered child of this run")
	}
	if strings.HasPrefix(child.Run.PauseReason, "waiting_input:") {
		hasInput := false
		for _, m := range child.Mailbox {
			hasInput = hasInput || m.Pending()
		}
		for _, m := range parent.WorkerMessages {
			hasInput = hasInput || m.To == workerID && workerMessageWakes(m)
		}
		hasInput = hasInput || a.hasPendingWorkerInput(&child)
		if !hasInput {
			unlock()
			return AgentWorker{}, fmt.Errorf("worker is waiting for input; send worker_message before worker_resume")
		}
	}
	workerCtx, release, w, err := a.prepareWorkerResume(ctx, child)
	if err != nil {
		unlock()
		return AgentWorker{}, err
	}
	go func() {
		defer release()
		defer unlock()
		a.runChild(workerCtx, id, w)
	}()
	return w, nil
}

func (a *Agent) hasPendingWorkerInput(s *Snapshot) bool {
	pending, err := a.pendingWorkerInput(s)
	return err == nil && pending
}

func (a *Agent) resumeWorkerOperation(ctx context.Context, child Snapshot, operation func(*Agent, context.Context, string) (Snapshot, error)) (Snapshot, error) {
	ctx, release, w, err := a.prepareWorkerResume(ctx, child)
	if err != nil {
		return child, err
	}
	defer release()
	return a.runChildOperation(ctx, child.Run.ParentRunID, w, operation)
}

// Parent continuation resumes only pauses it caused, or an input wait with new
// durable input. Pending tools require explicit child resume/manual resolution;
// automatic continuation never crosses an uncertain execution boundary.
func (a *Agent) resumePausedWorkers(ctx context.Context, id string) error {
	s, err := a.store.Observe(id)
	if err != nil {
		return err
	}
	for _, w := range s.Workers {
		if w.Status != "paused" || w.CleanupStatus != "completed" {
			continue
		}
		unlock, err := a.store.AcquireRunLock(w.RunID)
		if err != nil {
			if !errors.Is(err, ErrRunLockBusy) {
				return fmt.Errorf("resume paused worker %s: %w", w.ID, err)
			}
			continue // Another explicit resume, or final settlement, owns this Run.
		}
		child, err := a.store.Load(w.RunID)
		if err != nil {
			unlock()
			return err
		}
		ready := child.Run.PauseReason == "parent_pause" || child.Run.PauseReason == "report_translation_missing" || child.Run.PauseReason == "child_report_delivery_pending"
		if strings.HasPrefix(child.Run.PauseReason, "waiting_input:") {
			for _, q := range child.Mailbox {
				ready = ready || q.Pending()
			}
			for _, m := range s.WorkerMessages {
				ready = ready || m.To == w.ID && workerMessageWakes(m)
			}
			ready = ready || a.hasPendingWorkerInput(&child)
		}
		if child.Run.Status != StatusPaused || !ready || len(pendingToolCalls(&child)) > 0 {
			unlock()
			continue
		}
		workerCtx, release, current, err := a.prepareWorkerResume(ctx, child)
		if err != nil {
			unlock()
			var blocked *TaskBlockedError
			var busy *ResourceBusyError
			if errors.Is(err, errWorkerCapacity) || errors.As(err, &blocked) || errors.As(err, &busy) {
				continue
			}
			return err
		}
		go func() {
			defer release()
			defer unlock()
			a.runChild(workerCtx, id, current)
		}()
	}
	return nil
}
