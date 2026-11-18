package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type reportEvidenceProvider struct{}

func (reportEvidenceProvider) Complete(ctx context.Context, request ModelRequest) (ModelResponse, error) {
	last := lastConversationMessage(request)
	if last.Role != "tool" {
		return ModelResponse{ToolCalls: []ToolCall{{ID: "bookkeeping", Name: "assets", Arguments: "{}"}}}, nil
	}
	if last.ToolCallID == "bookkeeping" {
		return (resultProvider{}).Complete(ctx, ModelRequest{Messages: []Message{{Role: "user", Content: "observe"}}})
	}
	return (resultProvider{}).Complete(ctx, request)
}

func TestReportEvidenceLinksOnlyExplicitCurrentCitations(t *testing.T) {
	s := Snapshot{
		Run: Run{ID: "run-a"},
		Result: Result{Status: "failed", Summary: "English conclusion", EvidenceIDs: []string{"shared", "shared"}, Answers: []Answer{
			{Kind: "answer", Value: "English answer", EvidenceIDs: []string{"shared", "shared"}},
		}},
		Findings: []Finding{{ID: "finding", Title: "English finding", RootCause: "English root cause", Status: "invalid", EvidenceIDs: []string{"shared"}, References: []EvidenceReference{{ID: "shared"}}, Retests: []FindingRetest{
			{EvidenceIDs: []string{"shared"}, Status: "fail", Reason: "probe did not match"},
			{References: []EvidenceReference{{ID: "retest-only"}}, Status: "unknown"},
		}}},
		Requirements: []VerificationRequirement{{ID: "goal", Description: "English requirement"}},
		Checks: []VerificationCheck{
			{RequirementID: "goal", EvidenceIDs: []string{"old-check"}, Status: "pass"},
			{RequirementID: "removed", EvidenceIDs: []string{"removed-check"}, Status: "pass"},
			{RequirementID: "goal", EvidenceIDs: []string{"shared"}, References: []EvidenceReference{{ID: "shared"}}, Status: "fail", Reason: "missing marker"},
		},
		Plan:   PlanState{Tasks: []PlanTask{{ID: "step", Status: "verified", EvidenceIDs: []string{"raw-success"}}}},
		Assets: []Asset{{ID: "asset", Coverage: "verified", EvidenceIDs: []string{"asset-only"}}},
	}
	e := Evidence{ID: "shared", Verification: "invalid", Content: "raw output"}
	entry := catalogEntry(s, e)
	want := []string{"result/result/failed", "answer/answer:1/unverified", "finding/finding/invalid", "retest/finding:retest:1/fail", "check/goal/fail"}
	var got []string
	for _, link := range entry.ReportLinks {
		got = append(got, link.Kind+"/"+link.ID+"/"+link.Status)
		if link.Title == "" || link.Summary == "" {
			t.Fatalf("missing citation presentation: %+v", link)
		}
	}
	if !reflect.DeepEqual(got, want) || entry.Evidence != e {
		t.Fatalf("citation or integrity status changed: %+v", entry)
	}
	if entry.ReportLinks[0].OriginalSummary != "English conclusion" || entry.ReportLinks[2].OriginalTitle != "English finding" {
		t.Fatal("original report text was lost")
	}
	if links := reportEvidenceLinks(&s, "retest-only"); len(links) != 1 || links[0].Kind != "retest" || links[0].Status != "unknown" {
		t.Fatalf("explicit retest reference lost: %+v", links)
	}
	for _, id := range []string{"old-check", "removed-check", "raw-success", "asset-only", "absent", ""} {
		entry := catalogEntry(s, Evidence{ID: id, Verification: "verified"})
		if entry.ReportLinks == nil || len(entry.ReportLinks) != 0 {
			t.Fatalf("non-report record %q promoted: %+v", id, entry.ReportLinks)
		}
		encoded, err := json.Marshal(entry)
		if err != nil || !strings.Contains(string(encoded), `"report_links":[]`) {
			t.Fatalf("empty citation array missing: %s %v", encoded, err)
		}
	}
}

