package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Reconstruct archived request/action/event/message/CAS provenance, without a
// fresh target request. Runtime execution itself is covered by result_test.go.
func resultValidationSource(t *testing.T, s *Snapshot, id, tool, adapter, stdout string, refs ...ArtifactRef) {
	t.Helper()
	requestID, callID := "request-"+id, "call-"+id
	call := ToolCall{ID: callID, Name: tool, Arguments: `{}`}
	r := ToolResult{Stdout: stdout, EvidenceID: id, Artifacts: refs}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	scope := executionEvidenceScope(s)
	s.Requests = append(s.Requests, ModelRequest{ID: requestID})
	s.Responses = append(s.Responses, ResponseRecord{RequestID: requestID, Response: ModelResponse{ToolCalls: []ToolCall{call}}})
	s.Actions = append(s.Actions, Action{RequestID: requestID, CallID: callID, Name: tool, Adapter: adapter, Arguments: call.Arguments, Status: "completed", Result: &r})
	s.Messages = append(s.Messages, Message{Role: "tool", ToolCallID: callID, Content: string(raw)})
	fields := map[string]string{"call_id": callID, "evidence_id": id}
	if scope != nil {
		fields["evidence_scope_hash"] = valueHash(scope)
	}
	addEvent(s, "tool_result", "archived result", fields)
	s.Evidence = append(s.Evidence, Evidence{ID: id, RequestID: requestID, CallID: callID, SourceEventSequence: s.Run.EventSeq, Scope: scope, Content: string(raw), ContentHash: promptHash(string(raw)), Type: "tool_execution"})
	if verifyEvidence(s, &s.Evidence[len(s.Evidence)-1]) != "verified" {
		t.Fatal("fixture lost source integrity")
	}
}

func resultValidationClaim(t *testing.T, s *Snapshot, claim Result) {
	t.Helper()
	raw, err := json.Marshal(map[string]Result{"trace_result": claim})
	if err != nil {
		t.Fatal(err)
	}
	s.Messages = append(s.Messages, Message{Role: "assistant", Content: string(raw)})
	s.Run.Status = StatusCompleted
	refreshResult(s)
}

func resultValidationIssue(t *testing.T, s *Snapshot, code, field, id string, answerIndex int, blocking bool) ResultValidationIssue {
	t.Helper()
	for _, issue := range s.Result.ValidationIssues {
		index := -1
		if issue.AnswerIndex != nil {
			index = *issue.AnswerIndex
		}
		if issue.Code == code && issue.Field == field && issue.EvidenceID == id && index == answerIndex && issue.Blocking == blocking {
			return issue
		}
	}
	t.Fatalf("missing %s %s %s [%d], blocking=%t: %+v", code, field, id, answerIndex, blocking, s.Result.ValidationIssues)
	return ResultValidationIssue{}
}

