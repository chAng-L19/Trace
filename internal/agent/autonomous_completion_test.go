package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAutonomousCompletionContinuesWithoutDeclaredChecks(t *testing.T) {
	calls := 0
	p := settlementProviderFunc(func(_ context.Context, r ModelRequest) (ModelResponse, error) {
		calls++
		switch calls {
		case 1:
			return ModelResponse{Content: "Partial progress; stopping here."}, nil
		case 2:
			if !strings.Contains(lastConversationMessage(r).Content, "Runtime completion audit") {
				t.Error("missing completion continuation")
			}
			args, _ := json.Marshal(map[string]string{"command": "printf observed-proof"})
			return ModelResponse{ToolCalls: []ToolCall{{ID: "proof", Name: "exec", Arguments: string(args)}}}, nil
		default:
			var proof ToolResult
			for _, m := range r.Messages {
				if m.Role == "tool" {
					json.Unmarshal([]byte(m.Content), &proof)
				}
			}
			body, _ := json.Marshal(map[string]any{"trace_result": Result{Status: "success", EvidenceIDs: []string{proof.EvidenceID}, Answers: []Answer{{Value: "observed-proof", EvidenceIDs: []string{proof.EvidenceID}}}}})
			return ModelResponse{Content: fixtureReport(string(body))}, nil
		}
	})
	a, store := newTestAgent(t, p)
	a.SetRequireVerifiedCompletion(true)
	id := createTestRun(t, a)
	s, err := a.Run(context.Background(), id)
	if err != nil || calls != 3 || s.Run.Status != StatusCompleted || s.Result.Status != "success" || len(s.Actions) != 1 {
		t.Fatalf("premature end: calls=%d run=%+v result=%+v err=%v", calls, s.Run, s.Result, err)
	}
	saved, err := store.Load(id)
	if err != nil || !saved.Run.RequireVerifiedCompletion {
		t.Fatal("completion policy was not persisted")
	}
}

func TestAutonomousCompletionBudgetPauseRetainsPolicyOnResume(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	a.SetRequireVerifiedCompletion(true)
	limit := 2
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
	id := createTestRun(t, a)
	s, err := a.Run(context.Background(), id)
	if err != nil || s.Run.Status != StatusPaused || len(s.Requests) != limit {
		t.Fatalf("ordinary text ended autonomous task: %+v %v", s.Run, err)
	}
	resumed, err := New(store, successProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	limit++
	resumed.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
	s, err = resumed.Run(context.Background(), id)
	if err != nil || s.Run.Status != StatusPaused || !s.Run.RequireVerifiedCompletion || len(s.Requests) != limit {
		t.Fatalf("resume weakened completion: %+v %v", s.Run, err)
	}
	for _, e := range s.Events {
		if e.Type == "run_completed" {
			t.Fatal("incomplete task emitted completion")
		}
	}
}

func TestAutonomousCompletionFailedAttemptReplansUntilBudget(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"trace_result": Result{Status: "failed", Summary: "attempt made no progress"}})
	p := &scriptedProvider{responses: []ModelResponse{{Content: string(body)}, {Content: string(body)}}}
	a, _ := newTestAgent(t, p)
	a.SetRequireVerifiedCompletion(true)
	limit := 2
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
	s, err := a.Run(context.Background(), createTestRun(t, a))
	if err != nil || s.Run.Status != StatusPaused || !strings.Contains(s.Run.PauseReason, "budget") || len(s.Requests) != limit {
		t.Fatalf("failed attempt closed task: %+v %v", s.Run, err)
	}
	for _, e := range s.Events {
		if e.Type == "run_completed" {
			t.Fatal("failed attempt emitted completion")
		}
	}
}

func TestAutonomousWorkerInheritsPolicyAndDoesNotCompleteDependency(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	a.SetRequireVerifiedCompletion(true)
	limit := 1
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "one", Goal: "first"}, {ID: "two", Goal: "dependent", Dependencies: []string{"one"}}}); err != nil {
		t.Fatal(err)
	}
	// Inherit the persisted parent policy, not the mutable process default.
	a.SetRequireVerifiedCompletion(false)
	w, err := a.DispatchTask(context.Background(), id, "one")
	if err != nil {
		t.Fatal(err)
	}
	s, err := waitAllTestWorkers(a, context.Background(), id, 5)
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Load(w.RunID)
	if err != nil || !child.Run.RequireVerifiedCompletion || child.Run.Status != StatusPaused || s.Plan.Tasks[0].Status != "paused" || len(s.Plan.Frontier) != 0 {
		t.Fatalf("unfinished child unlocked dependency: child=%+v plan=%+v err=%v", child.Run, s.Plan, err)
	}
}
