package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestRoleModelsTreeCostAdmissionUsesActualChildPrice(t *testing.T) {
	a, store := newTestAgent(t, &OpenAIProvider{BaseURL: "https://fixture.invalid", Model: "parent"})
	worker := &OpenAIProvider{BaseURL: "https://worker.invalid", Model: "worker"}
	if err := a.ConfigureModels(map[string]*OpenAIProvider{"worker": worker}, RoleModels{Worker: &ModelSelection{Provider: "worker", Model: "worker"}}); err != nil {
		t.Fatal(err)
	}
	workerID, err := providerIdentity(worker.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	rates := CostConfig{Currency: "USD", Rates: []ModelPrice{
		{Model: "parent", InputPerMillion: "1", OutputPerMillion: "1", Currency: "USD", Source: "fixture", EffectiveAt: "2026-11-05"},
		{Model: "worker", ProviderID: workerID, InputPerMillion: "10", OutputPerMillion: "10", Currency: "USD", Source: "fixture", EffectiveAt: "2026-11-05"},
	}}
	if err := a.ConfigureCosts(rates); err != nil {
		t.Fatal(err)
	}
	root, err := a.Create("root")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{{ID: "task", Goal: "budget child"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ClaimTask(root.Run.ID, "task", "worker-id", 30); err != nil {
		t.Fatal(err)
	}
	child, err := a.CreateConfigured("child", RunConfiguration{role: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	parentAgent, err := a.withRunModel(root.Run)
	if err != nil {
		t.Fatal(err)
	}
	req := ModelRequest{ID: "root-request"}
	parentAgent.stampRequest(&req)
	if _, err = store.Update(root.Run.ID, func(s *Snapshot) error {
		s.Run.TreeBudget = TreeBudget{MaxCost: "0.00005", Currency: "USD"}
		s.Requests = []ModelRequest{req}
		s.Responses = []ResponseRecord{{RequestID: req.ID, Response: ModelResponse{Usage: &TokenUsage{InputTokens: int64Pointer(20), OutputTokens: int64Pointer(20), TotalTokens: int64Pointer(40)}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	selected, err := a.withRunModel(child.Run)
	if err != nil {
		t.Fatal(err)
	}
	next := ModelRequest{ID: "child-request", ContextTokens: 1, MaxOutputTokens: 1}
	selected.stampRequest(&next)
	_, err = store.Update(child.Run.ID, func(s *Snapshot) error {
		s.Run.ParentRunID, s.Run.ParentTaskID = root.Run.ID, "task"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(root.Run.ID, func(s *Snapshot) error {
		task, err := planTask(s, "task")
		if err != nil {
			return err
		}
		if task.WorkerID != "worker-id" || task.Status != "running" {
			return fmt.Errorf("fixture claim changed")
		}
		task.ChildRunID = child.Run.ID
		s.Workers = append(s.Workers, AgentWorker{ID: "worker-id", TaskID: "task", RunID: child.Run.ID, Status: "running", CleanupStatus: "pending"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(child.Run.ID, func(s *Snapshot) error { return selected.admitTreeRequestLocked(s, next) })
	var limit *TreeBudgetError
	if !errors.As(err, &limit) {
		t.Fatalf("actual worker price did not stop tree admission: %v", err)
	}
	if next.Price == nil || next.Price.InputPerMillion != "10" {
		t.Fatal("wrong worker price")
	}
}

func TestRoleModelsConcurrentHTTPAndDurableResume(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Model  string `json:"model"`
			Effort string `json:"reasoning_effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		seen[in.Model+":"+in.Effort]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		content, _ := json.Marshal(fixtureReport("fixture complete"))
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%s},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`, content)
	}))
	defer server.Close()
	base := &OpenAIProvider{BaseURL: server.URL, Model: "base"}
	a, store := newTestAgent(t, base)
	worker := &OpenAIProvider{BaseURL: server.URL + "/worker", Model: "worker", ReasoningEfforts: []string{"low"}}
	roles := RoleModels{Orchestrator: &ModelSelection{Model: "orchestrator"}, Worker: &ModelSelection{Provider: "worker", Model: "worker", ReasoningEffort: "low"}}
	if err := a.ConfigureModels(map[string]*OpenAIProvider{"worker": worker}, roles); err != nil {
		t.Fatal(err)
	}
	costs := CostConfig{Currency: "USD", Rates: []ModelPrice{
		{Model: "orchestrator", InputPerMillion: "2", OutputPerMillion: "4", Currency: "USD", Source: "fixture", EffectiveAt: "2026-11-05"},
		{Model: "worker", InputPerMillion: "1", OutputPerMillion: "2", Currency: "USD", Source: "fixture", EffectiveAt: "2026-11-05"},
	}}
	if err := a.ConfigureCosts(costs); err != nil {
		t.Fatal(err)
	}
	parent, err := a.Create("parent fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPlan(parent.Run.ID, []PlanTask{{ID: "work", Goal: "child fixture"}}); err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), parent.Run.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	// Simultaneous independent root requests exercise provider isolation without
	// asking a model to choose any orchestration strategy.
	errors := make(chan error, 4)
	for i := 0; i < 4; i++ {
		run, err := a.Create(fmt.Sprint("parallel ", i))
		if err != nil {
			t.Fatal(err)
		}
		go func(id string) { _, err := a.Run(context.Background(), id); errors <- err }(run.Run.ID)
	}
	for i := 0; i < 4; i++ {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if _, err = a.WaitWorkers(context.Background(), parent.Run.ID, 10); err != nil {
		t.Fatal(err)
	}
	child, err := store.Load(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Run.ModelSelection == nil || child.Run.ModelSelection.Model != "worker" || len(child.Requests) != 1 {
		t.Fatalf("child binding/requests: %+v %d", child.Run.ModelSelection, len(child.Requests))
	}
	req := child.Requests[0]
	workerID, err := providerIdentity(worker.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if req.ProviderModel != "worker" || req.ReasoningEffort != "low" || req.ProviderID != workerID || req.Price == nil || req.Price.InputPerMillion != "1" {
		t.Fatalf("wrong child request: %+v", req)
	}
	if base.Model != "base" || worker.Model != "worker" || base.ReasoningEffort != "" || worker.ReasoningEffort != "" {
		t.Fatal("shared provider mutated")
	}
	// Reopen the actual Store with changed defaults, then execute the saved root.
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := New(reopened, &OpenAIProvider{BaseURL: server.URL, Model: "changed-default"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = resumed.ConfigureModels(map[string]*OpenAIProvider{"worker": worker}, RoleModels{Orchestrator: &ModelSelection{Model: "changed-role"}, Worker: &ModelSelection{Model: "changed-worker-role"}}); err != nil {
		t.Fatal(err)
	}
	if err = resumed.ConfigureCosts(costs); err != nil {
		t.Fatal(err)
	}
	branch, err := resumed.Branch(child.Run.ID, child.Messages[len(child.Messages)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if branch.Run.ModelSelection == nil || *branch.Run.ModelSelection != *child.Run.ModelSelection {
		t.Fatal("branch lost pinned model")
	}
	if _, err = resumed.QueueMessage(child.Run.ID, "follow_up", "resume original worker"); err != nil {
		t.Fatal(err)
	}
	if _, err = resumed.Run(context.Background(), child.Run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = resumed.Run(context.Background(), parent.Run.ID); err != nil {
		t.Fatal(err)
	}
	report, err := reopened.UsageReport(parent.Run.ID, costs)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Cost.Complete || report.Cost.PricedResponses != 3 || report.Cost.KnownAmount == nil || *report.Cost.KnownAmount != "0.000056000000000000" {
		t.Fatalf("tree prices: %+v", report.Cost)
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["orchestrator:"] != 5 || seen["worker:low"] != 2 || len(seen) != 2 {
		t.Fatalf("wire routes: %+v", seen)
	}
}

func TestRoleModelsValidationOverridesLegacyAndFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "fixture provider failure", 400) }))
	defer server.Close()
	a, store := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "base", ReasoningEfforts: []string{"low"}})
	for _, selection := range []ModelSelection{{Provider: "missing", Model: "x"}, {Model: "base", ReasoningEffort: "high"}, {Model: "other", ReasoningEffort: "low"}, {}} {
		if _, err := a.CreateConfigured("reject", RunConfiguration{ModelSelection: &selection}); err == nil {
			t.Fatalf("accepted unsupported route %+v", selection)
		}
	}
	if err := a.ConfigureModels(nil, RoleModels{Orchestrator: &ModelSelection{Model: "role"}}); err != nil {
		t.Fatal(err)
	}
	manual, err := a.CreateConfigured("manual", RunConfiguration{ModelSelection: &ModelSelection{Model: "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	if manual.Run.ModelSelection.Model != "manual" {
		t.Fatal("role overwrote manual selection")
	}
	if _, err = a.Run(context.Background(), manual.Run.ID); err == nil {
		t.Fatal("provider failure hidden")
	}
	failed, err := store.Load(manual.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed.Requests) == 0 || failed.Requests[0].ProviderModel != "manual" || failed.Run.ModelSelection.Model != "manual" {
		t.Fatal("failed request lost binding")
	}
	drift, err := New(store, &OpenAIProvider{BaseURL: server.URL + "/changed", Model: "base"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = drift.Run(context.Background(), manual.Run.ID); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("endpoint drift accepted: %v", err)
	}
	imageAgent, _ := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "vision", ImageInput: true})
	if _, err := imageAgent.CreateConfigured("reject inferred capability", RunConfiguration{ModelSelection: &ModelSelection{Model: "unknown"}}); err == nil {
		t.Fatal("image capability inherited by unknown model")
	}
	pinned, err := imageAgent.Create("pinned vision")
	if err != nil {
		t.Fatal(err)
	}
	noImage, err := New(imageAgent.store, &OpenAIProvider{BaseURL: server.URL, Model: "vision"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = noImage.withRunModel(pinned.Run); err == nil {
		t.Fatal("image capability drift accepted")
	}
	legacy, legacyStore := newTestAgent(t, successProvider{})
	run, err := legacy.Create("legacy fixture")
	if err != nil {
		t.Fatal(err)
	}
	if run.Run.ModelSelection != nil {
		t.Fatal("custom legacy provider got an invented identity")
	}
	if _, err = legacy.Run(context.Background(), run.Run.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := legacyStore.Load(run.Run.ID)
	if err != nil || loaded.Run.ModelSelection != nil {
		t.Fatal("legacy identity changed", err)
	}
}

func TestRoleModelsCapabilitiesSurviveDefaultChangeWithoutBorrowing(t *testing.T) {
	a, store := newTestAgent(t, &OpenAIProvider{BaseURL: "https://fixture.invalid", Model: "A", ImageInput: true, ReasoningEfforts: []string{"low"}})
	run, err := a.CreateConfigured("pin A", RunConfiguration{ModelSelection: &ModelSelection{Model: "A", ReasoningEffort: "low"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		caps map[string]ModelCapabilities
		ok   bool
	}{
		{"retained A capability", map[string]ModelCapabilities{"A": {ImageInput: true, ReasoningEfforts: []string{"low"}}}, true},
		{"removed A", nil, false},
		{"revoked image", map[string]ModelCapabilities{"A": {ReasoningEfforts: []string{"low"}}}, false},
		{"revoked reasoning", map[string]ModelCapabilities{"A": {ImageInput: true}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// B's capabilities must never authorize A. A is resolved through its own
			// explicit declaration after the service's default changes.
			restarted, err := New(store, &OpenAIProvider{BaseURL: "https://fixture.invalid", Model: "B", ImageInput: true, ReasoningEfforts: []string{"low"}, ModelCapabilities: tc.caps}, "")
			if err != nil {
				t.Fatal(err)
			}
			bound, err := restarted.withRunModel(run.Run)
			if (err == nil) != tc.ok {
				t.Fatalf("restore = %v, want success %t", err, tc.ok)
			}
			if tc.ok {
				p := bound.provider.(*OpenAIProvider)
				if p.Model != "A" || !p.ImageInput || p.ReasoningEffort != "low" {
					t.Fatalf("route drifted: %+v", p)
				}
			}
		})
	}
}
