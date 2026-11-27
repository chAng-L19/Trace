package agent

import (
	"encoding/json"
	"testing"
	"time"
)

func TestWorkerTimingsVisitSpans(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	worker := AgentWorker{ID: "w", RunID: "child", TaskID: "work", Visit: 2, StartedAt: at(0)}
	report := ChildReport{ParentRunID: "parent", SourceRunID: "child", WorkerID: "w", TaskID: "work", Visit: 2, Current: true, SourceStatus: "available"}
	fields := map[string]string{"worker_id": "w", "run_id": "child", "task_id": "work", "visit": "2"}
	parent := Snapshot{Run: Run{ID: "parent"}, Events: []Event{
		{Type: "worker_resumed", Data: worker, CreatedAt: at(10)},
		{Type: "worker_result_ready", Data: map[string]string{"worker_id": "w", "run_id": "child", "task_id": "work", "visit": "1"}, CreatedAt: at(11)},
		{Type: "worker_result_ready", Data: fields, CreatedAt: at(20)},
		{Type: "worker_finished", Data: fields, CreatedAt: at(90)},
		{Type: "task_claimed", Data: PlanTask{ID: "unrelated"}, CreatedAt: at(91)},
		{Type: "task_claimed", Data: PlanTask{ID: "next", Dependencies: []string{"work"}}, CreatedAt: at(120)},
	}}
	child := Snapshot{Run: Run{ID: "child", ParentRunID: "parent", ParentTaskID: "work", Status: StatusCompleted},
		Result:           Result{Status: "incomplete", Summary: "current business result", Report: &u7Text},
		Actions:          []Action{{Adapter: "exec", Status: "completed", StartedAt: at(12), FinishedAt: at(15)}, {Adapter: "exec", Status: "uncertain", StartedAt: at(16), FinishedAt: at(18)}},
		Requests:         []ModelRequest{{Purpose: "report_delivery", CreatedAt: at(2)}, {Purpose: "report_delivery", CreatedAt: at(25)}, {Purpose: "report_delivery", CreatedAt: at(40)}},
		Reports:          []ReportArchive{{Kind: "final", SubjectID: "result", TranslationStatus: "translation_missing", CreatedAt: at(30)}, {Kind: "finding", TranslationStatus: "complete", CreatedAt: at(31)}, {Kind: "final", SubjectID: "result", TranslationStatus: "complete", CreatedAt: at(75)}},
		WorkerSettlement: &WorkerSettlement{ParentRunID: "parent", WorkerID: "w", RunID: "child", TaskID: "work", Visit: 2, ObservedAt: at(80)},
	}
	candidate := deliveryReportCandidates(&child)[0]
	body, _ := candidate.bodies()
	child.Reports[2].ID = "current-report"
	child.Reports[2].FormatVersion = candidate.format
	child.Reports[2].SourceDigest = candidate.digest()
	child.Reports[2].DocumentDigest = valueHash(body)
	for i := range child.Requests {
		documents, err := json.Marshal([]reportDeliveryDocument{{Kind: "final", SubjectID: "result", SourceDigest: candidate.digest()}})
		if err != nil {
			t.Fatal(err)
		}
		child.Requests[i].Messages = []Message{{Role: "user", Content: reportDeliveryInstruction + string(documents)}}
	}
	child.WorkerSettlement.BusinessSourceHash = workerBusinessSourceHash(&child)
	report.ReportPath = "/api/runs/child/export?format=report&report_id=current-report"
	report.SourceHash = childReportHash(&child)
	parent.Events[2].Data = map[string]string{"worker_id": "w", "run_id": "child", "task_id": "work", "visit": "2", "source_hash": child.WorkerSettlement.BusinessSourceHash}
	fields["source_hash"] = report.SourceHash
	v := workerTimings(&child, &parent, &report)
	if v == nil {
		t.Fatal("missing visit timings")
	}
	for _, tc := range []struct {
		span     *WorkerTimingSpan
		from, to int
	}{
		{v.LastObservationToEarlyReceipt, 15, 20}, {v.EarlyReceiptToReportRequest, 20, 25}, {v.ReportRequestToArchive, 25, 75},
		{v.ArchiveToSettlementObserved, 75, 80}, {v.SettlementObservedToParent, 80, 90}, {v.ParentSettlementToDependentClaim, 90, 120},
	} {
		if tc.span == nil || tc.span.From != at(tc.from) || tc.span.To != at(tc.to) || tc.span.Millis != int64(tc.to-tc.from) {
			t.Fatalf("%d..%d: %+v", tc.from, tc.to, tc.span)
		}
	}
	child.WorkerSettlement.Visit = 1
	child.Reports = nil
	child.Actions = nil
	parent.Events[2].Data = map[string]string{"worker_id": "w", "run_id": "child", "task_id": "work", "visit": "2", "source_hash": workerBusinessSourceHash(&child)}
	v = workerTimings(&child, &parent, &report)
	if v == nil || v.EarlyReceiptToReportRequest == nil || v.LastObservationToEarlyReceipt != nil || v.ReportRequestToArchive != nil || v.ArchiveToSettlementObserved != nil || v.SettlementObservedToParent != nil {
		t.Fatalf("invented missing endpoint or borrowed a past visit: %+v", v)
	}
	report.SourceStatus = "changed"
	if workerTimings(&child, &parent, &report) != nil {
		t.Fatal("changed receipt presented as current")
	}
	report.SourceStatus = "available"
	worker.Visit++
	parent.Events = append(parent.Events, Event{Type: "worker_resumed", Data: worker, CreatedAt: at(130)})
	if workerTimings(&child, &parent, &report) != nil {
		t.Fatal("projected historical receipt from new visit")
	}
	if workerTimingSpan(at(5), at(4)) != nil || workerTimingSpan(time.Time{}, at(4)) != nil {
		t.Fatal("unknown interval presented as zero")
	}
	if span := workerTimingSpan(at(5), at(5)); span == nil || span.Millis != 0 {
		t.Fatal("known zero interval omitted")
	}
}

