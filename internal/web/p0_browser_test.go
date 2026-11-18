package web

// Opt-in U3/U4 DOM acceptance over a real Store/API, not a B9 performance test.
// Run only on the designated server with fixed headed CloakBrowser + Xvfb.
import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"trace/internal/agent"
)

func TestP0BrowserAcceptance(t *testing.T) {
	if os.Getenv("TRACE_P0_BROWSER") != "1" {
		t.Skip("server-only: TRACE_P0_BROWSER=1, fixed headed CloakBrowser 0.6.4 and DISPLAY/Xvfb required")
	}
	if os.Getenv("DISPLAY") == "" || os.Getenv("TRACE_CLOAKBROWSER_NODE_MODULE") == "" || os.Getenv("CLOAKBROWSER_BINARY_PATH") == "" {
		t.Fatal("explicit DISPLAY, fixed CloakBrowser module and binary are required; no download/fallback")
	}
	output := os.Getenv("TRACE_P0_REPORT_DIR")
	if output == "" {
		output = t.TempDir()
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if os.Getenv("TRACE_P0_SQLITE") == "1" {
		if err := agent.MigrateStoreToSQLite(root); err != nil {
			t.Fatal(err)
		}
	}
	store, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a, err := agent.New(store, agent.MockProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(a, Options{InitialConfig: Config{Context: agent.DefaultContextConfig()}})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	ids, requests := []string{}, map[string]string{}
	for n := 0; n < 4; n++ {
		snap, err := a.Create(fmt.Sprintf("P0 browser layout %d", n))
		if err != nil {
			t.Fatal(err)
		}
		workers := []agent.AgentWorker{}
		// Intentionally persist historical/attention rows before active workers.
		for _, state := range []string{"completed", "paused", "running", "starting", "cancelling"} {
			child, err := a.Create("P0 worker " + state)
			if err != nil {
				t.Fatal(err)
			}
			workers = append(workers, agent.AgentWorker{ID: "worker-" + state, TaskID: "task-" + state, RunID: child.Run.ID, Status: state, CleanupStatus: "completed", Visit: 1})
		}
		request := fmt.Sprintf("p0-request-%d", n)
		_, err = store.Update(snap.Run.ID, func(s *agent.Snapshot) error {
			s.Run.Status = agent.StatusRunning
			s.Messages = nil
			for i := 0; i < 120; i++ {
				role := "user"
				if i%2 == 1 {
					role = "assistant"
				}
				s.Messages = append(s.Messages, agent.Message{ID: fmt.Sprintf("p0-history-%03d", i), Role: role, Content: fmt.Sprintf("P0_HISTORY_%03d\n\n", i) + strings.Repeat("可复核历史正文。保留阅读锚点，不自动跳到最新消息。\n\n", 7)})
			}
			s.Requests = []agent.ModelRequest{{ID: request}}
			s.Workers = workers
			s.Plan.Capacity = 5
			s.Plan.Tasks = []agent.PlanTask{
				{ID: "todo-ready", Goal: "P0 ready task", Status: "pending"},
				{ID: "todo-dependent", Goal: "P0 dependency wait", Status: "pending", Dependencies: []string{"todo-ready"}},
				{ID: "todo-capability", Goal: "P0 capability wait", Status: "blocked_capability"},
				{ID: "task-running", Goal: "P0 active task", Status: "running", WorkerID: "worker-running", ChildRunID: workers[2].RunID},
				{ID: "dispatch-pending", Goal: "P0 registration pending", Status: "running", WorkerID: "not-yet-registered"},
				{ID: "task-completed", Goal: "P0 historical task", Status: "completed"},
				{ID: "task-paused", Goal: "P0 paused task", Status: "paused"},
			}
			s.ChildReports = []agent.ChildReport{{ParentRunID: s.Run.ID, TaskID: "task-completed", WorkerID: workers[0].ID, SourceRunID: workers[0].RunID, Visit: 1, SourceHash: "p0-preview-source", RunStatus: agent.StatusCompleted, ResultStatus: "incomplete", CleanupStatus: "completed", Summary: "P0 inline child report", ReportPath: "/api/runs/" + workers[0].RunID + "/reports/p0-preview-source", SourceStatus: "available", Current: true}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, snap.Run.ID)
		requests[snap.Run.ID] = request
	}
	if os.Getenv("TRACE_ENTRY_BROWSER") == "1" {
		for _, id := range ids {
			if _, err := store.Update(id, func(s *agent.Snapshot) error {
				for i := range s.Messages {
					if s.Messages[i].Role == "assistant" {
						s.Messages[i].RequestID = requests[id]
					}
				}
				archived := make([]agent.ModelRequest, 0, 301)
				for i := 0; i < 300; i++ {
					requestID := fmt.Sprintf("entry-archive-%03d", i)
					manifest := &agent.ContextManifest{Boundary: "entry_fixture", Hash: strings.Repeat("a", 64)}
					for j := 0; j < 32; j++ {
						manifest.Parts = append(manifest.Parts, agent.ContextManifestPart{Kind: "message", Index: j, ID: strings.Repeat("identity-", 12), Hash: strings.Repeat("b", 64), Bytes: 400})
					}
					archived = append(archived, agent.ModelRequest{ID: requestID, ContextManifest: manifest, Messages: []agent.Message{{ID: "archived-input", Role: "user", Content: strings.Repeat("archived input ", 600)}}})
					s.Responses = append(s.Responses, agent.ResponseRecord{RequestID: requestID, Response: agent.ModelResponse{Content: "archived response"}})
				}
				s.Requests = append(archived, s.Requests...)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			events := make([]agent.Event, 50000)
			for i := range events {
				events[i] = agent.Event{Type: "model_stream", Data: map[string]any{"request_id": "entry-archive-000", "delta": agent.StreamDelta{Kind: "text", Text: strings.Repeat("x", 700)}}}
			}
			if err := store.AppendEvents(id, events); err != nil {
				t.Fatal(err)
			}
		}
	}
	var mu sync.Mutex
	traffic := []map[string]any{}
	mux := http.NewServeMux()
	mux.Handle("/", app.Handler())
	mux.HandleFunc("POST /_p0/report", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if _, ok := requests[id]; !ok {
			http.Error(w, "unknown fixture", 400)
			return
		}
		_, err := store.Update(id, func(s *agent.Snapshot) error {
			s.Plan.Tasks = append(s.Plan.Tasks, agent.PlanTask{ID: "task-second-report", Goal: "P0 second settled direction", Status: "completed"})
			report := s.ChildReports[0]
			report.TaskID = "task-second-report"
			report.Visit = 2
			report.SourceHash = "p0-preview-new-source"
			report.Summary = "P0 second inline report"
			s.ChildReports = append(s.ChildReports, report)
			return nil
		})
		if err == nil {
			err = store.AppendEvents(id, []agent.Event{{Type: "worker_finished", Summary: "fixture child settled"}})
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /_p0/traffic", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(traffic)
	})
	mux.HandleFunc("POST /_p0/stream", func(w http.ResponseWriter, r *http.Request) {
		id, batch := r.URL.Query().Get("id"), r.URL.Query().Get("batch")
		request, ok := requests[id]
		if !ok || batch == "" || len(batch) > 80 {
			http.Error(w, "invalid fixture stream", 400)
			return
		}
		for i := 0; i < 8; i++ {
			text := fmt.Sprintf("P0FRAME_%s_%d\n\n", batch, i)
			if err := store.AppendEvents(id, []agent.Event{{Type: "model_stream", Data: map[string]any{"request_id": request, "delta": agent.StreamDelta{Kind: "text", Text: text}}}}); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(90 * time.Millisecond):
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"frames": 8, "committed": true})
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/runs/") {
			mu.Lock()
			traffic = append(traffic, map[string]any{"index": len(traffic), "path": r.URL.Path, "query": r.URL.RawQuery, "at": time.Now().UnixMilli()})
			mu.Unlock()
		}
		mux.ServeHTTP(w, r)
	}))
	defer func() { server.CloseClientConnections(); server.Close() }()
	fixture, err := json.Marshal(map[string]any{"base": server.URL, "ids": ids, "output": output, "sqlite": os.Getenv("TRACE_P0_SQLITE") == "1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	node := os.Getenv("TRACE_NODE")
	if node == "" {
		node = "node"
	}
	script := "p0_browser.mjs"
	if os.Getenv("TRACE_ENTRY_BROWSER") == "1" {
		script = "entry_browser.mjs"
	}
	if os.Getenv("TRACE_NOTICE_BROWSER") == "1" {
		script = "notice_browser.mjs"
	}
	command := exec.CommandContext(ctx, node, script)
	command.Env = append(os.Environ(), "TRACE_P0_FIXTURE="+string(fixture))
	raw, runErr := command.CombinedOutput()
	t.Logf("P0_BROWSER %s", raw)
	mu.Lock()
	recorded, marshalErr := json.MarshalIndent(traffic, "", "  ")
	mu.Unlock()
	if marshalErr != nil {
		t.Error(marshalErr)
	} else if err := os.WriteFile(filepath.Join(output, "p0-http-traffic.json"), recorded, 0600); err != nil {
		t.Error(err)
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
}
