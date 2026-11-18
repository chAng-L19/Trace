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
	"trace/prompts"
)

func TestStandaloneBinaryCLI(t *testing.T) {
	deploy := t.TempDir()
	binary := filepath.Join(deploy, "trace")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		goBinary += ".exe"
	}
	build := exec.Command(goBinary, "build", "-buildvcs=false", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, output)
	}
	cwd := t.TempDir()
	state := filepath.Join(deploy, "state")
	env := standaloneEnv(os.Environ())
	env = append(env, "TRACE_DATA_DIR="+state)
	call := func(extra []string, wantErr bool, args ...string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, args...)
		command.Dir = cwd
		command.Env = append(append([]string(nil), env...), extra...)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if (err != nil) != wantErr {
			t.Fatalf("%v: error=%v stderr=%s", args, err, stderr.String())
		}
		return output
	}
	for _, args := range [][]string{{}, {"help"}, {"--help"}, {"version"}} {
		if len(call(nil, false, args...)) == 0 {
			t.Fatal("empty help/version")
		}
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("help/version created state directory")
	}
	call(nil, false, "list")
	call(nil, false, "prompt")
	var created agent.Run
	if err := json.Unmarshal(call(nil, false, "create", "standalone objective"), &created); err != nil {
		t.Fatal(err)
	}
	brokenPrompt := []string{"TRACE_PROMPT_FILE=" + filepath.Join(deploy, "missing.md")}
	var snap agent.Snapshot
	if err := json.Unmarshal(call(brokenPrompt, false, "run", created.ID), &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != agent.StatusCompleted || snap.Prompt != prompts.Agent {
		t.Fatal("binary prompt or execution mismatch")
	}
	call(brokenPrompt, false, "show", created.ID)
	call(brokenPrompt, false, "events", created.ID)
	call(brokenPrompt, false, "list")
	call(brokenPrompt, false, "message", created.ID, "follow up")
	call(brokenPrompt, true, "resume", created.ID)
	call(nil, true, "tool", created.ID, "unknown", "result")
	call(nil, true, "unknown-command")
	json.Unmarshal(call(nil, false, "create", "cancel objective"), &created)
	call(append(append([]string(nil), brokenPrompt...), "TRACE_MAX_ROUNDS=invalid"), false, "cancel", created.ID)
	call(brokenPrompt, true, "run", created.ID)
	command := "printf CLI_TOOL"
	if runtime.GOOS == "windows" {
		command = "echo CLI_TOOL"
	}
	arguments, _ := json.Marshal(map[string]string{"command": command})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []agent.Message `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		hasTool := standaloneToolObserved(req.Messages, "call-1")
		goal := ""
		for _, m := range req.Messages {
			// This fixture routes its fixed initial objective, not runtime data
			// projected after the conversation or a later audit continuation.
			if m.Role == "user" && goal == "" {
				goal = m.Content
			}
		}
		if hasTool {
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": standaloneReport("tool verified")}, "finish_reason": "stop"}}})
			return
		}
		name := "external"
		args := "{}"
		if goal == "automatic exec" {
			name = "exec"
			args = string(arguments)
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": name, "arguments": args}}}}, "finish_reason": "tool_calls"}}})
	}))
	defer server.Close()
	provider := append(append([]string(nil), brokenPrompt...), "TRACE_PROVIDER_URL="+server.URL, "TRACE_MODEL=fixture")
	json.Unmarshal(call(nil, false, "create", "manual tool"), &created)
	json.Unmarshal(call(provider, false, "run", created.ID), &snap)
	if snap.Run.Status != agent.StatusWaitingTool {
		t.Fatal("manual tool not pending")
	}
	beforeInvalidExec := call(provider, false, "show", created.ID)
	for _, command := range [][]string{nil, {""}, {"   "}} {
		args := append([]string{"tool", created.ID, "call-1", "--exec"}, command...)
		call(provider, true, args...)
		if after := call(provider, false, "show", created.ID); !bytes.Equal(beforeInvalidExec, after) {
			t.Fatalf("malformed exec changed pending run: %q", args)
		}
	}
	call(provider, false, "pause", created.ID)
	json.Unmarshal(call(provider, false, "resume", created.ID), &snap)
	if snap.Run.Status != agent.StatusWaitingTool {
		t.Fatal("resume bypassed tool")
	}
	manualResult := "manual output --exec"
	json.Unmarshal(call(provider, false, "tool", created.ID, "call-1", manualResult), &snap)
	if len(snap.Actions) != 0 || snap.Run.Status != agent.StatusCompleted {
		t.Fatal("manual result was treated as a command")
	}
	manualSaved := false
	for _, message := range snap.Messages {
		if message.Role == "tool" && message.ToolCallID == "call-1" {
			manualSaved = message.Content == manualResult
		}
	}
	if !manualSaved {
		t.Fatal("manual result text changed")
	}
	json.Unmarshal(call(nil, false, "create", "manual exec"), &created)
	call(provider, false, "run", created.ID)
	execArgs := []string{"tool", created.ID, "call-1", "--exec", "/bin/sh", "-c", "true"}
	if runtime.GOOS == "windows" {
		execArgs = []string{"tool", created.ID, "call-1", "--exec", "cmd.exe", "/d", "/c", "exit /b 0"}
	}
	json.Unmarshal(call(provider, false, execArgs...), &snap)
	if len(snap.Actions) != 1 || snap.Actions[0].Result == nil || snap.Actions[0].Result.ExitCode != 0 || snap.Actions[0].Result.WorkerPID == 0 {
		t.Fatalf("manual exec missing successful process result: status=%s actions=%+v", snap.Run.Status, snap.Actions)
	}
	json.Unmarshal(call(nil, false, "create", "automatic exec"), &created)
	json.Unmarshal(call(provider, false, "run", created.ID), &snap)
	if snap.Run.Status != agent.StatusCompleted || len(snap.Actions) != 1 || !strings.Contains(snap.Actions[0].Result.Stdout, "CLI_TOOL") {
		t.Fatal("automatic exec standalone smoke failed")
	}
	var result agent.Result
	json.Unmarshal(call(brokenPrompt, false, "result", created.ID), &result)
	if result.Status != "incomplete" {
		t.Fatal("plain model text claimed target success")
	}
	var evidence []agent.Evidence
	json.Unmarshal(call(brokenPrompt, false, "evidence", created.ID), &evidence)
	if len(evidence) != 1 || evidence[0].Verification != "verified" {
		t.Fatal("CLI evidence missing")
	}
	if report := call(brokenPrompt, false, "report", created.ID); !bytes.Contains(report, []byte("状态：未完成（incomplete）")) {
		t.Fatal("stdout report missing result")
	}
	reportPath := filepath.Join(cwd, "report.md")
	call(brokenPrompt, false, "report", created.ID, reportPath)
	if report, err := os.ReadFile(reportPath); err != nil || !bytes.Contains(report, []byte("暂无报告引用证据。")) || bytes.Contains(report, []byte(evidence[0].ContentHash)) {
		t.Fatal("export report promoted an uncited execution record")
	}
	call(brokenPrompt, true, "report", created.ID, reportPath)
	json.Unmarshal(call([]string{"TRACE_SUCCESS_CONTAINS=CLI_TOOL"}, false, "create", "automatic exec"), &created)
	json.Unmarshal(call(provider, false, "run", created.ID), &snap)
	if snap.Result.Status != "success" {
		t.Fatal("CLI persisted success predicate missing")
	}
	if report := call(brokenPrompt, false, "report", created.ID); len(snap.Evidence) == 0 || !bytes.Contains(report, []byte(snap.Evidence[0].ContentHash)) {
		t.Fatal("cited successful execution missing from exported report")
	}
	json.Unmarshal(call([]string{"TRACE_MAX_ROUNDS=1"}, false, "create", "automatic exec"), &created)
	json.Unmarshal(call(provider, false, "run", created.ID), &snap)
	if snap.Run.Status != agent.StatusPaused || snap.Run.PauseReason != "model request budget exhausted" {
		t.Fatal("CLI persisted budget missing")
	}
	json.Unmarshal(call(provider, false, "resume", created.ID), &snap)
	if snap.Run.Status != agent.StatusPaused {
		t.Fatal("CLI resume bypassed budget")
	}
	json.Unmarshal(call(append(append([]string(nil), provider...), "TRACE_MAX_ROUNDS=2"), false, "resume", created.ID), &snap)
	if snap.Run.Status != agent.StatusCompleted {
		t.Fatal("CLI budget override did not resume")
	}
}
