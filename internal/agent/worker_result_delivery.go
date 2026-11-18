package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// A report-only continuation may expose its business result, but never turns
// an uncertain action or a still-active executor into a settled dependency.
func businessResultReady(s *Snapshot) bool {
	if s == nil || s.Result.Status != "success" || len(pendingToolCalls(s)) > 0 || toolCleanupError(*s) != nil {
		return false
	}
	for _, action := range s.Actions {
		if action.Status == "started" || action.Status == "uncertain" || action.Status == "result_ready" {
			return false
		}
	}
	return true
}

func reportBusinessReady(s *Snapshot) bool {
	return businessResultReady(s) && reportDeliveryPending(s) && s.Run.PauseReason != "child_report_delivery_pending"
}

func taskAwaitingWorkerSettlement(s *Snapshot, task PlanTask) bool {
	if task.Status == "running" || task.Status == "uncertain" {
		return true
	}
	if taskDone(task) {
		for _, w := range s.Workers {
			if w.TaskID == task.ID && w.ID == task.WorkerID && w.RunID == task.ChildRunID && w.Status == "running" {
				return true
			}
		}
	}
	return false
}

// Translation text, report archives and lifecycle telemetry are deliberately
// excluded; all business values, source ledgers and scope remain fixed.
func workerBusinessSourceHash(s *Snapshot) string {
	result := s.Result
	result.Report = nil
	findings := append([]Finding(nil), s.Findings...)
	for i := range findings {
		findings[i].Report = nil
		findings[i].UpdatedAt = time.Time{}
	}
	return valueHash(struct {
		RunID, ParentID, TaskID, Goal, Predicate string
		VerifiedCompletion                       bool
		Scope                                    *EvidenceScope
		Result                                   Result
		Evidence                                 []Evidence
		Actions                                  []Action
		Requirements                             []VerificationRequirement
		Checks                                   []VerificationCheck
		Findings                                 []Finding
	}{s.Run.ID, s.Run.ParentRunID, s.Run.ParentTaskID, s.Run.Goal, s.Run.SuccessContains, s.Run.RequireVerifiedCompletion, executionEvidenceScope(s), result, s.Evidence, s.Actions, s.Requirements, s.Checks, findings})
}

func reportBusinessSettled(parent string, w AgentWorker, child *Snapshot) bool {
	return reportBusinessReady(child) && child.Run.Status == StatusPaused &&
		child.Run.PauseReason == "report_translation_missing" &&
		child.Run.ParentRunID == parent && child.Run.ParentTaskID == w.TaskID &&
		child.WorkerSettlement.matches(parent, w) && child.WorkerSettlement.CleanupStatus == "completed" &&
		child.WorkerSettlement.BusinessSourceHash == workerBusinessSourceHash(child) &&
		!child.WorkerSettlement.DescendantCleanupPending && !child.WorkerSettlement.DescendantSettlementPending &&
		w.Status == "paused" && w.CleanupStatus == "completed" && w.SettledVisit != nil && *w.SettledVisit == w.Visit
}

// A report-only resume retains the prior business completion, never its old
// capacity reservation. Current lifecycle cleanup remains pending until exit.
func reportBusinessContinuing(parent string, w AgentWorker, child *Snapshot) bool {
	if !businessResultReady(child) || w.Status != "running" || w.CleanupStatus != "pending" || w.SettledVisit == nil || w.Visit != *w.SettledVisit+1 || child.Run.ParentRunID != parent || child.Run.ParentTaskID != w.TaskID {
		return false
	}
	if child.Run.Status != StatusCompleted && !reportDeliveryPending(child) || child.Run.Status == StatusCancelled || child.Run.Status == StatusFailed {
		return false
	}
	previous := w
	previous.Visit = *w.SettledVisit
	receipt := child.WorkerSettlement
	if receipt == nil || receipt.CleanupStatus != "completed" || receipt.BusinessSourceHash != workerBusinessSourceHash(child) || receipt.DescendantCleanupPending || receipt.DescendantSettlementPending {
		return false
	}
	// Child receipt and parent settlement are two durable commits. Accept the
	// new cleanup receipt across that gap only when the executor proved it
	// continued this exact settled business source without any business change.
	if receipt.matches(parent, w) {
		return receipt.BusinessContinuationVisit != nil && *receipt.BusinessContinuationVisit == previous.Visit
	}
	return receipt.matches(parent, previous) && receipt.ReportDeliveryPending
}

