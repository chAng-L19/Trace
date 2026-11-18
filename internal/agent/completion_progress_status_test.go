package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCompletionDiagnosticRefreshesLegacyFeedbackAndDeduplicates(t *testing.T) {
	s := Snapshot{Run: Run{ID: "legacy-feedback", RequireVerifiedCompletion: true, PromptHash: "frozen-prompt"}, Messages: []Message{{Role: "system", Content: "frozen original prompt"}}}
	resultValidationSource(t, &s, "proof", "exec", "exec", "observed-proof")
	claim := Result{Status: "completed", EvidenceIDs: []string{"proof"}, Answers: []Answer{{Value: "observed-proof", EvidenceIDs: []string{"proof"}}}}
	resultValidationClaim(t, &s, claim)
	fingerprint := completionProgressFingerprint(&s)
	s.Messages = append(s.Messages, Message{Role: "user", Content: "Runtime completion audit: structured result reports incomplete work"})
	addEvent(&s, "completion_deferred", "legacy feedback", map[string]any{"progress_fingerprint": fingerprint})
	addEvent(&s, "completion_stalled", "legacy stall", map[string]any{"progress_fingerprint": fingerprint})
	// Persisted legacy events have no diagnostic signature and decode as maps.
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var restored Snapshot
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	s = restored
	before := valueHash([]any{s.Actions, s.Evidence, s.Requests, s.Responses, s.Messages[0], s.Run.PromptHash})
	messages := len(s.Messages)
	if !continueForEvidence(&s) || s.Run.Status != StatusRunning || len(s.Messages) != messages+1 {
		t.Fatalf("old progress event hid the new diagnostic: %+v", s.Run)
	}
	feedback := s.Messages[len(s.Messages)-1].Content
	for _, expected := range []string{"invalid_result_status", "trace_result.status", "completed", "allowed values are success, failed, incomplete"} {
		if !strings.Contains(feedback, expected) {
			t.Fatalf("precise feedback missing %q: %s", expected, feedback)
		}
	}
	var data struct {
		ProgressFingerprint   string `json:"progress_fingerprint"`
		DiagnosticFingerprint string `json:"diagnostic_fingerprint"`
	}
	raw, _ = json.Marshal(s.Events[len(s.Events)-1].Data)
	if err := json.Unmarshal(raw, &data); err != nil || data.ProgressFingerprint != fingerprint || data.DiagnosticFingerprint == "" {
		t.Fatalf("renewed event missing stable signatures: %+v %v", data, err)
	}
	for i := 0; i < 4; i++ {
		resultValidationClaim(t, &s, claim)
		s.Messages[len(s.Messages)-1].CreatedAt = time.Unix(int64(i+1), 0).UTC()
		s.Run.UpdatedAt = time.Unix(int64(i+10), 0).UTC()
		messages = len(s.Messages)
		events := len(s.Events)
		if !continueForEvidence(&s) || s.Run.Status != StatusRunning || s.Run.PauseReason != "" || len(s.Messages) != messages {
			t.Fatal("same diagnostic repeated feedback or introduced a stop budget")
		}
		if i > 0 && len(s.Events) != events {
			t.Fatal("same diagnostic repeated the stalled event")
		}
	}
	if s.Run.Budget != (Budget{}) || before != valueHash([]any{s.Actions, s.Evidence, s.Requests, s.Responses, s.Messages[0], s.Run.PromptHash}) {
		t.Fatal("feedback changed frozen prompt, source records or explicit budget")
	}
	// A new invalid value is actionable even with identical target observations.
	claim.Status = "running"
	resultValidationClaim(t, &s, claim)
	messages = len(s.Messages)
	if !continueForEvidence(&s) || len(s.Messages) != messages+1 || completionProgressFingerprint(&s) != fingerprint {
		t.Fatal("changed diagnostic was hidden by unchanged progress")
	}
	if !strings.Contains(s.Messages[len(s.Messages)-1].Content, `received \"running\"`) {
		t.Fatal("changed diagnostic omitted the new original value")
	}
	claim.Status = "success"
	resultValidationClaim(t, &s, claim)
	if continueForEvidence(&s) || s.Result.Status != "success" || len(s.Actions) != 1 {
		t.Fatalf("status correction could not reuse the existing proof: %+v", s.Result)
	}
}

func TestCompletionDiagnosticLegacyFeedbackRespectsExplicitBudget(t *testing.T) {
	s := Snapshot{Run: Run{ID: "legacy-budget", RequireVerifiedCompletion: true, Budget: Budget{MaxModelRequests: 1}}}
	resultValidationSource(t, &s, "proof", "exec", "exec", "observed-proof")
	resultValidationClaim(t, &s, Result{Status: "completed", EvidenceIDs: []string{"proof"}})
	addEvent(&s, "completion_deferred", "legacy feedback", map[string]any{"progress_fingerprint": completionProgressFingerprint(&s)})
	messages := len(s.Messages)
	if !continueForEvidence(&s) || s.Run.Status != StatusPaused || s.Run.PauseReason != "model request budget exhausted" || len(s.Messages) != messages {
		t.Fatalf("new diagnostic bypassed an explicit exhausted budget: %+v", s.Run)
	}
}
