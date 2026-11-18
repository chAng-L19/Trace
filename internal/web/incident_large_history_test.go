package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"trace/internal/agent"
)

// Run each subtest in a fresh process for cold-path timing on the same complete
// disposable incident copy. This measures HTTP work only, excluding fixture
// preparation and Server construction; neither path starts a Provider. The
// same public-route fixture also measures an older binary that ignores workers.
func TestIncidentWorkspaceEntryHTTP(t *testing.T) {
	for _, mode := range []string{"collapsed", "expanded"} {
		t.Run(mode, func(t *testing.T) {
			s, _, id := incidentLargeHistory(t)
			path := "/api/runs/" + id + "?view=workspace&activity=true"
			if mode == "collapsed" {
				path += "&workers=false"
			}
			r := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			started := time.Now()
			s.Handler().ServeHTTP(w, r)
			elapsed := time.Since(started)
			if w.Code != http.StatusOK {
				t.Fatalf("entry returned %d: %.512s", w.Code, w.Body.String())
			}
			var view workspaceView
			if err := json.Unmarshal(w.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.Run.ID != id || view.Projection != "activity" || view.Totals["requests"] != 422 {
				t.Fatal("entry lost real incident identity or request count")
			}
			if len(view.WorkerActivity) != len(view.Workers) && (mode == "expanded" || len(view.WorkerActivity) != 0) {
				t.Fatal("entry returned partial child detail rows")
			}
			t.Logf("entry=%s http_ms=%.2f response_bytes=%d messages=%d workers=%d worker_details=%d child_details_deferred=%t event_seq=%d", mode, float64(elapsed.Microseconds())/1000, w.Body.Len(), len(view.Messages), len(view.Workers), len(view.WorkerActivity), len(view.Workers) > 0 && len(view.WorkerActivity) == 0, view.Run.EventSeq)
			// Raw JSON lengths report actual field payload bytes without logging
			// any message, argument, credential or manifest contents.
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0, len(fields))
			for key := range fields {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				t.Logf("entry_field=%s bytes=%d", key, len(fields[key]))
			}
			var requests []struct {
				Manifest json.RawMessage `json:"context_manifest"`
			}
			if err := json.Unmarshal(fields["requests"], &requests); err != nil {
				t.Fatal(err)
			}
			count, bytes := 0, 0
			for _, row := range requests {
				if len(row.Manifest) != 0 && string(row.Manifest) != "null" {
					count++
					bytes += len(row.Manifest)
				}
			}
			t.Logf("entry_requests=%d context_manifest_count=%d context_manifest_bytes=%d", len(requests), count, bytes)
		})
	}
}

