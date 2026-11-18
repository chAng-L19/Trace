package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"trace/internal/agent"
	"trace/internal/localtool"
	"trace/internal/mcp"
)

func modelConfigFixture(t *testing.T) (*Server, *agent.Agent) {
	t.Helper()
	s, _ := fixture(t, nil)
	s.config.Provider = ProviderConfig{URL: "https://legacy.invalid/v1", Model: "legacy", APIKey: "legacy-fixture"}
	s.config.Providers = map[string]ProviderConfig{
		"a": {URL: "https://a.invalid/v1", Model: "model-a", APIKey: "a-fixture", ReasoningEfforts: []string{"high"}},
		"b": {URL: "https://b.invalid/v1", Model: "model-b", APIKey: "b-fixture"},
	}
	s.config.ActiveProvider = "a"
	p, providers, roles, err := s.config.ModelConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	s.a, err = s.a.WithModelConfiguration(p, providers, roles)
	if err != nil {
		t.Fatal(err)
	}
	if err = saveConfig(s.options.ConfigPath, s.config); err != nil {
		t.Fatal(err)
	}
	return s, s.a
}

func TestModelConfigurationSaveDuringActiveKeepsRuntime(t *testing.T) {
	s, captured := modelConfigFixture(t)
	oldRun, err := captured.Create("existing profile a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.store.Update(oldRun.Run.ID, func(s *agent.Snapshot) error { s.Run.Status = agent.StatusWaitingTool; return nil }); err != nil {
		t.Fatal(err)
	}
	s.active[oldRun.Run.ID] = true
	defer delete(s.active, oldRun.Run.ID)
	builds := 0
	s.options.BuildAgent = func(context.Context, Config) (*agent.Agent, error) {
		builds++
		return nil, errors.New("unrelated MCP unavailable")
	}
	next := redactConfig(s.config)
	next.ActiveProvider = "b"
	a := next.Providers["a"]
	a.APIKey = "rotated-fixture"
	next.Providers["a"] = a
	w := request(t, s.Handler(), "PUT", "/api/config", next, 200)
	var result struct {
		ModelOnly bool `json:"model_only"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.ModelOnly || builds != 0 || s.a == captured || s.a.Store() != captured.Store() || s.a.Registry() != captured.Registry() {
		t.Fatalf("model save rebuilt runtime: %v builds=%d", err, builds)
	}
	if strings.Contains(w.Body.String(), "rotated-fixture") || strings.Contains(w.Body.String(), "legacy-fixture") || s.config.Provider.APIKey != "legacy-fixture" || s.config.Providers["a"].APIKey != "rotated-fixture" {
		t.Fatal("secret preservation/redaction failed")
	}
	newRun, err := s.a.Create("new profile b")
	if err != nil || newRun.Run.ModelSelection.Provider != "b" {
		t.Fatalf("new profile was not selected: %v", err)
	}
	oldDefault, err := captured.Create("captured default still a")
	if err != nil || oldDefault.Run.ModelSelection.Provider != "a" {
		t.Fatalf("captured Agent default changed: %v", err)
	}
	stored, err := s.store.Load(oldRun.Run.ID)
	if err != nil || stored.Run.Status != agent.StatusWaitingTool || *stored.Run.ModelSelection != *oldRun.Run.ModelSelection {
		t.Fatalf("model save rewrote existing execution: %v", err)
	}
	loaded, err := LoadConfig(s.options.ConfigPath, Config{}, "")
	if err != nil || loaded.Config.ActiveProvider != "b" || loaded.Config.Providers["a"].APIKey != "rotated-fixture" || loaded.Config.Revision != s.config.Revision {
		t.Fatalf("profile did not survive persisted reload: %v", err)
	}
	if info, err := os.Stat(s.options.ConfigPath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("config permissions: %v", err)
	}
}

func TestModelConfigurationMixedChangesUseFullBoundary(t *testing.T) {
	changes := map[string]func(*Config){
		"context":         func(c *Config) { c.Context.WindowTokens++ },
		"costs":           func(c *Config) { c.Costs.Currency = "USD" },
		"search":          func(c *Config) { c.SearchSources = map[string]agent.SearchSourceConfig{"fofa": {}} },
		"mcp":             func(c *Config) { c.MCP = []mcp.Config{{Name: "fixture"}} },
		"local_tools":     func(c *Config) { c.LocalTools = []localtool.Manifest{{Name: "fixture"}} },
		"skill_paths":     func(c *Config) { c.SkillPaths = []string{"fixture"} },
		"disabled_skills": func(c *Config) { c.DisabledSkills = []string{"fixture"} },
		"integrations":    func(c *Config) { c.Integrations = []Integration{{}} },
		"prompt":          func(c *Config) { c.PromptPath = "fixture" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			s, original := modelConfigFixture(t)
			next := redactConfig(s.config)
			next.ActiveProvider = "b"
			change(&next)
			builds := 0
			s.options.BuildAgent = func(context.Context, Config) (*agent.Agent, error) {
				builds++
				return nil, errors.New("full rebuild fixture")
			}
			s.active["busy"] = true
			request(t, s.Handler(), "PUT", "/api/config", next, 409)
			delete(s.active, "busy")
			if builds != 0 || s.a != original || s.config.ActiveProvider != "a" {
				t.Fatal("mixed change bypassed running boundary")
			}
			request(t, s.Handler(), "PUT", "/api/config", next, 409)
			if builds != 1 || s.a != original || s.config.ActiveProvider != "a" {
				t.Fatal("mixed change did not retain full rebuild path")
			}
		})
	}
}

func TestModelConfigurationFailuresPreservePublishedState(t *testing.T) {
	for _, mode := range []string{"missing-profile", "invalid-url", "invalid-role", "unmatched-secret", "save-failure", "busy", "stale"} {
		t.Run(mode, func(t *testing.T) {
			s, original := modelConfigFixture(t)
			before, err := os.ReadFile(s.options.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			path, revision := s.options.ConfigPath, s.config.Revision
			next := redactConfig(s.config)
			next.ActiveProvider = "b"
			status, match := 409, `"`+revision+`"`
			switch mode {
			case "missing-profile":
				next.ActiveProvider = "missing"
			case "invalid-url":
				p := next.Providers["b"]
				p.URL = "invalid"
				next.Providers["b"] = p
			case "invalid-role":
				next.RoleModels.Worker = &agent.ModelSelection{Provider: "a", Model: "model-a", ReasoningEffort: "unsupported"}
			case "unmatched-secret":
				next.Providers["new"] = next.Providers["b"]
				status = 400
			case "save-failure":
				s.options.ConfigPath = t.TempDir()
			case "busy":
				s.configuring = true
			case "stale":
				match = `"stale"`
			}
			w := revisionRequest(t, s.Handler(), "PUT", "/api/config", match, next)
			s.configuring = false
			after, err := os.ReadFile(path)
			if err != nil || w.Code != status || s.a != original || s.config.Revision != revision || !bytes.Equal(before, after) {
				t.Fatalf("failed model update changed published state: mode=%s status=%d error=%v", mode, w.Code, err)
			}
		})
	}
}

func TestActiveProviderDefaultsPreserveExplicitRolesAndLegacy(t *testing.T) {
	s, _ := modelConfigFixture(t)
	c := cloneConfig(s.config)
	c.ActiveProvider = "b"
	explicit := &agent.ModelSelection{Provider: "a", Model: "model-a", ReasoningEffort: "high"}
	c.RoleModels.Orchestrator, c.RoleModels.Recon = explicit, explicit
	p, _, roles, err := c.ModelConfiguration()
	if err != nil || p.(*agent.OpenAIProvider).Model != "legacy" || roles.Worker == nil || roles.Worker.Provider != "b" || !reflect.DeepEqual(roles.Orchestrator, explicit) || !reflect.DeepEqual(roles.Recon, explicit) || c.RoleModels.Worker != nil {
		t.Fatalf("profile default overrode explicit or legacy route: %v", err)
	}
	for _, active := range []string{"", "default"} {
		c.ActiveProvider, c.RoleModels = active, agent.RoleModels{}
		_, _, roles, err = c.ModelConfiguration()
		if err != nil || roles.Orchestrator != nil || roles.Worker != nil || roles.Recon != nil {
			t.Fatalf("legacy profile supplied named defaults: %v", err)
		}
	}
	unchanged := cloneConfig(s.config)
	unchanged.Revision = "different"
	if modelConfigurationOnly(s.config, unchanged) {
		t.Fatal("unchanged save lost explicit full-refresh semantics")
	}
}

func TestModelConfigurationAcceptsOnlyExactSourceProjection(t *testing.T) {
	for _, changed := range []string{"none", "local_path", "github_url"} {
		t.Run(changed, func(t *testing.T) {
			s, original := modelConfigFixture(t)
			root := t.TempDir()
			script := filepath.Join(root, "fixture.js")
			if err := os.WriteFile(script, []byte("// static source fixture\n"), 0600); err != nil {
				t.Fatal(err)
			}
			s.config.LocalTools = []localtool.Manifest{{Name: "fixture", Command: "node", Args: []string{script}}}
			builds := 0
			s.options.BuildAgent = func(context.Context, Config) (*agent.Agent, error) {
				builds++
				return nil, errors.New("full rebuild fixture")
			}
			loaded := request(t, s.Handler(), "GET", "/api/config", nil, 200)
			var next Config
			if err := json.Unmarshal(loaded.Body.Bytes(), &next); err != nil {
				t.Fatal(err)
			}
			if next.LocalTools[0].LocalPath == "" || s.config.LocalTools[0].LocalPath != "" {
				t.Fatal("source projection fixture did not differ from raw config")
			}
			next.ActiveProvider = "b"
			switch changed {
			case "local_path":
				next.LocalTools[0].LocalPath = t.TempDir()
			case "github_url":
				next.LocalTools[0].GitHubURL = "https://github.com/fixture/changed"
			}
			status := 409
			if changed == "none" {
				status = 200
			}
			request(t, s.Handler(), "PUT", "/api/config", next, status)
			if changed == "none" {
				if builds != 0 || s.a == original || s.config.ActiveProvider != "b" || s.config.LocalTools[0].LocalPath != "" {
					t.Fatal("exact GET projection rebuilt runtime or persisted derived source")
				}
			} else if builds != 1 || s.a != original || s.config.ActiveProvider != "a" {
				t.Fatal("edited source metadata bypassed full rebuild")
			}
		})
	}
}
