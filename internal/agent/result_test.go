package agent

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

type resultProvider struct {
	badID  bool
	answer string
	plain  bool
}

func (p resultProvider) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	last := lastConversationMessage(request)
	if last.Role != "tool" {
		command := "printf 'flag{verified}'"
		if runtime.GOOS == "windows" {
			command = "echo flag{verified}"
		}
		arguments, _ := json.Marshal(map[string]string{"command": command})
		return ModelResponse{ToolCalls: []ToolCall{{ID: "proof", Name: "exec", Arguments: string(arguments)}}, StopReason: "tool_calls"}, nil
	}
	if p.plain {
		return ModelResponse{Content: fixtureReport("I succeeded")}, nil
	}
	var output ToolResult
	if err := json.Unmarshal([]byte(last.Content), &output); err != nil {
		return ModelResponse{}, err
	}
	id := output.EvidenceID
	if p.badID {
		id = "nonexistent"
	}
	answer := p.answer
	if answer == "" {
		answer = "flag{verified}"
	}
	value := Result{Status: "success", Summary: "fixture proof", EvidenceIDs: []string{id}, Answers: []Answer{{Kind: "flag", Value: answer, EvidenceIDs: []string{id}, Verified: true}}, ReproductionSteps: []string{"execute fixture command"}, CleanupStatus: "completed"}
	body, _ := json.Marshal(map[string]any{"trace_result": value})
	return ModelResponse{Content: fixtureReport(string(body)), StopReason: "stop"}, nil
}

func TestResultEvidenceJudgeAndReport(t *testing.T) {
	a, s := newTestAgent(t, resultProvider{})
	id := createTestRun(t, a)
	snap, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Result.Status != "success" || len(snap.Evidence) != 1 || snap.Evidence[0].Verification != "verified" || !snap.Result.Answers[0].Verified {
		t.Fatalf("verified result missing: %+v", snap.Result)
	}
	if snap.Result.CleanupStatus != "unknown" {
		t.Fatal("model cleanup assertion was trusted")
	}
	report, err := s.Report(id)
	if err != nil || !strings.Contains(report, "flag{verified}") || !strings.Contains(report, snap.Evidence[0].ContentHash) {
		t.Fatalf("report: %v %s", err, report)
	}
	persisted, _ := s.Load(id)
	if persisted.Result.Status != "success" {
		t.Fatal("result not persisted")
	}
	for _, mutation := range []string{"hash", "event", "message", "action"} {
		t.Run(mutation, func(t *testing.T) {
			copy := persisted
			bytes, _ := json.Marshal(persisted)
			json.Unmarshal(bytes, &copy)
			switch mutation {
			case "hash":
				copy.Evidence[0].Content += "changed"
			case "event":
				copy.Evidence[0].SourceEventSequence = 9999
			case "message":
				for i := range copy.Messages {
					if copy.Messages[i].Role == "tool" {
						copy.Messages[i].Content = "changed"
					}
				}
			case "action":
				copy.Actions[0].Result.ExitCode = 7
			}
			refreshResult(&copy)
			if copy.Result.Status == "success" || copy.Evidence[0].Verification == "verified" {
				t.Fatal("tampered evidence accepted")
			}
		})
	}
}

func TestMissingEvidenceAndFabricatedAnswerStayIncomplete(t *testing.T) {
	for name, p := range map[string]resultProvider{"missing": {badID: true}, "fabricated": {answer: "flag{invented}"}, "ordinary": {plain: true}} {
		t.Run(name, func(t *testing.T) {
			a, _ := newTestAgent(t, p)
			limit := 3
			a.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
			snap, err := a.Run(context.Background(), createTestRun(t, a))
			if err != nil {
				t.Fatal(err)
			}
			want := StatusPaused
			if p.plain {
				want = StatusCompleted
			} // Plain conversational turns remain supported.
			if snap.Run.Status != want || snap.Result.Status != "incomplete" {
				t.Fatalf("ordinary completion became target success: %+v", snap.Result)
			}
		})
	}
}

func TestSuccessPredicateRequiresExecutedEvidence(t *testing.T) {
	conflict, _ := newTestAgent(t, resultProvider{badID: true})
	conflict.SetSuccessContains("flag{verified}")
	limit := 3
	conflict.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
	conflictSnap, conflictErr := conflict.Run(context.Background(), createTestRun(t, conflict))
	if conflictErr != nil || conflictSnap.Result.Status == "success" || conflictSnap.Run.Status != StatusPaused || len(conflictSnap.Requests) != limit {
		t.Fatal("predicate bypassed invalid cited evidence")
	}
	a, _ := newTestAgent(t, resultProvider{plain: true})
	a.SetSuccessContains("flag{verified}")
	snap, err := a.Run(context.Background(), createTestRun(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Result.Status != "success" || len(snap.Result.Answers) != 1 || !snap.Result.Answers[0].Verified {
		t.Fatalf("predicate not evaluated: %+v", snap.Result)
	}
	p := manualProvider(ToolCall{ID: "manual", Name: "external", Arguments: "{}"})
	a, _ = newTestAgent(t, p)
	a.SetSuccessContains("flag{verified}")
	manualLimit := 2
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &manualLimit})
	id := createTestRun(t, a)
	waitTestRun(t, a, id)
	snap, err = a.SubmitTool(context.Background(), id, "manual", `{"stdout":"flag{verified}","stderr":"","exit_code":0}`)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Result.Status != "incomplete" || snap.Evidence[0].Verification != "unverified" {
		t.Fatal("manual text promoted into executed evidence")
	}
}

func TestOrdinaryFailedCancelledAndEmptyResult(t *testing.T) {
	a, s := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	result, err := s.Result(id)
	if err != nil || result.Status != "incomplete" {
		t.Fatal("new result contract missing")
	}
	snap, err := a.Run(context.Background(), id)
	if err != nil || snap.Result.Status != "incomplete" {
		t.Fatal("ordinary model reply claimed success")
	}
	id = createTestRun(t, a)
	a.Cancel(id)
	result, _ = s.Result(id)
	if result.Status != "cancelled" {
		t.Fatal("cancelled result missing")
	}
	a, _ = newTestAgent(t, emptyProvider{})
	snap, err = a.Run(context.Background(), createTestRun(t, a))
	if err == nil || snap.Result.Status != "failed" {
		t.Fatal("failure result missing")
	}
}
