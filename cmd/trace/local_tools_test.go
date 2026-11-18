package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"trace/internal/agent"
	"trace/internal/localtool"
)

func TestCLILocalFixture(t *testing.T) {
	if os.Getenv("TRACE_CLI_LOCAL_FIXTURE") == "" {
		return
	}
	_, _ = io.Copy(os.Stdout, os.Stdin)
	os.Exit(0)
}

func TestStandaloneLocalToolsCLI(t *testing.T) {
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
			t.Fatalf("%v: %v %s", args, err, stderr.String())
		}
		return out
	}
	fixture, _ := os.Executable()
	manifest := localtool.Manifest{Name: "cli_local", Command: fixture, Args: []string{"-test.run=^TestCLILocalFixture$"}, Mode: localtool.ModeStdio, Dir: cwd, Env: map[string]string{"TRACE_CLI_LOCAL_FIXTURE": "1"}}
	toolID := manifest.StableID()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []agent.Message        `json:"messages"`
			Tools    []agent.ToolDefinition `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		message := map[string]any{"role": "assistant", "content": standaloneReport("local tool recorded")}
		reason := "stop"
		if !standaloneToolObserved(request.Messages, "cli-call") {
			message = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "cli-call", "type": "function", "function": map[string]string{"name": toolID, "arguments": `{"message":"CLI_LOCAL"}`}}}}
			reason = "tool_calls"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": reason}}})
	}))
	defer provider.Close()
	providerEnv := []string{"TRACE_PROVIDER_URL=" + provider.URL, "TRACE_MODEL=fixture"}
	config, _ := json.Marshal([]localtool.Manifest{manifest})
	configPath := filepath.Join(deploy, "local-tools.json")
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	toolEnv := append(append([]string(nil), providerEnv...), "TRACE_LOCAL_TOOLS_CONFIG="+configPath)
	var tools []agent.RegisteredTool
	if err := json.Unmarshal(call(toolEnv, false, "tools"), &tools); err != nil {
		t.Fatalf("tools: %+v %v", tools, err)
	}
	found := false
	for _, tool := range tools {
		if tool.Definition.Function.Name == toolID && tool.Adapter == "local" && tool.LocalName == "cli_local" {
			found = true
		}
	}
	if !found {
		t.Fatal("custom local tool missing from directory")
	}
	var created agent.Run
	if err := json.Unmarshal(call(nil, false, "create", "local tools smoke"), &created); err != nil {
		t.Fatal(err)
	}
	call(providerEnv, false, "run", created.ID)
	before := call(nil, false, "show", created.ID)
	for _, args := range [][]string{{"tool", created.ID, "cli-call", "--registered"}, {"tool", created.ID, "cli-call", "--registered", ""}, {"tool", created.ID, "cli-call", "--registered", toolID, "extra"}, {"tool", created.ID, "cli-call", "--registered", "unknown"}} {
		call(toolEnv, true, args...)
		if after := call(nil, false, "show", created.ID); !bytes.Equal(before, after) {
			t.Fatal("malformed CLI changed snapshot")
		}
	}
	badEnv := append(append([]string(nil), providerEnv...), "TRACE_LOCAL_TOOLS=broken")
	call(badEnv, true, "resume", created.ID)
	if after := call(nil, false, "show", created.ID); !bytes.Equal(before, after) {
		t.Fatal("bad config changed snapshot")
	}
	conflictEnv := append(append([]string(nil), toolEnv...), "TRACE_LOCAL_TOOLS=[]")
	call(conflictEnv, true, "tools")
	var snap agent.Snapshot
	if err := json.Unmarshal(call(toolEnv, false, "tool", created.ID, "cli-call", "--registered", toolID), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != agent.StatusCompleted || len(snap.Actions) != 1 || snap.Actions[0].Adapter != "local" || !strings.Contains(snap.Actions[0].Result.Stdout, "CLI_LOCAL") || snap.Evidence[0].Verification != "verified" {
		t.Fatalf("explicit: %+v", snap)
	}
	_ = json.Unmarshal(call(nil, false, "create", "automatic local"), &created)
	inlineEnv := append(append([]string(nil), providerEnv...), "TRACE_LOCAL_TOOLS="+string(config))
	_ = json.Unmarshal(call(inlineEnv, false, "run", created.ID), &snap)
	if snap.Run.Status != agent.StatusCompleted || len(snap.Actions) != 1 {
		t.Fatal("automatic local invocation failed")
	}
}
