package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestP0ChildReportsMultipleAndBounded(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "one", Goal: "first"}, {ID: "two", Goal: "second"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, task := range []string{"one", "two"} {
		if _, err := a.DispatchTask(context.Background(), id, task); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ChildReports(id)
	if err != nil || len(rows) != 2 || rows[0].SourceRunID == rows[1].SourceRunID {
		t.Fatalf("multi-child %+v %v", rows, err)
	}
	p, err := store.Load(id)
	if err != nil || p.Run.Status != StatusRunning {
		t.Fatalf("parent stopped %v", err)
	}
	bounded, err := store.ChildReportsFrom(p, 1)
	if err != nil || len(bounded) != 1 {
		t.Fatal("bounded projection")
	}
	for _, r := range rows {
		if r.SourceStatus != "available" {
			t.Fatalf("source %+v", r)
		}
		if _, err := store.ReportVersion(r.SourceRunID, r.SourceHash); err != nil {
			t.Fatal(err)
		}
	}
}

func TestP0ExplicitParentInterfaceContext(t *testing.T) {
	a, store, id := contextFixture(t, "known-interface")
	parent, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	task := PlanTask{ID: "interface", Goal: "use known interface", CompletionCriteria: "describe result", ContextEvidenceIDs: []string{parent.Evidence[0].ID}}
	c, err := a.newWorkerContext(&parent, task)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Evidence) != 1 || c.Evidence[0].SourceRunID != id || c.Evidence[0].ContentHash != parent.Evidence[0].ContentHash || c.CompletionCriteria != task.CompletionCriteria {
		t.Fatalf("source %+v", c)
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "unrelated-parent-history") {
		t.Fatal("full history copied")
	}
	if _, err := a.UpdatePlan(id, []PlanTask{task}, "append"); err != nil {
		t.Fatal(err)
	}
	task.ID = "bad"
	task.ContextEvidenceIDs = []string{"not-real"}
	if _, err := a.UpdatePlan(id, []PlanTask{task}, "append"); err == nil {
		t.Fatal("missing source accepted")
	}
}