func TestResultValidationA05RepairsReferencesWithoutTargetReplay(t *testing.T) {
	s := Snapshot{Run: Run{ID: "a05"}, artifactRoot: t.TempDir()}
	store := Store{root: s.artifactRoot}
	ref, err := store.PutArtifact("file", []byte("bilingual report already written"))
	if err != nil {
		t.Fatal(err)
	}
	resultValidationSource(t, &s, "ev-3476", "exec", "exec", "flag{a05-existing-proof}")
	resultValidationSource(t, &s, "ev-3529", "artifact_register", "builtin", "registered", ref)
	resultValidationSource(t, &s, "ev-3634", "worker_status", "builtin", "settled")
	resultValidationSource(t, &s, "ev-3648", "worker_recover", "builtin", "")
	claim := Result{Status: "success", EvidenceIDs: []string{"ev-3476", "ev-3529", "ev-3634", "ev-3648"}, Answers: []Answer{{Value: "flag{a05-existing-proof}", EvidenceIDs: []string{"ev-3476"}}}}
	resultValidationClaim(t, &s, claim)
	if s.Result.Status != "incomplete" || !resultFormatRepairOnly(&s) {
		t.Fatalf("A05 reference errors were not repairable: %+v", s.Result)
	}
	for id, tool := range map[string]string{"ev-3529": "artifact_register", "ev-3634": "worker_status", "ev-3648": "worker_recover"} {
		issue := resultValidationIssue(t, &s, "evidence_not_observation", "evidence_ids", id, -1, true)
		if issue.ToolID != tool || issue.Adapter != "builtin" || !strings.Contains(s.Result.TerminalReason, id) {
			t.Fatalf("bookkeeping identity absent: %+v", issue)
		}
	}
	context := resultValidationRepairContext(&s)
	if !strings.Contains(context, ref.Hash) || !strings.Contains(context, "request-ev-3529") {
		t.Fatal("repair context omitted the existing artifact locator")
	}
	before := valueHash([]any{s.Actions, s.Evidence, s.Requests, s.Responses})
	claim.EvidenceIDs = []string{"ev-3476"}
	claim.ArtifactEvidenceIDs = []string{"ev-3529"}
	claim.SupportingEvidenceIDs = []string{"ev-3634", "ev-3648"}
	resultValidationClaim(t, &s, claim)
	if s.Result.Status != "success" || len(s.Result.ValidationIssues) != 2 || before != valueHash([]any{s.Actions, s.Evidence, s.Requests, s.Responses}) {
		t.Fatalf("reference repair replayed evidence or blocked outcome: %+v", s.Result)
	}
	resultValidationIssue(t, &s, "evidence_not_observation", "supporting_evidence_ids", "ev-3634", -1, false)
	if _, ok := verifiedOutput(&s, "ev-3529"); ok {
		t.Fatal("artifact registration became an observation")
	}
	report := renderReportSnapshot(&s, nil, nil)
	if !strings.Contains(report, "归档引用：ev-3529") || !strings.Contains(report, "附加引用未验证") {
		t.Fatal("report omitted supplemental classification")
	}
	for _, reason := range []string{"report_translation_missing", "child_report_delivery_pending"} {
		s.Run.Status, s.Run.PauseReason = StatusPaused, reason
		refreshResult(&s)
		if s.Result.Status != "success" {
			t.Fatalf("delivery pause discarded core result: %s %+v", reason, s.Result)
		}
	}
}

func TestResultValidationA09MissingTopReferenceAndOldJSONOutput(t *testing.T) {
	s := Snapshot{Run: Run{ID: "a09"}}
	value := `{"private":"report","path":"a\\b"}`
	resultValidationSource(t, &s, "ev-332", "exec", "exec", "response: "+value)
	resultValidationSource(t, &s, "ev-423", "artifact_register", "builtin", "registered")
	for i := 0; i < 9; i++ {
		resultValidationSource(t, &s, fmt.Sprintf("ev-padding-%d", i), "worker_status", "builtin", "status")
	}
	resultValidationSource(t, &s, "ev-638", "exec", "exec", "cleanup-confirmed")
	claim := Result{Status: "success", EvidenceIDs: []string{"ev-332", "ev-423"}, Answers: []Answer{{Value: value, EvidenceIDs: []string{"ev-332"}}, {Value: "cleanup-confirmed", EvidenceIDs: []string{"ev-638"}}}}
	resultValidationClaim(t, &s, claim)
	resultValidationIssue(t, &s, "evidence_not_observation", "evidence_ids", "ev-423", -1, true)
	issue := resultValidationIssue(t, &s, "answer_evidence_not_listed", "answers", "ev-638", 1, true)
	if !s.Result.Answers[0].Verified || !s.Result.Answers[1].Verified || issue.ToolID != "exec" || !resultFormatRepairOnly(&s) {
		t.Fatalf("JSON or old ledger was misdiagnosed: %+v", s.Result)
	}
	context := resultValidationRepairContext(&s)
	if !strings.Contains(context, "most recent eight") || !strings.Contains(context, "ev-638") || !strings.Contains(context, "artifact_read") {
		t.Fatal("repair context omitted actionable existing-source guidance")
	}
	s.Run.Status, s.Run.PauseReason = StatusPaused, "completion_blocked"
	refreshResult(&s)
	if len(s.Result.ValidationIssues) != 2 || !s.Result.Answers[0].Verified || s.Result.Status != "incomplete" {
		t.Fatal("blocked state discarded partial result or issues")
	}
	before := valueHash([]any{s.Actions, s.Evidence})
	claim.EvidenceIDs = []string{"ev-332", "ev-638"}
	resultValidationClaim(t, &s, claim)
	if s.Result.Status != "success" || len(s.Result.ValidationIssues) != 0 || before != valueHash([]any{s.Actions, s.Evidence}) {
		t.Fatalf("existing A09 output did not repair without replay: %+v", s.Result)
	}
}

