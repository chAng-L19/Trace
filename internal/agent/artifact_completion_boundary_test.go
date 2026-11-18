package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A registered file proves captured bytes and provenance, not that its producer
// finished. Even a success marker echoed in metadata must not become acceptance.
func TestArtifactRegistrationDoesNotCompletePartialWork(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		predicate     bool
	}{
		{"empty", "", false},
		{"partial-json", `{"results":[],"complete":false}`, false},
		{"metadata-predicate", `{"results":[],"complete":false}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const marker = "artifact-only-success-marker"
			path := filepath.Join(t.TempDir(), "partial.json")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			args, err := json.Marshal(ArtifactRegistration{Path: path, Name: marker, Purpose: "partial capture, not producer completion"})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			p := settlementProviderFunc(func(_ context.Context, r ModelRequest) (ModelResponse, error) {
				calls++
				if calls == 1 {
					return ModelResponse{ToolCalls: []ToolCall{{ID: "partial", Name: "artifact_register", Arguments: string(args)}}}, nil
				}
				var registered ToolResult
				for _, message := range r.Messages {
					if message.Role == "tool" && message.ToolCallID == "partial" {
						if err := json.Unmarshal([]byte(message.Content), &registered); err != nil {
							return ModelResponse{}, err
						}
					}
				}
				body, err := json.Marshal(map[string]any{"trace_result": Result{
					Status: "success", EvidenceIDs: []string{registered.EvidenceID},
					Answers:       []Answer{{Value: marker, EvidenceIDs: []string{registered.EvidenceID}}},
					CleanupStatus: "completed",
				}})
				return ModelResponse{Content: string(body)}, err
			})
			a, store := newTestAgent(t, p)
			a.SetRequireVerifiedCompletion(true)
			limit := 3
			a.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
			if tc.predicate {
				a.SetSuccessContains(marker)
			}
			id := createTestRun(t, a)
			s, err := a.Run(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if s.Run.Status != StatusPaused || s.Result.Status != "incomplete" || calls != limit || len(s.Actions) != 1 {
				t.Fatalf("partial artifact completed or replayed: run=%+v result=%+v calls=%d actions=%d", s.Run, s.Result, calls, len(s.Actions))
			}
			if len(s.Evidence) != 1 || s.Evidence[0].Verification != "verified" || s.Actions[0].Result == nil {
				t.Fatal("fixture must contain a genuinely verified registration source")
			}
			registered := s.Actions[0].Result
			if registered.ExitCode != 0 || len(registered.Artifacts) != 1 || registered.Artifacts[0].Hash != promptHash(tc.content) || !strings.Contains(registered.Stdout, marker) {
				t.Fatalf("registration fixture lost its captured file or echoed marker: %+v", registered)
			}
			if _, eligible := verifiedOutput(&s, s.Evidence[0].ID); eligible || s.Result.CleanupStatus != "unknown" {
				t.Fatal("registration metadata became goal or cleanup proof")
			}
			for _, event := range s.Events {
				if event.Type == "run_completed" {
					t.Fatal("partial artifact emitted run completion")
				}
			}
			saved, err := store.Load(id)
			if err != nil || saved.Result.Status != "incomplete" || len(saved.Actions) != 1 || len(saved.Evidence) != 1 || saved.Evidence[0].ContentHash != s.Evidence[0].ContentHash {
				t.Fatalf("reload promoted partial work or changed source: %v", err)
			}
		})
	}
}
