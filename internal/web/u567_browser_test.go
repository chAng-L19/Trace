package web

// Server-only real form/API/CAS acceptance; no external credentials or models.
import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"trace/internal/agent"
)

func TestU567BrowserAcceptance(t *testing.T) {
	if os.Getenv("TRACE_U567_BROWSER") != "1" {
		t.Skip("server-only fixed headed CloakBrowser acceptance")
	}
	if os.Getenv("DISPLAY") == "" || os.Getenv("CLOAKBROWSER_BINARY_PATH") == "" || os.Getenv("TRACE_CLOAKBROWSER_NODE_MODULE") == "" {
		t.Fatal("explicit fixed SDK, binary and headed DISPLAY required")
	}
	var probes atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/info/my" || r.URL.Query().Get("key") != "u567-fixture-secret" {
			t.Error("probe used unsaved credentials or submitted search")
			http.Error(w, "wrong request", 400)
			return
		}
		probes.Add(1)
		fmt.Fprint(w, `{"error":false,"email":"fixture@example.test","username":"fixture"}`)
	}))
	defer upstream.Close()
	store, err := agent.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config := Config{Provider: ProviderConfig{URL: upstream.URL, Model: "main", ReasoningEfforts: []string{"high"}}, Context: agent.DefaultContextConfig(), SearchSources: map[string]agent.SearchSourceConfig{"fofa": {Enabled: true, APIKey: "u567-fixture-secret", BaseURL: upstream.URL, MinIntervalMS: 1}}}
	build := func(_ context.Context, c Config) (*agent.Agent, error) {
		p := &agent.OpenAIProvider{BaseURL: c.Provider.URL, Model: c.Provider.Model, ReasoningEfforts: c.Provider.ReasoningEfforts}
		a, e := agent.New(store, p, "")
		if e != nil {
			return nil, e
		}
		if e = a.ConfigureModels(nil, c.RoleModels); e != nil {
			return nil, e
		}
		if e = a.ConfigureSearchSources(c.SearchSources); e != nil {
			return nil, e
		}
		return a, nil
	}
	a, err := build(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(a, Options{ConfigPath: filepath.Join(t.TempDir(), "config.json"), InitialConfig: config, BuildAgent: build})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	s, err := a.Create("双语总结和真实文件验收")
	if err != nil {
		t.Fatal(err)
	}
	claim := agent.Result{Status: "failed", Summary: "检查未满足预期条件，保留实际观察与待核项目。", Report: &agent.ReportText{ZhCN: "此次检查未满足预期条件，结论仅适用于已经观察到的对象；清理状态仍待核对，后续需要补充来源。", En: "The check did not meet the expected condition. The conclusion applies only to the observed object; cleanup remains unchecked and further work requires additional sources."}}
	raw, _ := json.Marshal(map[string]any{"trace_result": claim})
	internal, _ := store.PutArtifact("stdout", []byte("INTERNAL_OUTPUT_NOT_A_REPORT"))
	internal.Name = "RAW_CLI_OUTPUT"
	file, _ := store.PutArtifact("file", []byte("actual user file"))
	file.Name = "actual-deliverable.txt"
	s, err = store.Update(s.Run.ID, func(s *agent.Snapshot) error {
		s.Run.Status = agent.StatusCompleted
		s.Messages = append(s.Messages, agent.Message{Role: "assistant", Content: string(raw)})
		s.Actions = append(s.Actions, agent.Action{Name: "exec", CallID: "fixture", Status: "completed", Result: &agent.ToolResult{Artifacts: []agent.ArtifactRef{internal, file}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", app.Handler())
	mux.HandleFunc("GET /_u567/status", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"probes": probes.Load()})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	out := os.Getenv("TRACE_U567_REPORT_DIR")
	if out == "" {
		out = t.TempDir()
	}
	if err = os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	fixture, _ := json.Marshal(map[string]any{"base": server.URL, "run": s.Run.ID, "output": out})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	node := testNodeBinary()
	cmd := exec.CommandContext(ctx, node, "u567_browser.mjs")
	cmd.Env = append(os.Environ(), "TRACE_U567_FIXTURE="+string(fixture))
	output, err := cmd.CombinedOutput()
	t.Logf("U567_BROWSER %s", output)
	if err != nil {
		t.Fatal(err)
	}
	if probes.Load() != 1 {
		t.Fatalf("unexpected account calls: %d", probes.Load())
	}
}
