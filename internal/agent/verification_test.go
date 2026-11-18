package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEvidencePredicates(t *testing.T) {
	for _, tc := range []struct {
		p      EvidencePredicate
		output string
		want   bool
	}{
		{EvidencePredicate{Kind: "json_equals", Pointer: "/items/0/a~1b", Expected: true}, `{"items":[{"a/b":true}]}`, true},
		{EvidencePredicate{Kind: "json_equals", Pointer: "/value", Expected: 42}, `{"value":42}`, true},
		{EvidencePredicate{Kind: "json_equals", Pointer: "/value", Expected: nil}, `{}`, false},
		{EvidencePredicate{Kind: "json_equals", Pointer: "/value", Expected: true}, `{"value":true} {"extra":1}`, false},
		{EvidencePredicate{Kind: "regex", Value: "^ready:[0-9]+$"}, "ready:42", true},
		{EvidencePredicate{Kind: "equals", Value: "ok"}, "almost ok", false},
	} {
		got, err := predicateMatches(tc.p, tc.output)
		if got != tc.want || tc.want && err != nil {
			t.Fatalf("%+v got %t %v", tc, got, err)
		}
	}
}

func TestChecksRevokedAndCleanupNeedsProbe(t *testing.T) {
	a, store := newTestAgent(t, manualProvider(ToolCall{ID: "proof", Name: "external", Arguments: `{}`}))
	id := createTestRun(t, a)
	r := VerificationRequirement{ID: "proof", Description: "exact observed proof", Category: "goal", Predicate: EvidencePredicate{Kind: "contains", Value: "flag{verified}"}}
	if _, err := a.SetVerificationRequirements(id, []VerificationRequirement{r}); err != nil {
		t.Fatal(err)
	}
	// Produce execution without starting the completion loop until its check exists.
	waitTestRun(t, a, id)
	// resultProvider requested exec; execute the saved intent directly.
	s, err := a.executeCommand(context.Background(), id, "proof", []string{"sh", "-c", "printf 'flag{verified}'"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.CheckEvidence(context.Background(), id, r.ID, []string{s.Evidence[0].ID}); err != nil {
		t.Fatal(err)
	}
	a.provider = resultProvider{}
	s, err = a.Run(context.Background(), id)
	if err != nil || s.Result.Status != "success" {
		t.Fatalf("%v %+v", err, s.Result)
	}
	if s.Result.CleanupStatus != "unknown" {
		t.Fatal("model cleanup accepted")
	}
	if _, err = a.SetVerificationRequirements(id, []VerificationRequirement{r}); err == nil {
		t.Fatal("mutable acceptance criteria")
	}
	s, err = store.Update(id, func(s *Snapshot) error { s.Evidence[0].Content += "tampered"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if s.Checks[0].Status == "pass" || s.Result.Status == "success" {
		t.Fatal("stale check survived tampering")
	}
	other := createTestRun(t, a)
	r.Category = "cleanup"
	if _, err = a.SetVerificationRequirements(other, []VerificationRequirement{r}); err == nil {
		t.Fatal("cleanup accepted without explicit probe")
	}
}

type longEvidenceProvider struct {
	n, rounds int
	delay     time.Duration
	proof     string
}

func (p *longEvidenceProvider) Complete(ctx context.Context, r ModelRequest) (ModelResponse, error) {
	p.n++
	if p.delay > 0 {
		select {
		case <-time.After(p.delay):
		case <-ctx.Done():
			return ModelResponse{}, ctx.Err()
		}
	}
	if p.n == 1 {
		return ModelResponse{Content: "I am done prematurely", StopReason: "stop"}, nil
	}
	if p.n <= p.rounds {
		args, _ := json.Marshal(map[string]string{"command": fmt.Sprintf("printf 'checkpoint-%d'", p.n)})
		return ModelResponse{ToolCalls: []ToolCall{{ID: fmt.Sprintf("step-%d", p.n), Name: "exec", Arguments: string(args)}}, StopReason: "tool_calls"}, nil
	}
	if p.n == p.rounds+1 {
		args, _ := json.Marshal(map[string]string{"command": "printf '" + p.proof + "'"})
		return ModelResponse{ToolCalls: []ToolCall{{ID: "final-probe", Name: "exec", Arguments: string(args)}}, StopReason: "tool_calls"}, nil
	}
	if p.n == p.rounds+2 {
		var result ToolResult
		for i := len(r.Messages) - 1; i >= 0; i-- {
			if r.Messages[i].Role == "tool" {
				json.Unmarshal([]byte(r.Messages[i].Content), &result)
				break
			}
		}
		args, _ := json.Marshal(map[string]any{"requirement_id": "whole-task", "evidence_ids": []string{result.EvidenceID}})
		return ModelResponse{ToolCalls: []ToolCall{{ID: "check-final", Name: "evidence_check", Arguments: string(args)}}, StopReason: "tool_calls"}, nil
	}
	return ModelResponse{Content: fixtureReport("Task completed with all checkpoints and final probe."), StopReason: "stop"}, nil
}

func TestLongTaskNoImplicitRoundStop(t *testing.T) {
	p := &longEvidenceProvider{rounds: 24, proof: "all-steps-complete"}
	if text := os.Getenv("TRACE_SOAK_SECONDS"); text != "" {
		seconds, err := strconv.Atoi(text)
		if err != nil || seconds < 60 {
			t.Fatal("soak must be at least 60 seconds")
		}
		p.delay = time.Duration(seconds) * time.Second / time.Duration(p.rounds+3)
	}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	_, err := a.SetVerificationRequirements(id, []VerificationRequirement{{ID: "whole-task", Description: "finish every checkpoint and obtain final probe", Category: "goal", Predicate: EvidencePredicate{Kind: "equals", Value: p.proof}}})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	s, err := a.Run(context.Background(), id)
	if err != nil || s.Run.Status != StatusCompleted || s.Result.Status != "success" || len(s.Requests) != p.rounds+3 {
		t.Fatalf("long task ended early: %v %+v requests=%d", err, s.Run, len(s.Requests))
	}
	if len(s.Actions) != p.rounds+1 || len(s.Checks) != 1 || s.Checks[0].Status != "pass" {
		t.Fatalf("incomplete task actions=%d checks=%+v", len(s.Actions), s.Checks)
	}
	deferred := false
	for _, e := range s.Events {
		deferred = deferred || e.Type == "completion_deferred"
	}
	if !deferred {
		t.Fatal("premature completion was not challenged")
	}
	reloaded, _ := store.Load(id)
	if reloaded.Result.Status != "success" || strings.Contains(reloaded.Run.PauseReason, "budget") {
		t.Fatal("durable result lost")
	}
	t.Logf("long task completed: elapsed=%s requests=%d actions=%d result=%s run=%s", time.Since(started), len(s.Requests), len(s.Actions), s.Result.Status, id)
}
