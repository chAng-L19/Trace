package localtool

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trace/internal/worker"
)

func installedPresets(t *testing.T) map[string]Manifest {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manifests, status := Presets(ctx)
	result := map[string]Manifest{}
	for _, manifest := range manifests {
		if err := manifest.Validate(); err != nil {
			t.Fatal(err)
		}
		result[manifest.Name] = manifest
	}
	if _, ok := result["binary-info"]; !ok {
		t.Skipf("Python runtime not installed: %+v", status)
	}
	return result
}

func invokePreset(t *testing.T, manifest Manifest, args any, wantError bool) map[string]any {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Dir = t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	result, err := manifest.Execute(ctx, "preset-run", "preset-call", data)
	if (err != nil) != wantError || result.ExitCode == nil || result.OutcomeUncertain {
		t.Fatalf("%s: result=%+v err=%v", manifest.Name, result, err)
	}
	if wantError && (result.Status != worker.StatusFailed || *result.ExitCode != 1) {
		t.Fatalf("unexpected error status: %+v", result)
	}
	var output map[string]any
	if err := json.Unmarshal([]byte(result.Stdout), &output); err != nil {
		t.Fatalf("%s: invalid JSON output %q: %v", manifest.Name, result.Stdout, err)
	}
	return output
}

func writeFixture(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPresetsNetworkTools(t *testing.T) {
	tools := installedPresets(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/post":
			if r.Method != "POST" || r.Header.Get("X-Fixture") != "present" {
				w.WriteHeader(http.StatusBadRequest)
			}
			_, _ = io.Copy(w, r.Body)
		case "/missing":
			http.Error(w, "absent", http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, `<title>中文 &amp; title</title><script>hidden</script><p>Visible page</p><a href="/link">link</a><form action="/submit" method="post"><input name="query" value="hello"><textarea name="body"></textarea></form>`)
		}
	}))
	defer server.Close()
	out := invokePreset(t, tools["http-request"], map[string]any{"url": server.URL + "/post", "method": "POST", "body": "payload 中文", "headers": map[string]string{"X-Fixture": "present"}}, false)
	if out["status"] != float64(200) || out["body"] != "payload 中文" || out["truncated"] != false || out["tls_verified"] != nil {
		t.Fatalf("HTTP output: %+v", out)
	}
	out = invokePreset(t, tools["http-request"], map[string]any{"url": server.URL + "/missing", "max_bytes": 3}, false)
	if out["status"] != float64(404) || out["body"] != "abs" || out["truncated"] != true {
		t.Fatalf("HTTP error observation: %+v", out)
	}
	out = invokePreset(t, tools["page-fetch"], map[string]any{"url": server.URL}, false)
	forms := out["forms"].([]any)
	if out["title"] != "中文 & title" || strings.Contains(out["text"].(string), "hidden") || out["links"].([]any)[0] != server.URL+"/link" || len(forms) != 1 {
		t.Fatalf("page: %+v", out)
	}
	form := forms[0].(map[string]any)
	if form["action"] != server.URL+"/submit" || form["method"] != "POST" || len(form["fields"].([]any)) != 2 {
		t.Fatalf("form: %+v", form)
	}
	out = invokePreset(t, tools["dns-resolve"], map[string]any{"host": "127.0.0.1"}, false)
	if out["addresses"].([]any)[0].(map[string]any)["address"] != "127.0.0.1" {
		t.Fatalf("DNS: %+v", out)
	}
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	var openPort int
	_, _ = fmt.Sscan(port, &openPort)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPort := closed.Addr().(*net.TCPAddr).Port
	_ = closed.Close()
	out = invokePreset(t, tools["port-probe"], map[string]any{"host": "127.0.0.1", "ports": []int{openPort, closedPort}, "timeout": 0.1}, false)
	ports := out["ports"].([]any)
	if ports[0].(map[string]any)["connected"] != true || ports[1].(map[string]any)["connected"] != false {
		t.Fatalf("TCP: %+v", out)
	}
	for _, invalid := range []any{[]int{65536}, []int{0}, []any{true}, []any{1.5}, "80"} {
		out = invokePreset(t, tools["port-probe"], map[string]any{"host": "127.0.0.1", "ports": invalid}, true)
		if out["error"].(map[string]any)["type"] != "ValueError" {
			t.Fatalf("invalid port: %+v", out)
		}
	}
}

