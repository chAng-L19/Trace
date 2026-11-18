package localtool

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCloakBrowserManifest(t *testing.T) {
	missing, status := cloakBrowserPreset(context.Background(), "")
	if status.Available || status.Detail == "" || missing.Name != "" {
		t.Fatal(status)
	}
	m := cloakBrowserManifest("python-fixture")
	for _, token := range []string{"set_document", "set_content", "console/pageerror"} {
		if !strings.Contains(m.Description, token) || !strings.Contains(string(m.InputSchema), token) {
			t.Fatalf("browser capability contract missing %q", token)
		}
	}
	if m.Validate() != nil || m.Name != "cloakbrowser" || m.CancelGraceSeconds == 0 {
		t.Fatal(m)
	}
	args := json.RawMessage(`{"code":"result=args","args":{"literal":"{{run_id}};$(do-not-interpolate)"}}`)
	request, err := m.Request("run-one", "call", args)
	if err != nil || request.Stdin != string(args)+"\n" || !reflect.DeepEqual(request.Env, []string{"CLOAKBROWSER_AUTO_UPDATE=false", "PYTHONIOENCODING=utf-8", "TRACE_BROWSER_RUN_ID=run-one"}) {
		t.Fatalf("literal stdin/run environment: %+v %v", request, err)
	}
	if _, err := m.Request("", "call", args); err == nil {
		t.Fatal("missing run identity")
	}
}

func TestCloakBrowserAdapterContract(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	python, _, _, _ := discoverPython(ctx)
	if python == "" {
		t.Skip("Python is not installed")
	}
	if output, err := exec.CommandContext(ctx, python, "-c", "import psutil").CombinedOutput(); err != nil {
		t.Skipf("psutil is not installed: %s", output)
	}
	dir := t.TempDir()
	stub := `import os
__version__ = '0.6.4'
def binary_info():
    raise AssertionError('explicit binary must win')
def launch(**kwargs):
    assert kwargs['headless'] is False, kwargs
    assert kwargs['handle_sigterm'] is False, kwargs
    assert kwargs['handle_sigint'] is False, kwargs
    assert os.path.isdir(os.environ['TMPDIR'])
    if os.environ.get('TRACE_CLOAK_FAIL_LAUNCH'):
        raise RuntimeError('startup failed')
    print('SDK diagnostic')
    return Browser()
class Browser:
    def new_context(self): return Context()
    def close(self): print('browser closed')
class Context:
    def set_default_timeout(self, value): assert value == 10000
    def set_default_navigation_timeout(self, value): assert value == 30000
    def new_page(self): return object()
    def close(self):
        print('context closed')
        if os.environ.get('TRACE_CLOAK_FAIL_CLOSE'): raise RuntimeError('close failed')
`
	if err := os.WriteFile(filepath.Join(dir, "cloakbrowser.py"), []byte(stub), 0600); err != nil {
		t.Fatal(err)
	}
	// An existing file suffices for the stub. No browser is started or downloaded.
	// Keep packaged dependencies (psutil) while the SDK stub takes precedence.
	pythonPath := dir
	if existing := os.Getenv("PYTHONPATH"); existing != "" {
		pythonPath += string(os.PathListSeparator) + existing
	}
	t.Setenv("PYTHONPATH", pythonPath)
	t.Setenv("CLOAKBROWSER_BINARY_PATH", filepath.Join(dir, "cloakbrowser.py"))
	t.Setenv("DISPLAY", ":fixture")
	m, status := cloakBrowserPreset(ctx, python)
	if !status.Available {
		t.Fatalf("SDK-only discovery invoked browser setup: %+v", status)
	}
	m.Dir = dir
	for _, test := range []struct {
		name, code, failEnv, cleanup string
		ok                           bool
	}{
		{"literal and headed", "print('user diagnostic'); result=args", "", "completed", true},
		{"action failure", "raise RuntimeError('intentional-error')", "", "completed", false},
		{"serialization", "result=object()", "", "completed", false},
		{"startup failure", "result='not reached'", "TRACE_CLOAK_FAIL_LAUNCH", "completed", false},
		{"cleanup failure", "result='observed'", "TRACE_CLOAK_FAIL_CLOSE", "failed", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.failEnv != "" {
				t.Setenv(test.failEnv, "1")
			}
			args, _ := json.Marshal(map[string]any{"code": test.code, "args": map[string]string{"value": "中文 {{run_id}} $(literal)"}})
			result, err := m.Execute(ctx, "fixture", "call", args)
			var out map[string]any
			if json.Unmarshal([]byte(result.Stdout), &out) != nil || (err == nil) != test.ok || out["ok"] != test.ok || out["cleanup"] != test.cleanup {
				t.Fatalf("contract: %+v %v", result, err)
			}
			if test.ok && (out["result"].(map[string]any)["value"] != "中文 {{run_id}} $(literal)" || !strings.Contains(result.Stderr, "SDK diagnostic") || !strings.Contains(result.Stderr, "user diagnostic") || !strings.Contains(result.Stderr, "browser closed")) {
				t.Fatalf("literal/diagnostic/cleanup: %+v", result)
			}
		})
	}
	t.Setenv("CLOAKBROWSER_BINARY_PATH", filepath.Join(dir, "missing-browser"))
	result, err := m.Execute(ctx, "fixture", "missing-binary", json.RawMessage(`{"code":"result=1"}`))
	if err == nil || !strings.Contains(result.Stdout, "preinstall") || strings.Contains(result.Stderr, "SDK diagnostic") {
		t.Fatalf("missing binary must fail without download/launch: %+v %v", result, err)
	}
}

