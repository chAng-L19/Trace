package agent

import (
	"fmt"
	"strings"
	"testing"
)

func TestResultStatusRejectsInvalidEnumAlongsideReferenceErrors(t *testing.T) {
	for _, status := range []string{"completed", "running", "cancelled", "unknown", "Success", " success ", ""} {
		for _, badReference := range []bool{false, true} {
			t.Run(fmt.Sprintf("%q/missing-reference=%t", status, badReference), func(t *testing.T) {
				s := Snapshot{Run: Run{ID: "result-status", SuccessContains: "observed-proof"}}
				resultValidationSource(t, &s, "proof", "exec", "exec", "observed-proof")
				claim := Result{Status: status, EvidenceIDs: []string{"proof"}, Answers: []Answer{{Value: "observed-proof", EvidenceIDs: []string{"proof"}}}}
				if badReference {
					claim.EvidenceIDs = append(claim.EvidenceIDs, "missing")
				}
				resultValidationClaim(t, &s, claim)
				issue := resultValidationIssue(t, &s, "invalid_result_status", "trace_result.status", "", -1, true)
				if !strings.Contains(issue.Repair, fmt.Sprintf("received %q", status)) || !strings.Contains(issue.Repair, "allowed values are success, failed, incomplete") {
					t.Fatalf("missing exact rejected value or allowed enum: %+v", issue)
				}
				if s.Result.Status != "incomplete" || !strings.Contains(s.Result.TerminalReason, "trace_result.status: invalid_result_status") {
					t.Fatalf("invalid enum was hidden or accepted by a passing predicate: %+v", s.Result)
				}
				if badReference {
					resultValidationIssue(t, &s, "evidence_missing", "evidence_ids", "missing", -1, true)
				}
				if len(s.Result.Answers) != 1 || !s.Result.Answers[0].Verified || s.Result.Answers[0].Value != "observed-proof" || len(s.Result.EvidenceIDs) != len(claim.EvidenceIDs) {
					t.Fatalf("status diagnostic discarded existing proof or references: %+v", s.Result)
				}
				if resultFormatRepairOnly(&s) || resultReferenceRepairFingerprint(&s) != "" {
					t.Fatal("invalid enum entered the reference-only repair path")
				}
				final, _ := finalReportContent(&s)
				if saved := traceResultClaim(final); saved == nil || saved.Status != status {
					t.Fatal("raw model claim was rewritten")
				}
			})
		}
	}
}

func TestResultStatusPreservesExplicitIncompleteAndFailed(t *testing.T) {
	for _, test := range []struct {
		status, reason string
	}{
		{"incomplete", "structured result reports incomplete work"},
		{"failed", "structured result reports failure"},
	} {
		t.Run(test.status, func(t *testing.T) {
			s := Snapshot{Run: Run{ID: "explicit-result", SuccessContains: "observed-proof"}}
			resultValidationSource(t, &s, "proof", "exec", "exec", "observed-proof")
			resultValidationClaim(t, &s, Result{Status: test.status, EvidenceIDs: []string{"proof", "missing"}})
			if s.Result.Status != test.status || s.Result.TerminalReason != test.reason {
				t.Fatalf("legal explicit result changed semantics: %+v", s.Result)
			}
			resultValidationIssue(t, &s, "evidence_missing", "evidence_ids", "missing", -1, true)
			for _, issue := range s.Result.ValidationIssues {
				if issue.Code == "invalid_result_status" {
					t.Fatalf("legal enum rejected: %+v", issue)
				}
			}
		})
	}
}
