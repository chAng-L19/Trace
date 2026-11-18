package web

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"trace/internal/agent"
)

func TestStageModelsConfigPersistenceAndPlanAPI(t *testing.T) {
	s, a := fixture(t, &agent.OpenAIProvider{BaseURL: "https://fixture.invalid", Model: "base"})
	original := Config{Provider: ProviderConfig{URL: "https://fixture.invalid", Model: "base", APIKey: "fixture-secret"}, RoleModels: agent.RoleModels{StageRoutes: []agent.ModelRoute{{Provider: "default", Model: "base"}}}}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := saveConfig(path, original); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path, Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Config.RoleModels.StageRoutes) != 1 || loaded.Config.RoleModels.StageRoutes[0] != original.RoleModels.StageRoutes[0] {
		t.Fatal("stage policy lost in persisted config")
	}
	if err := a.ConfigureModels(nil, loaded.Config.RoleModels); err != nil {
		t.Fatal(err)
	}
	root, err := a.Create("stage API fixture")
	if err != nil {
		t.Fatal(err)
	}
	selection := map[string]any{"provider": "default", "model": "base"}
	task := map[string]any{"id": "work", "goal": "review", "model_selection": selection}
	response := request(t, s.Handler(), "PUT", "/api/runs/"+root.Run.ID+"/plan", map[string]any{"tasks": []any{task}}, 200)
	var saved agent.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Plan.Tasks[0].ModelSelection == nil || saved.Plan.Tasks[0].ModelSelection.Model != "base" {
		t.Fatal("API dropped stage route")
	}
	selection["model"] = "not-allowed"
	request(t, s.Handler(), "PUT", "/api/runs/"+root.Run.ID+"/plan", map[string]any{"tasks": []any{task}}, 409)
	selection["model"] = "base"
	task["required_image_input"] = true
	request(t, s.Handler(), "PUT", "/api/runs/"+root.Run.ID+"/plan", map[string]any{"tasks": []any{task}}, 409)
	delete(task, "required_image_input")
	task["model_binding"] = selection
	request(t, s.Handler(), "PUT", "/api/runs/"+root.Run.ID+"/plan", map[string]any{"tasks": []any{task}}, 409)
}
