package web

// Server-only opt-in acceptance: source delta -> committed SSE -> actual DOM,
// under three 50k-event histories + five real pi/Go workers. Complements, rather
// than replaces, TestCapacityMatrix and TestCapacitySSEMatrix's original metric.
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

type p2BrowserProvider struct {
	gate    chan struct{}
	entered chan struct{}
}

func (p *p2BrowserProvider) Complete(ctx context.Context, r agent.ModelRequest) (agent.ModelResponse, error) {
	return p.CompleteStream(ctx, r, func(agent.StreamDelta) error { return nil })
}
func (p *p2BrowserProvider) CompleteStream(ctx context.Context, r agent.ModelRequest, emit func(agent.StreamDelta) error) (agent.ModelResponse, error) {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return agent.ModelResponse{}, ctx.Err()
	case <-p.gate:
	}
	var body strings.Builder
	for i := 0; i < 60; i++ {
		marker := fmt.Sprintf("P2FRAME_%s_%d_%d \n", r.ID, i, time.Now().UnixMilli())
		body.WriteString(marker)
		if err := emit(agent.StreamDelta{Kind: "text", Text: marker}); err != nil {
			return agent.ModelResponse{}, err
		}
		select {
		case <-ctx.Done():
			return agent.ModelResponse{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return agent.ModelResponse{Content: body.String(), StopReason: "stop"}, nil
}

func TestP2MixedBrowserAcceptance(t *testing.T) {
	testP2BrowserAcceptance(t, "three-pages")
}

func TestP2ActiveBrowserAcceptance(t *testing.T) {
	testP2BrowserAcceptance(t, "single-active")
}

func testP2BrowserAcceptance(t *testing.T, mode string) {
	t.Helper()
	if os.Getenv("TRACE_P2_BROWSER") != "1" {
		t.Skip("server-only: TRACE_P2_BROWSER=1 requires fixed headed CloakBrowser 0.6.4 and DISPLAY/Xvfb")
	}
	output := os.Getenv("TRACE_P2_REPORT_DIR")
	if output == "" {
		output = t.TempDir()
	}
	if mode == "single-active" {
		output = filepath.Join(output, "single-active")
	}
	if err := os.MkdirAll(output, 0700); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TRACE_P2_SQLITE") == "1" {
		if err = store.Close(); err != nil {
			t.Fatal(err)
		}
		if err = agent.MigrateStoreToSQLite(root); err != nil {
			t.Fatal(err)
		}
		store, err = agent.NewStore(root)
		if err != nil {
			t.Fatal(err)
		}
	}
	defer store.Close()
	provider := &p2BrowserProvider{gate: make(chan struct{}), entered: make(chan struct{}, 32)}
	a, err := agent.New(store, provider, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.ConfigureContext(agent.ContextConfig{WindowTokens: 1000000, OutputReserve: 16384, KeepRecentTokens: 20000, CompactionMode: "projection"}); err != nil {
		t.Fatal(err)
	}
	app, err := New(a, Options{InitialConfig: Config{Context: agent.DefaultContextConfig()}})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	ids := []string{}
	sizes := []int64{}
	for n := 0; n < 3; n++ {
		snap, err := a.Create(fmt.Sprintf("P2 mixed large history %d", n))
		if err != nil {
			t.Fatal(err)
		}
		snap, err = store.Update(snap.Run.ID, func(s *agent.Snapshot) error {
			for i := 0; i < 200; i++ {
				role := "user"
				request := ""
				if i%2 == 1 {
					role = "assistant"
					request = fmt.Sprintf("old-%d", i)
				}
				s.Messages = append(s.Messages, agent.Message{ID: fmt.Sprintf("history-%d", i), Role: role, RequestID: request, Content: fmt.Sprintf("HISTORY_%d ", i) + strings.Repeat("long-message ", 1000)})
			}
			s.Requests = []agent.ModelRequest{{ID: "archived"}}
			s.Responses = []agent.ResponseRecord{{RequestID: "archived", Response: agent.ModelResponse{Content: "settled"}}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		events := make([]agent.Event, 50000)
		for i := range events {
			events[i] = agent.Event{Type: "model_stream", Data: map[string]any{"request_id": "archived", "delta": agent.StreamDelta{Kind: "text", Text: fmt.Sprintf("%08d", i) + strings.Repeat("x", 700)}}}
		}
		if err = store.AppendEvents(snap.Run.ID, events); err != nil {
			t.Fatal(err)
		}
		var size int64
		if os.Getenv("TRACE_P2_SQLITE") == "1" {
			full, loadErr := store.Load(snap.Run.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if len(full.Events) < 50000 {
				t.Fatal("SQLite fixture lost historical events")
			}
			raw, marshalErr := json.Marshal(full)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			size = int64(len(raw))
		} else {
			info, statErr := os.Stat(filepath.Join(root, snap.Run.ID+".json"))
			if statErr != nil {
				t.Fatal(statErr)
			}
			size = info.Size()
		}
		ids = append(ids, snap.Run.ID)
		sizes = append(sizes, size)
	}
	parent, err := a.Create("P2 five-worker lifecycle")
	if err != nil {
		t.Fatal(err)
	}
	tasks := []agent.PlanTask{}
	for i := 0; i < 6; i++ {
		tasks = append(tasks, agent.PlanTask{ID: fmt.Sprint(i), Goal: "P2 independent streaming worker"})
	}
	if _, err = a.SetPlan(parent.Run.ID, tasks); err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPlanCapacity(parent.Run.ID, 5); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	var started sync.Once
	var release sync.Once
	handler := http.NewServeMux()
	handler.Handle("/", app.Handler())
	handler.HandleFunc("POST /_p2/start", func(w http.ResponseWriter, r *http.Request) {
		started.Do(func() {
			for _, id := range ids {
				wg.Add(1)
				go func(id string) {
					defer wg.Done()
					_, err := a.Run(ctx, id)
					if err != nil {
						failures <- err
					}
				}(id)
			}
			workers, err := a.DispatchFrontier(ctx, parent.Run.ID)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			if len(workers) != 5 {
				http.Error(w, "five workers not admitted", 500)
				return
			}
			if _, err = a.DispatchTask(ctx, parent.Run.ID, "5"); err == nil {
				http.Error(w, "N+1 was admitted", 500)
				return
			}
			for i := 0; i < 8; i++ {
				select {
				case <-provider.entered:
				case <-ctx.Done():
					http.Error(w, ctx.Err().Error(), 500)
					return
				}
			}
		})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"workers":5,"large_runs":3}`)
	})
	handler.HandleFunc("POST /_p2/release", func(w http.ResponseWriter, r *http.Request) {
		release.Do(func() { close(provider.gate) })
		fmt.Fprint(w, `{"started":true}`)
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	data, _ := json.Marshal(map[string]any{"base": server.URL, "ids": ids, "parent": parent.Run.ID, "sizes": sizes, "output": output, "mode": mode})
	node := testNodeBinary()
	command := exec.CommandContext(ctx, node, "p2_mixed_browser.mjs")
	command.Env = append(os.Environ(), "TRACE_P2_FIXTURE="+string(data))
	raw, err := command.CombinedOutput()
	t.Logf("P2_BROWSER %s", raw)
	if err != nil {
		release.Do(func() { close(provider.gate) })
		cancel()
		t.Fatal(err)
	}
	release.Do(func() { close(provider.gate) })
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	var final agent.Snapshot
	for a.HasActiveWorkers() {
		final, err = a.WaitWorkers(ctx, parent.Run.ID, 60)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		default:
		}
	}
	final, err = store.Load(parent.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, worker := range final.Workers {
		if worker.SettledVisit == nil || worker.CleanupStatus != "complete" && worker.CleanupStatus != "completed" {
			t.Errorf("worker not settled: %+v", worker)
		}
	}
	report, _ := json.Marshal(map[string]any{"mode": mode, "initial_events_per_run": 50000, "large_runs": 3, "initial_bytes": sizes, "workers": final.Workers, "passed": !t.Failed()})
	if err = os.WriteFile(filepath.Join(output, "mixed-lifecycle.json"), report, 0600); err != nil {
		t.Fatal(err)
	}
}