func TestReportEvidenceLinksRetainedAndRunScoped(t *testing.T) {
	a, store, snap, artifact := catalogFixture(t)
	id := snap.Evidence[0].ID
	other, err := a.Create("same evidence ID, different run")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(other.Run.ID, func(s *Snapshot) error {
		s.Evidence = []Evidence{{ID: id, Content: "unrelated raw record"}}
		s.Plan.Tasks = []PlanTask{{ID: "child", Status: "completed", EvidenceIDs: []string{id}, EvidenceRunID: snap.Run.ID}}
		s.Assets = []Asset{{ID: "asset", EvidenceIDs: []string{id}, SourceRunID: snap.Run.ID}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DeleteWithEvidence(snap.Run.ID, true); err != nil {
		t.Fatal(err)
	}
	entries, err := store.ListEvidence()
	if err != nil || len(entries) != 2 {
		t.Fatalf("raw catalog changed: %+v %v", entries, err)
	}
	for _, entry := range entries {
		if entry.RunID == snap.Run.ID {
			if !entry.RunDeleted || len(entry.ReportLinks) != 2 || entry.ReportLinks[0].Status != "success" {
				t.Fatalf("retained report citations lost: %+v", entry)
			}
		} else if entry.RunID != other.Run.ID || len(entry.ReportLinks) != 0 {
			t.Fatalf("cross-run ID collision promoted an unrelated record: %+v", entry)
		}
		detail, err := store.GetEvidence(entry.RunID, id)
		if err != nil || !reflect.DeepEqual(detail.ReportLinks, entry.ReportLinks) {
			t.Fatalf("list/detail citations differ: %+v %v", detail, err)
		}
	}
	// The same explicitly cited record remains visible after its source breaks;
	// neither its original verified flag nor its old success claim survives.
	if err = os.Remove(filepath.Join(store.root, "artifacts", artifact.Hash)); err != nil {
		t.Fatal(err)
	}
	entry, err := store.GetEvidence(snap.Run.ID, id)
	if err != nil || len(entry.ReportLinks) != 2 || entry.Evidence.Verification == "verified" || entry.ReportLinks[0].Status == "success" || entry.ReportLinks[1].Status != "unverified" {
		t.Fatalf("broken source was hidden or promoted: %+v %v", entry, err)
	}
}

func TestReportEvidenceTextPreservesChineseAndOriginal(t *testing.T) {
	text, original := reportEvidenceText("发现远程服务异常", "发现", 80)
	if text != "发现远程服务异常" || original != "" {
		t.Fatalf("Chinese title changed: %q %q", text, original)
	}
	raw := "请求\n" + strings.Repeat("证", 190)
	text, original = reportEvidenceText(raw, "简介", 180)
	if len([]rune(text)) != 181 || original != raw || strings.Contains(text, "\n") {
		t.Fatal("summary truncation lost original content")
	}
	text, original = reportEvidenceText("English finding title", "发现", 80)
	if text != "发现" || original != "English finding title" {
		t.Fatal("English source replaced instead of retained")
	}
}

func TestReportEvidenceExportExcludesUncitedBookkeeping(t *testing.T) {
	a, store := newTestAgent(t, reportEvidenceProvider{})
	snap, err := a.Run(context.Background(), createTestRun(t, a))
	if err != nil || snap.Result.Status != "success" || len(snap.Evidence) != 2 {
		t.Fatalf("execution fixture: %+v %v", snap.Result, err)
	}
	if snap.Evidence[0].Verification != "verified" || snap.Evidence[1].Verification != "verified" {
		t.Fatal("both bookkeeping and observation must have valid execution sources")
	}
	report, err := store.Report(snap.Run.ID)
	if err != nil || !strings.Contains(report, "# Trace 研究报告") || !strings.Contains(report, "状态：成功（success）") || !strings.Contains(report, "### 证据 "+snap.Evidence[1].ID) || strings.Contains(report, "### 证据 "+snap.Evidence[0].ID+"\n") {
		t.Fatalf("report citations: %s %v", report, err)
	}
	ledger, err := store.EvidenceLedger(snap.Run.ID)
	if err != nil || len(ledger) != 2 {
		t.Fatal("report filtering changed the raw ledger", err)
	}
	_, err = store.Update(snap.Run.ID, func(s *Snapshot) error {
		reportBusinessMessage(t, s).Content = "已完成观察，尚无引用结论。"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err = store.Report(snap.Run.ID)
	if err != nil || !strings.Contains(report, "暂无报告引用证据。") || strings.Contains(report, "### 证据 ") {
		t.Fatalf("uncited successful execution promoted to report evidence: %s %v", report, err)
	}
	ledger, err = store.EvidenceLedger(snap.Run.ID)
	if err != nil || len(ledger) != 2 {
		t.Fatal("empty report removed original records", err)
	}
}

func TestReportEvidenceAnswerMetadataPreservesOldContract(t *testing.T) {
	var legacy Answer
	if err := json.Unmarshal([]byte(`{"kind":"answer","value":"raw","evidence_ids":["ev"],"verified":false}`), &legacy); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(legacy)
	if err != nil || legacy.Title != "" || legacy.Summary != "" || strings.Contains(string(encoded), `"title"`) || strings.Contains(string(encoded), `"summary"`) {
		t.Fatalf("legacy answer shape changed: %s %v", encoded, err)
	}
	_, store, snap, _ := catalogFixture(t)
	original := snap.Evidence[0]
	setAnswer := func(value string) Snapshot {
		t.Helper()
		updated, err := store.Update(snap.Run.ID, func(s *Snapshot) error {
			last := reportBusinessMessage(t, s)
			claim, present := traceResultEnvelope(last.Content)
			if !present || claim == nil || len(claim.Answers) != 1 {
				t.Fatal("fixture business result missing")
			}
			claim.Answers[0].Title = "已观察到目标响应"
			claim.Answers[0].Summary = "执行输出包含已声明的响应标记。"
			claim.Answers[0].Value = value
			body, err := json.Marshal(map[string]any{"trace_result": claim})
			last.Content = fixtureReport(string(body))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return updated
	}
	updated := setAnswer("flag{verified}")
	if updated.Result.Status != "success" || !updated.Result.Answers[0].Verified || updated.Evidence[0] != original {
		t.Fatalf("presentation metadata changed evidence validation: %+v", updated.Result)
	}
	links := reportEvidenceLinks(&updated, original.ID)
	if len(links) != 2 || links[1].Title != "已观察到目标响应" || links[1].Summary != "执行输出包含已声明的响应标记。" {
		t.Fatalf("Chinese answer metadata ignored: %+v", links)
	}
	report, err := store.Report(snap.Run.ID)
	if err != nil || !strings.Contains(report, "### 已观察到目标响应") || !strings.Contains(report, "原始值：flag{verified}") {
		t.Fatalf("report dropped raw answer or title: %s %v", report, err)
	}
	updated = setAnswer("not-in-execution-output")
	if updated.Result.Status == "success" || updated.Result.Answers[0].Verified || updated.Evidence[0] != original {
		t.Fatal("Chinese presentation metadata substituted for the actual answer predicate")
	}
}

func TestReportEvidenceLegacyAnswerUsesReportContext(t *testing.T) {
	s := Snapshot{Result: Result{Summary: "观察到本地响应中的目标标记。", Answers: []Answer{{Kind: "flag", Value: "raw-marker", EvidenceIDs: []string{"ev"}}}}}
	links := reportEvidenceLinks(&s, "ev")
	if len(links) != 1 || links[0].Title != "目标结果标记" || links[0].Summary != s.Result.Summary || links[0].OriginalSummary != "raw-marker" {
		t.Fatalf("legacy context missing: %+v", links)
	}
}