func (a *Agent) reportWorkerVisit(child *Snapshot) bool {
	if child.Run.ID != a.workerRunID || child.Run.ParentRunID == "" {
		return false
	}
	parent, err := a.store.loadRawLocked(child.Run.ParentRunID)
	if err != nil || isStopped(parent.Run.Status) {
		return false
	}
	task, err := planTask(&parent, child.Run.ParentTaskID)
	if err != nil || !taskDone(*task) || task.ChildRunID != child.Run.ID {
		return false
	}
	for _, w := range parent.Workers {
		if w.ID == task.WorkerID && w.Visit == a.workerVisit && reportBusinessContinuing(parent.Run.ID, w, child) {
			return true
		}
	}
	return false
}

// Called after the child result commit, while its original execution lock is
// held. A parent write failure leaves the result durable for retry/settlement.
func (a *Agent) publishWorkerResult(id string) error {
	if a.workerRunID != id {
		return nil
	}
	child, err := a.store.Load(id)
	if err != nil || child.Run.ParentRunID == "" || child.Run.ParentTaskID == "" {
		return err
	}
	if child.Run.Status != StatusRunning || !reportBusinessReady(&child) {
		return nil
	}
	unchangedReport := errors.New("child business report unchanged")
	_, err = a.store.Update(child.Run.ParentRunID, func(parent *Snapshot) error {
		if isStopped(parent.Run.Status) || parent.Run.Status == StatusFailed || parent.Run.Status == StatusCompleted {
			return nil
		}
		current, err := a.store.loadLocked(id)
		if err != nil {
			return err
		}
		if current.Run.ParentRunID != parent.Run.ID || current.Run.ParentTaskID != child.Run.ParentTaskID || current.Run.Status != StatusRunning || !reportBusinessReady(&current) {
			return nil
		}
		task, err := planTask(parent, current.Run.ParentTaskID)
		if err != nil || task.Status != "running" || task.ChildRunID != id {
			return nil
		}
		if !taskScopeMatches(&current, *task) || !sameTaskRequirements(current.Requirements, task.Requirements) || !taskRequirementsSatisfied(&current, *task) || task.SuccessContains != "" && !evidenceSupports(&current, current.Result.EvidenceIDs, task.SuccessContains) {
			return nil
		}
		if len(a.pendingChildReportDeliveryLocked(&current, map[string]bool{})) > 0 {
			return nil
		}
		for _, w := range parent.Workers {
			if w.ID != task.WorkerID || w.TaskID != task.ID || w.RunID != id || w.Visit != a.workerVisit || w.Status != "running" {
				continue
			}
			report := newChildReport(parent.Run.ID, w, &current, "pending")
			if !appendChildReport(parent, report) {
				// Verified Load already persisted any source revocation. Only
				// this unchanged receipt can skip the callback's full commit.
				return unchangedReport
			}
			addEvent(parent, "worker_result_ready", "子任务核心结果已保存；报告交付与生命周期结算仍待完成", map[string]string{"worker_id": w.ID, "run_id": id, "source_hash": report.SourceHash})
			return appendWorkerMessage(parent, w.ID, "parent", childReportMessage(report))
		}
		return nil
	})
	if err == unchangedReport {
		return nil
	}
	return err
}

// Pending flags are durable scheduling state; current source digests and
// substantive language bodies also reject old or partial document versions.
func reportDeliveryIncomplete(s *Snapshot) bool {
	if reportDeliveryPending(s) || s.Run.PauseReason == "child_report_delivery_pending" {
		return true
	}
	if needsReportDelivery(s) {
		for _, c := range reportCandidates(s) {
			if _, missing := c.bodies(); len(missing) > 0 {
				return true
			}
		}
	}
	return false
}

