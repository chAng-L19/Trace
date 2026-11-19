package localtool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
)

func TestLightpandaOfflineDiscovery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixtures")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	write := func(name, data string, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(data), mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	if err := os.Mkdir(filepath.Join(dir, "playwright"), 0700); err != nil {
		t.Fatal(err)
	}
	write("playwright/__init__.py", "", 0600)
	write("playwright/sync_api.py", "def sync_playwright(): raise AssertionError('driver started during discovery')", 0600)
	write("psutil.py", "", 0600)
	write("sitecustomize.py", `import os, sys
def audit(event, args):
    if event in ('socket.connect', 'socket.bind', 'socket.getaddrinfo', 'subprocess.Popen', 'os.system', 'os.posix_spawn', 'os.exec'):
        with open(os.environ['TRACE_PROBE_TOUCHED'], 'a') as marker:
            marker.write(event)
        raise RuntimeError('unexpected discovery side effect: ' + event)
sys.addaudithook(audit)
`, 0600)
	const sentinel = "#!/bin/sh\nprintf launched >>\"$TRACE_PROBE_TOUCHED\"\nexit 1\n"
	fromPath := write("lightpanda", sentinel, 0700)
	explicit := write("pinned binary", sentinel, 0700)
	marker := filepath.Join(dir, "side-effects")
	t.Setenv("TRACE_PROBE_TOUCHED", marker)
	t.Setenv("PYTHONPATH", dir)
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, tc := range []struct{ setting, want string }{
		{explicit, explicit}, {"", fromPath}, {filepath.Join(dir, "absent"), ""},
	} {
		t.Setenv("LIGHTPANDA_BINARY_PATH", tc.setting)
		path, status := discoverLightpanda(ctx, python)
		if path != tc.want || status.Available != (tc.want != "") || status.Detail == "" {
			t.Fatalf("setting=%q: path=%q status=%+v", tc.setting, path, status)
		}
	}
	t.Setenv("LIGHTPANDA_BINARY_PATH", explicit)
	if path, status := discoverLightpanda(ctx, ""); path != "" || status.Available {
		t.Fatalf("missing Python: %q %+v", path, status)
	}
	write("psutil.py", "raise ImportError('fixture missing psutil')", 0600)
	if path, status := discoverLightpanda(ctx, python); path != "" || status.Available || !strings.Contains(status.Detail, "fixture missing psutil") {
		t.Fatalf("missing dependency: %q %+v", path, status)
	}
	if data, err := os.ReadFile(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discovery side effects: %q %v", data, err)
	}
}

