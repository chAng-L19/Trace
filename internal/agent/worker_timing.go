package agent

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// WorkerTimingSpan is elapsed wall time between two ledger timestamps, not
// service time or proof that the earlier event blocked the later one.
type WorkerTimingSpan struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Millis int64     `json:"millis"`
}

type WorkerTimings struct {
	LastObservationToEarlyReceipt    *WorkerTimingSpan `json:"last_observation_to_early_receipt,omitempty"`
	EarlyReceiptToReportRequest      *WorkerTimingSpan `json:"early_receipt_to_report_request,omitempty"`
	ReportRequestToArchive           *WorkerTimingSpan `json:"report_request_to_archive,omitempty"`
	ArchiveToSettlementObserved      *WorkerTimingSpan `json:"archive_to_settlement_observed,omitempty"`
	SettlementObservedToParent       *WorkerTimingSpan `json:"settlement_observed_to_parent,omitempty"`
	ParentSettlementToDependentClaim *WorkerTimingSpan `json:"parent_settlement_to_dependent_claim,omitempty"`
}

func workerTimingSpan(from, to time.Time) *WorkerTimingSpan {
	if from.IsZero() || to.IsZero() || to.Before(from) {
		return nil
	}
	return &WorkerTimingSpan{From: from, To: to, Millis: to.Sub(from).Milliseconds()}
}

// Called on a revalidated, current ChildReport in the read projection only.
// An action finish, a report and even a local cleanup receipt say nothing
// about the time an external resource was released.
func workerTimings(child, parent *Snapshot, r *ChildReport) *WorkerTimings {
	if child == nil || parent == nil || r == nil || !r.Current || r.SourceStatus != "available" || child.Run.Deleted || parent.Run.Deleted || child.Run.ParentRunID != parent.Run.ID || r.ParentRunID != parent.Run.ID || child.Run.ID != r.SourceRunID || child.Run.ParentTaskID != r.TaskID {
		return nil
	}
	var start, early, finish, dependent time.Time
	var w AgentWorker
	visit := strconv.FormatUint(r.Visit, 10)
	businessHash := workerBusinessSourceHash(child)
	for _, e := range parent.Events {
		switch e.Type {
		case "worker_started", "worker_resumed":
			var registered AgentWorker
			if decodeTimingEvent(e, &registered) && registered.ID == r.WorkerID && registered.RunID == r.SourceRunID && registered.TaskID == r.TaskID {
				if registered.Visit > r.Visit { // Newer visit owns the child snapshot.
					return nil
				}
				if registered.Visit == r.Visit {
					w, start = registered, e.CreatedAt
				}
			}
		case "worker_result_ready", "worker_finished":
			var fields map[string]string
			if start.IsZero() || !decodeTimingEvent(e, &fields) || fields["visit"] != visit || fields["run_id"] != r.SourceRunID || fields["worker_id"] != r.WorkerID || fields["task_id"] != r.TaskID || e.CreatedAt.Before(start) {
				continue
			}
			if e.Type == "worker_result_ready" && fields["source_hash"] == businessHash && early.IsZero() {
				early = e.CreatedAt
			}
			if e.Type == "worker_finished" && !r.SettlementPending && fields["source_hash"] == r.SourceHash && r.SourceHash != "" && finish.IsZero() {
				finish = e.CreatedAt
			}
		case "task_claimed":
			if finish.IsZero() || !dependent.IsZero() {
				continue
			}
			var task PlanTask
			if decodeTimingEvent(e, &task) && task.ID != "" && task.ID != r.TaskID && hasString(task.Dependencies, r.TaskID) {
				dependent = e.CreatedAt
			}
		}
	}
	if start.IsZero() {
		return nil
	}
	inVisit := func(t time.Time) bool { return !t.Before(start) && (finish.IsZero() || !t.After(finish)) }
	var action, request, archive, observed time.Time
	for _, a := range child.Actions {
		if a.Status == "completed" && observationAction(a) && inVisit(a.StartedAt) && inVisit(a.FinishedAt) && !a.FinishedAt.Before(a.StartedAt) && !a.FinishedAt.After(early) && a.FinishedAt.After(action) {
			action = a.FinishedAt
		}
	}
	var sourceDigest string
	for _, candidate := range deliveryReportCandidates(child) {
		if candidate.source.Kind != "final" || candidate.source.SubjectID != "result" {
			continue
		}
		sourceDigest = candidate.digest()
		path, err := url.Parse(r.ReportPath)
		if err != nil || path.Query().Get("report_id") == "" {
			break
		}
		for _, report := range child.Reports {
			if report.ID == path.Query().Get("report_id") && candidate.matchesDocument(report) && report.TranslationStatus == "complete" && inVisit(report.CreatedAt) {
				archive = report.CreatedAt
				break
			}
		}
		break
	}
	for _, q := range child.Requests {
		if q.Purpose == "report_delivery" && sourceDigest != "" && inVisit(q.CreatedAt) && workerTimingRequestMatches(q, sourceDigest) && (request.IsZero() || q.CreatedAt.Before(request)) {
			request = q.CreatedAt
		}
	}
	if s := child.WorkerSettlement; s.matches(parent.Run.ID, w) && s.BusinessSourceHash == businessHash && inVisit(s.ObservedAt) {
		observed = s.ObservedAt
	}
	out := WorkerTimings{
		LastObservationToEarlyReceipt: workerTimingSpan(action, early), EarlyReceiptToReportRequest: workerTimingSpan(early, request),
		ReportRequestToArchive: workerTimingSpan(request, archive), ArchiveToSettlementObserved: workerTimingSpan(archive, observed),
		SettlementObservedToParent: workerTimingSpan(observed, finish), ParentSettlementToDependentClaim: workerTimingSpan(finish, dependent),
	}
	if out == (WorkerTimings{}) {
		return nil
	}
	return &out
}

func decodeTimingEvent(e Event, dst any) bool {
	data, err := json.Marshal(e.Data)
	return err == nil && json.Unmarshal(data, dst) == nil
}

func workerTimingRequestMatches(q ModelRequest, sourceDigest string) bool {
	for i := len(q.Messages) - 1; i >= 0; i-- {
		m := q.Messages[i]
		if m.Role != "user" || !strings.HasPrefix(m.Content, reportDeliveryInstruction) {
			continue
		}
		var documents []reportDeliveryDocument
		if json.Unmarshal([]byte(strings.TrimPrefix(m.Content, reportDeliveryInstruction)), &documents) != nil {
			return false
		}
		for _, document := range documents {
			if document.Kind == "final" && document.SubjectID == "result" && document.SourceDigest == sourceDigest {
				return true
			}
		}
		return false
	}
	return false
}