func installedCloakBrowser(t *testing.T) Manifest {
	t.Helper()
	if os.Getenv("TRACE_CLOAKBROWSER_TEST") != "1" {
		t.Skip("set TRACE_CLOAKBROWSER_TEST=1 for live headed browser regression")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	python, _, _, _ := discoverPython(ctx)
	m, status := cloakBrowserPreset(ctx, python)
	if !status.Available {
		t.Fatalf("CloakBrowser unavailable: %+v", status)
	}
	return m
}

func TestCloakBrowserBrowser(t *testing.T) {
	m := installedCloakBrowser(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<title>Cloak fixture</title><input id="value"><button onclick="document.querySelector('#answer').textContent=document.querySelector('#value').value">Apply</button><p id="answer">initial</p>`))
	}))
	defer server.Close()
	dir := t.TempDir()
	m.Dir = dir
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	invoke := func(code, run string) (map[string]any, error) {
		t.Helper()
		args, _ := json.Marshal(map[string]any{"code": code, "args": map[string]string{"url": server.URL, "text": "中文 {{run_id}} $(literal)", "screenshot": filepath.Join(dir, "browser.png")}})
		result, err := m.Execute(ctx, run, "browser", args)
		var out map[string]any
		if json.Unmarshal([]byte(result.Stdout), &out) != nil {
			t.Fatalf("JSONL: %+v %v", result, err)
		}
		if out["cleanup"] != "completed" {
			t.Fatalf("cleanup: %+v stderr=%s", out, result.Stderr)
		}
		return out, err
	}
	code := `import psutil
headed = False
for child in psutil.Process().children(recursive=True):
    try:
        command = child.cmdline()
    except psutil.NoSuchProcess:
        continue
    if '--remote-debugging-pipe' in command:
        assert not any(arg.startswith('--headless') for arg in command), command
        headed = True
assert headed, 'real Chromium process not observed'
page.goto(args['url']); page.locator('#value').fill(args['text']); page.get_by_role('button', name='Apply').click(); page.screenshot(path=args['screenshot']); result={'title':page.title(),'text':page.locator('#answer').inner_text(),'old':page.evaluate("localStorage.getItem('trace')"),'headed':headed}; page.evaluate("localStorage.setItem('trace','first')"); print('diagnostic output')`
	out, err := invoke(code, "run-one")
	if err != nil || out["ok"] != true {
		t.Fatalf("browser: %+v %v", out, err)
	}
	data := out["result"].(map[string]any)
	if data["title"] != "Cloak fixture" || data["text"] != "中文 {{run_id}} $(literal)" || data["old"] != nil || data["headed"] != true {
		t.Fatal(data)
	}
	image, err := os.ReadFile(filepath.Join(dir, "browser.png"))
	if err != nil || len(image) < 8 || string(image[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("screenshot: %v", err)
	}
	// Calls within the same Run are isolated too, not just different Runs.
	for _, run := range []string{"run-one", "run-two"} {
		out, err = invoke(`page.goto(args['url']); result=page.evaluate("localStorage.getItem('trace')")`, run)
		if err != nil || out["result"] != nil || out["ok"] != true {
			t.Fatalf("browser state leaked: %+v %v", out, err)
		}
	}
	for _, code := range []string{"raise RuntimeError('intentional-browser-error')", "result=object()"} {
		out, err = invoke(code, "run-one")
		if err == nil || out["ok"] != false || out["error"] == nil {
			t.Fatalf("error must propagate: %+v %v", out, err)
		}
	}
}

func TestCloakBrowserCancellation(t *testing.T) {
	m := installedCloakBrowser(t)
	dir := t.TempDir()
	m.Dir = dir
	marker := filepath.Join(dir, "ready")
	code := `from pathlib import Path; import os, time, json, psutil; root=psutil.Process(); Path(args['marker']).write_text(json.dumps({'children':[{'pid':p.pid,'created':p.create_time()} for p in root.children(recursive=True)],'profile':os.environ['TMPDIR']})); time.sleep(60)`
	args, _ := json.Marshal(map[string]any{"code": code, "args": map[string]string{"marker": marker}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan struct{})
	go func() {
		defer close(ready)
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		cancel()
	}()
	result, err := m.Execute(ctx, "cancel-run", "browser", args)
	<-ready
	if _, markerErr := os.Stat(marker); markerErr != nil {
		t.Fatalf("browser did not reach running action: %s", result.Stderr)
	}
	if err == nil || !result.OutcomeUncertain || !strings.Contains(result.Stdout, `"cleanup": "completed"`) {
		t.Fatalf("cancel cleanup: %+v %v", result, err)
	}
	owned, readErr := os.ReadFile(marker)
	if readErr != nil {
		t.Fatal(readErr)
	}
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer checkCancel()
	check := `import json, sys, pathlib, psutil
owned=json.loads(sys.argv[1])
assert not pathlib.Path(owned['profile']).exists(), owned
for p in psutil.process_iter(['pid','create_time','status']):
    assert not any(p.info['pid']==c['pid'] and p.info['create_time']==c['created'] and p.info['status']!=psutil.STATUS_ZOMBIE for c in owned['children']), owned
`
	if output, checkErr := exec.CommandContext(checkCtx, m.Command, "-c", check, string(owned)).CombinedOutput(); checkErr != nil {
		t.Fatalf("owned browser/profile remain: %s %v", output, checkErr)
	}
}
