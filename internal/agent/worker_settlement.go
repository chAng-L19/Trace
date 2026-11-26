package agent

// Contract reference: Muteki c60c8fee coordinator_race.py:1545-1557 and
// coordinator_finalize.py:294-338. Runtime exit is separate from durable
// retirement. Trace retains its single Store and never replays work to settle it.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"trace/internal/worker"
)

// WorkerSettlement is the last actual lifecycle observation in the original
// child Run. A new visit invalidates it without creating another journal.
type WorkerSettlement struct {
	ParentRunID                 string    `json:"parent_run_id"`
	TaskID                      string    `json:"task_id"`
	WorkerID                    string    `json:"worker_id"`
	RunID                       string    `json:"run_id"`
	Visit                       uint64    `json:"visit"`
	CleanupStatus               string    `json:"cleanup_status"`
	RunError                    string    `json:"run_error,omitempty"`
	CleanupError                string    `json:"cleanup_error,omitempty"`
	CleanupVersion              int       `json:"cleanup_version,omitempty"`
	LifecycleCleanupError       string    `json:"lifecycle_cleanup_error,omitempty"`
	ToolCleanupError            string    `json:"tool_cleanup_error,omitempty"`
	BusinessSourceHash          string    `json:"business_source_hash,omitempty"`
	BusinessContinuationVisit   *uint64   `json:"business_continuation_visit,omitempty"`
	ReportDeliveryPending       bool      `json:"report_delivery_pending,omitempty"`
	DescendantCleanupPending    bool      `json:"descendant_cleanup_pending,omitempty"`
	DescendantSettlementPending bool      `json:"descendant_settlement_pending,omitempty"`
	ObservedAt                  time.Time `json:"observed_at"`
}

func (r *WorkerSettlement) matches(id string, w AgentWorker) bool {
	return r != nil && r.ParentRunID == id && r.TaskID == w.TaskID && r.WorkerID == w.ID && r.RunID == w.RunID && r.Visit == w.Visit && !r.ObservedAt.IsZero()
}

// Only versioned receipts distinguish re-observable tool resources from MCP
// and unknown lifecycle failures. Legacy aggregate errors remain unresolved.
func (a *Agent) refreshWorkerCleanupReceiptLocked(s *Snapshot) bool {
	r := s.WorkerSettlement
	if r == nil || r.CleanupVersion != 1 || r.RunID != s.Run.ID || r.ParentRunID != s.Run.ParentRunID || r.TaskID != s.Run.ParentTaskID || r.ObservedAt.IsZero() {
		return false
	}
	before := valueHash(r)
	var cleanupErr error
	if r.LifecycleCleanupError != "" {
		cleanupErr = errors.New(r.LifecycleCleanupError)
	}
	r.ToolCleanupError = ""
	if err := toolCleanupError(*s); err != nil {
		r.ToolCleanupError = err.Error()
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if r.DescendantCleanupPending {
		if err := a.workerDescendantsSettledLocked(*s, map[string]bool{}); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		} else {
			r.DescendantCleanupPending = false
		}
	}
	r.CleanupStatus, r.CleanupError = "completed", ""
	if cleanupErr != nil {
		r.CleanupStatus, r.CleanupError = "uncertain", cleanupErr.Error()
	}
	return before != valueHash(r)
}

func (a *Agent) recordWorkerSettlementError(id string, w AgentWorker, err error) {
	a.workers.mu.Lock()
	defer a.workers.mu.Unlock()
	key := fmt.Sprintf("%s/%s/%d", id, w.ID, w.Visit)
	if err == nil {
		delete(a.workers.settlementErrors, key)
	} else {
		a.workers.settlementErrors[key] = fmt.Errorf("worker %s visit %d settlement: %w", w.ID, w.Visit, err)
	}
}

// Reconcile only idle, current visits. The normal wait path reads scheduling
// metadata and probes locks; it never rewrites active workers or replays work.
func (a *Agent) reconcileWorkerSettlements(id string) error {
	view, err := a.store.Observe(id)
	if err != nil {
		return err
	}
	if isStopped(view.Run.Status) {
		return nil
	}
	for _, w := range view.Workers {
		if w.Status != "running" && w.Status != "uncertain" && w.CleanupStatus == "completed" {
			continue
		}
		unlock, lockErr := a.store.AcquireRunLock(w.RunID)
		if lockErr != nil {
			if !errors.Is(lockErr, ErrRunLockBusy) {
				return fmt.Errorf("reconcile worker settlement %s: %w", w.ID, lockErr)
			}
			continue
		}
		child, loadErr := a.store.Load(w.RunID)
		if loadErr == nil && child.Run.ParentRunID == id && child.Run.ParentTaskID == w.TaskID {
			child, loadErr = a.reconcileExecResourcesLocked(w.RunID, worker.ReconcileResourceOwner)
		}
		unlock()
		if loadErr != nil {
			return loadErr
		}
		if child.WorkerSettlement.matches(id, w) && (child.WorkerSettlement.CleanupStatus == "completed" || child.WorkerSettlement.DescendantCleanupPending || child.WorkerSettlement.DescendantSettlementPending) {
			_, err := a.RecoverWorkers(id)
			return err // At most one recovery pass at each caller checkpoint.
		}
		a.workers.mu.Lock()
		pending := a.workers.settlementErrors[fmt.Sprintf("%s/%s/%d", id, w.ID, w.Visit)]
		a.workers.mu.Unlock()
		if pending != nil {
			return &workerSettlementPendingError{err: pending}
		}
		if w.Status == "running" {
			for _, task := range view.Plan.Tasks {
				if task.WorkerID == w.ID && task.ChildRunID == w.RunID && (task.Status == "running" || taskDone(task)) && !task.LeaseUntil.After(time.Now()) {
					_, err := a.RecoverWorkers(id)
					return err
				}
			}
		}
	}
	return nil
}