func TestPresetsHTTPExplicitTLSVerification(t *testing.T) {
	tools := installedPresets(t)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<title>TLS fixture</title>")
	}))
	defer server.Close()
	for _, name := range []string{"http-request", "page-fetch"} {
		out := invokePreset(t, tools[name], map[string]any{"url": server.URL}, true)
		if !strings.Contains(fmt.Sprint(out["error"]), "CERTIFICATE_VERIFY_FAILED") {
			t.Fatalf("%s accepted an untrusted certificate: %+v", name, out)
		}
		out = invokePreset(t, tools[name], map[string]any{"url": server.URL, "verify_tls": true}, true)
		if !strings.Contains(fmt.Sprint(out["error"]), "CERTIFICATE_VERIFY_FAILED") {
			t.Fatalf("%s ignored explicit certificate verification: %+v", name, out)
		}
		out = invokePreset(t, tools[name], map[string]any{"url": server.URL, "verify_tls": false}, false)
		if out["status"] != float64(200) || out["tls_verified"] != false {
			t.Fatalf("%s explicit TLS result: %+v", name, out)
		}
		if name == "page-fetch" && out["title"] != "TLS fixture" {
			t.Fatalf("page title: %+v", out)
		}
		out = invokePreset(t, tools[name], map[string]any{"url": server.URL, "verify_tls": "false"}, true)
		if out["error"].(map[string]any)["type"] != "ValueError" {
			t.Fatalf("%s invalid TLS option: %+v", name, out)
		}
	}
}

func TestPresetsBinaryTools(t *testing.T) {
	tools := installedPresets(t)
	elf := make([]byte, 64)
	copy(elf, []byte{0x7f, 'E', 'L', 'F', 2, 1})
	binary.LittleEndian.PutUint16(elf[16:], 3)
	binary.LittleEndian.PutUint16(elf[18:], 62)
	pe := make([]byte, 128)
	copy(pe, "MZ")
	binary.LittleEndian.PutUint32(pe[60:], 64)
	copy(pe[64:], "PE\x00\x00")
	binary.LittleEndian.PutUint16(pe[68:], 0x8664)
	binary.LittleEndian.PutUint16(pe[70:], 3)
	binary.LittleEndian.PutUint16(pe[88:], 0x20b)
	macho := make([]byte, 32)
	copy(macho, []byte{0xcf, 0xfa, 0xed, 0xfe})
	binary.LittleEndian.PutUint32(macho[4:], 0x1000007)
	for format, content := range map[string][]byte{"ELF": elf, "PE": pe, "Mach-O": macho} {
		t.Run(format, func(t *testing.T) {
			path := writeFixture(t, "binary & literal.bin", content)
			out := invokePreset(t, tools["binary-info"], map[string]any{"path": path}, false)
			if out["format"] != format || out["bits"] != float64(64) || out["sha256"] != fmt.Sprintf("%x", sha256.Sum256(content)) || out["size"] != float64(len(content)) {
				t.Fatalf("binary: %+v", out)
			}
		})
	}
	path := writeFixture(t, "strings.bin", []byte("\x00HELLO\x00SECOND\x00T\x00E\x00X\x00T\x00"))
	out := invokePreset(t, tools["strings"], map[string]any{"path": path, "limit": 1}, false)
	first := out["strings"].([]any)[0].(map[string]any)
	if first["text"] != "HELLO" || first["offset"] != float64(1) || out["truncated"] != true {
		t.Fatalf("ASCII: %+v", out)
	}
	out = invokePreset(t, tools["strings"], map[string]any{"path": path, "encoding": "utf-16le"}, false)
	if !strings.HasSuffix(out["strings"].([]any)[0].(map[string]any)["text"].(string), "TEXT") {
		t.Fatalf("UTF-16: %+v", out)
	}
	out = invokePreset(t, tools["analysis"], map[string]any{"path": path}, false)
	if out["binary"].(map[string]any)["sha256"] == nil || out["strings"] == nil {
		t.Fatalf("analysis: %+v", out)
	}
	bad := writeFixture(t, "truncated.elf", []byte("\x7fELF"))
	out = invokePreset(t, tools["binary-info"], map[string]any{"path": bad}, true)
	if !strings.Contains(out["error"].(map[string]any)["message"].(string), "truncated") {
		t.Fatalf("bad header: %+v", out)
	}
	invokePreset(t, tools["binary-info"], map[string]any{"path": filepath.Join(t.TempDir(), "absent")}, true)
	java := writeFixture(t, "Example.class", []byte{0xca, 0xfe, 0xba, 0xbe, 0, 0, 0, 61, 0, 1})
	out = invokePreset(t, tools["binary-info"], map[string]any{"path": java}, false)
	if out["format"] != "ambiguous" || len(out["format_candidates"].([]any)) != 2 {
		t.Fatalf("shared Java/Mach-O magic: %+v", out)
	}
}

