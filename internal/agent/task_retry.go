package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// RetryTask reopens only an attempt proven to have executed no actions. The old
// child is retired under its run lock so a concurrent resume cannot run it too.
// Existing worker/run history remains available; dispatch creates a new child.
func (a *Agent) RetryTask(id, taskID string) (Snapshot, error) {
	before, err := a.store.Load(id)
	if err != nil {
		return Snapshot{}, err
	}
	previous, err := planTask(&before, taskID)
	if err != nil {
		return Snapshot{}, err
	}
	if previous.ChildRunID != "" {
		release, err := a.store.AcquireRunLock(previous.ChildRunID)
		if err != nil {
			return Snapshot{}, err
		}
		defer release()
	}
	return a.store.Update(id, func(s *Snapshot) error {
		task, err := planTask(s, taskID)
		if err != nil {
			return err
		}
		if task.Status != "failed" || task.ChildRunID != previous.ChildRunID || task.WorkerID != previous.WorkerID {
			return fmt.Errorf("only an unchanged failed task can be retried")
		}
		if task.LocalExecution {
			return fmt.Errorf("local task effects share the current run; inspect them and create an explicit replacement instead of replaying the task")
		}
		if isStopped(s.Run.Status) {
			return fmt.Errorf("parent run is stopped")
		}
		oldWorker, oldRun := task.WorkerID, task.ChildRunID
		if oldRun != "" {
			workerIndex := -1
			for i, w := range s.Workers {
				if w.ID == oldWorker && w.RunID == oldRun && w.TaskID == taskID {
					workerIndex = i
					break
				}
			}
			if workerIndex < 0 {
				return fmt.Errorf("failed attempt has no worker identity")
			}
			w := &s.Workers[workerIndex]
			if w.Status == "running" || w.CleanupStatus != "completed" {
				return fmt.Errorf("worker completion and cleanup must be settled before retry")
			}
			child, err := a.store.loadLocked(oldRun)
			if err != nil {
				return err
			}
			if child.Run.ParentRunID != id || child.Run.ParentTaskID != taskID {
				return fmt.Errorf("child identity does not match task")
			}
			if child.Run.ModelSelection != nil {
				if task.ModelBinding != nil && *task.ModelBinding != *child.Run.ModelSelection {
					return fmt.Errorf("failed child model identity differs from task binding")
				}
				selection := *child.Run.ModelSelection
				task.ModelBinding = &selection
			}
			if child.Run.Status != StatusFailed && child.Run.Status != StatusCancelled {
				return fmt.Errorf("child is not a failed attempt")
			}
			if len(child.Actions) > 0 || len(child.Workers) > 0 || len(child.Evidence) > 0 || len(pendingToolCalls(&child)) > 0 {
				return fmt.Errorf("attempt contains execution intent or effects; inspect it instead of replaying")
			}
			child.Run.Status = StatusCancelled
			child.Run.UpdatedAt = time.Now().UTC()
			addEvent(&child, "worker_retry_superseded", "未执行动作的失败尝试已封存", map[string]string{"parent_run_id": id, "task_id": taskID})
			refreshResult(&child)
			if err = a.store.saveLocked(child); err != nil {
				return err
			}
			w.Status = "superseded"
		} else {
			for _, w := range s.Workers {
				if w.ID == oldWorker {
					return fmt.Errorf("worker identity has no child run")
				}
			}
		}
		task.Status, task.Started = "pending", true
		task.WorkerID = ""
		task.LocalExecution, task.LocalWorkerVisit = false, 0
		task.ChildRunID = ""
		task.LeaseUntil = time.Time{}
		task.EvidenceRunID = ""
		task.EvidenceIDs = nil
		task.Error = ""
		refreshFrontier(&s.Plan)
		addEvent(s, "task_retry_requested", "零动作失败任务已重新进入前沿", map[string]string{"task_id": taskID, "previous_worker_id": oldWorker, "previous_run_id": oldRun})
		return nil
	})
}

// Disposition closes an attempt, not its goal. Muteki's explicit terminal
// reasons are adapted to the existing task record; no second ledger is used.
func taskDisposed(t PlanTask) bool {
	return t.Status == "blocked" || t.Status == "abandoned" || t.Status == "superseded"
}

