package localtool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"trace/internal/worker"
)

func lightpandaBoundaryPython(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("server-only Linux process/signal regression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	python, _, _, detail := discoverPython(ctx)
	if python == "" {
		t.Skipf("Python missing: %s", detail)
	}
	if output, err := exec.CommandContext(ctx, python, "-B", "-c", "import psutil; from playwright.sync_api import sync_playwright").CombinedOutput(); err != nil {
		if os.Getenv("TRACE_LIGHTPANDA_TEST") == "1" {
			t.Fatalf("required browser test dependencies: %s %v", output, err)
		}
		t.Skipf("browser test dependencies missing: %s %v", output, err)
	}
	return python
}

// A real subprocess with no listener, driver or browser implementation.
func lightpandaBoundaryFake(t *testing.T, python string) (Manifest, string, string) {
	t.Helper()
	dir := t.TempDir()
	state, userMarker := filepath.Join(dir, "owned.json"), filepath.Join(dir, "user-code")
	binary := filepath.Join(dir, "lightpanda")
	const script = `import json, os, sys, time, psutil
from pathlib import Path
assert sys.argv[1] == 'serve', sys.argv
assert sys.argv[sys.argv.index('--host') + 1] == '127.0.0.1', sys.argv
p = psutil.Process()
Path(os.environ['TRACE_BOUNDARY_STATE']).write_text(json.dumps({'port': int(sys.argv[sys.argv.index('--port') + 1]), 'children': [[p.pid, p.create_time()]], 'tmp': os.environ['TMPDIR']}))
print('boundary-fake-started', file=sys.stderr, flush=True)
time.sleep(60)
`
	if err := os.WriteFile(binary, []byte("#!"+python+"\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
	m := lightpandaManifest(python, binary)
	m.Dir, m.TimeoutSeconds = dir, 25
	m.Env["TRACE_BOUNDARY_STATE"] = state
	return m, state, userMarker
}

func lightpandaBoundarySignal(marker string, notify func()) (func(string, string), <-chan time.Time) {
	seen := make(chan time.Time, 1)
	var diagnostic strings.Builder
	var once sync.Once
	return func(stream, chunk string) {
		if stream != "stderr" {
			return
		}
		diagnostic.WriteString(chunk)
		if strings.Contains(diagnostic.String(), marker) {
			once.Do(func() { seen <- time.Now(); notify() })
		}
	}, seen
}

func lightpandaBoundaryResult(t *testing.T, r worker.Result, err error, cancelled bool) string {
	t.Helper()
	var out struct {
		Backend, Cleanup, Error string
		OK                      *bool
	}
	if decode := json.Unmarshal([]byte(r.Stdout), &out); decode != nil || strings.Contains(strings.TrimSpace(r.Stdout), "\n") || out.Backend != "lightpanda" || out.Cleanup != "completed" || out.OK == nil || *out.OK || err == nil {
		t.Errorf("failure/JSONL/cleanup contract: stdout=%q stderr=%s err=%v", r.Stdout, r.Stderr, err)
	}
	if cancelled {
		if !errors.Is(err, context.Canceled) || !r.OutcomeUncertain {
			t.Errorf("cancellation must retain uncertainty: %+v %v", r, err)
		}
	} else if r.OutcomeUncertain || r.ExitCode == nil || *r.ExitCode != 1 {
		t.Errorf("expected ordinary ownership failure: %+v %v", r, err)
	}
	return out.Error
}

func lightpandaBoundaryGrace(t *testing.T, m Manifest, seen <-chan time.Time, finished time.Time) {
	t.Helper()
	if m.CancelGraceSeconds != 10 {
		t.Errorf("expected worker's 10-second cancellation grace, got %d", m.CancelGraceSeconds)
	}
	select {
	case start := <-seen:
		if elapsed := finished.Sub(start); elapsed >= 10*time.Second {
			t.Errorf("cleanup exceeded worker grace: %s", elapsed)
		}
	default:
		t.Error("fixture never reached cancellation marker")
	}
}

func lightpandaBoundaryCleanup(t *testing.T, python, state, userMarker string) {
	t.Helper()
	const check = `import json, os, psutil, sys
with open(sys.argv[1]) as f: owned = json.load(f)
assert owned['children'] and owned['tmp'], owned
assert not os.path.lexists(owned['tmp']), owned
for pid, born in owned['children']:
    try: assert psutil.Process(pid).create_time() != born, ('owned PID identity survives', pid, born)
    except psutil.NoSuchProcess: pass
if len(sys.argv) > 2: assert not os.path.lexists(sys.argv[2]), 'user code ran before readiness'
`
	argv := []string{"-B", "-c", check, state}
	if userMarker != "" {
		argv = append(argv, userMarker)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, python, argv...).CombinedOutput(); err != nil {
		t.Errorf("independent PID/tempdir/user-code check: %s %v", output, err)
	}
}

func TestLightpandaCancelBeforeReadiness(t *testing.T) {
	python := lightpandaBoundaryPython(t)
	m, state, userMarker := lightpandaBoundaryFake(t, python)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	output, seen := lightpandaBoundarySignal("boundary-fake-started", cancel)
	input, _ := json.Marshal(map[string]any{"code": "from pathlib import Path; Path(args['marker']).touch()", "args": map[string]string{"marker": userMarker}})
	r, err := m.Execute(ctx, "boundary-run", "before-readiness", input, output)
	finished := time.Now()
	lightpandaBoundaryResult(t, r, err, true)
	lightpandaBoundaryGrace(t, m, seen, finished)
	lightpandaBoundaryCleanup(t, python, state, userMarker)
}

func TestLightpandaRejectForeignListener(t *testing.T) {
	python := lightpandaBoundaryPython(t)
	m, state, userMarker := lightpandaBoundaryFake(t, python)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ready := make(chan struct{})
	output, _ := lightpandaBoundarySignal("boundary-fake-started", func() { close(ready) })
	type listening struct {
		listener net.Listener
		err      error
	}
	bound := make(chan listening, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ready:
		case <-ctx.Done():
			bound <- listening{err: ctx.Err()}
			return
		}
		data, err := os.ReadFile(state)
		var owned struct{ Port int }
		if err == nil {
			err = json.Unmarshal(data, &owned)
		}
		if err != nil || owned.Port <= 0 {
			bound <- listening{err: fmt.Errorf("fake server port: %s %v", data, err)}
			return
		}
		listener, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", owned.Port))
		bound <- listening{listener, err}
		if err != nil {
			return
		}
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.SetDeadline(time.Now().Add(time.Second))
			var ping [5]byte
			if _, err := io.ReadFull(conn, ping[:]); err == nil && string(ping[:]) == "alive" {
				io.WriteString(conn, "alive")
			}
			conn.Close()
		}
	}()
	input, _ := json.Marshal(map[string]any{"code": "from pathlib import Path; Path(args['marker']).touch()", "args": map[string]string{"marker": userMarker}})
	r, err := m.Execute(ctx, "boundary-run", "foreign-listener", input, output)
	cancel()
	foreign := <-bound
	if foreign.err != nil {
		<-done
		t.Fatalf("external fixture listener: %v; stdout=%s stderr=%s", foreign.err, r.Stdout, r.Stderr)
	}
	defer func() { foreign.listener.Close(); <-done }()
	if message := lightpandaBoundaryResult(t, r, err, false); !strings.Contains(message, "CDP listener does not belong") {
		t.Errorf("expected explicit listener ownership rejection, got %q", message)
	}
	lightpandaBoundaryCleanup(t, python, state, userMarker)
	conn, err := net.DialTimeout("tcp4", foreign.listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("tool closed external listener: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(conn, "alive"); err != nil {
		t.Fatal(err)
	}
	var pong [5]byte
	if _, err := io.ReadFull(conn, pong[:]); err != nil || string(pong[:]) != "alive" {
		t.Fatalf("external listener no longer accepts/responds: %q %v", pong, err)
	}
}

func TestLightpandaCancelStoppedDriver(t *testing.T) {
	if os.Getenv("TRACE_LIGHTPANDA_TEST") != "1" {
		t.Skip("server only: TRACE_LIGHTPANDA_TEST=1 and explicit LIGHTPANDA_BINARY_PATH required")
	}
	python := lightpandaBoundaryPython(t)
	binary := os.Getenv("LIGHTPANDA_BINARY_PATH")
	if binary == "" {
		t.Fatal("explicit LIGHTPANDA_BINARY_PATH required")
	}
	binary, err := filepath.Abs(binary)
	if err != nil {
		t.Fatal(err)
	}
	m := lightpandaManifest(python, binary)
	m.Dir = t.TempDir()
	state := filepath.Join(m.Dir, "owned.json")
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/submit" {
			if r.Method != "POST" {
				t.Errorf("unexpected submission method: %s", r.Method)
			}
			posts.Add(1)
			io.WriteString(w, "accepted")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, `<button id="send" onclick="fetch('/submit',{method:'POST'}).then(r=>r.text()).then(t=>this.textContent=t)">Send</button>`)
	}))
	defer server.Close()
	const action = `import json, os, psutil, signal, time
from pathlib import Path
page.goto(args['url'])
page.locator('#send').click()
page.wait_for_function("document.querySelector('#send').textContent === 'accepted'", timeout=5000)
children = psutil.Process().children(recursive=True)
drivers = [p for p in children if Path(p.exe()).name == 'node' and 'run-driver' in p.cmdline()]
assert len(drivers) == 1, [(p.pid, p.cmdline()) for p in children]
driver = drivers[0]
owned = {'children': [[p.pid, p.create_time()] for p in children], 'tmp': os.environ['TMPDIR']}
driver.send_signal(signal.SIGSTOP)
deadline = time.monotonic() + 1
while driver.status() != psutil.STATUS_STOPPED and time.monotonic() < deadline:
    time.sleep(0.01)
assert driver.status() == psutil.STATUS_STOPPED, 'driver was not stopped'
owned['driver_stopped'] = True
Path(args['state']).write_text(json.dumps(owned))
print('boundary-driver-stopped', flush=True)
time.sleep(60)
`
	input, err := json.Marshal(map[string]any{"code": action, "args": map[string]string{"url": server.URL, "state": state}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	output, seen := lightpandaBoundarySignal("boundary-driver-stopped", cancel)
	r, err := m.Execute(ctx, "boundary-run", "stopped-driver", input, output)
	finished := time.Now()
	lightpandaBoundaryResult(t, r, err, true)
	lightpandaBoundaryGrace(t, m, seen, finished)
	lightpandaBoundaryCleanup(t, python, state, "")
	if posts.Load() != 1 {
		t.Errorf("submission must execute once without replay, got %d POSTs", posts.Load())
	}
}

// Transport ownership only: these sockets do not implement WebSocket or CDP.
func TestLightpandaRejectReplacedSession(t *testing.T) {
	python := lightpandaBoundaryPython(t)
	dir := t.TempDir()
	state, userMarker := filepath.Join(dir, "owned.json"), filepath.Join(dir, "user-code")
	release, rebound := filepath.Join(dir, "release"), filepath.Join(dir, "rebound")
	newContext, foreignClose := filepath.Join(dir, "new-context"), filepath.Join(dir, "foreign-close")
	const replacementServer = `import json, os, socket, sys, time, psutil
from pathlib import Path
assert sys.argv[1] == 'serve', sys.argv
port = int(sys.argv[sys.argv.index('--port') + 1])
listener = socket.socket()
listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
listener.bind(('127.0.0.1', port))
listener.listen()
p = psutil.Process()
Path(os.environ['TRACE_BOUNDARY_STATE']).write_text(json.dumps({'port': port, 'children': [[p.pid, p.create_time()]], 'tmp': os.environ['TMPDIR']}))
while not Path(os.environ['TRACE_BOUNDARY_RELEASE']).exists(): time.sleep(0.01)
listener.close()
print('boundary-rebind-ready', file=sys.stderr, flush=True)
time.sleep(60)
`
	const replacementPlaywright = `import json, os, socket, subprocess, sys, time, psutil
from pathlib import Path
from urllib.parse import urlsplit
class Browser:
    def new_context(self):
        Path(os.environ['TRACE_BOUNDARY_NEW_CONTEXT']).touch()
        return self
    def set_default_timeout(self, value): pass
    def set_default_navigation_timeout(self, value): pass
    def new_page(self): return self
    def close(self): Path(os.environ['TRACE_BOUNDARY_FOREIGN_CLOSE']).touch()
class Driver:
    def __init__(self): self.chromium = self
    def start(self):
        Path(os.environ['TRACE_BOUNDARY_RELEASE']).touch()
        deadline = time.monotonic() + 5
        while not Path(os.environ['TRACE_BOUNDARY_REBOUND']).exists():
            if time.monotonic() >= deadline: raise TimeoutError('external fixture did not rebind')
            time.sleep(0.01)
        return self
    def connect_over_cdp(self, endpoint, timeout):
        target = urlsplit(endpoint)
        child = "import socket,sys,time; s=socket.create_connection(('127.0.0.1',int(sys.argv[1])),timeout=2); print('connected',flush=True); time.sleep(60)"
        self.transport = subprocess.Popen([sys.executable, '-B', '-c', child, str(target.port)], stdout=subprocess.PIPE, text=True)
        assert self.transport.stdout.readline().strip() == 'connected', 'transport did not connect'
        state = Path(os.environ['TRACE_BOUNDARY_STATE'])
        owned = json.loads(state.read_text())
        server = psutil.Process(owned['children'][0][0])
        assert server.is_running() and server.create_time() == owned['children'][0][1]
        assert not any(c.status == psutil.CONN_LISTEN for c in server.net_connections(kind='tcp'))
        assert any(c.status == psutil.CONN_ESTABLISHED and c.raddr.port == target.port for c in psutil.Process(self.transport.pid).net_connections(kind='tcp'))
        owned['children'] = [[p.pid, p.create_time()] for p in psutil.Process().children(recursive=True)]
        state.write_text(json.dumps(owned))
        print('boundary-session-connected', flush=True)
        return Browser()
    def stop(self): pass
def sync_playwright(): return Driver()
`
	if err := os.Mkdir(filepath.Join(dir, "playwright"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"lightpanda":             "#!" + python + "\n" + replacementServer,
		"playwright/__init__.py": "", "playwright/sync_api.py": replacementPlaywright,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0700); err != nil {
			t.Fatal(err)
		}
	}
	m := lightpandaManifest(python, filepath.Join(dir, "lightpanda"))
	m.Dir, m.TimeoutSeconds = dir, 25
	for key, value := range map[string]string{
		"TRACE_BOUNDARY_STATE": state, "TRACE_BOUNDARY_RELEASE": release,
		"TRACE_BOUNDARY_REBOUND": rebound, "TRACE_BOUNDARY_NEW_CONTEXT": newContext,
		"TRACE_BOUNDARY_FOREIGN_CLOSE": foreignClose,
		"PYTHONPATH":                   dir + string(os.PathListSeparator) + os.Getenv("PYTHONPATH"),
	} {
		m.Env[key] = value
	}
	type listening struct {
		listener *net.TCPListener
		err      error
	}
	bound := make(chan listening, 1)
	output, _ := lightpandaBoundarySignal("boundary-rebind-ready", func() {
		var owned struct{ Port int }
		data, err := os.ReadFile(state)
		if err == nil {
			err = json.Unmarshal(data, &owned)
		}
		var listener *net.TCPListener
		if err == nil && owned.Port > 0 {
			listener, err = net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: owned.Port})
		} else if err == nil {
			err = fmt.Errorf("invalid fixture port: %s", data)
		}
		if err == nil {
			err = os.WriteFile(rebound, []byte("bound"), 0600)
		}
		bound <- listening{listener, err}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	input, _ := json.Marshal(map[string]any{"code": "from pathlib import Path; Path(args['marker']).touch()", "args": map[string]string{"marker": userMarker}})
	r, err := m.Execute(ctx, "boundary-run", "replaced-session", input, output)
	if message := lightpandaBoundaryResult(t, r, err, false); message != "RuntimeError: CDP session does not belong to this Lightpanda process" {
		t.Errorf("expected session ownership rejection, got %q", message)
	}
	lightpandaBoundaryCleanup(t, python, state, userMarker)
	for _, marker := range []string{newContext, foreignClose} {
		if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("foreign browser operation sentinel %s: %v", marker, err)
		}
	}
	if !strings.Contains(r.Stderr, "boundary-session-connected") {
		t.Errorf("fixture did not reach established foreign transport: %s", r.Stderr)
	}
	var foreign listening
	select {
	case foreign = <-bound:
	default:
		t.Fatal("startup probe did not reach the fake Playwright start() handoff")
	}
	if foreign.listener != nil {
		defer foreign.listener.Close()
	}
	if foreign.err != nil {
		t.Fatalf("external fixture rebind: %v", foreign.err)
	}
	conn, err := net.DialTimeout("tcp4", foreign.listener.Addr().String(), 2*time.Second)
	if err != nil {
		t.Fatalf("tool closed replacement listener: %v", err)
	}
	defer conn.Close()
	foreign.listener.SetDeadline(time.Now().Add(2 * time.Second))
	for {
		peer, err := foreign.listener.Accept()
		if err != nil {
			t.Fatalf("replacement listener stopped accepting: %v", err)
		}
		matches := peer.RemoteAddr().String() == conn.LocalAddr().String()
		peer.Close()
		if matches {
			break
		}
	}
}
