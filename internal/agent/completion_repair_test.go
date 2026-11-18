package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompletionReferenceRepairPiUsesExistingOutput(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.md")
			if err := os.WriteFile(path, []byte("saved report"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			provider := settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
				calls++
				if calls == 1 {
					return ModelResponse{ToolCalls: []ToolCall{{ID: "proof", Name: "exec", Arguments: `{"command":"printf already-observed-proof"}`}}}, nil
				}
				if calls == 2 {
					args, _ := json.Marshal(ArtifactRegistration{Path: path, Purpose: "saved report"})
					return ModelResponse{ToolCalls: []ToolCall{{ID: "registration", Name: "artifact_register", Arguments: string(args)}}}, nil
				}
				ids := map[string]string{}
				for _, message := range request.Messages {
					if message.Role == "tool" {
						var result ToolResult
						if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
							return ModelResponse{}, err
						}
						ids[message.ToolCallID] = result.EvidenceID
					}
				}
				claim := Result{Status: "success", EvidenceIDs: []string{ids["proof"]}, Answers: []Answer{{Value: "already-observed-proof", EvidenceIDs: []string{ids["proof"]}}}}
				if calls == 3 || calls == 4 {
					claim.EvidenceIDs = append(claim.EvidenceIDs, ids["registration"])
				} else {
					if calls != 5 || !strings.Contains(lastConversationMessage(request).Content, "evidence_not_observation") {
						t.Error("missing precise correction")
					}
					claim.ArtifactEvidenceIDs = []string{ids["registration"]}
				}
				body, _ := json.Marshal(map[string]Result{"trace_result": claim})
				return ModelResponse{Content: fixtureReport(string(body))}, nil
			})
			a, store := newTestAgent(t, provider)
			if backend == "sqlite" {
				if err := MigrateStoreToSQLite(store.root); err != nil {
					t.Fatal(err)
				}
				var err error
				store, err = NewStore(store.root)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				a, err = New(store, provider, "")
				if err != nil {
					t.Fatal(err)
				}
			}
			a.SetRequireVerifiedCompletion(true)
			s, err := a.Run(context.Background(), createTestRun(t, a))
			if err != nil || s.Run.Status != StatusCompleted || s.Result.Status != "success" || calls != 5 || len(s.Actions) != 2 {
				t.Fatalf("correction replayed target or dropped delivery: calls=%d run=%+v result=%+v err=%v", calls, s.Run, s.Result, err)
			}
			if s.Actions[0].Name != "exec" || s.Actions[1].Name != "artifact_register" || !a.reportFilesComplete(&s) {
				t.Fatal("missing original evidence or complete bilingual reports")
			}
		})
	}
}

func referenceRepairFixture(t *testing.T) (Snapshot, Result) {
	t.Helper()
	s := Snapshot{Run: Run{ID: "reference-repair", RequireVerifiedCompletion: true}}
	resultValidationSource(t, &s, "proof", "exec", "exec", "flag{already-verified}")
	resultValidationSource(t, &s, "registration", "artifact_register", "builtin", "registered")
	claim := Result{Status: "success", EvidenceIDs: []string{"proof", "registration"}, Answers: []Answer{{Value: "flag{already-verified}", EvidenceIDs: []string{"proof"}}}}
	resultValidationClaim(t, &s, claim)
	return s, claim
}

func TestCompletionReferenceRepairConvergesWithoutReplay(t *testing.T) {
	s, claim := referenceRepairFixture(t)
	before := valueHash([]any{s.Actions, s.Evidence, s.Requests, s.Responses})
	if !continueForEvidence(&s) || s.Run.Status != StatusRunning || s.Events[len(s.Events)-1].Type != "completion_reference_repair" {
		t.Fatalf("missing correction-only turn: %+v", s.Run)
	}
	if !strings.Contains(s.Messages[len(s.Messages)-1].Content, "registration") || !strings.Contains(s.Messages[len(s.Messages)-1].Content, "Do not repeat target requests") || before != valueHash([]any{s.Actions, s.Evidence, s.Requests, s.Responses}) {
		t.Fatal("audit did not identify the existing bad reference without execution")
	}
	fingerprint := resultReferenceRepairFingerprint(&s)
	// Fresh target output and heartbeat timestamps do not repair the same bad ref.
	resultValidationSource(t, &s, "unnecessary-replay", "exec", "exec", "flag{already-verified}; fresh date")
	addEvent(&s, "model_stream", "heartbeat", nil)
	resultValidationClaim(t, &s, claim)
	if resultReferenceRepairFingerprint(&s) != fingerprint || !continueForEvidence(&s) || s.Run.Status != StatusRunning || s.Run.PauseReason != "" {
		t.Fatalf("unchanged bookkeeping fault stopped automatic repair: %+v", s.Run)
	}
	if len(s.Result.Answers) != 1 || !s.Result.Answers[0].Verified || s.Result.Status == "success" || s.Run.Budget != (Budget{}) {
		t.Fatal("blocked correction discarded partial proof, accepted bad result or installed a budget")
	}
	before = valueHash([]any{s.Actions, s.Evidence, s.Requests, s.Responses})
	claim.EvidenceIDs = []string{"proof"}
	resultValidationClaim(t, &s, claim)
	if continueForEvidence(&s) || s.Result.Status != "success" || before != valueHash([]any{s.Actions, s.Evidence, s.Requests, s.Responses}) {
		t.Fatalf("existing evidence could not complete: %+v", s.Result)
	}
}