func TestWorkerTimingsSameVisitReportVersions(t *testing.T) {
	store, _ := u7Store(t, "json")
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	at := func(ms int) time.Time { return base.Add(time.Duration(ms) * time.Millisecond) }
	w := AgentWorker{ID: "w", RunID: "child", TaskID: "work", Visit: 1}
	child := Snapshot{
		Run:     Run{ID: "child", ParentRunID: "parent", ParentTaskID: "work", Status: StatusCompleted},
		Result:  Result{Status: "incomplete", Summary: "business A", CleanupStatus: "completed"},
		Actions: []Action{{Adapter: "exec", Status: "completed", StartedAt: at(5), FinishedAt: at(10)}},
	}
	archiveAt := func(ms int) ReportArchive {
		t.Helper()
		before := len(child.Reports)
		if err := store.archiveReports(&child); err != nil {
			t.Fatal(err)
		}
		if len(child.Reports) != before+1 {
			t.Fatalf("expected one actual report version, got %d", len(child.Reports)-before)
		}
		child.Reports[before].CreatedAt = at(ms)
		return child.Reports[before]
	}
	requestAt := func(ms int) ModelRequest {
		t.Helper()
		documents := pendingReportDocuments(&child)
		if len(documents) != 1 || documents[0].Kind != "final" {
			t.Fatalf("missing final delivery input: %+v", documents)
		}
		raw, err := json.Marshal(documents)
		if err != nil {
			t.Fatal(err)
		}
		return ModelRequest{Purpose: "report_delivery", CreatedAt: at(ms), Messages: []Message{{Role: "user", Content: reportDeliveryInstruction + string(raw)}}}
	}
	fields := func(hash string) map[string]string {
		return map[string]string{"worker_id": "w", "run_id": "child", "task_id": "work", "visit": "1", "source_hash": hash}
	}
	archiveAt(12)
	child.Requests = append(child.Requests, requestAt(20))
	hashA := workerBusinessSourceHash(&child)
	textA := u7Text
	child.Result.Report = &textA
	reportA := archiveAt(30)
	child.WorkerSettlement = &WorkerSettlement{ParentRunID: "parent", WorkerID: "w", RunID: "child", TaskID: "work", Visit: 1, BusinessSourceHash: hashA, ObservedAt: at(32)}
	receiptA := newChildReport("parent", w, &child, "completed")
	parent := Snapshot{Run: Run{ID: "parent"}, Events: []Event{
		{Type: "worker_started", Data: w, CreatedAt: at(0)},
		{Type: "worker_result_ready", Data: fields(hashA), CreatedAt: at(15)},
		{Type: "worker_finished", Data: fields(receiptA.SourceHash), CreatedAt: at(35)},
	}}

	child.Result.Summary, child.Result.Report = "business B after new input", nil
	child.Actions = append(child.Actions, Action{Adapter: "exec", Status: "completed", StartedAt: at(38), FinishedAt: at(40)})
	hashB := workerBusinessSourceHash(&child)
	parent.Events = append(parent.Events, Event{Type: "worker_result_ready", Data: fields(hashB), CreatedAt: at(50)})
	earlyB := newChildReport("parent", w, &child, "pending")
	if v := workerTimings(&child, &parent, &earlyB); v == nil || v.LastObservationToEarlyReceipt == nil || v.LastObservationToEarlyReceipt.From != at(40) || v.LastObservationToEarlyReceipt.To != at(50) || v.EarlyReceiptToReportRequest != nil || v.ReportRequestToArchive != nil || v.ArchiveToSettlementObserved != nil || v.SettlementObservedToParent != nil || v.ParentSettlementToDependentClaim != nil {
		t.Fatalf("business B borrowed business A's delivery or settlement: %+v", v)
	}
	child.Requests = append(child.Requests, requestAt(60))
	archiveAt(65)
	textB := u7Text
	textB.ZhCN += " 本次还核对了业务 B 的新输入。"
	textB.En += " This revision also checks the new business B input."
	child.Result.Report = &textB
	reportB := archiveAt(75)
	if reportA.ID == reportB.ID || reportA.SourceDigest == reportB.SourceDigest || reportA.DocumentDigest == reportB.DocumentDigest {
		t.Fatal("fixture did not archive distinct business and document versions")
	}
	child.WorkerSettlement = &WorkerSettlement{ParentRunID: "parent", WorkerID: "w", RunID: "child", TaskID: "work", Visit: 1, BusinessSourceHash: hashB, ObservedAt: at(80)}
	receiptB := newChildReport("parent", w, &child, "completed")
	if receiptA.ReportPath == receiptB.ReportPath {
		t.Fatal("current receipt did not pin business B's report")
	}
	parent.Events = append(parent.Events,
		Event{Type: "worker_finished", Data: fields(receiptB.SourceHash), CreatedAt: at(90)},
		Event{Type: "task_claimed", Data: PlanTask{ID: "next", Dependencies: []string{"work"}}, CreatedAt: at(120)},
	)
	v := workerTimings(&child, &parent, &receiptB)
	if v == nil {
		t.Fatal("missing current business B timings")
	}
	for _, tc := range []struct {
		span     *WorkerTimingSpan
		from, to int
	}{
		{v.LastObservationToEarlyReceipt, 40, 50}, {v.EarlyReceiptToReportRequest, 50, 60}, {v.ReportRequestToArchive, 60, 75},
		{v.ArchiveToSettlementObserved, 75, 80}, {v.SettlementObservedToParent, 80, 90}, {v.ParentSettlementToDependentClaim, 90, 120},
	} {
		if tc.span == nil || tc.span.From != at(tc.from) || tc.span.To != at(tc.to) {
			t.Fatalf("current business B %d..%d: %+v", tc.from, tc.to, tc.span)
		}
	}
	t.Run("historical archive pin", func(t *testing.T) {
		receipt := receiptB
		receipt.ReportPath = receiptA.ReportPath
		v := workerTimings(&child, &parent, &receipt)
		if v == nil || v.EarlyReceiptToReportRequest == nil || v.ReportRequestToArchive != nil || v.ArchiveToSettlementObserved != nil {
			t.Fatalf("historical archive became current: %+v", v)
		}
	})
	t.Run("unbound request", func(t *testing.T) {
		copy := child
		copy.Requests = append([]ModelRequest(nil), child.Requests...)
		copy.Requests[1].Messages = nil
		v := workerTimings(&copy, &parent, &receiptB)
		if v == nil || v.EarlyReceiptToReportRequest != nil || v.ReportRequestToArchive != nil || v.ArchiveToSettlementObserved == nil {
			t.Fatalf("unbound request borrowed a past version: %+v", v)
		}
	})
	t.Run("different document for same source", func(t *testing.T) {
		copy := child
		copy.Reports = append([]ReportArchive(nil), child.Reports...)
		copy.Reports[len(copy.Reports)-1].DocumentDigest = reportA.DocumentDigest
		v := workerTimings(&copy, &parent, &receiptB)
		if v == nil || v.ReportRequestToArchive != nil || v.ArchiveToSettlementObserved != nil {
			t.Fatalf("different prose became the receipt archive: %+v", v)
		}
	})
	t.Run("unbound early event", func(t *testing.T) {
		copy := parent
		copy.Events = append([]Event(nil), parent.Events...)
		copy.Events[3].Data = fields("")
		v := workerTimings(&child, &copy, &receiptB)
		if v == nil || v.LastObservationToEarlyReceipt != nil || v.EarlyReceiptToReportRequest != nil || v.ReportRequestToArchive == nil {
			t.Fatalf("unbound early event borrowed a past version: %+v", v)
		}
	})
	t.Run("past settlement in same visit", func(t *testing.T) {
		copy := child
		settlement := *child.WorkerSettlement
		settlement.BusinessSourceHash = hashA
		copy.WorkerSettlement = &settlement
		v := workerTimings(&copy, &parent, &receiptB)
		if v == nil || v.ArchiveToSettlementObserved != nil || v.SettlementObservedToParent != nil {
			t.Fatalf("past settlement became current: %+v", v)
		}
	})
}

func TestWorkerTimingsFixedReceiptsOmitLiveProjection(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	live := &WorkerTimings{EarlyReceiptToReportRequest: workerTimingSpan(at, at)}
	parent := Snapshot{Run: Run{ID: "parent"}, ChildReports: []ChildReport{{ParentRunID: "parent", SourceHash: "fixed", Timings: live}}}
	fixed := ChildReportReceipts(parent, 0)
	if len(fixed) != 1 || fixed[0].Timings != nil || fixed[0].SourceHash != "fixed" || parent.ChildReports[0].Timings != live {
		t.Fatal("fixed projection must strip timings from its detached copy only")
	}
	raw, err := json.Marshal(fixed[0])
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["timings"]; ok {
		t.Fatal("fixed receipt JSON contains the live timings field")
	}
}
