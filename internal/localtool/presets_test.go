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
	"reflect"
	"strings"
	"sync/atomic"
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

func TestPresetsHTTPJSONSchema(t *testing.T) {
	var shared string
	for _, spec := range pythonPresets {
		if spec.name != "http-request" && spec.name != "page-fetch" {
			continue
		}
		if shared != "" && shared != spec.schema {
			t.Fatal("HTTP tools must expose the same request schema")
		}
		shared = spec.schema
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Not        struct {
				Required []string `json:"required"`
			} `json:"not"`
		}
		if err := json.Unmarshal([]byte(spec.schema), &schema); err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Type    []string        `json:"type"`
			Default json.RawMessage `json:"default"`
		}
		if err := json.Unmarshal(schema.Properties["json"], &payload); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(payload.Type, []string{"object", "array", "string", "number", "boolean", "null"}) || len(payload.Default) != 0 {
			t.Fatalf("%s JSON types or omission default: %+v", spec.name, payload)
		}
		if !reflect.DeepEqual(schema.Not.Required, []string{"body", "json"}) || schema.Properties["method"] == nil || schema.Properties["body"] == nil {
			t.Fatalf("%s request body contract: %+v", spec.name, schema)
		}
	}
	if shared == "" {
		t.Fatal("HTTP presets missing")
	}
}

