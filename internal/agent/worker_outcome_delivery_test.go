package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerOutcomeDeliveryReceipt(t *testing.T) {
	for _, tc := range []struct {
		name, reply, wantWorker string
		strict                  bool
	}{
		{"no claim", "Saved a draft.", "completed", false},
		{"partial", `{"trace_result":{"status":"incomplete","summary":"One section remains."}}`, "completed", false},
		{"strict incomplete", "Saved a draft.", "uncertain", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, store, id, w := settlementFixture(t)
			err := settlementOperation(t, a, store, id, w, func(s *Snapshot) {
				s.Run.Status, s.Run.RequireVerifiedCompletion = StatusCompleted, tc.strict
				s.Messages = append(s.Messages, Message{Role: "assistant", Content: tc.reply})
			})
			if err != nil {
				t.Fatal(err)
			}
			parent, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if len(parent.ChildReports) != 1 {
				t.Fatalf("want one fixed child receipt, got %d", len(parent.ChildReports))
			}
			r := parent.ChildReports[0]
			if r.RunStatus != StatusCompleted || r.ResultStatus != "incomplete" || r.CleanupStatus != "completed" {
				t.Fatalf("unexpected receipt: run=%s result=%s cleanup=%s", r.RunStatus, r.ResultStatus, r.CleanupStatus)
			}
			for _, e := range parent.Events {
				if e.Type != "worker_finished" {
					continue
				}
				d, ok := e.Data.(map[string]any)
				if !ok || d["status"] != tc.wantWorker || d["run_status"] != string(r.RunStatus) || d["result_status"] != r.ResultStatus || d["cleanup_status"] != r.CleanupStatus {
					t.Fatalf("settlement event disagrees with fixed receipt: %v", e.Data)
				}
				return
			}
			t.Fatal("missing worker_finished event")
		})
	}
}

// Evaluate the stop/continue decision itself; a settlement receipt alone can't
// show whether the runtime would ask the model for the same partial result again.
func TestWorkerOutcomeAuditBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
	}{
		{"partial", ""},
		{"strict", "autonomous task has no verified final result"},
		{"requirement", "unmet verification requirements: review"},
		{"predicate", "persisted stdout-contains predicate"},
		{"invalid reference", "structured final result remains incomplete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := Snapshot{
				Run:           Run{ID: "draft", ParentRunID: "parent", ParentTaskID: "review"},
				WorkerContext: &WorkerContext{RunID: "draft", ParentRunID: "parent", TaskID: "review"},
			}
			switch tc.name {
			case "strict":
				s.Run.RequireVerifiedCompletion = true
			case "requirement":
				s.Requirements = []VerificationRequirement{{ID: "review", Description: "Document approval", Category: "goal", ToolID: "read", Predicate: EvidencePredicate{Kind: "contains", Value: "approved"}}}
			case "predicate":
				s.Run.SuccessContains = "approved"
			}
			resultValidationSource(t, &s, "section", "read", "builtin", "One section was reviewed.")
			claim := Result{Status: "incomplete", Summary: "One section was reviewed; another remains."}
			if tc.name == "invalid reference" {
				claim.EvidenceIDs = []string{"missing-source"}
			}
			resultValidationClaim(t, &s, claim)
			messages := len(s.Messages)
			deferred := continueForEvidence(&s)
			if tc.reason == "" {
				if deferred || s.Run.Status != StatusCompleted || s.Result.Status != "incomplete" || len(s.Messages) != messages {
					t.Fatalf("explicit partial with no remaining requirement: deferred=%t run=%s result=%s added_messages=%d", deferred, s.Run.Status, s.Result.Status, len(s.Messages)-messages)
				}
				return
			}
			if !deferred || s.Run.Status != StatusRunning || s.Result.Status != "incomplete" || len(s.Messages) != messages+1 || !strings.Contains(s.Messages[messages].Content, tc.reason) {
				t.Fatalf("explicit partial must not waive %q: deferred=%t run=%s result=%s", tc.reason, deferred, s.Run.Status, s.Result.Status)
			}
		})
	}
	t.Run("ordinary root chat", func(t *testing.T) {
		s := Snapshot{Run: Run{ID: "chat", Status: StatusCompleted}}
		resultValidationSource(t, &s, "section", "read", "builtin", "One section was reviewed.")
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: "The section is ready to read."})
		messages := len(s.Messages)
		if continueForEvidence(&s) || s.Run.Status != StatusCompleted || s.Result.Status != "incomplete" || len(s.Messages) != messages {
			t.Fatal("a root conversation must not acquire a delegated outcome requirement")
		}
	})
}