// Caller holds the child execution lock and the Store transaction. Receipt
// matching is the cleanup proof; an idle lock alone never releases capacity.
func (a *Agent) applyWorkerSettlementLocked(s *Snapshot, w AgentWorker) error {
	child, loadErr := a.store.loadLocked(w.RunID)
	if loadErr != nil {
		return loadErr
	}
	receipt := child.WorkerSettlement
	if !receipt.matches(s.Run.ID, w) || child.Run.ParentRunID != s.Run.ID || child.Run.ParentTaskID != w.TaskID {
		return fmt.Errorf("worker settlement receipt does not match current visit")
	}
	task, taskErr := planTask(s, w.TaskID)
	if taskErr != nil {
		return taskErr
	}
	if task.WorkerID != w.ID || task.ChildRunID != w.RunID {
		return fmt.Errorf("worker settlement ownership changed")
	}
	if taskDisposed(*task) {
		return nil // A late receipt never erases a committed disposition.
	}
	var current *AgentWorker
	for i := range s.Workers {
		if s.Workers[i].ID == w.ID && s.Workers[i].RunID == w.RunID && s.Workers[i].TaskID == w.TaskID && s.Workers[i].Visit == w.Visit {
			current = &s.Workers[i]
			break
		}
	}
	if current == nil {
		return fmt.Errorf("worker settlement visit changed")
	}
	alreadySettled := current.SettledVisit != nil && *current.SettledVisit == w.Visit
	if alreadySettled && (current.CleanupStatus == "completed" || receipt.CleanupStatus != "completed" || toolCleanupError(child) != nil) {
		return nil
	}
	if receipt.DescendantSettlementPending {
		if err := a.workerDescendantsSettledLocked(child, map[string]bool{}); err != nil {
			return &workerSettlementPendingError{err: err}
		}
	}
	var err, cleanupErr error
	if receipt.RunError != "" {
		err = errors.New(receipt.RunError)
	}
	if receipt.CleanupStatus != "completed" {
		cleanupErr = fmt.Errorf("worker cleanup %s: %s", receipt.CleanupStatus, receipt.CleanupError)
	}
	cleanupErr = errors.Join(cleanupErr, toolCleanupError(child))
	// Pause may race the child's final provider return. Read its durable
	// control state under the same transaction as parent settlement.
	if err != nil && child.Run.Status == StatusFailed && child.Run.Error != err.Error() {
		child.Run.Error = err.Error()
		addEvent(&child, "worker_interrupted", "Worker 中断原因已保存", map[string]string{"worker_id": w.ID, "error": err.Error()})
		refreshResult(&child)
		if saveErr := a.store.saveLocked(child); saveErr != nil {
			return saveErr
		}
	}
	if s.Run.Status == StatusPaused && child.Run.Status != StatusPaused && child.Run.Status != StatusCompleted && child.Run.Status != StatusCancelled {
		child.Run.Status, child.Run.PauseReason = StatusPaused, "parent_pause"
		addEvent(&child, "run_paused", "父任务暂停，保留原 Worker 运行等待恢复", parentPauseEventData(*s))
		refreshResult(&child)
		if err := a.store.saveLocked(child); err != nil {
			return err
		}
	}
	status := "completed"
	if err != nil || child.Run.Status == StatusFailed || child.Result.Status == "failed" {
		status = "failed"
	}
	// Autonomous children must satisfy their persisted Judge contract before
	// normal termination can unlock dependent work. Ordinary chat workers
	// retain their existing turn-completion semantics.
	if child.Run.Status == StatusCompleted && child.Run.RequireVerifiedCompletion && child.Result.Status == "incomplete" {
		status = "uncertain"
	}
	if child.Run.Status == StatusWaitingTool || child.Run.Status == StatusRunning || child.Run.Status == StatusCreated {
		status = "uncertain"
	}
	if child.Run.Status == StatusPaused {
		status = "paused"
	}
	if child.Run.Status == StatusCancelled || s.Run.Status == StatusCancelled {
		status = "cancelled"
	}
	// A report-only pause retains a resumable Worker, but successful business
	// work can satisfy dependencies after this visit's actual cleanup. The
	// parent's separate delivery gate still requires all bilingual file reports.
	taskStatus := status
	if status == "paused" && child.Run.PauseReason == "report_translation_missing" && reportBusinessReady(&child) && cleanupErr == nil && s.Run.Status != StatusPaused && len(a.pendingChildReportDeliveryLocked(&child, map[string]bool{})) == 0 {
		taskStatus = "completed"
	}
	task.Status, task.Error = taskStatus, ""
	task.LeaseUntil = time.Time{}
	task.EvidenceRunID = w.RunID
	task.EvidenceIDs = nil
	if taskStatus == "completed" && task.SuccessContains != "" {
		task.Status = "failed"
		for _, ev := range child.Evidence {
			output, valid := verifiedOutput(&child, ev.ID)
			if valid && child.Result.Status == "success" && strings.Contains(output, task.SuccessContains) {
				task.Status = "verified"
				task.EvidenceIDs = append(task.EvidenceIDs, ev.ID)
			}
		}
		if task.Status == "failed" {
			task.Error = "task predicate not satisfied by child execution evidence"
		}
	}
	if taskStatus == "completed" && len(task.Requirements) > 0 {
		if task.Status != "failed" && child.Result.Status == "success" && sameTaskRequirements(child.Requirements, task.Requirements) && taskRequirementsSatisfied(&child, *task) {
			task.Status = "verified"
			for _, requirement := range task.Requirements {
				for _, evidenceID := range latestCheck(&child, requirement.ID).EvidenceIDs {
					if !hasString(task.EvidenceIDs, evidenceID) {
						task.EvidenceIDs = append(task.EvidenceIDs, evidenceID)
					}
				}
			}
		} else {
			task.Status = "failed"
			task.Error = "task structured requirements not satisfied by child execution evidence"
		}
	}
	if taskStatus == "completed" && !taskScopeMatches(&child, *task) {
		task.Status = "failed"
		task.Error = "child evidence scope differs from task contract"
	}
	if cleanupErr != nil {
		task.Status = "uncertain"
		task.Error = cleanupErr.Error()
	}

	for i := range s.Workers {
		if s.Workers[i].ID == w.ID {
			sw := &s.Workers[i]
			sw.Status, sw.Error = status, ""
			sw.FinishedAt = receipt.ObservedAt
			visit := w.Visit
			sw.SettledVisit = &visit
			sw.CleanupStatus = "completed"
			if cleanupErr != nil {
				sw.CleanupStatus = "uncertain"
				sw.Error = cleanupErr.Error()
			}
			if err != nil {
				sw.Error = err.Error()
			}
		}
	}
	refreshFrontier(&s.Plan)
	eventType := "worker_finished"
	if alreadySettled {
		eventType = "worker_cleanup_reconciled"
	}
	report := newChildReport(s.Run.ID, w, &child, current.CleanupStatus)
	addEvent(s, eventType, fmt.Sprintf("子任务结算：运行 %s，业务 %s，清理 %s", report.RunStatus, report.ResultStatus, report.CleanupStatus), map[string]string{
		"worker_id": w.ID, "run_id": w.RunID, "status": status,
		"task_id": w.TaskID, "visit": fmt.Sprint(w.Visit), "source_hash": report.SourceHash,
		"run_status": string(report.RunStatus), "result_status": report.ResultStatus, "cleanup_status": report.CleanupStatus,
	})
	addEvent(s, "task_completed", "子任务最终状态已保存", *task)
	appendChildReport(s, report)
	return appendWorkerMessage(s, w.ID, "parent", childReportMessage(report))
}

