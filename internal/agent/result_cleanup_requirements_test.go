package agent

import "testing"

func TestRequirementsAutomaticSuccessNeedsGoal(t *testing.T) {
	for _, test := range []struct {
		name, claim, predicate, want   string
		goal, invalidSource, malformed bool
	}{
		{name: "cleanup-only", want: "incomplete"},
		{name: "goal-and-cleanup", goal: true, want: "success"},
		{name: "cleanup-with-explicit-success", claim: "success", want: "success"},
		{name: "cleanup-with-predicate", predicate: "observed-proof", want: "success"},
		{name: "explicit-incomplete", goal: true, claim: "incomplete", predicate: "observed-proof", want: "incomplete"},
		{name: "explicit-failed", goal: true, claim: "failed", predicate: "observed-proof", want: "failed"},
		{name: "malformed-result", goal: true, malformed: true, want: "incomplete"},
		{name: "invalid-source", goal: true, claim: "success", invalidSource: true, want: "incomplete"},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := Snapshot{Run: Run{ID: "requirements-result", Status: StatusCompleted, SuccessContains: test.predicate}}
			resultValidationSource(t, &s, "proof", "read", "builtin", "observed-proof")
			s.Requirements = []VerificationRequirement{{ID: "cleanup", Description: "cleanup observed", Category: "cleanup", ToolID: "read", ArgumentsHash: promptHash("{}"), Predicate: EvidencePredicate{Kind: "contains", Value: "observed-proof"}}}
			if test.goal {
				goal := s.Requirements[0]
				goal.ID, goal.Category, goal.Description = "goal", "goal", "business goal observed"
				s.Requirements = append(s.Requirements, goal)
			}
			for _, requirement := range s.Requirements {
				s.Checks = append(s.Checks, VerificationCheck{RequirementID: requirement.ID, RequirementHash: valueHash(requirement), EvidenceIDs: []string{"proof"}, References: []EvidenceReference{{ID: "proof", Hash: s.Evidence[0].ContentHash}}})
			}
			s.Messages = append(s.Messages, Message{Role: "assistant", Content: "The probe completed."})
			if test.claim != "" {
				resultValidationClaim(t, &s, Result{Status: test.claim, EvidenceIDs: []string{"proof"}, Answers: []Answer{{Value: "observed-proof", EvidenceIDs: []string{"proof"}}}})
			}
			if test.malformed {
				s.Messages = append(s.Messages, Message{Role: "assistant", Content: "{\"trace_result\":null}"})
			}
			if test.invalidSource {
				s.Evidence[0].Content += "changed"
			}
			refreshResult(&s)
			if s.Result.Status != test.want {
				t.Fatalf("business status: want %s, got %+v", test.want, s.Result)
			}
			wantCleanup := "completed"
			if test.invalidSource {
				wantCleanup = "unknown"
			}
			if s.Result.CleanupStatus != wantCleanup {
				t.Fatalf("cleanup status: want %s, got %+v", wantCleanup, s.Result)
			}
		})
	}
}
