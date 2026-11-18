package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"trace/internal/agent"
	"trace/internal/localtool"
	"trace/internal/mcp"
	"trace/internal/web"
)

func configRevisionForTest(t *testing.T, h http.Handler) string {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if w.Code != http.StatusOK || w.Header().Get("ETag") == "" {
		t.Fatal("configuration revision unavailable")
	}
	return w.Header().Get("ETag")
}

func isolateServiceConfigEnv(t *testing.T) {
	t.Helper()
	for _, item := range os.Environ() {
		name := strings.SplitN(item, "=", 2)[0]
		if strings.HasPrefix(strings.ToUpper(name), "TRACE_") && !standaloneRuntimeEnv(name) {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("TRACE_DATA_DIR", t.TempDir())
}

func TestWebSavedConfigurationUsedByCLI(t *testing.T) {
	isolateServiceConfigEnv(t)
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "service.json")
	t.Setenv("TRACE_CONFIG_FILE", path)
	var requests, discoveries atomic.Int32
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "saved-model" || r.Header.Get("Authorization") != "Bearer saved-key" {
			t.Error("CLI did not use the saved provider model and credential")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "configuration observed"}, "finish_reason": "stop"}}})
	}))
	defer provider.Close()
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Header.Get("Authorization") != "saved-mcp-key" {
			t.Error("MCP connection lost saved headers")
		}
		var rpc cliRPC
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			return
		}
		if rpc.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if rpc.Method == "tools/list" {
			discoveries.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cliMCPReply(rpc))
	}))
	defer remote.Close()
	skillRoot := filepath.Join(root, "skill")
	if err := os.MkdirAll(skillRoot, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillRoot, "SKILL.md"), []byte("---\nname: saved-skill\ndescription: fixture\n---\nsaved skill body"), 0600); err != nil {
		t.Fatal(err)
	}
	promptPath := filepath.Join(root, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("saved configuration prompt"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := localtool.Manifest{Name: "saved-local", Command: "saved-command", Args: []string{"{{value}}"}, Dir: root, Env: map[string]string{"TOKEN": "saved-local-key"}, TimeoutSeconds: 12, CancelGraceSeconds: 2}
	c := web.Config{
		Provider:   web.ProviderConfig{URL: provider.URL, Model: "saved-model", APIKey: "saved-key", ImageInput: true},
		Context:    agent.ContextConfig{WindowTokens: 64000, ModelMaxTokens: 80000, OutputReserve: 4096, KeepRecentTokens: 6000, CompactionMode: "projection"},
		SkillPaths: []string{skillRoot}, DisabledSkills: []string{"saved-skill"}, PromptPath: promptPath,
		MCP:        []mcp.Config{{Name: "saved-mcp", URL: remote.URL, Headers: map[string]string{"Authorization": "saved-mcp-key"}}},
		LocalTools: []localtool.Manifest{manifest},
	}
	store, err := agent.NewStore(os.Getenv("TRACE_DATA_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	live, err := agent.New(store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	app, err := web.New(live, web.Options{ConfigPath: path, BuildAgent: func(ctx context.Context, c web.Config) (*agent.Agent, error) { return buildWebAgent(ctx, store, c) }})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	data, _ := json.Marshal(c)
	w := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("If-Match", configRevisionForTest(t, app.Handler()))
	app.Handler().ServeHTTP(w, request)
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "saved-key") || strings.Contains(w.Body.String(), "saved-local-key") {
		t.Fatalf("save response: %d %s", w.Code, w.Body.String())
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Saved configuration wins even when stale bootstrap environment is invalid.
	t.Setenv("TRACE_PROVIDER_URL", "http://127.0.0.1:1")
	t.Setenv("TRACE_MODEL", "wrong-model")
	t.Setenv("TRACE_PROVIDER_KEY", "wrong-key")
	t.Setenv("TRACE_CONTEXT_TOKENS", "invalid")
	t.Setenv("TRACE_MCP_SERVERS", "invalid")
	t.Setenv("TRACE_LOCAL_TOOLS_CONFIG", filepath.Join(root, "missing.json"))
	t.Setenv("TRACE_SKILL_PATHS", filepath.Join(root, "missing-skill"))
	t.Setenv("TRACE_MAX_ROUNDS", "1")
	t.Setenv("TRACE_SUCCESS_CONTAINS", "standalone-predicate")
	t.Setenv("TRACE_REQUIRE_VERIFIED_COMPLETION", "true")
	loaded, err := cliServiceConfig()
	if err != nil || loaded.IsFirstBoot || !reflect.DeepEqual(loaded.Config.Provider, c.Provider) || !reflect.DeepEqual(loaded.Config.Context, c.Context) || !reflect.DeepEqual(loaded.Config.MCP, c.MCP) {
		t.Fatalf("saved configuration was not selected: %v", err)
	}
	creator, err := newAgent(store)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := creator.Create("check shared configuration")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Prompt != "saved configuration prompt" || snap.Run.Context != c.Context || snap.Run.Budget.MaxModelRequests != 1 || snap.Run.SuccessContains != "standalone-predicate" || !snap.Run.RequireVerifiedCompletion {
		t.Fatalf("new CLI Run lost shared settings or independent budget/predicate: %+v", snap.Run)
	}
	// Resuming reads the Run prompt, not a stale service prompt path.
	if err := os.Remove(promptPath); err != nil {
		t.Fatal(err)
	}
	runner, err := configuredRuntimeAgent(ctx, store)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.CloseMCP()
	if _, err := runner.ReadModelSkill("saved-skill"); err == nil {
		t.Fatal("saved disabled Skill became model-visible")
	}
	if _, err := runner.ReadSkill("saved-skill"); err != nil {
		t.Fatal("saved Skill root was not loaded")
	}
	tool, ok := runner.Registry().Lookup(manifest.StableID())
	if !ok || tool.Local == nil || !reflect.DeepEqual(*tool.Local, manifest) {
		t.Fatal("CLI localtool connection parameters differ from Web save")
	}
	cloned, clients, err := runner.Registry().Clone(ctx)
	for _, client := range clients {
		_ = client.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cloned.Lookup(mcp.ToolID("saved-mcp", "echo")); !ok || discoveries.Load() != 3 {
		t.Fatal("Web, CLI and child did not share the saved MCP connection")
	}
	if _, err := runner.Run(ctx, snap.Run.ID); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("saved provider requests=%d", requests.Load())
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("CLI configuration read rewrote the shared file")
	}
}

func TestServiceConfigBootstrapAndErrors(t *testing.T) {
	isolateServiceConfigEnv(t)
	path := serviceConfigPath()
	t.Setenv("TRACE_PROVIDER_URL", "http://bootstrap.invalid")
	t.Setenv("TRACE_KEEP_RECENT_TOKENS", "1234")
	result, err := cliServiceConfig()
	if err != nil || !result.IsFirstBoot || result.Config.Provider.URL != "http://bootstrap.invalid" || result.Config.Context.KeepRecentTokens != 1234 || !cliContextOverride(result) {
		t.Fatalf("environment bootstrap failed: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("loading default configuration unexpectedly persisted a file")
	}
	t.Setenv("TRACE_MCP_SERVERS", "[]")
	t.Setenv("TRACE_MCP_CONFIG", "conflicting.json")
	if _, err := cliServiceConfig(); err == nil {
		t.Fatal("conflicting bootstrap sources accepted")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := cliServiceConfig(); err == nil {
		t.Fatal("malformed saved config fell back to environment")
	}
}

func TestEnvironmentOnlyCLIResumeRetainsRunContext(t *testing.T) {
	isolateServiceConfigEnv(t)
	store, err := agent.NewStore(os.Getenv("TRACE_DATA_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	creator, err := agent.New(store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	want := agent.ContextConfig{WindowTokens: 64000, OutputReserve: 4096, KeepRecentTokens: 5000, CompactionMode: "projection"}
	if err := creator.ConfigureContext(want); err != nil {
		t.Fatal(err)
	}
	created, err := creator.Create("retain context without a service configuration")
	if err != nil {
		t.Fatal(err)
	}
	runner, err := configuredRuntimeAgent(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.CloseMCP()
	resumed, err := runner.Run(context.Background(), created.Run.ID)
	if err != nil || resumed.Run.Context != want {
		t.Fatalf("environment-only resume replaced the persisted context: %+v %v", resumed.Run.Context, err)
	}
}

func TestDirectoryUsesSavedConnectionsWithoutRunBudget(t *testing.T) {
	isolateServiceConfigEnv(t)
	t.Setenv("TRACE_MAX_ROUNDS", "not-a-run-budget")
	t.Setenv("TRACE_PROVIDER_URL", ":unused")
	t.Setenv("TRACE_PROMPT_FILE", filepath.Join(t.TempDir(), "missing.md"))
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("TRACE_CONFIG_FILE", path)
	manifest := localtool.Manifest{Name: "saved-directory", Command: "saved-command"}
	config := web.Config{LocalTools: []localtool.Manifest{manifest}}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := agent.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner, err := configuredDirectoryAgent(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.CloseMCP()
	tool, ok := runner.Registry().Lookup(manifest.StableID())
	if !ok || tool.Local == nil || tool.Local.Command != manifest.Command {
		t.Fatal("directory did not use the saved local connection")
	}
	if _, err := configuredRuntimeAgent(context.Background(), store); err == nil {
		t.Fatal("execution accepted an invalid run budget")
	}
}
