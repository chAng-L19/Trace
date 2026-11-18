package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func longHistory(t *testing.T, a *Agent, store *Store, useDefault ...bool) (string, []Message) {
	t.Helper()
	a.prompt, a.promptID = "fixture system", promptHash("fixture system")
	a.registry = &ToolRegistry{tools: map[string]RegisteredTool{}}
	config, repetitions := ContextConfig{WindowTokens: 7000, OutputReserve: 500}, 100
	if len(useDefault) > 0 && useDefault[0] {
		config, repetitions = DefaultContextConfig(), 3000
	}
	if err := a.ConfigureContext(config); err != nil {
		t.Fatal(err)
	}
	id := createTestRun(t, a)
	s, err := store.Update(id, func(s *Snapshot) error {
		s.Messages = []Message{{Role: "user", Content: "objective: exact evidence"}}
		for i := 0; i < 8; i++ {
			call := fmt.Sprintf("old-%d", i)
			s.Messages = append(s.Messages, Message{Role: "assistant", Content: "inspect", ToolCalls: []ToolCall{{ID: call, Name: "external", Arguments: "{}"}}}, Message{Role: "tool", ToolCallID: call, Content: strings.Repeat("observation ", repetitions)})
		}
		s.Messages = append(s.Messages, Message{Role: "user", Content: "latest constraint: preserve identifiers"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return id, s.Messages
}

func TestSemanticHTTPFixtureBudgetResumeAndArchive(t *testing.T) {
	var requests [][]wireMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []wireMessage    `json:"messages"`
			Tools    []ToolDefinition `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		requests = append(requests, input.Messages)
		answer := "final response"
		if input.Messages[0].Content == summaryPrompt {
			if len(input.Tools) != 0 {
				t.Error("summary requested tools")
			}
			answer = "Objective: exact evidence. Tool observations old-0 onward retained. Pending: preserve identifiers."
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": answer}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 19, "completion_tokens": 7, "total_tokens": 26}})
	}))
	defer server.Close()
	a, store := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "summary-fixture"})
	id, original := longHistory(t, a, store, true)
	if EstimateContextTokens(original, nil) <= 250000 {
		t.Fatal("fixture did not cross default 250k window")
	}
	one := 1
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &one})
	paused, err := a.Run(context.Background(), id)
	if err != nil || paused.Run.Status != StatusPaused || len(paused.Compactions) != 1 || paused.Compactions[0].Status != "completed" {
		t.Fatalf("summary budget boundary: %v %+v", err, paused.Run)
	}
	if len(paused.Requests) != 1 || len(paused.Responses) != 1 || *paused.Run.Usage.KnownTotalTokens != 26 {
		t.Fatal("summary escaped existing usage/budget ledger")
	}
	if len(paused.Messages) != len(original) || messageHash(paused.Messages) != messageHash(original) {
		t.Fatal("summary rewrote original archive")
	}
	firstID := paused.Compactions[0].ID
	// Fresh Agent/Store uses the durable summary instead of asking for it again.
	reopened, _ := NewStore(store.root)
	restarted, _ := New(reopened, a.provider, "")
	restarted.registry = a.registry
	twenty := 20
	restarted.ConfigureBudget(BudgetOverride{MaxModelRequests: &twenty})
	final, err := restarted.Run(context.Background(), id)
	if err != nil || final.Run.Status != StatusCompleted {
		t.Fatalf("resume: %v %+v", err, final.Run)
	}
	if final.Compactions[0].ID != firstID || messageHash(final.Messages[:len(original)]) != messageHash(original) {
		t.Fatal("resume changed source history or summary")
	}
	if final.Run.Usage.ModelRequests != len(requests) || *final.Run.Usage.KnownTotalTokens != int64(26*len(requests)) {
		t.Fatal("usage was not cumulative")
	}
	last := final.Requests[len(final.Requests)-1]
	if last.ContextTokens > 250000-16384 || last.CompactionID == "" {
		t.Fatal("unbounded projection")
	}
	calls := map[string]bool{}
	for _, m := range last.Messages {
		for _, call := range m.ToolCalls {
			calls[call.ID] = true
		}
		if m.Role == "tool" && !calls[m.ToolCallID] {
			t.Fatal("orphan tool result")
		}
	}
	history, observations := splitContextObservations(id, last.Messages)
	if len(observations) == 0 || len(history) == 0 || history[len(history)-1].Content != "latest constraint: preserve identifiers" {
		t.Fatal("latest instruction lost")
	}
}

func TestSemanticFailureAndSourceMutation(t *testing.T) {
	summaries := 0
	p := settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		if request.Purpose != "context_summary" {
			return ModelResponse{Content: "original observations retained"}, nil
		}
		summaries++
		if summaries == 1 {
			return ModelResponse{Content: "partial summary", StopReason: "length"}, nil
		}
		if summaries == 2 && !strings.Contains(request.Messages[0].Content, "previous summary was rejected") {
			t.Error("summary retry omitted correction feedback")
		}
		return ModelResponse{Content: "Objective and exact identifiers retained; original tool observations remain in the archive."}, nil
	})
	a, store := newTestAgent(t, p)
	id, original := longHistory(t, a, store)
	s, err := a.Run(context.Background(), id)
	if err != nil || s.Run.Status != StatusCompleted || summaries < 2 || s.Compactions[0].Status != "failed" || messageHash(s.Messages[:len(original)]) != messageHash(original) || s.Responses[0].Error == "" {
		t.Fatalf("summary correction stopped or rewrote history: %v %+v", err, s.Run)
	}
	// A saved summary with a stale source hash is not trusted for projection.
	s.Compactions[0].Status, s.Compactions[0].Summary = "completed", "stale summary"
	s.Messages[0].Content = "changed source constraint"
	_, _, _, projectionErr := projectContext(&s, nil, "")
	plan, ok := projectionErr.(*summaryNeeded)
	if !ok || strings.Contains(plan.messages[1].Content, "stale summary") {
		t.Fatal("mutated source reused stale summary")
	}
}

func TestSemanticOversizedLatestTurnDoesNotSpendRequest(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id, _ := longHistory(t, a, store)
	store.Update(id, func(s *Snapshot) error { s.Messages[len(s.Messages)-1].Content = strings.Repeat("x", 8000); return nil })
	s, err := a.Run(context.Background(), id)
	if err != nil || s.Run.Status != StatusPaused || len(s.Requests) != 0 {
		t.Fatal("oversized current turn spent summary request")
	}
}

func TestSemanticInterruptedRequestKeepsBudgetOnRestart(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id, _ := longHistory(t, a, store)
	_, err := store.Update(id, func(s *Snapshot) error {
		_, _, _, err := projectContext(s, nil, "")
		plan, ok := err.(*summaryNeeded)
		if !ok {
			return fmt.Errorf("fixture needs compaction: %v", err)
		}
		s.Run.RequestSeq = 1
		plan.record.ID, plan.record.RequestID = "interrupted-compact", id+"-0001"
		s.Compactions = append(s.Compactions, plan.record)
		s.Requests = append(s.Requests, ModelRequest{ID: plan.record.RequestID, Messages: plan.messages, CompactionID: plan.record.ID})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	one := 1
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &one})
	s, err := a.Run(context.Background(), id)
	if err != nil || s.Run.Status != StatusPaused || len(s.Requests) != 1 || s.Compactions[0].Status != "interrupted" {
		t.Fatal("restart replayed or lost pending request budget")
	}
	twenty := 20
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &twenty})
	s, err = a.Run(context.Background(), id)
	if err != nil || s.Run.Status != StatusCompleted || len(s.Responses) != len(s.Requests)-1 || s.Requests[1].ID == s.Requests[0].ID {
		t.Fatalf("summary retry ledger invalid: %v %+v", err, s.Run)
	}
}

func TestSkillsOrderedRootsAndDefaultPrecedence(t *testing.T) {
	root := t.TempDir()
	write := func(path, description string) {
		t.Helper()
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "SKILL.md"), []byte("---\nname: same\ndescription: "+description+"\n---\nbody"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	first, second := filepath.Join(root, "explicit"), filepath.Join(root, "user")
	write(first, "explicit wins")
	write(second, "user loses")
	skills, err := DiscoverSkills([]string{first, second, first})
	if err != nil || len(skills) != 1 || skills[0].Description != "explicit wins" || skills[0].Source != first {
		t.Fatalf("priority: %v %+v", err, skills)
	}
	a, _ := newTestAgent(t, successProvider{})
	if err := a.ConfigureSkills([]string{first, second}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.skillCatalog(), first) || strings.Contains(a.skillCatalog(), "user loses") {
		t.Fatal("catalog missing winning provenance")
	}
	project, userhome := filepath.Join(root, "project"), filepath.Join(root, "home")
	write(filepath.Join(project, ".trace", "skills", "same"), "project wins")
	write(filepath.Join(userhome, ".trace", "skills", "same"), "home loses")
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(oldWD); err != nil {
			t.Error(err)
		}
	})
	t.Setenv("USERPROFILE", userhome)
	t.Setenv("HOME", userhome)
	if err := a.ConfigureSkills(nil); err != nil {
		t.Fatal(err)
	}
	if len(a.Skills()) != 1 || a.Skills()[0].Description != "project wins" {
		t.Fatal("project did not override user default")
	}
	if err := a.ConfigureSkills([]string{first}); err != nil {
		t.Fatal(err)
	}
	if a.Skills()[0].Description != "explicit wins" {
		t.Fatal("explicit path did not override defaults")
	}
}