func TestWorkerOutcomeMissingClaimRequestsExistingHandoff(t *testing.T) {
	s := Snapshot{Run: Run{ID: "draft", ParentRunID: "parent", ParentTaskID: "review", Status: StatusCompleted}, WorkerContext: &WorkerContext{RunID: "draft", ParentRunID: "parent", TaskID: "review"}}
	resultValidationSource(t, &s, "section", "read", "builtin", "One section was reviewed.")
	s.Messages = append(s.Messages, Message{Role: "assistant", Content: "One section is ready; another remains."})
	if !continueForEvidence(&s) || s.Run.Status != StatusRunning || !strings.Contains(s.Messages[len(s.Messages)-1].Content, "trace_result handoff using existing observations") {
		t.Fatal("missing delegated outcome silently ended")
	}
	actions := len(s.Actions)
	resultValidationClaim(t, &s, Result{Status: "incomplete", Summary: "One section is ready; another remains."})
	if continueForEvidence(&s) || s.Result.Status != "incomplete" || s.Run.Status != StatusCompleted || len(s.Actions) != actions {
		t.Fatal("partial handoff repeated work or became success")
	}
}

func TestWorkerOutcomePiHandoffDoesNotExecuteTools(t *testing.T) {
	for _, mode := range []string{"tool", "repeated text"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "section.txt")
			marker := filepath.Join(t.TempDir(), "unexpected-write")
			if err := os.WriteFile(path, []byte("Reviewed section"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			p := settlementProviderFunc(func(_ context.Context, r ModelRequest) (ModelResponse, error) {
				calls++
				switch calls {
				case 1:
					args, _ := json.Marshal(map[string]any{"path": path})
					return ModelResponse{ToolCalls: []ToolCall{{ID: "read-section", Name: "read", Arguments: string(args)}}}, nil
				case 2:
					return ModelResponse{Content: "The first section was reviewed; the remaining sections need follow-up."}, nil
				case 3, 4:
					if r.Purpose != "outcome_delivery" || len(r.Tools) != 0 {
						t.Errorf("handoff admitted execution tools: purpose=%s tools=%d", r.Purpose, len(r.Tools))
					}
					if calls == 3 {
						if mode == "repeated text" {
							return ModelResponse{Content: "The first section was reviewed; the remaining sections need follow-up."}, nil
						}
						args, _ := json.Marshal(map[string]string{"path": marker, "content": "should not execute"})
						return ModelResponse{ToolCalls: []ToolCall{{ID: "forbidden-write", Name: "write", Arguments: string(args)}}}, nil
					}
					return ModelResponse{Content: fixtureReport(`{"trace_result":{"status":"incomplete","summary":"The first section was reviewed; the remaining sections need follow-up."}}`)}, nil
				default:
					return ModelResponse{}, fmt.Errorf("unexpected extra handoff model call %d", calls)
				}
			})
			a, store := newTestAgent(t, p)
			parent := createTestRun(t, a)
			if _, err := a.SetPlan(parent, []PlanTask{{ID: "review", Goal: "Review the document"}}); err != nil {
				t.Fatal(err)
			}
			w, err := a.DispatchTask(context.Background(), parent, "review")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = waitAllTestWorkers(a, context.Background(), parent, 5); err != nil {
				t.Fatal(err)
			}
			child, err := store.Load(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 4 || child.Run.Status != StatusCompleted || child.Result.Status != "incomplete" || len(child.Actions) != 1 || child.Actions[0].Name != "read" || len(pendingToolCalls(&child)) != 0 {
				t.Fatalf("handoff replayed work or lost partial result: calls=%d run=%s result=%s actions=%d", calls, child.Run.Status, child.Result.Status, len(child.Actions))
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("tool attempted during handoff executed:", err)
			}
		})
	}
}

func TestWorkerOutcomeOrdinaryChatQuoteKeepsTools(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "user", Content: "Runtime completion audit: " + workerOutcomeHandoffReason})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r, err := a.preparePiRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Purpose == "outcome_delivery" || len(r.Tools) == 0 {
		t.Fatal("quoted audit disabled ordinary chat tools")
	}
}