func TestCompletionReferenceRepairRespectsSolvingAndExplicitBudget(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Snapshot, *Result)
	}{
		{"active-worker", func(s *Snapshot, _ *Result) { s.Workers = []AgentWorker{{ID: "active", Status: "running"}} }},
		{"pending-task", func(s *Snapshot, _ *Result) { s.Plan.Tasks = []PlanTask{{ID: "todo", Status: "pending"}} }},
		{"required-check", func(s *Snapshot, _ *Result) {
			s.Requirements = []VerificationRequirement{{ID: "required", Description: "another required observation"}}
		}},
		{"fabricated-answer", func(_ *Snapshot, r *Result) { r.Answers[0].Value = "invented" }},
		{"uncertain-source", func(s *Snapshot, _ *Result) { s.Actions[0].Status = "uncertain" }},
		{"missing-predicate", func(s *Snapshot, _ *Result) { s.Run.SuccessContains = "not-observed" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, claim := referenceRepairFixture(t)
			test.edit(&s, &claim)
			resultValidationClaim(t, &s, claim)
			if resultReferenceRepairFingerprint(&s) != "" || deferResultReferenceRepair(&s, nil) {
				t.Fatal("ordinary solving entered the reference-only stop path")
			}
		})
	}
	s, _ := referenceRepairFixture(t)
	s.Run.Budget.MaxModelRequests = len(s.Requests)
	if !deferResultReferenceRepair(&s, nil) || s.Run.PauseReason != "model request budget exhausted" {
		t.Fatalf("explicit budget lost precedence: %+v", s.Run)
	}
	for _, e := range s.Events {
		if e.Type == "completion_reference_repair" {
			t.Fatal("spent a correction turn after explicit budget exhaustion")
		}
	}
}

func TestCompletionReferenceRepairPersistsAndAcceptsNewInput(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store *Store
			if backend == "sqlite" {
				store = sqliteTestStore(t)
			} else {
				var err error
				store, err = NewStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { store.Close() })
			}
			created, err := store.Create("repair", "prompt", "hash")
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Update(created.Run.ID, func(s *Snapshot) error {
				resultValidationSource(t, s, "proof", "exec", "exec", "observed")
				resultValidationSource(t, s, "registration", "artifact_register", "builtin", "registered")
				claim := Result{Status: "success", EvidenceIDs: []string{"proof", "registration"}, Answers: []Answer{{Value: "observed", EvidenceIDs: []string{"proof"}}}}
				resultValidationClaim(t, s, claim)
				if !deferResultReferenceRepair(s, nil) {
					t.Fatal("missing initial repair")
				}
				resultValidationClaim(t, s, claim)
				if !deferResultReferenceRepair(s, nil) {
					t.Fatal("missing blocked repair")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			s, err := reopened.Load(created.Run.ID)
			if err != nil || s.Run.Status != StatusRunning || s.Run.PauseReason != "" || !s.Result.Answers[0].Verified {
				t.Fatalf("reload discarded correction state: %+v %v", s.Result, err)
			}
			previous := resultReferenceRepairFingerprint(&s)
			s.Messages = append(s.Messages, Message{Role: "user", Content: "Use the saved proof and repair only the JSON references."})
			if previous == resultReferenceRepairFingerprint(&s) || !deferResultReferenceRepair(&s, nil) || s.Run.Status != StatusRunning {
				t.Fatal("new explicit input did not permit a fresh correction")
			}
			s.Run.Status = StatusCancelled
			refreshResult(&s)
			if s.Result.Status == "success" || s.Result.Status == "incomplete" {
				t.Fatal("stale completion pause overrode cancellation")
			}
		})
	}
}
