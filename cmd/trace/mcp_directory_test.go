package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"trace/internal/agent"
	"trace/internal/localtool"
	"trace/internal/mcp"
)

func TestMCPDirectory(t *testing.T) {
	for _, value := range os.Environ() {
		name := strings.SplitN(value, "=", 2)[0]
		if strings.HasPrefix(strings.ToUpper(name), "TRACE_") {
			t.Setenv(name, "")
		}
	}
	t.Setenv("TRACE_DATA_DIR", t.TempDir())
	// Hide optional preset executables so the empty-directory case is reproducible.
	t.Setenv("PATH", t.TempDir())
	// Discovery must not depend on model, prompt, or run budget configuration.
	t.Setenv("TRACE_PROVIDER_URL", ":invalid-provider")
	t.Setenv("TRACE_PROMPT_FILE", filepath.Join(t.TempDir(), "missing.md"))
	t.Setenv("TRACE_MAX_ROUNDS", "invalid-budget")
	output := filepath.Join(t.TempDir(), "stdout.json")
	call := func(args ...string) ([]byte, error) {
		t.Helper()
		file, err := os.Create(output)
		if err != nil {
			t.Fatal(err)
		}
		original := os.Stdout
		os.Stdout = file
		defer func() { os.Stdout = original }()
		err = run(args)
		if closeErr := file.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		data, readErr := os.ReadFile(output)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return data, err
	}
	if data, err := call("mcp"); err != nil || strings.TrimSpace(string(data)) != "[]" {
		t.Fatalf("empty directory: %s %v", data, err)
	}
	if _, err := call("mcp", "extra"); err == nil || err.Error() != "usage: trace mcp" {
		t.Fatalf("invalid arguments: %v", err)
	}

	var discoveries, calls, closes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			closes.Add(1)
			w.WriteHeader(http.StatusNoContent)
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
		switch rpc.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "directory")
		case "tools/list":
			discoveries.Add(1)
		case "tools/call":
			calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cliMCPReply(rpc))
	}))
	defer server.Close()
	config, _ := json.Marshal([]mcp.Config{{Name: "external", URL: server.URL, Headers: map[string]string{"Authorization": "private-header"}}})
	t.Setenv("TRACE_MCP_SERVERS", string(config))
	manifest := localtool.Manifest{Name: "external_cli", Command: "not-executed", Env: map[string]string{"TOKEN": "private-env"}, InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}}}`)}
	localConfig, _ := json.Marshal([]localtool.Manifest{manifest})
	t.Setenv("TRACE_LOCAL_TOOLS", string(localConfig))
	for _, command := range []string{"mcp", "tools"} {
		data, err := call(command)
		if err != nil {
			t.Fatal(err)
		}
		var tools []agent.RegisteredTool
		if err := json.Unmarshal(data, &tools); err != nil {
			t.Fatal(err)
		}
		byID := map[string]agent.RegisteredTool{}
		for _, tool := range tools {
			byID[tool.Definition.Function.Name] = tool
			if command == "mcp" && tool.Adapter == "exec" {
				t.Fatalf("external directory contains built-in exec: %s", data)
			}
		}
		if command == "tools" && byID["exec"].Adapter != "exec" {
			t.Fatalf("tools lost built-in exec: %s", data)
		}
		local := byID[manifest.StableID()]
		remote := byID[mcp.ToolID("external", "echo")]
		localSchema, _ := json.Marshal(local.Definition.Function.Parameters)
		remoteSchema, _ := json.Marshal(remote.Definition.Function.Parameters)
		if local.Adapter != "local" || local.LocalName != manifest.Name || !strings.Contains(string(localSchema), `"message":{"type":"string"}`) {
			t.Fatalf("local metadata: %s", data)
		}
		if remote.Adapter != "mcp" || remote.ServerName != "external" || remote.RemoteName != "echo" || string(remoteSchema) != `{"type":"object"}` {
			t.Fatalf("MCP metadata: %s", data)
		}
		if strings.Contains(string(data), "private-") || strings.Contains(string(data), "not-executed") {
			t.Fatalf("directory exposed execution configuration: %s", data)
		}
	}
	if discoveries.Load() != 2 || closes.Load() != 2 || calls.Load() != 0 {
		t.Fatalf("lifecycle: discoveries=%d closes=%d calls=%d", discoveries.Load(), closes.Load(), calls.Load())
	}
	// A later server's failure must still close an already registered session.
	config, _ = json.Marshal([]mcp.Config{{Name: "external", URL: server.URL}, {Name: "broken", URL: ":invalid"}})
	t.Setenv("TRACE_MCP_SERVERS", string(config))
	if data, err := call("mcp"); err == nil || len(data) != 0 || closes.Load() != 3 {
		t.Fatalf("failed discovery cleanup: output=%s error=%v closes=%d", data, err, closes.Load())
	}
}
