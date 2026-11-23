package main

import (
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
	"testing"
	"time"

	"trace/internal/agent"
	"trace/internal/localtool"
)

func TestLocalPresetCustomOverride(t *testing.T) {
	isolateServiceConfigEnv(t)
	t.Setenv("TRACE_LOCAL_TOOLS_CONFIG", "")
	store, err := agent.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	custom := localtool.Manifest{Name: "code-search", Command: "custom-command", Args: []string{"{{path}}"}}
	for _, duplicate := range []bool{false, true} {
		runner, err := agent.New(store, agent.MockProvider{}, "")
		if err != nil {
			t.Fatal(err)
		}
		manifests := []localtool.Manifest{custom}
		if duplicate {
			manifests = append(manifests, custom)
		}
		data, _ := json.Marshal(manifests)
		t.Setenv("TRACE_LOCAL_TOOLS", string(data))
		before, _ := json.Marshal(runner.Registry().Tools())
		loaded, err := cliServiceConfig()
		if err != nil {
			t.Fatal(err)
		}
		err = runner.AddLocalTools(loaded.Config.LocalTools)
		if duplicate {
			after, _ := json.Marshal(runner.Registry().Tools())
			if err == nil || !bytes.Equal(before, after) {
				t.Fatal("duplicate custom tools must fail atomically")
			}
			continue
		}
		tool, ok := runner.Registry().Lookup(custom.StableID())
		if err != nil || !ok || tool.Local == nil || tool.Local.Command != "custom-command" {
			t.Fatalf("custom override: %+v %v", tool, err)
		}
	}
}

func TestStandaloneAutomaticPresetAgent(t *testing.T) {
	presets, status := localtool.Presets(context.Background())
	var preset localtool.Manifest
	for _, manifest := range presets {
		if manifest.Name == "python-ast-audit" {
			preset = manifest
		}
	}
	if preset.Name == "" {
		t.Skipf("Python runtime not installed: %+v", status)
	}
	deploy, cwd := t.TempDir(), t.TempDir()
	binary := filepath.Join(deploy, "trace")
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		binary += ".exe"
		goBinary += ".exe"
	}
	if output, err := exec.Command(goBinary, "build", "-buildvcs=false", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	path := filepath.Join(cwd, "source & 中文.py")
	if err := os.WriteFile(path, []byte("import pickle\npickle.loads(b'fixture')\n"), 0600); err != nil {
		t.Fatal(err)
	}
	arguments, _ := json.MarshalIndent(map[string]string{"path": path}, "", "  ")
	toolID := preset.StableID()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []agent.Message        `json:"messages"`
			Tools    []agent.ToolDefinition `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		found := false
		for _, definition := range request.Tools {
			found = found || definition.Function.Name == toolID
		}
		if !found {
			t.Error("automatic preset missing from model request")
		}
		message := map[string]any{"role": "assistant", "content": standaloneReport("AST observation recorded")}
		reason := "stop"
		if !standaloneToolObserved(request.Messages, "preset-call") {
			message = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "preset-call", "type": "function", "function": map[string]string{"name": toolID, "arguments": string(arguments)}}}}
			reason = "tool_calls"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": reason}}})
	}))
	defer provider.Close()
	env := standaloneEnv(os.Environ())
	env = append(env, "TRACE_DATA_DIR="+filepath.Join(deploy, "state"), "TRACE_PROVIDER_URL="+provider.URL, "TRACE_MODEL=fixture")
	call := func(args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, args...)
		command.Dir, command.Env = cwd, env
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			t.Fatalf("%v: %v %s", args, err, stderr.String())
		}
		return output
	}
	var directory []agent.RegisteredTool
	if err := json.Unmarshal(call("mcp"), &directory); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tool := range directory {
		found = found || (tool.Definition.Function.Name == toolID && tool.LocalName == "python-ast-audit")
	}
	if !found {
		t.Fatal("automatic preset missing from mcp directory")
	}
	var run agent.Run
	if err := json.Unmarshal(call("create", "audit fixture source"), &run); err != nil {
		t.Fatal(err)
	}
	// This pre-role preset fixture owns direct execution. Check the CLI's
	// production default before explicitly restoring its legacy identity.
	if run.Responsibility != agent.ResponsibilityCoordinator {
		t.Fatalf("CLI create did not default to Coordinator: %+v", run)
	}
	store, err := agent.NewStore(filepath.Join(deploy, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Update(run.ID, func(s *agent.Snapshot) error {
		s.Run.Responsibility = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var snap agent.Snapshot
	if err := json.Unmarshal(call("run", run.ID), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != agent.StatusCompleted || len(snap.Actions) != 1 || len(snap.Evidence) != 1 {
		t.Fatalf("Agent completion: %+v", snap)
	}
	action := snap.Actions[0]
	if action.Adapter != "local" || action.LocalName != "python-ast-audit" || action.Result.ExitCode != 0 || action.Result.WorkerPID == 0 || !strings.Contains(action.Result.Stdout, `"call": "pickle.loads"`) || snap.Evidence[0].Verification != "verified" {
		t.Fatalf("Agent observation: %+v", action)
	}
	entries, err := os.ReadDir(cwd)
	if err != nil || len(entries) != 1 {
		t.Fatalf("helper left temporary source files in cwd: %+v %v", entries, err)
	}
}
