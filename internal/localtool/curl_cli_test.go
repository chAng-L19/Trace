package localtool

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestManagedCurlCLI(t *testing.T) {
	tools := installedPresets(t)
	tool, ok := tools["http-request"]
	if !ok {
		t.Skip("curl_cffi not installed")
	}
	_, file, _, _ := runtime.Caller(0)
	entry := filepath.Join(filepath.Dir(file), "..", "..", "scripts", "curl")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("curl-cffi fixture " + r.Method))
	}))
	defer server.Close()
	out, err := exec.Command(tool.Command, entry, "get", server.URL, "--body", "--impersonate", "chrome").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "curl-cffi fixture GET") || calls.Load() != 1 {
		t.Fatalf("official CLI GET: %q %v, calls=%d", out, err, calls.Load())
	}
	out, err = exec.Command(tool.Command, entry, "-s", server.URL).CombinedOutput()
	if err == nil || calls.Load() != 1 || !strings.Contains(string(out), "curl-cffi") {
		t.Fatalf("legacy flags reached a native client/request: %q %v, calls=%d", out, err, calls.Load())
	}
}