func TestPresetsNetworkTools(t *testing.T) {
	tools := installedPresets(t)
	type requestObservation struct {
		Body         string   `json:"body"`
		ContentTypes []string `json:"content_types"`
		Method       string   `json:"method"`
		Fixture      string   `json:"fixture"`
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/echo":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				http.Error(w, "read request body", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(requestObservation{
				Body: string(body), ContentTypes: r.Header.Values("Content-Type"),
				Method: r.Method, Fixture: r.Header.Get("X-Fixture"),
			}); err != nil {
				t.Error(err)
			}
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
	t.Run("http", func(t *testing.T) {
		if _, ok := tools["http-request"]; !ok {
			t.Skip("curl_cffi not installed")
		}
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
		for _, name := range []string{"http-request", "page-fetch"} {
			t.Run(name, func(t *testing.T) {
				observe := func(t *testing.T, args map[string]any) requestObservation {
					t.Helper()
					args["url"] = server.URL + "/echo"
					before := requests.Load()
					out := invokePreset(t, tools[name], args, false)
					if out["status"] != float64(200) || requests.Load() != before+1 {
						t.Fatalf("request count/status: count=%d before=%d output=%+v", requests.Load(), before, out)
					}
					field := "body"
					if name == "page-fetch" {
						field = "text"
					}
					var observed requestObservation
					if err := json.Unmarshal([]byte(out[field].(string)), &observed); err != nil {
						t.Fatalf("decode request observation: %v output=%+v", err, out)
					}
					if observed.Method != args["method"] {
						t.Fatalf("method changed: %+v", observed)
					}
					return observed
				}
				t.Run("omitted", func(t *testing.T) {
					observed := observe(t, map[string]any{"method": "GET"})
					if observed.Body != "" || len(observed.ContentTypes) != 0 {
						t.Fatalf("omitted json sent a payload or JSON Content-Type: %+v", observed)
					}
				})
				for _, tc := range []struct {
					name        string
					payload     any
					contentType string
				}{
					{"object", map[string]any{"text": "中文", "nested": []any{true, nil, 1.25}}, ""},
					{"null", nil, ""},
					{"array", []any{false, "中文", nil}, "application/vnd.fixture+json; charset=UTF-8"},
					{"string", `{"stays":"a string"}`, ""},
					{"number", 1.25, "application/json; charset=utf-8"},
					{"zero", 0, ""},
					{"false", false, ""},
					{"empty-string", "", ""},
				} {
					t.Run(tc.name, func(t *testing.T) {
						headers := map[string]string{"X-Fixture": "present"}
						if tc.contentType != "" {
							headers["content-type"] = tc.contentType
						}
						observed := observe(t, map[string]any{"method": "POST", "json": tc.payload, "headers": headers})
						wantContentType := tc.contentType
						if wantContentType == "" {
							wantContentType = "application/json"
						}
						if !reflect.DeepEqual(observed.ContentTypes, []string{wantContentType}) || observed.Fixture != "present" {
							t.Fatalf("JSON request headers: %+v", observed)
						}
						var got, want any
						if err := json.Unmarshal([]byte(observed.Body), &got); err != nil {
							t.Fatalf("invalid JSON on wire: %q: %v", observed.Body, err)
						}
						encoded, err := json.Marshal(tc.payload)
						if err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal(encoded, &want); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(got, want) {
							t.Fatalf("JSON value/type changed: got=%#v want=%#v wire=%q", got, want, observed.Body)
						}
					})
				}
				t.Run("raw-body", func(t *testing.T) {
					const body = "{raw body}\r\n中文"
					observed := observe(t, map[string]any{"method": "POST", "body": body, "headers": map[string]string{"Content-Type": "application/x-fixture"}})
					if observed.Body != body || !reflect.DeepEqual(observed.ContentTypes, []string{"application/x-fixture"}) {
						t.Fatalf("raw body/header changed: %+v", observed)
					}
				})
				for _, tc := range []struct {
					name    string
					args    map[string]any
					message string
				}{
					{"body-json", map[string]any{"body": "raw", "json": map[string]any{}}, "mutually exclusive"},
					{"empty-body-null-json", map[string]any{"body": "", "json": nil}, "mutually exclusive"},
					{"null-body-null-json", map[string]any{"body": nil, "json": nil}, "mutually exclusive"},
					{"content-type", map[string]any{"json": nil, "headers": map[string]string{"cOnTeNt-TyPe": "text/plain"}}, "Content-Type"},
					{"charset", map[string]any{"json": nil, "headers": map[string]string{"Content-Type": "application/json; charset=iso-8859-1"}}, "UTF-8"},
					{"duplicate-charset", map[string]any{"json": nil, "headers": map[string]string{"Content-Type": "application/json; charset=utf-8; charset=iso-8859-1"}}, "single UTF-8"},
					{"duplicate-content-type", map[string]any{"json": nil, "headers": map[string]string{"Content-Type": "application/json", "content-type": "application/json"}}, "single Content-Type"},
					{"non-finite-number", map[string]any{"json": json.RawMessage("1e400")}, "Out of range float"},
				} {
					t.Run(tc.name, func(t *testing.T) {
						tc.args["url"], tc.args["method"] = server.URL+"/echo", "POST"
						before := requests.Load()
						out := invokePreset(t, tools[name], tc.args, true)
						failure := out["error"].(map[string]any)
						if failure["type"] != "ValueError" || !strings.Contains(failure["message"].(string), tc.message) || requests.Load() != before {
							t.Fatalf("invalid request reached server or wrong error: before=%d after=%d output=%+v", before, requests.Load(), out)
						}
					})
				}
			})
		}
	})
	out := invokePreset(t, tools["dns-resolve"], map[string]any{"host": "127.0.0.1"}, false)
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
	if _, ok := tools["http-request"]; !ok {
		t.Skip("curl_cffi not installed")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "<title>TLS fixture</title>")
	}))
	defer server.Close()
	for _, name := range []string{"http-request", "page-fetch"} {
		out := invokePreset(t, tools[name], map[string]any{"url": server.URL}, true)
		if out["error"].(map[string]any)["type"] != "CertificateVerifyError" {
			t.Fatalf("%s accepted an untrusted certificate: %+v", name, out)
		}
		out = invokePreset(t, tools[name], map[string]any{"url": server.URL, "verify_tls": true}, true)
		if out["error"].(map[string]any)["type"] != "CertificateVerifyError" {
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

func TestPresetsHTTPTransport(t *testing.T) {
	tools := installedPresets(t)
	if _, ok := tools["http-request"]; !ok {
		t.Skip("curl_cffi not installed")
	}
	for _, name := range []string{"http-request", "page-fetch"} {
		m := tools[name]
		if m.GitHubURL != "https://github.com/lexiforest/curl_cffi" {
			t.Fatalf("%s source identity: %+v", name, m)
		}
		var schema struct {
			Properties map[string]struct {
				Default any `json:"default"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(m.InputSchema, &schema); err != nil || schema.Properties["impersonate"].Default != "chrome" {
			t.Fatalf("%s impersonation schema: %+v %v", name, schema, err)
		}
	}
	var dropped, redirected, unavailable atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/fingerprint":
			if !strings.Contains(r.Header.Get("User-Agent"), "Chrome/") {
				t.Error("missing default Chrome impersonation")
			}
			_, _ = io.WriteString(w, "<title>Chrome fixture</title>")
		case "/headers":
			if r.Header.Get("User-Agent") != "fixture-agent" {
				t.Error("explicit User-Agent overridden by impersonation")
			}
			w.Header().Set("Content-Type", `text/plain; charset="ISO-8859-1"`)
			w.Header().Add("Set-Cookie", "a=1")
			w.Header().Add("Set-Cookie", "b=2")
			_, _ = w.Write([]byte{'c', 'a', 'f', 0xe9})
		case "/redirect":
			http.Redirect(w, r, "/dir/page", http.StatusFound)
		case "/dir/page":
			_, _ = io.WriteString(w, `<title>Final page</title><a href="child">child</a><form action="submit"></form>`)
		case "/drop":
			dropped.Add(1)
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
		case "/post-redirect":
			redirected.Add(1)
			http.Redirect(w, r, "/post-redirect", http.StatusTemporaryRedirect)
		case "/unavailable":
			unavailable.Add(1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		default:
			_, _ = io.WriteString(w, "abcdefgh")
		}
	}))
	defer server.Close()
	for _, name := range []string{"http-request", "page-fetch"} {
		invokePreset(t, tools[name], map[string]any{"url": server.URL + "/fingerprint"}, false)
	}
	out := invokePreset(t, tools["http-request"], map[string]any{"url": server.URL + "/headers", "headers": map[string]string{"User-Agent": "fixture-agent"}}, false)
	if out["body"] != "café" || out["url"] != server.URL+"/headers" || out["status"] != float64(200) || out["truncated"] != false {
		t.Fatalf("charset/URL/status: %+v", out)
	}
	var cookies []string
	for _, raw := range out["headers"].([]any) {
		header := raw.([]any)
		if strings.EqualFold(header[0].(string), "Set-Cookie") {
			cookies = append(cookies, header[1].(string))
		}
	}
	if strings.Join(cookies, ",") != "a=1,b=2" {
		t.Fatalf("duplicate headers: %+v", out)
	}
	for _, limit := range []int{7, 8, 9} {
		out = invokePreset(t, tools["http-request"], map[string]any{"url": server.URL, "max_bytes": limit}, false)
		want := "abcdefgh"
		if limit < len(want) {
			want = want[:limit]
		}
		if out["body"] != want || out["truncated"] != (limit < 8) {
			t.Fatalf("limit=%d: %+v", limit, out)
		}
	}
	out = invokePreset(t, tools["page-fetch"], map[string]any{"url": server.URL + "/redirect"}, false)
	if out["url"] != server.URL+"/dir/page" || out["title"] != "Final page" || out["links"].([]any)[0] != server.URL+"/dir/child" || out["forms"].([]any)[0].(map[string]any)["action"] != server.URL+"/dir/submit" {
		t.Fatalf("redirected page base: %+v", out)
	}
	out = invokePreset(t, tools["http-request"], map[string]any{"url": server.URL + "/drop", "method": "POST", "body": "effect"}, true)
	if dropped.Load() != 1 || out["error"].(map[string]any)["type"] == "" {
		t.Fatalf("transport failure replayed: count=%d output=%+v", dropped.Load(), out)
	}
	out = invokePreset(t, tools["http-request"], map[string]any{"url": server.URL + "/post-redirect", "method": "POST", "body": "effect"}, false)
	if redirected.Load() != 1 || out["status"] != float64(307) {
		t.Fatalf("POST redirect replayed: count=%d output=%+v", redirected.Load(), out)
	}
	out = invokePreset(t, tools["http-request"], map[string]any{"url": server.URL + "/unavailable", "method": "POST", "body": "effect"}, false)
	if unavailable.Load() != 1 || out["status"] != float64(503) || out["body"] != "unavailable\n" {
		t.Fatalf("HTTP error retried: count=%d output=%+v", unavailable.Load(), out)
	}
	out = invokePreset(t, tools["http-request"], map[string]any{"url": server.URL, "impersonate": ""}, false)
	if out["body"] != "abcdefgh" {
		t.Fatalf("explicit impersonation opt-out: %+v", out)
	}
	invokePreset(t, tools["http-request"], map[string]any{"url": server.URL, "impersonate": "fixture-unsupported-browser"}, true)
}

func TestPresetsHTTPMissingDependency(t *testing.T) {
	installedPresets(t)
	path := writeFixture(t, "curl_cffi.py", []byte("raise ImportError('fixture curl_cffi unavailable')\n"))
	t.Setenv("PYTHONPATH", filepath.Dir(path))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	manifests, statuses := Presets(ctx)
	otherTools := false
	for _, m := range manifests {
		if m.Name == "binary-info" {
			otherTools = true
		}
		if m.Name == "http-request" || m.Name == "page-fetch" {
			t.Fatalf("HTTP registered without curl_cffi: %+v", m)
		}
	}
	if !otherTools {
		t.Fatal("missing curl_cffi suppressed unrelated Python tools")
	}
	for _, status := range statuses {
		if status.Name == "curl_cffi" {
			if status.Available || !strings.Contains(status.Detail, "fixture curl_cffi unavailable") {
				t.Fatalf("missing dependency observation: %+v", status)
			}
			return
		}
	}
	t.Fatal("missing curl_cffi dependency status")
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
	want := map[string]bool{"Python 3": true, "curl_cffi": true, "capstone": true, "droidasc": true, "frida": true, "CloakBrowser": true, "Lightpanda": true, "Rizin/radare2": true, "Trace Web Analysis": true}
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
