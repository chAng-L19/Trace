package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"trace/internal/agent"
)

func TestVerificationReviewBlocksReconfigureAndCloseCancels(t *testing.T) {
	entered := make(chan struct{})
	calls := 0
	s, a := fixture(t, providerFunc(func(ctx context.Context, r agent.ModelRequest) (agent.ModelResponse, error) {
		if r.Purpose == "evidence_review" {
			close(entered)
			<-ctx.Done()
			return agent.ModelResponse{}, ctx.Err()
		}
		calls++
		if calls == 1 {
			return agent.ModelResponse{ToolCalls: []agent.ToolCall{{ID: "probe", Name: "exec", Arguments: `{"command":"printf observed-marker"}`}}, StopReason: "tool_calls"}, nil
		}
		body, _ := json.Marshal(map[string]any{"trace_report": agent.ReportText{ZhCN: "执行已经返回观察标记；本记录只描述此项检查，影响与清理状态仍需独立核对。", En: "The execution returned the observed marker. This record describes only that check; impact and cleanup remain subject to independent verification."}})
		return agent.ModelResponse{Content: "observed\n\n```json\n" + string(body) + "\n```", StopReason: "stop"}, nil
	}))
	// This pre-role review lifecycle fixture produces its own execution evidence.
	run, err := a.Store().Create("observe fixture", agent.DefaultPrompt, a.PromptHash())
	if err != nil {
		t.Fatal(err)
	}
	run, err = a.Run(context.Background(), run.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Evidence) == 0 {
		t.Fatal("probe did not produce evidence")
	}
	_, err = a.SubmitFinding(run.Run.ID, agent.Finding{ID: "review", Title: "observation", Severity: "info", RootCause: "fixture", Impact: "marker returned", Remediation: "fixture", ReproductionSteps: []string{"run probe"}, EvidenceIDs: []string{run.Evidence[0].ID}, Predicate: agent.EvidencePredicate{Kind: "contains", Value: "observed-marker"}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/api/runs/"+run.Run.ID+"/findings/review/review", bytes.NewBufferString(`{}`)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handlerDone := make(chan struct{})
	go func() { s.Handler().ServeHTTP(w, r); close(handlerDone) }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("review never reached provider")
	}
	s.mu.Lock()
	active := s.active[run.Run.ID]
	configErr := s.configurationReady()
	s.mu.Unlock()
	if !active || configErr == nil {
		t.Fatal("operator review did not block agent replacement")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("server Close did not cancel operator review")
	}
	<-handlerDone
	if w.Code == 200 {
		t.Fatal("cancelled review returned success")
	}
	latest, err := a.Store().Load(run.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Run.Status != agent.StatusPaused || !latest.Run.BudgetActiveSince.IsZero() {
		t.Fatalf("shutdown pause was lost: %+v", latest.Run)
	}
	s.mu.Lock()
	active = s.active[run.Run.ID]
	s.mu.Unlock()
	if active {
		t.Fatal("review lifecycle ownership leaked")
	}
}