// Revalidate every descendant against the same Store transaction. A completed
// resource receipt does not promote uncertain task outcomes or stale evidence.
func (a *Agent) workerDescendantsSettledLocked(s Snapshot, visiting map[string]bool) error {
	if err := toolCleanupError(s); err != nil {
		return err
	}
	if visiting[s.Run.ID] {
		return fmt.Errorf("worker settlement ancestry contains a cycle")
	}
	visiting[s.Run.ID] = true
	defer delete(visiting, s.Run.ID)
	for _, task := range s.Plan.Tasks {
		if task.Status == "running" {
			return fmt.Errorf("descendant task %s still running", task.ID)
		}
	}
	for _, w := range s.Workers {
		if w.Status == "running" || w.CleanupStatus != "completed" {
			return fmt.Errorf("descendant worker %s cleanup or settlement unresolved", w.ID)
		}
		child, err := a.store.loadLocked(w.RunID)
		if err != nil {
			return err
		}
		legacySettled := w.Visit == 0 && w.SettledVisit == nil && child.WorkerSettlement == nil
		if child.Run.ParentRunID != s.Run.ID || child.Run.ParentTaskID != w.TaskID || (!legacySettled && (!child.WorkerSettlement.matches(s.Run.ID, w) || child.WorkerSettlement.CleanupStatus != "completed" || w.SettledVisit == nil || *w.SettledVisit != w.Visit)) {
			return fmt.Errorf("descendant worker %s has no committed current settlement", w.ID)
		}
		if err := a.workerDescendantsSettledLocked(child, visiting); err != nil {
			return err
		}
	}
	return nil
}
