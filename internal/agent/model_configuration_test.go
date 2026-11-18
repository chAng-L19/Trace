package agent

import (
	"sync"
	"testing"
)

func TestModelConfigurationSnapshotKeepsResourcesAndPinnedRoutes(t *testing.T) {
	legacy := &OpenAIProvider{BaseURL: "https://legacy.invalid/v1", Model: "legacy", APIKey: "legacy-fixture"}
	a, _ := newTestAgent(t, legacy)
	old := &OpenAIProvider{BaseURL: "https://profile.invalid/v1", Model: "model-a", APIKey: "old-fixture", ReasoningEfforts: []string{"high"}}
	roles := RoleModels{Orchestrator: &ModelSelection{Provider: "profile-a", Model: "model-a", ReasoningEffort: "high"}}
	if err := a.ConfigureModels(map[string]*OpenAIProvider{"profile-a": old}, roles); err != nil {
		t.Fatal(err)
	}
	run, err := a.Create("keep model binding")
	if err != nil {
		t.Fatal(err)
	}
	rotated := copyModelProvider(old)
	rotated.APIKey = "new-fixture"
	b := &OpenAIProvider{BaseURL: "https://profile-b.invalid/v1", Model: "model-b"}
	next, err := a.WithModelConfiguration(legacy, map[string]*OpenAIProvider{"profile-a": rotated, "profile-b": b}, RoleModels{Orchestrator: &ModelSelection{Provider: "profile-b", Model: "model-b"}})
	if err != nil {
		t.Fatal(err)
	}
	if next == a || next.store != a.store || next.registry != a.registry || next.workers != a.workers || next.mcpState != a.mcpState || a.mcpState.closed {
		t.Fatal("model publication replaced or closed runtime resources")
	}
	currentOld, err := a.withRunModel(run.Run)
	if err != nil || currentOld.provider.(*OpenAIProvider).APIKey != "old-fixture" {
		t.Fatalf("captured execution changed: %v", err)
	}
	resumed, err := next.withRunModel(run.Run)
	if err != nil || resumed.provider.(*OpenAIProvider).APIKey != "new-fixture" || resumed.provider.(*OpenAIProvider).Model != "model-a" {
		t.Fatalf("resume lost stable route or rotated credential: %v", err)
	}
	created, err := next.Create("new default")
	if err != nil || created.Run.ModelSelection == nil || created.Run.ModelSelection.Provider != "profile-b" {
		t.Fatalf("new run did not select new profile: %v", err)
	}
	rotated.APIKey, rotated.ReasoningEfforts[0], b.Model = "mutated", "low", "mutated"
	roles.Orchestrator.Model = "mutated"
	if next.modelProviders["profile-a"].APIKey != "new-fixture" || next.modelProviders["profile-a"].ReasoningEfforts[0] != "high" || next.modelProviders["profile-b"].Model != "model-b" || a.roleModels.Orchestrator.Model != "model-a" {
		t.Fatal("model configuration retained mutable caller input")
	}
	changedEndpoint := copyModelProvider(next.modelProviders["profile-a"])
	changedEndpoint.BaseURL = "https://changed.invalid/v1"
	changed, err := next.WithModelConfiguration(legacy, map[string]*OpenAIProvider{"profile-a": changedEndpoint}, RoleModels{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = changed.withRunModel(run.Run); err == nil {
		t.Fatal("old run was rebound to a changed endpoint")
	}
	stored, err := a.store.Load(run.Run.ID)
	if err != nil || *stored.Run.ModelSelection != *run.Run.ModelSelection {
		t.Fatalf("profile switch rewrote persisted run identity: %v", err)
	}
}

func TestModelConfigurationInvalidSnapshotDoesNotMutateAgent(t *testing.T) {
	a, _ := newTestAgent(t, &OpenAIProvider{BaseURL: "https://old.invalid", Model: "old"})
	before := a.modelProviders["default"]
	for _, p := range []*OpenAIProvider{{BaseURL: "bad-url", Model: "bad"}, nil} {
		next, err := a.WithModelConfiguration(p, nil, RoleModels{})
		if err == nil || next != nil || a.modelProviders["default"] != before || a.mcpState.closed {
			t.Fatal("invalid default model snapshot changed published Agent")
		}
	}
	if _, err := a.WithModelConfiguration(nil, map[string]*OpenAIProvider{"default": {BaseURL: "https://reserved.invalid", Model: "bad"}}, RoleModels{}); err == nil {
		t.Fatal("reserved named provider was accepted")
	}
}

func TestModelConfigurationConcurrentCopiesKeepCapturedRoute(t *testing.T) {
	a, _ := newTestAgent(t, &OpenAIProvider{BaseURL: "https://old.invalid", Model: "old", APIKey: "old-fixture"})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_, provider, err := a.resolveModel(nil, false)
				if err != nil || provider.(*OpenAIProvider).APIKey != "old-fixture" {
					t.Error("captured route changed during concurrent copy")
					return
				}
				next, err := a.WithModelConfiguration(&OpenAIProvider{BaseURL: "https://old.invalid", Model: "old", APIKey: "new-fixture"}, nil, RoleModels{})
				if err != nil || next.modelProviders["default"].APIKey != "new-fixture" {
					t.Error("model copy failed")
					return
				}
			}
		}()
	}
	wg.Wait()
}