func TestP0ChildReportSettlementAndSourceChanges(t *testing.T) {
	a, store, id, w := settlementFixture(t)
	if err := settlementOperation(t, a, store, id, w, func(s *Snapshot) {
		s.Run.Status = StatusCompleted
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: "child conclusion without predicate"})
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ChildReports(id)
	if err != nil || len(rows) != 1 {
		t.Fatalf("reports %v %+v", err, rows)
	}
	if rows[0].SourceStatus != "available" || rows[0].SourceHash == "" || !strings.Contains(rows[0].Summary, "child conclusion") {
		t.Fatalf("report %+v", rows[0])
	}
	original := rows[0].SourceHash
	if _, err = store.Update(id, func(s *Snapshot) error { return a.applyWorkerSettlementLocked(s, w) }); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Load(id)
	if len(p.ChildReports) != 1 || len(p.WorkerMessages) != 1 || len(p.Evidence) != 0 || p.Result.Status == "success" {
		t.Fatal("receipt duplicated or child promoted to parent")
	}
	report, err := store.Report(id)
	if err != nil || !strings.Contains(report, "child conclusion") {
		t.Fatalf("parent report %v", err)
	}
	if _, err = store.Update(w.RunID, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: "changed conclusion"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rows, err = store.ChildReports(id)
	if err != nil || rows[0].SourceStatus != "changed" || rows[0].SourceHash != original {
		t.Fatalf("source mutation %v %+v", err, rows)
	}
	if _, err = store.ReportVersion(w.RunID, original); err == nil {
		t.Fatal("changed version exported as original")
	}
	if _, err = store.Update(w.RunID, func(s *Snapshot) error { s.Run.Deleted = true; return nil }); err != nil {
		t.Fatal(err)
	}
	rows, err = store.ChildReports(id)
	if err != nil || rows[0].SourceStatus != "unavailable" {
		t.Fatalf("deleted source %v %+v", err, rows)
	}
}

func TestP0ChildReportVisitIdentity(t *testing.T) {
	a, store, id, w := settlementFixture(t)
	if err := settlementOperation(t, a, store, id, w, func(s *Snapshot) { s.Run.Status = StatusPaused; s.Run.PauseReason = "checkpoint" }); err != nil {
		t.Fatal(err)
	}
	old, _ := store.ChildReports(id)
	if _, err := store.Update(id, func(s *Snapshot) error { s.Workers[0].Visit++; s.Workers[0].SettledVisit = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ChildReports(id)
	if err != nil || rows[0].Current || rows[0].SourceStatus != "historical" || rows[0].SourceHash != old[0].SourceHash {
		t.Fatalf("old visit %+v %v", rows, err)
	}
	if _, err = store.Update(id, func(s *Snapshot) error { return a.applyWorkerSettlementLocked(s, s.Workers[0]) }); err == nil {
		t.Fatal("old receipt accepted for new visit")
	}
}

func TestP0ActivityReceiptsNeverAssertSourceValidity(t *testing.T) {
	parent := Snapshot{Run: Run{ID: "parent"}, Workers: []AgentWorker{{ID: "w", RunID: "child", TaskID: "task", Visit: 2}}, ChildReports: []ChildReport{{ParentRunID: "parent", SourceRunID: "child", WorkerID: "w", TaskID: "task", Visit: 1, SourceStatus: "available"}, {ParentRunID: "parent", SourceRunID: "child", WorkerID: "w", TaskID: "task", Visit: 2, SourceStatus: "available", Findings: []string{"fixed"}}}}
	rows := ChildReportReceipts(parent, 1)
	if len(rows) != 1 || !rows[0].Current || rows[0].SourceStatus != "not_checked" {
		t.Fatalf("activity %+v", rows)
	}
	rows[0].Findings[0] = "mutated"
	if parent.ChildReports[1].Findings[0] != "fixed" || parent.ChildReports[1].SourceStatus != "available" {
		t.Fatal("projection mutated receipt")
	}
}

func TestP0ChildReportSameEvidenceIDDifferentRuns(t *testing.T) {
	a := stageScopeSnapshot(t, nil)
	b := stageScopeSnapshot(t, nil)
	a.Run.ID = "child-one"
	b.Run.ID = "child-two"
	b.Evidence[0].ID = a.Evidence[0].ID
	if childReportHash(&a) == childReportHash(&b) {
		t.Fatal("cross-run bare ID collision")
	}
	parent := Snapshot{}
	appendChildReport(&parent, ChildReport{SourceRunID: a.Run.ID, WorkerID: "a", TaskID: "a", Visit: 1, SourceHash: childReportHash(&a)})
	appendChildReport(&parent, ChildReport{SourceRunID: b.Run.ID, WorkerID: "b", TaskID: "b", Visit: 1, SourceHash: childReportHash(&b)})
	if len(parent.ChildReports) != 2 || len(parent.Evidence) != 0 {
		t.Fatal("reports collapsed or promoted evidence")
	}
}

func TestP0PlanPatchPreservesStartedContract(t *testing.T) {
	a, _ := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	task := PlanTask{ID: "started", Goal: "original", CompletionCriteria: "explain with sources"}
	if _, err := a.SetPlan(id, []PlanTask{task}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, task.ID, "manual", 30); err != nil {
		t.Fatal(err)
	}
	s, err := a.UpdatePlan(id, []PlanTask{{ID: "new", Goal: "next"}}, "append")
	if err != nil || len(s.Plan.Tasks) != 2 || !s.Plan.Tasks[0].Started {
		t.Fatalf("append %v", err)
	}
	s, err = a.UpdatePlan(id, []PlanTask{{ID: "new", Goal: "better next"}}, "patch")
	if err != nil || s.Plan.Tasks[0].CompletionCriteria != task.CompletionCriteria {
		t.Fatalf("patch %v", err)
	}
	task.CompletionCriteria = "weaker"
	if _, err = a.UpdatePlan(id, []PlanTask{task}, "patch"); err == nil {
		t.Fatal("started contract weakened")
	}
	if _, err = a.SetPlan(id, []PlanTask{{ID: "new", Goal: "next"}}); err == nil {
		t.Fatal("replace removed started task")
	}
}

func TestP0NaturalRootAndPendingStages(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	s, err := a.CreateConfigured("research", RunConfiguration{CompletionCriteria: "explain causes and unresolved questions"})
	if err != nil {
		t.Fatal(err)
	}
	if s.RootContract == nil || s.RootContract.CompletionCriteria == "" || len(s.Requirements) != 0 || s.Run.SuccessContains != "" {
		t.Fatal("natural criteria became fake machine predicate")
	}
	tasks := []PlanTask{{ID: "failed-path", Goal: "first method", Stage: true}}
	for i := 0; i < 51; i++ {
		tasks = append(tasks, PlanTask{ID: fmt.Sprint("pending-", i), Goal: "required remaining work", Stage: true})
	}
	if _, err = a.SetPlan(s.Run.ID, tasks); err != nil {
		t.Fatal(err)
	}
	s, err = store.Update(s.Run.ID, func(s *Snapshot) error {
		s.Plan.Tasks[0].Status = "abandoned"
		s.Run.Status = StatusCompleted
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if planDispositionFailure(&s) != "" || !continueForEvidence(&s) || s.Run.Status == StatusCompleted || s.Run.Status == StatusFailed {
		t.Fatal("failed stage or empty claim prematurely terminated root")
	}
	if _, err = store.Update(s.Run.ID, func(s *Snapshot) error { s.Requests = []ModelRequest{{ID: "started"}}; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetRootContract(s.Run.ID, RootContract{Version: 2, Goal: "changed"}); err == nil {
		t.Fatal("late root mutation accepted")
	}
}

func TestP0NaturalCheckedCompletionVersusYield(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	s := stageScopeSnapshot(t, nil)
	s.Run.ID = id
	s.Run.ParentRunID = "parent"
	s.Run.ParentTaskID = "task"
	s.Run.Status = StatusRunning
	s.Run.SuccessContains = "true"
	if _, err := store.Update(id, func(dst *Snapshot) error { *dst = s; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.YieldWorker(id, "done", "all done"); err == nil {
		t.Fatal("successful predicate yielded incomplete")
	}
	s.Run.Status = StatusCompleted
	s.Messages = append(s.Messages, Message{Role: "assistant", Content: "Observed successful result; finished."})
	refreshResult(&s)
	if s.Result.Status != "success" {
		t.Fatalf("natural verified completion %+v", s.Result)
	}
	s.Run.SuccessContains = "missing"
	s.Run.Status = StatusRunning
	if _, err := store.Update(id, func(dst *Snapshot) error { *dst = s; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.YieldWorker(id, "blocked", "inspect unresolved observation"); err != nil {
		t.Fatal(err)
	}
	s, _ = store.Load(id)
	if s.Run.Status != StatusPaused || s.Result.Status == "success" {
		t.Fatal("unfinished yield incorrectly completed")
	}
}

func TestP0PassedChecksFinishWithoutJSON(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	s := stageScopeSnapshot(t, nil)
	s.Run.ID = id
	s.Run.Status = StatusRunning
	s.Run.ParentRunID = "parent"
	s.Run.ParentTaskID = "task"
	s.Requirements = []VerificationRequirement{{ID: "observed", Description: "observed accepted output", Category: "goal", Predicate: EvidencePredicate{Kind: "json_equals", Pointer: "/ok", Expected: true}}}
	if _, err := store.Update(id, func(dst *Snapshot) error { *dst = s; return nil }); err != nil {
		t.Fatal(err)
	}
	checked, err := a.CheckEvidence(context.Background(), id, "observed", []string{s.Evidence[0].ID})
	if err != nil || len(unmetRequirements(&checked)) != 0 {
		t.Fatalf("check %v %+v", err, checked.Checks)
	}
	if _, err = a.YieldWorker(id, "done", "done"); err == nil {
		t.Fatal("passed checks incorrectly yielded")
	}
	checked.Run.Status = StatusCompleted
	checked.Messages = append(checked.Messages, Message{Role: "assistant", Content: "The requested observation passed its check."})
	refreshResult(&checked)
	if checked.Result.Status != "success" || len(checked.Result.EvidenceIDs) != 1 {
		t.Fatalf("plain checked final %+v", checked.Result)
	}
	checked.Run.Status = StatusPaused
	refreshResult(&checked)
	if checked.Result.Status == "success" {
		t.Fatal("pause promoted to success")
	}
}
