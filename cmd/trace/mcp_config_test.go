package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"trace/internal/agent"
	"trace/internal/mcp"
	"trace/internal/web"
)

func TestMCPDisabledConfigRoundTrip(t *testing.T) {
	isolateServiceConfigEnv(t)
	ctx := context.Background()
	var discoveries atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Header.Get("Authorization") != "private-header" {
			t.Error("stored MCP header was not restored")
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
	store, err := agent.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	build := func(ctx context.Context, c web.Config) (*agent.Agent, error) {
		a, err := agent.New(store, nil, "")
		if err != nil {
			return nil, err
		}
		for _, server := range c.MCP {
			if _, err = a.AddMCP(ctx, server); err != nil {
				_ = a.CloseMCP()
				return nil, err
			}
		}
		return a, nil
	}
	a, err := build(ctx, web.Config{})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "service", "config.json")
	s, err := web.New(a, web.Options{ConfigPath: path, BuildAgent: build})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	request := func(method, path string, body any, out any) {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, path, bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/json")
		if method == http.MethodPut && path == "/api/config" {
			r.Header.Set("If-Match", configRevisionForTest(t, s.Handler()))
		}
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
		}
		if out != nil {
			if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
				t.Fatal(err)
			}
		}
	}
	checkDirectory := func(tools []agent.RegisteredTool, disabled bool) {
		t.Helper()
		count := 0
		for _, tool := range tools {
			if tool.Adapter == "mcp" {
				count++
				if tool.Definition.Function.Name != mcp.ToolID("fixture", "echo") {
					t.Fatalf("unexpected MCP tool: %+v", tool)
				}
			}
		}
		if (disabled && count != 0) || (!disabled && count != 1) {
			t.Fatalf("disabled=%v MCP tools=%d", disabled, count)
		}
	}
	config := web.Config{MCP: []mcp.Config{{Name: "fixture", Description: "Fixture service description", Disabled: true,
		Command: filepath.Join(t.TempDir(), "does-not-exist"), Env: map[string]string{"TOKEN": "private-env"},
		Headers: map[string]string{"Authorization": "private-header"}}}}
	t.Setenv("TRACE_MCP_CONFIG", "")
	for _, disabled := range []bool{true, false, true} {
		before := discoveries.Load()
		config.MCP[0].Disabled = disabled
		if !disabled {
			config.MCP[0].Command = ""
			config.MCP[0].URL = remote.URL
		}
		request("PUT", "/api/config", config, nil)
		request("GET", "/api/config", nil, &config)
		if len(config.MCP) != 1 || config.MCP[0].Disabled != disabled || config.MCP[0].Description != "Fixture service description" {
			t.Fatalf("config metadata changed: %+v", config.MCP)
		}
		if config.MCP[0].Env["TOKEN"] != "[stored secret]" || config.MCP[0].Headers["Authorization"] != "[stored secret]" {
			t.Fatal("config response exposed stored secrets")
		}
		loadedConfig, err := web.LoadConfig(path, web.Config{}, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		persisted := loadedConfig.Config
		if len(persisted.MCP) != 1 || persisted.MCP[0].Disabled != disabled || persisted.MCP[0].Description != config.MCP[0].Description || persisted.MCP[0].Env["TOKEN"] != "private-env" || persisted.MCP[0].Headers["Authorization"] != "private-header" {
			t.Fatal("saved MCP metadata or secrets changed")
		}
		var directory []agent.RegisteredTool
		request("GET", "/api/tools", nil, &directory)
		checkDirectory(directory, disabled)
		restored, err := build(ctx, persisted)
		if err != nil {
			t.Fatal(err)
		}
		checkDirectory(restored.Registry().Tools(), disabled)
		child, clients, err := restored.Registry().Clone(ctx)
		if err != nil {
			t.Fatal(err)
		}
		checkDirectory(child.Tools(), disabled)
		for _, client := range clients {
			if err := client.Close(); err != nil {
				t.Fatal(err)
			}
		}
		if err := restored.CloseMCP(); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(persisted.MCP)
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("TRACE_MCP_SERVERS", string(data))
		cli, err := agent.New(store, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := cliServiceConfig()
		if err != nil {
			t.Fatal(err)
		}
		if err := registerConfiguredMCP(ctx, cli, loaded.Config.MCP); err != nil {
			t.Fatal(err)
		}
		checkDirectory(cli.Registry().Tools(), disabled)
		if err := cli.CloseMCP(); err != nil {
			t.Fatal(err)
		}
		want := int32(4) // Web rebuild, persisted restart, child clone, CLI environment.
		if disabled {
			want = 0
		}
		if delta := discoveries.Load() - before; delta != want {
			t.Fatalf("disabled=%v discoveries=%d want=%d", disabled, delta, want)
		}
	}
}

func TestBuildWebAgentRetainsPrimaryAndCandidateCleanupFailures(t *testing.T) {
	var deletes atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("cleanup-secret"))
			return
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
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "session-secret")
		if r.URL.Path == "/second" && rpc.Method == "initialize" {
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "error": map[string]any{"code": -32001, "message": "primary-secret"}})
			return
		}
		json.NewEncoder(w).Encode(cliMCPReply(rpc))
	}))
	defer remote.Close()
	store, err := agent.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	config := web.Config{Context: agent.DefaultContextConfig(), MCP: []mcp.Config{
		{Name: "first", URL: remote.URL + "/first?token=url-secret"},
		{Name: "second", URL: remote.URL + "/second?token=url-secret"},
	}}
	_, err = buildWebAgent(context.Background(), store, config)
	var connection *mcp.ConnectionError
	var rpc *mcp.RPCError
	if !errors.As(err, &connection) || !errors.As(err, &rpc) || rpc.Code != -32001 || deletes.Load() != 2 {
		t.Fatalf("primary failure or candidate cleanup lost: deletes=%d err=%v", deletes.Load(), err)
	}
	status := connection.Status
	if status.Stage != "initialize" || status.Code != "rpc" || len(status.Cleanup) != 2 || status.Cleanup[0].Stage != "cleanup" || status.Cleanup[1].Stage != "candidate_cleanup" || status.Cleanup[1].Code != "http" {
		t.Fatalf("combined candidate status lost: %+v", status)
	}
	data, _ := json.Marshal(status)
	if strings.Contains(string(data), "secret") || strings.Contains(string(data), remote.URL) {
		t.Fatalf("candidate cleanup leaked connection data: %s", data)
	}
	live, err := agent.New(store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	app, err := web.New(live, web.Options{ConfigPath: filepath.Join(t.TempDir(), "config.json"), InitialConfig: config, BuildAgent: func(ctx context.Context, c web.Config) (*agent.Agent, error) {
		return buildWebAgent(ctx, store, c)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	data, _ = json.Marshal(config)
	r := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("If-Match", configRevisionForTest(t, app.Handler()))
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusConflict || strings.Contains(w.Body.String(), "secret") || !strings.Contains(w.Body.String(), "清理失败") || deletes.Load() != 4 {
		t.Fatalf("rebuild response lost sanitized cleanup: deletes=%d %s", deletes.Load(), w.Body.String())
	}
	w = httptest.NewRecorder()
	app.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/mcp/status", nil))
	var rows []struct {
		Name        string      `json:"name"`
		LastAttempt *mcp.Status `json:"last_attempt"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].LastAttempt == nil || len(rows[1].LastAttempt.Cleanup) != 2 || rows[1].LastAttempt.Stage != "initialize" {
		t.Fatalf("candidate cleanup not publicly observable: %s", w.Body.String())
	}
}
