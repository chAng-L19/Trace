package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ChildReport is a fixed result/settlement receipt, never parent execution evidence.
// Behavior references: Harness fcc0ad462172 continuation-messages.ts and DSH
// 8c32a9dc5d72 reportRelay (MIT); Trace uses its own Store/visit transaction.
type ChildReport struct {
	ParentRunID       string        `json:"parent_run_id"`
	SourceRunID       string        `json:"source_run_id"`
	TaskID            string        `json:"task_id"`
	WorkerID          string        `json:"worker_id"`
	Visit             uint64        `json:"visit"`
	SourceHash        string        `json:"source_hash"`
	RunStatus         Status        `json:"run_status"`
	ResultStatus      string        `json:"result_status"`
	CleanupStatus     string        `json:"cleanup_status"`
	DeliveryPending   bool          `json:"delivery_pending"`
	SettlementPending bool          `json:"settlement_pending,omitempty"`
	Summary           string        `json:"summary"`
	Findings          []string      `json:"findings"`
	Unresolved        []string      `json:"unresolved"`
	AnswerCount       int           `json:"answer_count"`
	Answers           []Answer      `json:"answers,omitempty"`
	EvidenceIDs       []string      `json:"evidence_ids,omitempty"`
	FindingCount      int           `json:"finding_count"`
	ReportPath        string        `json:"report_path"`
	ReportFiles       []ArtifactRef `json:"report_files,omitempty"`
	SourceStatus      string        `json:"source_status"`
	Current           bool          `json:"current"`
	CreatedAt         time.Time     `json:"created_at"`
}

// Hash the fixed body as well as its source ledger. Rendering is pure: every
// displayed field is covered without unrelated Run.UpdatedAt invalidations.
func childReportHash(s *Snapshot) string {
	return valueHash(struct {
		RunID, ParentID, TaskID string
		RunStatus               Status
		PauseReason             string
		CheckpointSummary       string
		Result                  Result
		Findings                []Finding
		Evidence                []Evidence
		Actions                 []Action
		Checks                  []VerificationCheck
		Messages                []Message
		Events                  []Event
		Requirements            []VerificationRequirement
		Settlement              *WorkerSettlement
		Delivery                *ReportDeliveryState
		Report                  string
	}{s.Run.ID, s.Run.ParentRunID, s.Run.ParentTaskID, s.Run.Status, s.Run.PauseReason, childReportCheckpointSummary(s), s.Result, s.Findings, s.Evidence, s.Actions, s.Checks, s.Messages, s.Events, s.Requirements, s.WorkerSettlement, s.ReportDelivery, renderReportSnapshot(s, nil, ChildReportReceipts(*s, 0))})
}

func childReportCheckpointSummary(s *Snapshot) string {
	if s.Result.Summary == "" && s.WorkerCheckpoint != nil {
		return excerpt(s.WorkerCheckpoint.Summary, 1600)
	}
	return ""
}

func newChildReport(parent string, w AgentWorker, child *Snapshot, cleanup string) ChildReport {
	r := ChildReport{ParentRunID: parent, SourceRunID: w.RunID, TaskID: w.TaskID, WorkerID: w.ID, Visit: w.Visit, SourceHash: childReportHash(child), RunStatus: child.Run.Status, ResultStatus: child.Result.Status, CleanupStatus: cleanup, DeliveryPending: reportDeliveryIncomplete(child), SettlementPending: cleanup == "pending", Summary: excerpt(child.Result.Summary, 1600), Findings: []string{}, Unresolved: []string{}, AnswerCount: len(child.Result.Answers), FindingCount: len(child.Findings), SourceStatus: "available", Current: true, CreatedAt: time.Now().UTC()}
	if child.WorkerSettlement.matches(parent, w) && !r.SettlementPending {
		r.CreatedAt = child.WorkerSettlement.ObservedAt
	}
	r.Answers = append([]Answer(nil), child.Result.Answers...)
	r.EvidenceIDs = append([]string(nil), child.Result.EvidenceIDs...)
	r.ReportPath = fmt.Sprintf("/api/runs/%s/export?format=report&source_hash=%s", w.RunID, r.SourceHash)
	if r.SettlementPending {
		// An early receipt pins the business ledger, so report-only telemetry
		// does not invalidate a result while its translation request is running.
		r.SourceHash = workerBusinessSourceHash(child)
		r.ReportPath = fmt.Sprintf("/api/runs/%s", w.RunID)
	}
	if archived := preferredReport(*child); archived != nil {
		if archived.Kind == "final" || archived.Kind == "stage" {
			r.ReportPath = fmt.Sprintf("/api/runs/%s/export?format=report&report_id=%s&language=zh-CN", w.RunID, archived.ID)
			r.ReportFiles = append([]ArtifactRef(nil), archived.Files...)
		}
	}
	if r.Summary == "" {
		r.Summary = childReportCheckpointSummary(child)
	}
	for _, f := range child.Findings {
		if len(r.Findings) == 8 {
			break
		}
		r.Findings = append(r.Findings, excerpt(f.Title+" ["+f.Status+"; source="+f.SourceStatus+"]", 240))
	}
	if child.Result.Status != "success" {
		r.Unresolved = append(r.Unresolved, excerpt(child.Result.TerminalReason, 600))
	}
	if cleanup != "completed" {
		r.Unresolved = append(r.Unresolved, "worker cleanup is "+cleanup)
	}
	if r.DeliveryPending {
		r.Unresolved = append(r.Unresolved, "bilingual file report delivery is pending; resume the existing worker to finish delivery without repeating target execution")
	}
	if child.Run.PauseReason != "" {
		r.Unresolved = append(r.Unresolved, excerpt(child.Run.PauseReason, 400))
	}
	return r
}

