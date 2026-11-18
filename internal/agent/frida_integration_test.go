package agent

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trace/internal/localtool"
)

func TestFridaInstalledIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manifests, status := localtool.Presets(ctx)
	var frida localtool.Manifest
	for _, manifest := range manifests {
		if manifest.Name == "frida" {
			frida = manifest
		}
	}
	if frida.Name == "" {
		t.Skipf("optional Frida installation absent: %+v", status)
	}
	// Yama restricts sibling attach; only this disposable fixture opts in.
	target := exec.Command(frida.Command, "-c", `import sys, time
if sys.platform == "linux":
 import ctypes
 assert ctypes.CDLL(None).prctl(0x59616d61, ctypes.c_ulong(-1), 0, 0, 0) == 0
time.sleep(60)
`)
	if err := target.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Process.Kill(); _ = target.Wait() }()
	selected, _ := json.Marshal(map[string]any{"ids": []string{frida.StableID()}})
	probe, _ := json.Marshal(map[string]any{"action": "instrument", "pid": target.Process.Pid, "script": "send('trace-agent-frida-marker');", "duration_ms": 100})
	bad, _ := json.Marshal(map[string]any{"action": "instrument", "pid": target.Process.Pid, "script": "throw new Error('trace-agent-frida-error');", "duration_ms": 100})
	p := &scriptedProvider{responses: []ModelResponse{
		{ToolCalls: []ToolCall{{ID: "search", Name: "tool_search", Arguments: `{"query":"frida"}`}}},
		{ToolCalls: []ToolCall{{ID: "select", Name: "tool_select", Arguments: string(selected)}}},
		{ToolCalls: []ToolCall{{ID: "skill", Name: "read_skill", Arguments: `{"name":"frida"}`}}},
		{ToolCalls: []ToolCall{{ID: "marker", Name: frida.StableID(), Arguments: string(probe)}}},
		{ToolCalls: []ToolCall{{ID: "script-error", Name: frida.StableID(), Arguments: string(bad)}}},
		{Content: "complete"},
	}}
	a, _ := newTestAgent(t, p)
	if err := a.AddLocalTools([]localtool.Manifest{frida}); err != nil {
		t.Fatal(err)
	}
	if err := a.ConfigureSkills([]string{filepath.Join("..", "..", ".trace", "skills", "frida")}); err != nil {
		t.Fatal(err)
	}
	s, err := a.Run(ctx, createTestRun(t, a))
	if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 5 {
		t.Fatalf("Frida chain: status=%s actions=%d err=%v", s.Run.Status, len(s.Actions), err)
	}
	if !strings.Contains(s.Actions[0].Result.Stdout, frida.StableID()) || !strings.Contains(s.Actions[2].Result.Stdout, "duration_ms") {
		t.Fatal("Frida discovery or skill failed")
	}
	for _, action := range s.Actions[3:] {
		if action.Adapter != "local" || action.LocalName != "frida" || action.Result.EvidenceID == "" || action.Result.WorkerPID == 0 {
			t.Fatalf("missing local execution evidence: %+v", action)
		}
		if !strings.Contains(action.Result.Stdout, `"detached": true`) {
			t.Fatalf("session was not detached: %+v", action.Result)
		}
	}
	if s.Actions[3].Result.ExitCode != 0 || !strings.Contains(s.Actions[3].Result.Stdout, "trace-agent-frida-marker") {
		t.Fatalf("marker: %+v", s.Actions[3].Result)
	}
	if s.Actions[4].Result.ExitCode != 1 || !strings.Contains(s.Actions[4].Result.Stdout, "trace-agent-frida-error") {
		t.Fatalf("script failure became success: %+v", s.Actions[4].Result)
	}
	for _, evidence := range s.Evidence {
		if evidence.CallID == "marker" && evidence.Verification != "verified" {
			t.Fatalf("marker execution not verified: %+v", evidence)
		}
		if evidence.CallID == "script-error" && evidence.Verification != "unverified" {
			t.Fatalf("script failure verified: %+v", evidence)
		}
	}
}
