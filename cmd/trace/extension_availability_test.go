package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"trace/internal/agent"
	"trace/internal/mcp"
	"trace/internal/web"
)

func TestCLIUsesSavedExtensionAvailability(t *testing.T) {
	isolateServiceConfigEnv(t)
	root := t.TempDir()
	t.Setenv("TRACE_DATA_DIR", root)
	t.Setenv("TRACE_CONFIG_FILE", "")
	configPath := serviceConfigPath()
	if configPath != filepath.Join(root, "service", "web-config.json") {
		t.Fatal("CLI and Web default config paths differ")
	}
	configPath = filepath.Join(root, "custom.json")
	t.Setenv("TRACE_CONFIG_FILE", configPath)
	skillPath := filepath.Join(root, "skill")
	if err := os.MkdirAll(skillPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillPath, "SKILL.md"), []byte("---\nname: fixture\ndescription: metadata\n---\nbody"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRACE_SKILL_PATHS", skillPath)
	t.Setenv("TRACE_MCP_CONFIG", "")
	server := mcp.Config{Name: "fixture", Command: filepath.Join(root, "does-not-exist")}
	raw, err := json.Marshal([]mcp.Config{server})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRACE_MCP_SERVERS", string(raw))
	store, err := agent.NewStore(filepath.Join(root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	// Missing service config preserves the historical enabled-by-default state.
	before, err := configuredAgent(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := before.ReadModelSkill("fixture"); err != nil {
		t.Fatal(err)
	}
	server.Disabled = true
	c := web.Config{SkillPaths: []string{skillPath}, DisabledSkills: []string{"fixture"}, MCP: []mcp.Config{server}}
	raw, err = json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	after, err := configuredAgent(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := after.ReadModelSkill("fixture"); err == nil {
		t.Fatal("CLI ignored the saved Skill disable state")
	}
	if _, err := after.ReadSkill("fixture"); err != nil {
		t.Fatal("CLI disabled Skill lost management read")
	}
	loaded, err := cliServiceConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := registerConfiguredMCP(context.Background(), after, loaded.Config.MCP); err != nil {
		t.Fatalf("disabled MCP attempted to start: %v", err)
	}
	for _, tool := range after.Registry().Tools() {
		if tool.Adapter == "mcp" {
			t.Fatalf("disabled MCP exposed tool: %s", tool.Definition.Function.Name)
		}
	}
	_, clients, err := after.Registry().Clone(context.Background())
	for _, client := range clients {
		_ = client.Close()
	}
	if err != nil || len(clients) != 0 {
		t.Fatalf("child clone started disabled MCP: clients=%d err=%v", len(clients), err)
	}
}