func taskStarted(s *Snapshot, t PlanTask) bool {
	if t.Started || (t.Status != "pending" && t.Status != "blocked_capability") || t.WorkerID != "" || t.ChildRunID != "" {
		return true
	}
	for _, w := range s.Workers {
		if w.TaskID == t.ID {
			return true
		}
	}
	// Legacy retry snapshots cleared the attempt IDs but retained these events.
	for _, e := range s.Events {
		if e.Type != "task_claimed" && e.Type != "task_retry_requested" {
			continue
		}
		raw, _ := json.Marshal(e.Data)
		var fields struct {
			ID     string `json:"id"`
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal(raw, &fields) == nil && (fields.ID == t.ID || fields.TaskID == t.ID) {
			return true
		}
	}
	return false
}

func sameTaskRequirements(a, b []VerificationRequirement) bool {
	if len(a) != len(b) {
		return false
	}
	byID := map[string]string{}
	for _, r := range a {
		byID[r.ID] = valueHash(r)
	}
	for _, r := range b {
		if byID[r.ID] != valueHash(r) {
			return false
		}
	}
	return true
}

func taskContractCovers(replacement, original PlanTask) bool {
	if !slices.Equal(replacement.ContextEvidenceIDs, original.ContextEvidenceIDs) {
		return false
	}
	if replacement.CompletionCriteria != original.CompletionCriteria {
		return false
	}
	// A replacement freezes business inputs, not the old execution route or
	// estimate. It may add the retired attempt as a pinned handoff source.
	for _, source := range original.HandoffTaskIDs {
		if !hasString(replacement.HandoffTaskIDs, source) {
			return false
		}
	}
	for _, source := range replacement.HandoffTaskIDs {
		if source != original.ID && !hasString(original.HandoffTaskIDs, source) {
			return false
		}
	}
	return replacement.Stage == original.Stage && (!original.RequiredImageInput || replacement.RequiredImageInput) && sameEvidenceScope(replacement.EvidenceScope, original.EvidenceScope) && replacement.Goal == original.Goal && replacement.SuccessContains == original.SuccessContains &&
		sameTaskRequirements(replacement.Requirements, original.Requirements) &&
		slices.Equal(replacement.InputAttachmentIDs, original.InputAttachmentIDs) &&
		slices.Equal(replacement.AssetIDs, original.AssetIDs) &&
		slices.Equal(replacement.Dependencies, original.Dependencies) &&
		slices.Equal(replacement.ResourceKeys, original.ResourceKeys) &&
		slices.Equal(replacement.RequiredToolIDs, original.RequiredToolIDs) &&
		slices.Equal(replacement.RequiredAssetIDs, original.RequiredAssetIDs) &&
		slices.Equal(replacement.ExpectedArtifacts, original.ExpectedArtifacts)
}

// taskTerminal is deliberately different from taskDone: an explicitly closed
// failure stops planning but never unlocks dependent work.
func taskTerminal(t PlanTask) bool { return taskDone(t) || taskDisposed(t) }

func planDispositionFailure(s *Snapshot) string {
	if len(s.Plan.Tasks) == 0 || len(pendingToolCalls(s)) > 0 || occupiedWorkerSlots(s) > 0 {
		return ""
	}
	for _, t := range s.Plan.Tasks {
		if !taskTerminal(t) {
			return ""
		}
	}
	var failures []string
	for _, t := range s.Plan.Tasks {
		if (t.Status == "blocked" || t.Status == "abandoned") && !settledStage(s, t) {
			failures = append(failures, t.ID+" ("+t.Status+"): "+t.DispositionReason)
		}
	}
	if len(failures) == 0 {
		return ""
	}
	return "plan explicitly closed without completing its goals: " + strings.Join(failures, "; ")
}

// A replacement can satisfy the plan's aggregate goal, never the old task ID
// as a dependency. Cycles are rejected on mutation and guarded on legacy loads.
func planTaskSatisfied(p *PlanState, t PlanTask, seen map[string]bool) bool {
	if taskDone(t) {
		return true
	}
	if t.Status != "superseded" || seen[t.ID] {
		return false
	}
	seen[t.ID] = true
	for _, replacement := range p.Tasks {
		if replacement.ID == t.ReplacementTaskID && taskContractCovers(replacement, t) {
			return planTaskSatisfied(p, replacement, seen)
		}
	}
	return false
}

// DisposeTask records an explicit outcome without changing child history or
// replaying effects. The same child lock used for resume excludes live work.
func (a *Agent) DisposeTask(id, taskID, status, reason, replacementID string) (Snapshot, error) {
	if !taskDisposed(PlanTask{Status: status}) || strings.TrimSpace(reason) == "" {
		return Snapshot{}, fmt.Errorf("disposition requires blocked, abandoned or superseded and a concrete reason")
	}
	if (status == "superseded") != (replacementID != "") || replacementID == taskID {
		return Snapshot{}, fmt.Errorf("only superseded tasks require a different replacement_task_id")
	}
	before, err := a.store.Load(id)
	if err != nil {
		return Snapshot{}, err
	}
	previous, err := planTask(&before, taskID)
	if err != nil {
		return Snapshot{}, err
	}
	if previous.ChildRunID != "" {
		release, err := a.store.AcquireRunLock(previous.ChildRunID)
		if err != nil {
			code := "child_lock_unavailable"
			if errors.Is(err, ErrRunLockBusy) {
				code = "child_active"
			}
			// The failed lock remains authoritative; this optional read only
			// enriches the rejection and never initiates worker recovery.
			var child *Snapshot
			if observed, loadErr := a.store.Load(previous.ChildRunID); loadErr == nil {
				child = &observed
			}
			return Snapshot{}, newTaskDispositionError(&before, *previous, child, nil, code, err)
		}
		defer release()
	}
	return a.store.Update(id, func(s *Snapshot) error {
		task, err := planTask(s, taskID)
		if err != nil {
			return err
		}
		var childState *Snapshot
		childReadAttempted := false
		var blockedWorker *AgentWorker
		reject := func(code string, cause error) error {
			if !childReadAttempted && task.ChildRunID != "" {
				childReadAttempted = true
				if child, loadErr := a.store.loadLocked(task.ChildRunID); loadErr == nil {
					childState = &child
				}
			}
			return newTaskDispositionError(s, *task, childState, blockedWorker, code, cause)
		}
		if task.ChildRunID != previous.ChildRunID || task.WorkerID != previous.WorkerID {
			return reject("task_attempt_changed", fmt.Errorf("task attempt changed before disposition"))
		}
		if taskDisposed(*task) {
			if task.Status == status && task.DispositionReason == reason && task.ReplacementTaskID == replacementID {
				return nil
			}
			return reject("disposition_immutable", fmt.Errorf("task disposition is immutable"))
		}
		if taskDone(*task) || task.Status == "running" {
			code := "task_finished"
			if task.Status == "running" {
				code = "task_running"
			}
			return reject(code, fmt.Errorf("task must be unfinished and its execution settled before disposition"))
		}
		if task.ChildRunID == "" && task.Status == "uncertain" && task.WorkerID != "" {
			return reject("manual_outcome_uncertain", fmt.Errorf("manual worker outcome is still uncertain"))
		}
		for _, w := range s.Workers {
			if w.TaskID == taskID && (w.Status == "running" || w.CleanupStatus != "completed") {
				blockedWorker = &w
				return reject("worker_unsettled", fmt.Errorf("worker execution and cleanup must settle before disposition"))
			}
		}
		if task.ChildRunID != "" {
			childReadAttempted = true
			child, err := a.store.loadLocked(task.ChildRunID)
			if err != nil {
				return reject("child_unavailable", err)
			}
			childState = &child
			registered := false
			settledCancelledFailure := false
			for _, w := range s.Workers {
				if w.ID != task.WorkerID || w.RunID != child.Run.ID || w.TaskID != taskID {
					continue
				}
				registered = true
				legacySettled := w.Visit == 0 && w.SettledVisit == nil && child.WorkerSettlement == nil
				if !legacySettled && (!child.WorkerSettlement.matches(id, w) || child.WorkerSettlement.CleanupStatus != "completed" || w.SettledVisit == nil || *w.SettledVisit != w.Visit) {
					return reject("child_settlement_uncommitted", fmt.Errorf("child has no committed current cleanup settlement"))
				}
				settledCancelledFailure = child.Run.Status == StatusCancelled && !legacySettled && (status == "blocked" || status == "abandoned")
			}
			if !registered || child.Run.ParentRunID != id || child.Run.ParentTaskID != taskID {
				return reject("child_identity_mismatch", fmt.Errorf("child identity does not match the task"))
			}
			if child.Run.Status == StatusCreated || child.Run.Status == StatusRunning || child.Run.Status == StatusWaitingTool {
				return reject("child_execution_open", fmt.Errorf("child execution remains open"))
			}
			if err := a.workerDescendantsSettledLocked(child, map[string]bool{}); err != nil {
				return reject("descendants_unsettled", err)
			}
			for _, action := range child.Actions {
				// Harness fcc0ad462172 lifecycle.ts keeps cancellation separate
				// from cleanup. Retiring this attempt as failure preserves unknown
				// effects; it never authorizes replacement, replay or success.
				if action.Status == "started" || action.Status == "uncertain" && !settledCancelledFailure {
					return reject("child_action_uncertain", fmt.Errorf("child action outcome remains uncertain"))
				}
			}
			for _, requirement := range child.Requirements {
				check := latestCheck(&child, requirement.ID)
				if requirement.Category == "cleanup" && (check == nil || check.Status != "pass") {
					return reject("child_cleanup_unresolved", fmt.Errorf("child cleanup requirement %s remains unresolved", requirement.ID))
				}
			}
		}
		if status == "superseded" {
			replacement, err := planTask(s, replacementID)
			if err != nil || !taskContractCovers(*replacement, *task) {
				return reject("replacement_invalid", fmt.Errorf("replacement must preserve the goal and acceptance contract"))
			}
			seen := map[string]bool{}
			var reaches func(string) bool
			reaches = func(next string) bool {
				if next == taskID {
					return true
				}
				if seen[next] {
					return false
				}
				seen[next] = true
				t, err := planTask(s, next)
				if err != nil {
					return false
				}
				for _, dep := range t.Dependencies {
					if reaches(dep) {
						return true
					}
				}
				return t.Status == "superseded" && reaches(t.ReplacementTaskID)
			}
			if reaches(replacementID) {
				return reject("replacement_cycle", fmt.Errorf("replacement creates a dependency or disposition cycle"))
			}
		}
		task.PreviousStatus, task.Status, task.Started = task.Status, status, true
		task.DispositionReason, task.ReplacementTaskID, task.LeaseUntil = reason, replacementID, time.Time{}
		refreshFrontier(&s.Plan)
		addEvent(s, "task_disposed", "任务已显式处置，原始结果与证据保留", *task)
		return nil
	})
}