func TestResultValidationPreservesRequiredProofAndSourceBoundaries(t *testing.T) {
	for _, test := range []struct {
		name, code string
		edit       func(*Snapshot, *Result)
	}{
		{"missing", "evidence_missing", func(s *Snapshot, r *Result) { s.Evidence = nil }},
		{"hash", "evidence_source_invalid", func(s *Snapshot, r *Result) { s.Evidence[0].ContentHash = "forged" }},
		{"event", "evidence_source_invalid", func(s *Snapshot, r *Result) { s.Events = nil }},
		{"response_arguments", "evidence_source_invalid", func(s *Snapshot, r *Result) { s.Responses[0].Response.ToolCalls[0].Arguments = "changed" }},
		{"uncertain", "evidence_source_unverified", func(s *Snapshot, r *Result) { s.Actions[0].Status = "uncertain" }},
		{"manual", "evidence_source_unverified", func(s *Snapshot, r *Result) { s.Evidence[0].Type = "manual_observation" }},
		{"target", "evidence_scope_mismatch", func(s *Snapshot, r *Result) {
			s.WorkerContext = &WorkerContext{EvidenceScope: &EvidenceScope{ObjectID: "other", Version: "v1"}}
		}},
		{"version", "evidence_scope_mismatch", func(s *Snapshot, r *Result) {
			s.WorkerContext = &WorkerContext{EvidenceScope: &EvidenceScope{ObjectID: "target", Version: "v2"}}
		}},
		{"false_answer", "answer_value_mismatch", func(s *Snapshot, r *Result) { r.Answers[0].Value = "not-observed" }},
		{"empty_answer", "answer_empty", func(s *Snapshot, r *Result) { r.Answers[0].Value = "" }},
		{"unsupported_answer", "answer_evidence_missing", func(s *Snapshot, r *Result) { r.Answers[0].EvidenceIDs = nil }},
		{"only_supplemental", "outcome_evidence_missing", func(s *Snapshot, r *Result) {
			r.SupportingEvidenceIDs, r.EvidenceIDs, r.Answers = r.EvidenceIDs, nil, nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := Snapshot{Run: Run{ID: "negative"}, WorkerContext: &WorkerContext{EvidenceScope: &EvidenceScope{ObjectID: "target", Version: "v1"}}}
			resultValidationSource(t, &s, "ev-proof", "exec", "exec", "real-observation")
			claim := Result{Status: "success", EvidenceIDs: []string{"ev-proof"}, Answers: []Answer{{Value: "real-observation", EvidenceIDs: []string{"ev-proof"}, Verified: true}}}
			test.edit(&s, &claim)
			resultValidationClaim(t, &s, claim)
			found := false
			for _, issue := range s.Result.ValidationIssues {
				found = found || issue.Blocking && issue.Code == test.code
			}
			if s.Result.Status == "success" || !found || resultFormatRepairOnly(&s) {
				t.Fatalf("negative control bypassed: %+v", s.Result)
			}
		})
	}
}

func TestResultValidationSupplementalCannotWaiveChecksOrRequiredAnswers(t *testing.T) {
	for _, mode := range []string{"requirement", "predicate", "legacy_answer", "legacy_evidence"} {
		t.Run(mode, func(t *testing.T) {
			s := Snapshot{Run: Run{ID: "required"}}
			resultValidationSource(t, &s, "ev-unrelated", "exec", "exec", "unrelated observation")
			resultValidationSource(t, &s, "ev-bookkeeping", "worker_status", "builtin", "required-output")
			claim := Result{Status: "success", EvidenceIDs: []string{"ev-unrelated"}, SupportingEvidenceIDs: []string{"ev-bookkeeping"}, ArtifactEvidenceIDs: []string{"ev-missing"}}
			switch mode {
			case "requirement":
				s.Requirements = []VerificationRequirement{{ID: "required", Description: "required user output", Category: "goal", Predicate: EvidencePredicate{Kind: "contains", Value: "required-output"}}}
			case "predicate":
				s.Run.SuccessContains = "required-output"
			case "legacy_answer":
				claim.Answers = []Answer{{Value: "required-output", EvidenceIDs: []string{"ev-bookkeeping"}, Verified: true}}
			case "legacy_evidence":
				claim.EvidenceIDs = append(claim.EvidenceIDs, "ev-bookkeeping")
			}
			// Model supplied diagnostics/verified are display claims, never authority.
			claim.ValidationIssues = []ResultValidationIssue{{Code: "fake-pass", Blocking: false}}
			resultValidationClaim(t, &s, claim)
			if s.Result.Status == "success" {
				t.Fatalf("supplemental classification waived %s: %+v", mode, s.Result)
			}
			for _, issue := range s.Result.ValidationIssues {
				if issue.Code == "fake-pass" {
					t.Fatal("model diagnostics trusted")
				}
			}
		})
	}
}

