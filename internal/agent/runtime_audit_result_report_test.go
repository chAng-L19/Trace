package agent

import (
	"encoding/json"
	"testing"
)

func TestRuntimeAuditResultReportIsNotBusinessClaim(t *testing.T) {
	claim := Result{Status: "success", Summary: "Reviewed the section.", EvidenceIDs: []string{"section"}, Answers: []Answer{{Value: "reviewed", EvidenceIDs: []string{"section"}}}}
	envelope := func(fields map[string]any) string {
		t.Helper()
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	example := "Report format example:\n```json\n" + envelope(map[string]any{"trace_result": claim}) + "\n```"
	for _, tc := range []struct {
		name    string
		report  reportSubmission
		summary string
	}{
		{"quoted claim", reportSubmission{ReportText: ReportText{En: example}}, example},
		{"document only after earlier success", reportSubmission{Documents: []reportTranslation{{Kind: "stage", SubjectID: "checkpoint"}}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Snapshot{Run: Run{ID: "draft", ParentRunID: "parent", ParentTaskID: "review"}, WorkerContext: &WorkerContext{RunID: "draft", ParentRunID: "parent", TaskID: "review"}}
			resultValidationSource(t, &s, "section", "read", "builtin", "section reviewed")
			resultValidationClaim(t, &s, claim)
			s.Messages = append(s.Messages, Message{Role: "user", Content: "What remains in the next section?"}, Message{Role: "assistant", Content: envelope(map[string]any{"trace_report": tc.report})})
			refreshResult(&s)
			if s.Result.Status != "incomplete" || s.Result.Summary != tc.summary || len(s.Result.EvidenceIDs) != 0 || len(s.Result.Answers) != 0 {
				t.Fatalf("report supplied or revived a business claim: %+v", s.Result)
			}
			if !continueForEvidence(&s) || !outcomeHandoffPending(&s) {
				t.Fatal("missing current business claim must use the existing outcome handoff")
			}

			// The same observation and claim are valid as a sibling of trace_report.
			s.Run.Status = StatusCompleted
			s.Messages = append(s.Messages, Message{Role: "assistant", Content: envelope(map[string]any{"trace_report": tc.report, "trace_result": claim})})
			if continueForEvidence(&s) || s.Result.Status != "success" || len(s.Result.Answers) != 1 || !s.Result.Answers[0].Verified || len(s.Actions) != 1 {
				t.Fatalf("explicit source-bound handoff did not settle: %+v", s.Result)
			}
		})
	}
}

func TestRuntimeAuditResultReportFencePreservesBusinessSource(t *testing.T) {
	s := Snapshot{Run: Run{ID: "draft", ParentRunID: "parent", ParentTaskID: "review"}}
	resultValidationSource(t, &s, "section", "read", "builtin", "section reviewed")
	resultValidationClaim(t, &s, Result{Status: "success", Summary: "Reviewed the section.", EvidenceIDs: []string{"section"}, Answers: []Answer{{Value: "reviewed", EvidenceIDs: []string{"section"}}}})
	if s.Result.Status != "success" || len(s.Result.Answers) != 1 || !s.Result.Answers[0].Verified {
		t.Fatalf("bare result is not a valid baseline: %+v", s.Result)
	}
	final := func() reportCandidate {
		t.Helper()
		cs := reportCandidates(&s)
		if len(cs) != 1 || cs[0].source.Kind != "final" {
			t.Fatalf("expected one final archive source, got %d candidates", len(cs))
		}
		return cs[0]
	}
	businessHash, digest := workerBusinessSourceHash(&s), final().digest()
	reportFence := func(body ReportText) string {
		t.Helper()
		raw, err := json.Marshal(map[string]ReportText{"trace_report": body})
		if err != nil {
			t.Fatal(err)
		}
		return "```json\n" + string(raw) + "\n```"
	}
	check := func(stage string, body ReportText) {
		t.Helper()
		refreshResult(&s)
		if s.Result.Status != "success" || workerBusinessSourceHash(&s) != businessHash || final().digest() != digest {
			t.Fatalf("%s changed the fixed business or final archive source: %+v", stage, s.Result)
		}
		if s.Result.Report == nil || *s.Result.Report != body {
			t.Fatalf("%s lost report body or digest: %+v", stage, s.Result.Report)
		}
	}
	business := &s.Messages[len(s.Messages)-1]
	bare := business.Content
	original := ReportText{ZhCN: u7Text.ZhCN}
	fence := reportFence(original)
	business.Content = fence + "\n" + bare
	check("report then bare claim", original)
	business.Content = bare + "\n" + fence
	check("bare claim then report", original)

	translated := u7Text
	translated.SourceDigest = digest
	s.ReportDelivery = &ReportDeliveryState{Pending: true}
	s.Run.Status = StatusRunning
	s.Messages = append(s.Messages, Message{Role: "assistant", ReportOnly: true, Content: reportFence(translated)})
	check("source-bound translation", translated)
	if body, missing := final().bodies(); len(missing) != 0 || body != u7Text {
		t.Fatalf("fixed source rejected its translation: body=%+v missing=%v", body, missing)
	}
}
