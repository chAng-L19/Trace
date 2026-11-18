package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"trace/internal/agent"
	"trace/internal/mcp"
)

type cliRPC struct {
	ID     int    `json:"id"`
	Method string `json:"method"`
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

func cliMCPReply(rpc cliRPC) any {
	var result any
	switch rpc.Method {
	case "initialize":
		result = map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
	default:
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "CLI_MCP " + string(rpc.Params.Arguments)}}, "structuredContent": map[string]string{"source": "fixture"}}
	}
	return map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result}
}

func TestCLIStdioFixture(t *testing.T) {
	if os.Getenv("TRACE_CLI_MCP_FIXTURE") == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var rpc cliRPC
		if json.Unmarshal(scanner.Bytes(), &rpc) != nil {
			os.Exit(3)
		}
		if rpc.ID != 0 {
			_ = json.NewEncoder(os.Stdout).Encode(cliMCPReply(rpc))
		}
	}
	os.Exit(0)
}

func TestStandaloneMCPCLI(t *testing.T) {
	deploy, cwd := t.TempDir(), t.TempDir()
	binary := filepath.Join(deploy, "trace")
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		binary += ".exe"
		goBinary += ".exe"
	}
	if out, err := exec.Command(goBinary, "build", "-buildvcs=false", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	env := standaloneEnv(os.Environ())
	env = append(env, "TRACE_DATA_DIR="+filepath.Join(deploy, "state"))
	call := func(extra []string, wantErr bool, args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = cwd
		cmd.Env = append(append([]string(nil), env...), extra...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if (err != nil) != wantErr {
			t.Fatalf("%v error=%v stderr=%s", args, err, stderr.String())
		}
		return out
	}
	var calls, closes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			closes.Add(1)
			w.WriteHeader(204)
			return
		}
		var rpc cliRPC
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			return
		}
		if rpc.ID == 0 {
			w.WriteHeader(202)
			return
		}
		if rpc.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "standalone")
		}
		if rpc.Method == "tools/call" {
			calls.Add(1)
			if rpc.Params.Name != "echo" {
				t.Errorf("remote name=%s", rpc.Params.Name)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cliMCPReply(rpc))
	}))
	defer server.Close()
	toolID := mcp.ToolID("cli", "echo")
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []agent.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		message := map[string]any{"role": "assistant", "content": standaloneReport("MCP tool recorded")}
		reason := "stop"
		if !standaloneToolObserved(request.Messages, "cli-call") {
			message = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "cli-call", "type": "function", "function": map[string]string{"name": toolID, "arguments": `{"value":"hello"}`}}}}
			reason = "tool_calls"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": reason}}})
	}))
	defer provider.Close()
	providerEnv := []string{"TRACE_PROVIDER_URL=" + provider.URL, "TRACE_MODEL=fixture"}
	config, _ := json.Marshal([]mcp.Config{{Name: "cli", URL: server.URL}})
	mcpEnv := append(append([]string(nil), providerEnv...), "TRACE_MCP_SERVERS="+string(config))
	var tools []agent.RegisteredTool
	if err := json.Unmarshal(call(mcpEnv, false, "tools"), &tools); err != nil {
		t.Fatalf("tools: %v %+v", err, tools)
	}
	found := false
	for _, tool := range tools {
		if tool.Definition.Function.Name == toolID && tool.Adapter == "mcp" && tool.ServerName == "cli" && tool.RemoteName == "echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("MCP tool missing: %+v", tools)
	}
	var created agent.Run
	if err := json.Unmarshal(call(nil, false, "create", "MCP CLI smoke"), &created); err != nil {
		t.Fatal(err)
	}
	call(providerEnv, false, "run", created.ID) // without registration stays pending
	before := call(nil, false, "show", created.ID)
	for _, args := range [][]string{{"tool", created.ID, "cli-call", "--mcp"}, {"tool", created.ID, "cli-call", "--mcp", ""}, {"tool", created.ID, "cli-call", "--mcp", toolID, "extra"}, {"tool", created.ID, "cli-call", "--mcp", "unknown"}} {
		call(mcpEnv, true, args...)
		if after := call(nil, false, "show", created.ID); !bytes.Equal(before, after) {
			t.Fatal("invalid MCP CLI mutated snapshot")
		}
	}
	badEnv := append(append([]string(nil), providerEnv...), "TRACE_MCP_SERVERS=broken")
	call(badEnv, true, "resume", created.ID)
	if after := call(nil, false, "show", created.ID); !bytes.Equal(before, after) {
		t.Fatal("invalid config mutated snapshot")
	}
	var snap agent.Snapshot
	if err := json.Unmarshal(call(mcpEnv, false, "tool", created.ID, "cli-call", "--mcp", toolID), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != agent.StatusCompleted || len(snap.Actions) != 1 || snap.Actions[0].Result.MCPResult == nil || snap.Evidence[0].Verification != "verified" || calls.Load() != 1 {
		t.Fatalf("explicit MCP: %+v", snap)
	}
	// A fresh process discovers the same stable ID and executes automatically.
	_ = json.Unmarshal(call(nil, false, "create", "automatic MCP"), &created)
	_ = json.Unmarshal(call(mcpEnv, false, "run", created.ID), &snap)
	if snap.Run.Status != agent.StatusCompleted || calls.Load() != 2 || len(snap.Actions) != 1 {
		t.Fatal("automatic MCP failed")
	}
	if closes.Load() < 3 {
		t.Fatal("HTTP sessions not closed")
	}
	// Config-file registration exercises a real stdio child from standalone CLI.
	fixture, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	config, _ = json.Marshal([]mcp.Config{{Name: "cli", Command: fixture, Args: []string{"-test.run=^TestCLIStdioFixture$"}, Env: map[string]string{"TRACE_CLI_MCP_FIXTURE": "1"}, Dir: cwd}})
	configPath := filepath.Join(deploy, "mcp.json")
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	fileEnv := append(append([]string(nil), providerEnv...), "TRACE_MCP_CONFIG="+configPath)
	if out := call(fileEnv, false, "tools"); !bytes.Contains(out, []byte(toolID)) {
		t.Fatal("stdio discovery missing")
	}
	_ = json.Unmarshal(call(nil, false, "create", "stdio MCP"), &created)
	_ = json.Unmarshal(call(fileEnv, false, "run", created.ID), &snap)
	if snap.Run.Status != agent.StatusCompleted || len(snap.Actions) != 1 || !strings.Contains(snap.Actions[0].Result.Stdout, "CLI_MCP") {
		t.Fatal("stdio standalone call missing")
	}
}