func TestResultValidationPredicateCannotHideOmittedAnswerReference(t *testing.T) {
	s := Snapshot{Run: Run{ID: "predicate", SuccessContains: "required-proof"}}
	resultValidationSource(t, &s, "ev-top", "exec", "exec", "required-proof")
	resultValidationSource(t, &s, "ev-answer", "exec", "exec", "required-answer")
	claim := Result{Status: "success", EvidenceIDs: []string{"ev-top"}, Answers: []Answer{{Value: "required-answer", EvidenceIDs: []string{"ev-answer"}}}}
	resultValidationClaim(t, &s, claim)
	resultValidationIssue(t, &s, "answer_evidence_not_listed", "answers", "ev-answer", 0, true)
	if s.Result.Status == "success" || !resultFormatRepairOnly(&s) {
		t.Fatalf("predicate hid malformed required reference: %+v", s.Result)
	}
	claim.EvidenceIDs = append(claim.EvidenceIDs, "ev-answer")
	resultValidationClaim(t, &s, claim)
	if s.Result.Status != "success" || len(s.Result.ValidationIssues) != 0 {
		t.Fatalf("same archived observations failed after reference repair: %+v", s.Result)
	}
	// Preserve the existing predicate-only result path with no explicit refs.
	resultValidationClaim(t, &s, Result{Status: "success"})
	if s.Result.Status != "success" || len(s.Result.ValidationIssues) != 0 {
		t.Fatalf("persisted predicate could not supply outcome proof: %+v", s.Result)
	}
}

func TestResultValidationCASAndTerminalStateRemainAuthoritative(t *testing.T) {
	s := Snapshot{Run: Run{ID: "cas"}, artifactRoot: t.TempDir()}
	store := Store{root: s.artifactRoot}
	ref, err := store.PutArtifact("stdout", []byte("durable-proof"))
	if err != nil {
		t.Fatal(err)
	}
	resultValidationSource(t, &s, "ev-cas", "exec", "exec", "", ref)
	claim := Result{Status: "success", EvidenceIDs: []string{"ev-cas"}, Answers: []Answer{{Value: "durable-proof", EvidenceIDs: []string{"ev-cas"}}}, ArtifactEvidenceIDs: []string{"ev-cas"}}
	resultValidationClaim(t, &s, claim)
	if s.Result.Status != "success" {
		t.Fatal("CAS stdout did not verify")
	}
	for _, status := range []Status{StatusCancelled, StatusFailed} {
		s.Run.Status, s.Run.PauseReason = status, "report_translation_missing"
		refreshResult(&s)
		if s.Result.Status != string(status) {
			t.Fatal("stale delivery pause overrode terminal run status")
		}
	}
	f, err := store.OpenArtifact(ref.Hash)
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(s.artifactRoot)+string(os.PathSeparator)) {
		t.Fatal("fixture CAS outside test directory")
	}
	if err := os.WriteFile(path, []byte("tampered-proof"), 0600); err != nil {
		t.Fatal(err)
	}
	resultValidationClaim(t, &s, claim)
	resultValidationIssue(t, &s, "evidence_source_invalid", "evidence_ids", "ev-cas", -1, true)
	resultValidationIssue(t, &s, "evidence_source_invalid", "artifact_evidence_ids", "ev-cas", -1, false)
	if s.Result.Status == "success" || s.Result.Answers[0].Verified {
		t.Fatal("corrupted CAS accepted")
	}
	raw, _ := json.Marshal(s)
	var loaded Snapshot
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatal(err)
	}
	loaded.artifactRoot = s.artifactRoot
	refreshResult(&loaded)
	if !reflect.DeepEqual(s.Result, loaded.Result) {
		t.Fatal("reloaded diagnostics differ")
	}
}