// Opt-in acceptance of the *registered* tool, never a test-only replacement.
func TestLightpandaHTTPActions(t *testing.T) {
	if os.Getenv("TRACE_LIGHTPANDA_TEST") != "1" {
		t.Skip("server only: TRACE_LIGHTPANDA_TEST=1 and an explicit LIGHTPANDA_BINARY_PATH required")
	}
	binary := os.Getenv("LIGHTPANDA_BINARY_PATH")
	if binary == "" {
		t.Fatal("explicit LIGHTPANDA_BINARY_PATH required for acceptance")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTPANDA_BINARY_PATH", binary)
	t.Setenv("LIGHTPANDA_DISABLE_TELEMETRY", "false")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	manifests, statuses := Presets(ctx)
	cancel()
	var m Manifest
	for _, candidate := range manifests {
		if candidate.Name == "cloakbrowser" && m.Name == "" {
			t.Fatalf("lightpanda missing before cloakbrowser: %+v", statuses)
		}
		if candidate.Name == "lightpanda" {
			m = candidate
		}
	}
	if m.Name == "" {
		t.Fatalf("no registered lightpanda preset: %+v", statuses)
	}
	if m.Env["LIGHTPANDA_DISABLE_TELEMETRY"] != "true" {
		t.Fatal("not starting browser: preset must pin LIGHTPANDA_DISABLE_TELEMETRY=true")
	}
	const literal = "中文 {{run_id}} $(literal) ' \""
	for _, mode := range []string{"success", "error", "nonjson", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var posts atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				io.WriteString(w, `<title>Lightpanda fixture</title><input id="v"><button id="apply">Apply</button><output id="o"></output><script>document.querySelector('#apply').onclick = async () => { const r = await fetch('/submit', {method:'POST', body:document.querySelector('#v').value}); document.querySelector('#o').textContent = await r.text(); };</script>`)
			})
			mux.HandleFunc("/submit", func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				data, err := io.ReadAll(r.Body)
				if err != nil || r.Method != "POST" || string(data) != literal {
					t.Errorf("unexpected submit: %s %q %v", r.Method, data, err)
				}
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.Write(data)
			})
			server := httptest.NewServer(mux)
			defer server.Close()
			m.Dir = t.TempDir()
			marker := filepath.Join(m.Dir, "owned.json")
			const action = `import json, os, psutil, subprocess, sys, time
from pathlib import Path
children = psutil.Process().children(recursive=True)
servers = [p for p in children if os.path.samefile(p.exe(), args['binary']) and 'serve' in p.cmdline()]
assert len(servers) == 1, 'expected one owned lightpanda serve'
assert servers[0].environ().get('LIGHTPANDA_DISABLE_TELEMETRY') == 'true', 'server telemetry not disabled'
Path(args['marker']).write_text(json.dumps({'children': [[p.pid, p.create_time()] for p in children], 'tmp': os.environ['TMPDIR']}))
page.goto(args['url'])
page.locator('#v').fill(args['text'])
page.locator('#apply').click()
page.wait_for_function("v => document.querySelector('#o').textContent === v", arg=args['text'], timeout=5000)
result = [page.title(), page.locator('#o').text_content()]
os.write(1, b'fixture-native-diagnostic\n')
subprocess.run([sys.executable, '-c', "print('fixture-child-diagnostic')"], check=True)
print('fixture-action-finished', flush=True)
if args['mode'] == 'error': raise ValueError('after-submit')
if args['mode'] == 'nonjson': result = object()
if args['mode'] == 'cancel': time.sleep(60)
`
			input, err := json.Marshal(map[string]any{"code": action, "args": map[string]string{
				"url": server.URL, "mode": mode, "text": literal, "marker": marker, "binary": binary,
			}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
			defer cancel()
			var diagnostics strings.Builder
			r, err := m.Execute(ctx, "fixture", mode, input, func(stream, chunk string) {
				if stream == "stderr" && mode == "cancel" {
					diagnostics.WriteString(chunk)
					if strings.Contains(diagnostics.String(), "fixture-action-finished") {
						cancel()
					}
				}
			})
			var out struct {
				Backend, Cleanup, Error string
				OK                      *bool
				Result                  []string
			}
			if decode := json.Unmarshal([]byte(r.Stdout), &out); decode != nil || strings.Contains(strings.TrimSpace(r.Stdout), "\n") || out.Backend != "lightpanda" || out.Cleanup != "completed" || out.OK == nil {
				t.Fatalf("JSONL/cleanup: %q, stderr=%s, err=%v", r.Stdout, r.Stderr, err)
			}
			checkCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			const check = `import json, os, psutil, sys
with open(sys.argv[1]) as f: owned = json.load(f)
assert not os.path.lexists(owned['tmp']), owned
for pid, born in owned['children']:
    try: assert psutil.Process(pid).create_time() != born, (pid, born)
    except psutil.NoSuchProcess: pass
`
			if output, e := exec.CommandContext(checkCtx, m.Command, "-B", "-c", check, marker).CombinedOutput(); e != nil {
				t.Errorf("independent cleanup check: %s %v", output, e)
			}
			if posts.Load() != 1 || (!strings.Contains(r.Stderr, "fixture-action-finished") || !strings.Contains(r.Stderr, "fixture-native-diagnostic") || !strings.Contains(r.Stderr, "fixture-child-diagnostic")) || *out.OK != (mode == "success") || (err == nil) != *out.OK {
				t.Fatalf("posts=%d stdout=%s stderr=%s err=%v", posts.Load(), r.Stdout, r.Stderr, err)
			}
			switch mode {
			case "success":
				if len(out.Result) != 2 || out.Result[0] != "Lightpanda fixture" || out.Result[1] != literal {
					t.Fatal(out.Result)
				}
			case "error":
				if !strings.Contains(out.Error, "after-submit") {
					t.Fatal(out.Error)
				}
			case "nonjson":
				if !strings.Contains(out.Error, "TypeError") {
					t.Fatal(out.Error)
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) || !r.OutcomeUncertain {
					t.Fatalf("cancellation result: %s %v", r.Status, err)
				}
			}
		})
	}
}