// Caller owns the Store transaction. Reuse archived report/CAS validation;
// no publication flag or fixed child receipt can waive required file outputs.
func (a *Agent) pendingChildReportDeliveryLocked(s *Snapshot, visiting map[string]bool) []string {
	if visiting[s.Run.ID] {
		return []string{"cyclic child report source: " + s.Run.ID}
	}
	visiting[s.Run.ID] = true
	defer delete(visiting, s.Run.ID)
	var pending []string
	for _, w := range s.Workers {
		task, err := planTask(s, w.TaskID)
		if err != nil || task.ChildRunID != w.RunID || task.WorkerID != w.ID || taskDisposed(*task) {
			continue
		}
		child, err := a.store.loadLocked(w.RunID)
		if err != nil || child.Run.Deleted || child.Run.ParentRunID != s.Run.ID || child.Run.ParentTaskID != w.TaskID {
			pending = append(pending, "child report source unavailable: "+w.ID)
			continue
		}
		if reportDeliveryIncomplete(&child) || !a.reportFilesComplete(&child) {
			pending = append(pending, fmt.Sprintf("worker %s (run %s, visit %d): bilingual file reports pending; use worker_resume for the existing report-only continuation", w.ID, w.RunID, w.Visit))
		}
		pending = append(pending, a.pendingChildReportDeliveryLocked(&child, visiting)...)
	}
	return pending
}

func (a *Agent) reportFilesComplete(s *Snapshot) bool {
	if !needsReportDelivery(s) {
		return true
	}
	for _, c := range reportCandidates(s) {
		body, missing := c.bodies()
		if len(missing) > 0 {
			return false
		}
		found := false
		for _, report := range s.Reports {
			if report.Kind != c.source.Kind || report.SubjectID != c.source.SubjectID || report.SourceDigest != c.digest() || report.DocumentDigest != valueHash(body) || report.TranslationStatus != "complete" {
				continue
			}
			languages := map[string]bool{}
			for _, ref := range report.Files {
				language := ""
				for _, localized := range []struct{ language, body string }{{"zh-CN", body.ZhCN}, {"en", body.En}} {
					expected := renderArchivedReport(c, report, localized.language, localized.body)
					if strings.HasSuffix(ref.Name, "."+localized.language+".md") && ref.Hash == promptHash(expected) && ref.Bytes == int64(len(expected)) {
						language = localized.language
					}
				}
				if language == "" {
					continue
				}
				f, err := a.store.OpenArtifact(ref.Hash)
				if err != nil {
					continue
				}
				f.Close()
				languages[language] = true
			}
			found = found || languages["zh-CN"] && languages["en"]
		}
		if !found {
			return false
		}
	}
	return true
}

func (a *Agent) deferForChildReportDelivery(s *Snapshot) (bool, error) {
	pending := a.pendingChildReportDeliveryLocked(s, map[string]bool{})
	if len(pending) == 0 {
		return false, nil
	}
	// Report-only child continuations are scheduled automatically; do not turn
	// a missing file translation into an operator pause.
	s.Run.Status, s.Run.PauseReason = StatusRunning, ""
	message := "Business results are retained; full delivery still requires the current source-bound Chinese and English file reports. Use worker_wait while a child is active; settled report-only continuations resume automatically through the existing worker scheduler. Do not retry completed tasks or repeat target tools to fill report delivery gaps. " + strings.Join(pending, "; ")
	if len(s.Messages) == 0 || s.Messages[len(s.Messages)-1].Content != message {
		s.Messages = append(s.Messages, Message{Role: "user", Content: message, CreatedAt: time.Now().UTC()})
	}
	addEvent(s, "child_report_delivery_pending", "子任务核心结果已保留；自动继续等待中英文件报告", map[string]any{"pending": pending, "next_action": "continue report-only delivery; do not repeat target tools or retry successful tasks"})
	return true, nil
}