// The caller supplies a DISPOSABLE, complete incident Store copy. First-pause
// fixtures must have their root header set to running before the memory-limited
// test process starts; retry fixtures retain the original operator_pause.
// Uses public APIs so the identical regression can run against the baseline.
func incidentLargeHistory(t *testing.T) (*Server, *agent.Agent, string) {
	t.Helper()
	root := os.Getenv("TRACE_INCIDENT_FIXTURE_DIR")
	if root == "" {
		t.Skip("requires isolated complete incident fixture")
	}
	id := os.Getenv("TRACE_INCIDENT_ROOT_ID")
	if id == "" {
		id = "21747a3b09678fd8d7e854dc82969267"
	}
	if info, err := os.Stat(filepath.Join(root, id+".json")); err == nil {
		if info.Size() < 200<<20 {
			t.Fatalf("fixture is only %d bytes; expected the real 229 MiB history", info.Size())
		}
		t.Logf("incident JSON source bytes=%d", info.Size())
	}
	store, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	a, err := agent.New(store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(a, Options{InitialConfig: Config{Context: agent.DefaultContextConfig()}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, a, id
}

func TestIncidentColdActivity(t *testing.T) {
	for _, readers := range []int{1, 2} {
		t.Run(fmt.Sprint(readers), func(t *testing.T) {
			_, a, id := incidentLargeHistory(t)
			start := make(chan struct{})
			errors := make(chan error, readers)
			var wg sync.WaitGroup
			for i := 0; i < readers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					p, err := a.Store().ActivityWorkspace(id)
					if err == nil && (p.Totals["requests"] != 422 || p.Snapshot.Run.ID != id) {
						err = fmt.Errorf("wrong real history identity/count: %d", p.Totals["requests"])
					}
					errors <- err
				}()
			}
			close(start)
			wg.Wait()
			close(errors)
			for err := range errors {
				if err != nil {
					t.Error(err)
				}
			}
		})
	}
}

func TestIncidentPauseHTTP(t *testing.T) {
	for _, state := range []string{"first", "retry"} {
		t.Run(state, func(t *testing.T) {
			s, a, id := incidentLargeHistory(t)
			runs, err := a.Store().List()
			if err != nil {
				t.Fatal(err)
			}
			var before agent.Run
			for _, run := range runs {
				if run.ID == id {
					before = run
				}
			}
			want := http.StatusOK
			if state == "first" && before.Status != agent.StatusRunning {
				t.Fatalf("first-pause fixture must already be running; got %s", before.Status)
			}
			if state == "retry" {
				if before.Status != agent.StatusPaused {
					t.Fatalf("retry fixture must already be paused; got %s", before.Status)
				}
				want = http.StatusConflict
			}
			r := httptest.NewRequest(http.MethodPost, "/api/runs/"+id+"/pause", strings.NewReader(`{}`))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			started := time.Now()
			s.Handler().ServeHTTP(w, r)
			handlerElapsed := time.Since(started)
			if w.Code != want {
				t.Fatalf("pause returned %d, expected %d: %.512s", w.Code, want, w.Body.String())
			}
			if w.Body.Len() > 512 {
				t.Fatalf("pause returned %d bytes of history", w.Body.Len())
			}
			observed, err := a.Store().Observe(id)
			if err != nil {
				t.Fatal(err)
			}
			if observed.Run.Status != agent.StatusPaused || observed.Run.PauseReason != "operator_pause" {
				t.Fatal("pause did not persist operator intent")
			}
			if state == "retry" && observed.Run.EventSeq != before.EventSeq {
				t.Fatal("pause retry added an event")
			}
			t.Logf("pause=%s handler_ms=%.2f response_bytes=%d event_seq=%d", state, float64(handlerElapsed)/float64(time.Millisecond), w.Body.Len(), observed.Run.EventSeq)
		})
	}
}

func TestIncidentActivityWithEvents(t *testing.T) {
	_, a, id := incidentLargeHistory(t)
	start := make(chan struct{})
	result := make(chan error, 2)
	go func() {
		<-start
		p, err := a.Store().ActivityWorkspace(id)
		if err == nil && p.Totals["requests"] != 422 {
			err = fmt.Errorf("wrong real history request count: %d", p.Totals["requests"])
		}
		result <- err
	}()
	go func() {
		<-start
		run, events, err := a.Store().EventBatch(id, 0)
		if err == nil && (run.ID != id || len(events) == 0) {
			err = fmt.Errorf("missing committed incident event prefix")
		}
		result <- err
	}()
	close(start)
	for i := 0; i < 2; i++ {
		if err := <-result; err != nil {
			t.Error(err)
		}
	}
}

func TestIncidentPauseWithEventSubscription(t *testing.T) {
	s, a, id := incidentLargeHistory(t)
	release := a.Store().HoldEventView(id)
	defer release()
	// An attached page retains the committed event view while it waits for
	// the next sequence. Prime that same Store cache before clicking Pause.
	before, _, err := a.Store().EventBatch(id, int(^uint(0)>>1))
	if err != nil || before.Status != agent.StatusRunning {
		t.Fatalf("requires running fixture before subscription: %s %v", before.Status, err)
	}
	r := httptest.NewRequest(http.MethodPost, "/api/runs/"+id+"/pause", strings.NewReader(`{}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.Len() > 512 {
		t.Fatalf("pause with subscribed history: code=%d bytes=%d", w.Code, w.Body.Len())
	}
	after, batch, err := a.Store().EventBatch(id, before.EventSeq)
	if err != nil || after.Status != agent.StatusPaused || after.PauseReason != "operator_pause" || after.EventSeq <= before.EventSeq || len(batch) == 0 {
		t.Fatalf("subscribed pause not committed: status=%s event_seq=%d events=%d err=%v", after.Status, after.EventSeq, len(batch), err)
	}
	t.Logf("subscribed pause response_bytes=%d new_events=%d event_seq=%d", w.Body.Len(), len(batch), after.EventSeq)
}