func appendChildReport(s *Snapshot, r ChildReport) bool {
	for _, old := range s.ChildReports {
		if old.SourceRunID == r.SourceRunID && old.WorkerID == r.WorkerID && old.TaskID == r.TaskID && old.Visit == r.Visit && old.SourceHash == r.SourceHash {
			return false
		}
	}
	s.ChildReports = append(s.ChildReports, r)
	return true
}

// ChildReports rechecks registered sources without changing fixed receipts.
func (s *Store) ChildReports(id string) ([]ChildReport, error) {
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	parent, err := s.loadRawLocked(id)
	if err != nil {
		return nil, err
	}
	return s.childReportsFromLocked(parent, 0), nil
}

func (s *Store) ChildReportsFrom(parent Snapshot, limit int) ([]ChildReport, error) {
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	return s.childReportsFromLocked(parent, limit), nil
}

// ChildReportReceipts is the activity projection: no source I/O or claims of
// current source integrity. Live reports recheck sources; fixed exports only
// pin these local references, never a claim of their current availability.
func ChildReportReceipts(parent Snapshot, limit int) []ChildReport {
	start := 0
	if limit > 0 && len(parent.ChildReports) > limit {
		start = len(parent.ChildReports) - limit
	}
	out := append([]ChildReport{}, parent.ChildReports[start:]...)
	for i := range out {
		r := &out[i]
		r.Current = false
		r.SourceStatus = "unavailable"
		r.Findings = append([]string{}, r.Findings...)
		r.Unresolved = append([]string{}, r.Unresolved...)
		r.Answers = append([]Answer(nil), r.Answers...)
		for j := range r.Answers {
			r.Answers[j].EvidenceIDs = append([]string(nil), r.Answers[j].EvidenceIDs...)
		}
		r.EvidenceIDs = append([]string(nil), r.EvidenceIDs...)
		if parent.Run.Deleted || r.ParentRunID != parent.Run.ID {
			continue
		}
		for _, w := range parent.Workers {
			if w.ID == r.WorkerID && w.TaskID == r.TaskID && w.RunID == r.SourceRunID {
				r.Current = w.Visit == r.Visit
				r.SourceStatus = "not_checked"
				break
			}
		}
	}
	return out
}

func (s *Store) childReportsFromLocked(parent Snapshot, limit int) []ChildReport {
	id := parent.Run.ID
	out := ChildReportReceipts(parent, limit)
	sources := map[string]*Snapshot{}
	for i := range out {
		r := &out[i]
		r.Current = false
		r.SourceStatus = "unavailable"
		registered := false
		for _, w := range parent.Workers {
			if w.ID == r.WorkerID && w.RunID == r.SourceRunID && w.TaskID == r.TaskID {
				registered = true
				r.Current = w.Visit == r.Visit
			}
		}
		if !registered || parent.Run.Deleted || r.ParentRunID != id {
			continue
		}
		child, loaded := sources[r.SourceRunID]
		if !loaded {
			v, e := s.verifySourceLocked(r.SourceRunID, map[string]bool{id: true}, readSourceOnly)
			if e == nil {
				child = &v
			}
			sources[r.SourceRunID] = child
		}
		if child == nil || child.Run.Deleted || child.Run.ParentRunID != id || child.Run.ParentTaskID != r.TaskID {
			continue
		}
		if !r.Current {
			r.SourceStatus = "historical"
			continue
		}
		r.SourceStatus = "changed"
		if !r.SettlementPending && childReportHash(child) == r.SourceHash || r.SettlementPending && businessResultReady(child) && workerBusinessSourceHash(child) == r.SourceHash {
			r.SourceStatus = "available"
		}
	}
	return out
}

func childReportMessage(r ChildReport) string {
	raw, _ := json.Marshal(r)
	if r.SettlementPending {
		return "Child result receipt (delivery and lifecycle settlement pending; source data, not parent execution proof): " + string(raw)
	}
	return "Child settlement report (source data, not parent execution proof): " + string(raw)
}

func writeChildReports(b *strings.Builder, reports []ChildReport) {
	if len(reports) == 0 {
		return
	}
	b.WriteString("\n## 子任务报告\n\n固定结算摘要仅引用原始子运行，不代表父运行重新执行或已通过验收。\n")
	for _, r := range reports {
		fmt.Fprintf(b, "\n### %s / visit %d\n\n%s\n\n来源：%s；Worker：%s；SHA256：%s；来源状态：%s；当前 visit：%t\n\n执行：%s；结果：%s；清理：%s；结论：%d；发现：%d\n\n报告：%s\n", r.TaskID, r.Visit, r.Summary, r.SourceRunID, r.WorkerID, r.SourceHash, r.SourceStatus, r.Current, r.RunStatus, r.ResultStatus, r.CleanupStatus, r.AnswerCount, r.FindingCount, r.ReportPath)
		for _, v := range r.Findings {
			fmt.Fprintf(b, "\n- 发现：%s\n", v)
		}
		for _, v := range r.Unresolved {
			fmt.Fprintf(b, "\n- 未决：%s\n", v)
		}
	}
}
