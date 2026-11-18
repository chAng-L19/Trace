package web

import (
	"encoding/json"
	"strings"
	"testing"
	"trace/internal/agent"
)

func TestRoleModelsConfigSecretRoundTrip(t *testing.T) {
	original := Config{Provider: ProviderConfig{APIKey: "default-fixture-secret"}, Providers: map[string]ProviderConfig{"fast": {URL: "https://fixture.invalid", Model: "worker", APIKey: "worker-fixture-secret", ReasoningEfforts: []string{"low"}}}, RoleModels: agent.RoleModels{Worker: &agent.ModelSelection{Provider: "fast", Model: "worker", ReasoningEffort: "low"}}}
	redacted := redactConfig(original)
	b, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "fixture-secret") || redacted.Providers["fast"].APIKey != hiddenSecret || original.Providers["fast"].APIKey != "worker-fixture-secret" {
		t.Fatal("secret leak or source mutation")
	}
	restoreSecrets(&redacted, original)
	if redacted.Provider.APIKey != original.Provider.APIKey || redacted.Providers["fast"].APIKey != original.Providers["fast"].APIKey || redacted.RoleModels.Worker.Model != "worker" {
		t.Fatal("secret/role round trip failed")
	}
	redacted.Providers["fast"] = ProviderConfig{Model: "worker"}
	restoreSecrets(&redacted, original)
	if redacted.Providers["fast"].APIKey != "" {
		t.Fatal("explicit key clearing ignored")
	}
}

func TestRoleModelsCreateAPIManualOverride(t *testing.T) {
	s, a := fixture(t, nil)
	if err := a.ConfigureModels(map[string]*agent.OpenAIProvider{"named": {BaseURL: "https://fixture.invalid", Model: "configured"}}, agent.RoleModels{Orchestrator: &agent.ModelSelection{Provider: "named", Model: "configured"}}); err != nil {
		t.Fatal(err)
	}
	response := request(t, s.Handler(), "POST", "/api/runs", map[string]any{"goal": "manual model", "model_selection": map[string]string{"provider": "named", "model": "manual"}}, 201)
	var created agent.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Run.ModelSelection == nil || created.Run.ModelSelection.Model != "manual" || created.Run.ModelSelection.ProviderID == "" {
		t.Fatalf("API lost model override: %+v", created.Run.ModelSelection)
	}
}
