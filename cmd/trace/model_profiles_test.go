package main

import (
	"context"
	"testing"

	"trace/internal/agent"
	"trace/internal/web"
)

func TestConfiguredAgentActiveProviderPreservesLegacyAndExplicitRoutes(t *testing.T) {
	isolateServiceConfigEnv(t)
	store, err := agent.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c := web.Config{
		Provider: web.ProviderConfig{URL: "https://legacy.invalid/v1", Model: "legacy"},
		Providers: map[string]web.ProviderConfig{
			"profile-a": {URL: "https://profile-a.invalid/v1", Model: "model-a"},
			"profile-b": {URL: "https://profile-b.invalid/v1", Model: "model-b"},
		},
		ActiveProvider: "profile-b",
		Context:        agent.DefaultContextConfig(),
	}
	a, err := buildConfiguredAgent(context.Background(), store, c, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.CloseMCP()
	created, err := a.Create("new active model")
	if err != nil || created.Run.ModelSelection == nil || created.Run.ModelSelection.Provider != "profile-b" || created.Run.ModelSelection.Model != "model-b" {
		t.Fatalf("CLI/shared builder did not use active profile: %v", err)
	}
	legacy, err := a.CreateConfigured("legacy explicit route", agent.RunConfiguration{ModelSelection: &agent.ModelSelection{Provider: "default", Model: "legacy"}})
	if err != nil || legacy.Run.ModelSelection.Provider != "default" || legacy.Run.ModelSelection.Model != "legacy" {
		t.Fatalf("active profile overwrote legacy default: %v", err)
	}
	c.RoleModels.Orchestrator = &agent.ModelSelection{Provider: "profile-a", Model: "model-a"}
	explicit, err := buildConfiguredAgent(context.Background(), store, c, false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer explicit.CloseMCP()
	overridden, err := explicit.Create("explicit role wins")
	if err != nil || overridden.Run.ModelSelection.Provider != "profile-a" {
		t.Fatalf("active profile replaced explicit role: %v", err)
	}
	c.ActiveProvider = "missing"
	if _, err = buildConfiguredAgent(context.Background(), store, c, false, true); err == nil {
		t.Fatal("missing active profile was accepted")
	}
}
