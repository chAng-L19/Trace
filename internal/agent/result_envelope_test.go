package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestTraceResultClaimAcceptsOnlyOneExplicitEnvelope(t *testing.T) {
	body := `{"trace_result":{"status":"incomplete","summary":"观察结果","evidence_ids":["ev-1"]}}`
	fence := "```json\n" + body + "\n```"
	for _, tt := range []struct {
		name, text string
		accepted   bool
	}{
		{"raw", body, true},
		{"fence", fence, true},
		{"report_then_fence", "# 研究报告\n\n已有观察和复现步骤。\n\n" + fence, true},
		{"fence_then_report", fence + "\n\n以上为本次观察。", true},
		{"windows_newlines", strings.ReplaceAll("报告\n"+fence, "\n", "\r\n"), true},
		{"ordinary_json_then_result", "```json\n{\"status\":\"success\"}\n```\n" + fence, true},
		{"no_envelope", "```json\n{\"status\":\"success\"}\n```", false},
		{"nested_envelope", "```json\n{\"example\":" + body + "}\n```", false},
		{"array", "```json\n[" + body + "]\n```", false},
		{"unlabelled_code", "```\n" + body + "\n```", false},
		{"javascript_code", "```javascript\n" + body + "\n```", false},
		{"markdown_example", "````markdown\n" + fence + "\n````", false},
		{"tilde_markdown_example", "~~~markdown\n" + fence + "\n~~~", false},
		{"unclosed", "报告\n```json\n" + body, false},
		{"second_unclosed", fence + "\n```json\n" + body, false},
		{"inline_json", "报告引用 " + body, false},
		{"two_results", fence + "\n" + fence, false},
		{"conflicting_results", fence + "\n```json\n{\"trace_result\":{\"status\":\"failed\"}}\n```", false},
		{"null_then_result", "```json\n{\"trace_result\":null}\n```\n" + fence, false},
		{"malformed_then_result", "```json\n{\"trace_result\":\n```\n" + fence, false},
		{"duplicate_envelope_key", "```json\n{\"trace_result\":{},\"trace_result\":{}}\n```", false},
		{"multiple_objects_in_fence", "```json\n" + body + "\n" + body + "\n```", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := traceResultClaim(tt.text)
			if (result != nil) != tt.accepted {
				t.Fatalf("claim = %+v; accepted = %t", result, tt.accepted)
			}
			if tt.accepted && (result.Summary != "观察结果" || !reflect.DeepEqual(result.EvidenceIDs, []string{"ev-1"})) {
				t.Fatalf("parsed a different result: %+v", result)
			}
		})
	}
}

func TestReportWithFencedResultRetainsSourceAndAnswerValidation(t *testing.T) {
	_, store, snap, _ := catalogFixture(t)
	original := snap.Evidence[0]
	base := snap.Result
	for _, tt := range []struct {
		name string
		edit func(*Result)
		want string
	}{
		{"referenced_observation", func(*Result) {}, "success"},
		{"unknown_evidence", func(r *Result) { r.EvidenceIDs = []string{"ev-missing"} }, "incomplete"},
		{"unsupported_answer", func(r *Result) { r.Answers[0].Value = "not-observed" }, "incomplete"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			claim := base
			claim.Answers = append([]Answer(nil), base.Answers...)
			tt.edit(&claim)
			encoded, err := json.Marshal(map[string]Result{"trace_result": claim})
			if err != nil {
				t.Fatal(err)
			}
			updated, err := store.Update(snap.Run.ID, func(s *Snapshot) error {
				reportBusinessMessage(t, s).Content = "# 研究报告\n\n以下结论附带精确证据引用。\n\n```json\n" + string(encoded) + "\n```"
				return nil
			})
			if err != nil || updated.Result.Status != tt.want || updated.Evidence[0] != original {
				t.Fatalf("envelope changed verification: %+v %v", updated.Result, err)
			}
			loaded, err := store.Load(snap.Run.ID)
			if err != nil || !reflect.DeepEqual(loaded.Result, updated.Result) {
				t.Fatalf("historical reload disagrees: %+v %v", loaded.Result, err)
			}
			entry, err := store.GetEvidence(snap.Run.ID, original.ID)
			if err != nil || len(entry.ReportLinks) == 0 {
				t.Fatalf("explicit report citation missing: %+v %v", entry, err)
			}
			if tt.name == "unsupported_answer" && loaded.Result.Answers[0].Verified {
				t.Fatal("fenced answer bypassed output matching")
			}
			if tt.want == "success" {
				report, err := store.Report(snap.Run.ID)
				if err != nil || !strings.Contains(report, "### 证据 "+original.ID) || !strings.Contains(report, "输出匹配：通过") {
					t.Fatalf("report omitted parsed proof: %s %v", report, err)
				}
			}
		})
	}
	encoded, err := json.Marshal(map[string]Result{"trace_result": base})
	if err != nil {
		t.Fatal(err)
	}
	broken, err := store.Update(snap.Run.ID, func(s *Snapshot) error {
		reportBusinessMessage(t, s).Content = "正文\n```json\n" + string(encoded) + "\n```"
		s.Evidence[0].Content += "changed"
		return nil
	})
	if err != nil || broken.Result.Status == "success" || broken.Evidence[0].Verification == "verified" || len(broken.Result.Answers) != 1 || broken.Result.Answers[0].Verified {
		t.Fatalf("fenced result bypassed source integrity: %+v %v", broken.Result, err)
	}
}
