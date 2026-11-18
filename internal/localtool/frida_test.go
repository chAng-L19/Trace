package localtool

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"trace/internal/worker"
)

// This runs the installed Frida SDK against a disposable process, not a mock.
func TestFridaInstrumentationAndCancellation(t *testing.T) {
	tool, ok := installedPresets(t)["frida"]
	if !ok {
		t.Skip("Frida not installed")
	}
	// Yama restricts sibling attach; only this disposable fixture opts in.
	target := exec.Command(tool.Command, "-c", `import sys, time
if sys.platform == "linux":
 import ctypes
 assert ctypes.CDLL(None).prctl(0x59616d61, ctypes.c_ulong(-1), 0, 0, 0) == 0
time.sleep(60)
`)
	if err := target.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Process.Kill(); _ = target.Wait() }()
	pid := target.Process.Pid
	processes := invokePreset(t, tool, map[string]any{"action": "processes"}, false)
	found := false
	for _, process := range processes["processes"].([]any) {
		if process.(map[string]any)["pid"] == float64(pid) {
			found = true
		}
	}
	if !found {
		t.Fatalf("spawned fixture missing from process enumeration: %d", pid)
	}
	args := map[string]any{"action": "instrument", "pid": pid, "script": "send({marker:'trace-frida', pid:Process.id});", "duration_ms": 100}
	out := invokePreset(t, tool, args, false)
	messages := out["messages"].([]any)
	if out["detached"] != true || out["ok"] != true || len(messages) != 1 || messages[0].(map[string]any)["payload"].(map[string]any)["pid"] != float64(pid) {
		t.Fatalf("instrument: %+v", out)
	}
	args["script"] = "throw new Error('trace-script-error');"
	out = invokePreset(t, tool, args, true)
	if out["ok"] != false || out["detached"] != true || len(out["errors"].([]any)) == 0 {
		t.Fatalf("script error: %+v", out)
	}
	// A repeating script writes ticks. Killing the worker must unload that script,
	// stop the ticks, and leave the independently started target alive.
	marker := filepath.Join(t.TempDir(), "ticks.txt")
	encoded, _ := json.Marshal(marker)
	args["script"] = "let n=0; function tick(){let f=new File(" + string(encoded) + ",'w'); f.write(String(++n)); f.close();} tick(); setInterval(tick,20);"
	args["duration_ms"] = 60000
	data, _ := json.Marshal(args)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type completed struct {
		result worker.Result
		err    error
	}
	done := make(chan completed, 1)
	go func() {
		result, err := tool.Execute(ctx, "frida-cancel", "instrument", data)
		done <- completed{result, err}
	}()
	deadline := time.After(10 * time.Second)
	for {
		if contents, err := os.ReadFile(marker); err == nil && len(contents) > 0 {
			break
		}
		select {
		case result := <-done:
			t.Fatalf("instrument exited before timer ready: %+v %v", result.result, result.err)
		case <-deadline:
			cancel()
			<-done
			t.Fatal("Frida timer did not start")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	result := <-done
	if result.err == nil || result.result.Status != worker.StatusCancelled {
		t.Fatalf("cancel: %+v %v", result.result, result.err)
	}
	time.Sleep(300 * time.Millisecond)
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	after, err := os.ReadFile(marker)
	if err != nil || string(before) != string(after) {
		t.Fatalf("script remains active after cancellation: %q -> %q (%v)", before, after, err)
	}
	args["script"] = "send('reattached');"
	args["duration_ms"] = 100
	out = invokePreset(t, tool, args, false)
	if out["detached"] != true || len(out["messages"].([]any)) != 1 {
		t.Fatalf("reattach after cancel: %+v", out)
	}
}

func TestFridaCleanupFailure(t *testing.T) {
	tools := installedPresets(t)
	tool := tools["binary-info"]
	tool.Name = "frida"
	tool.Args = []string{"-c", helperSource, "frida"}
	module := writeFixture(t, "frida.py", []byte(`
class Script:
 def on(self, event, callback): pass
 def load(self): pass
 def unload(self): raise RuntimeError("unload failed")
class Session:
 def create_script(self, source): return Script()
 def detach(self): raise RuntimeError("detach failed")
class Device:
 id = "fixture"
 def attach(self, pid): return Session()
def get_local_device(): return Device()
`))
	tool.Env["PYTHONPATH"] = filepath.Dir(module)
	out := invokePreset(t, tool, map[string]any{"action": "instrument", "pid": 1, "script": "send(1)", "duration_ms": 0}, true)
	if out["ok"] != false || out["detached"] != false || len(out["errors"].([]any)) != 2 {
		t.Fatalf("cleanup failures reported as success: %+v", out)
	}
}