func TestPresetsSourceTools(t *testing.T) {
	tools := installedPresets(t)
	path := writeFixture(t, "source.py", []byte("import subprocess as sp\nfrom pickle import loads as decode\nsp.run('echo data', shell=True)\ndecode(b'data')\n# 中文 needle\n"))
	out := invokePreset(t, tools["code-search"], map[string]any{"path": path, "pattern": "中文 needle"}, false)
	matches := out["matches"].([]any)
	if len(matches) != 1 || matches[0].(map[string]any)["line"] != float64(5) {
		t.Fatalf("search: %+v", out)
	}
	out = invokePreset(t, tools["code-search"], map[string]any{"path": filepath.Dir(path), "pattern": "sp\\.run", "regex": true, "glob": "*.py"}, false)
	if len(out["matches"].([]any)) != 1 {
		t.Fatalf("regex search: %+v", out)
	}
	out = invokePreset(t, tools["python-ast-audit"], map[string]any{"path": path}, false)
	findings := out["findings"].([]any)
	if len(findings) != 2 || findings[0].(map[string]any)["rule"] != "subprocess-shell" || findings[1].(map[string]any)["call"] != "pickle.loads" || out["heuristic"] != true {
		t.Fatalf("audit aliases: %+v", out)
	}
	bad := writeFixture(t, "invalid.py", []byte("def broken(:"))
	out = invokePreset(t, tools["python-ast-audit"], map[string]any{"path": bad}, false)
	if len(out["errors"].([]any)) != 1 || len(out["findings"].([]any)) != 0 {
		t.Fatalf("syntax: %+v", out)
	}
	invokePreset(t, tools["code-search"], map[string]any{"path": path, "pattern": "[", "regex": true}, true)
	invokePreset(t, tools["strings"], map[string]any{"path": path, "limit": 0}, true)
}

func TestPresetsOptionalInstalledTools(t *testing.T) {
	tools := installedPresets(t)
	t.Run("capstone", func(t *testing.T) {
		tool, ok := tools["disassemble"]
		if !ok {
			t.Skip("Capstone not installed")
		}
		path := writeFixture(t, "instructions.bin", []byte{0x90, 0xc3})
		out := invokePreset(t, tool, map[string]any{"path": path, "arch": "x64", "address": 4096}, false)
		instructions := out["instructions"].([]any)
		if len(instructions) != 2 || instructions[0].(map[string]any)["mnemonic"] != "nop" || instructions[0].(map[string]any)["address"] != float64(4096) || instructions[1].(map[string]any)["mnemonic"] != "ret" {
			t.Fatalf("disassembly: %+v", out)
		}
	})
}

func TestPresetsFrida(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manifests, status := Presets(ctx)
	var frida Manifest
	for _, manifest := range manifests {
		if manifest.Name == "frida" {
			frida = manifest
		}
	}
	if frida.Name == "" {
		t.Skipf("Frida not installed: %+v", status)
	}
	out := invokePreset(t, frida, map[string]any{"action": "devices"}, false)
	devices, ok := out["devices"].([]any)
	if !ok || len(devices) == 0 {
		t.Fatalf("Frida devices: %+v", out)
	}
}

func TestPresetsMissingDependencies(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	manifests, status := Presets(context.Background())
	want := map[string]bool{"Python 3": true, "capstone": true, "droidasc": true, "frida": true, "CloakBrowser": true, "Lightpanda": true, "Rizin/radare2": true, "Trace Web Analysis": true}
	if len(manifests) != 0 || len(status) != len(want) {
		t.Fatalf("missing runtimes: %+v %+v", manifests, status)
	}
	for _, dependency := range status {
		if !want[dependency.Name] || dependency.Available || dependency.Detail == "" {
			t.Fatalf("dependency status: %+v", dependency)
		}
		delete(want, dependency.Name)
	}
}

func TestPresetFormattedJSONInput(t *testing.T) {
	tools := installedPresets(t)
	path := writeFixture(t, "pretty.json", []byte("formatted request"))
	args, err := json.MarshalIndent(map[string]string{"path": path}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	result, err := tools["binary-info"].Execute(context.Background(), "pretty-run", "pretty-call", args)
	if err != nil || result.ExitCode == nil || *result.ExitCode != 0 || !strings.Contains(result.Stdout, `"sha256"`) {
		t.Fatalf("formatted JSON request: %+v %v", result, err)
	}
}
