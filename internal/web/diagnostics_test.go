package web

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"trace/internal/agent"
)

type diagnosticNonFlusher struct {
	header http.Header
	status int
}

func (w *diagnosticNonFlusher) Header() http.Header         { return w.header }
func (w *diagnosticNonFlusher) WriteHeader(status int)      { w.status = status }
func (w *diagnosticNonFlusher) Write(b []byte) (int, error) { return len(b), nil }

func TestDiagnosticMiddlewarePreservesFlusherAndBoundsLabels(t *testing.T) {
	s, _ := fixture(t, nil)
	for _, hasFlusher := range []bool{false, true} {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /api/runs/{id}/events", func(w http.ResponseWriter, r *http.Request) {
			flusher, ok := w.(http.Flusher)
			if ok != hasFlusher {
				t.Fatal("Flusher availability changed")
			}
			w.Write([]byte("data: fixture\n\n"))
			if ok {
				flusher.Flush()
			}
		})
		h := s.diagnosticHandler(mux, mux)
		var w http.ResponseWriter = &diagnosticNonFlusher{header: http.Header{}}
		if hasFlusher {
			w = httptest.NewRecorder()
		}
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/runs/secret-run/events?token=secret-token", nil))
	}
	for i := 0; i < 300; i++ {
		path := "/fixture/" + strconv.Itoa(i)
		mux := http.NewServeMux()
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
		h := s.diagnosticHandler(mux, mux)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", path, nil))
	}
	w := httptest.NewRecorder()
	s.getDiagnostics(w, httptest.NewRequest("GET", "/api/diagnostics", nil))
	var output struct {
		HTTP     []httpDiagnostic `json:"http"`
		InFlight int64            `json:"in_flight"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.HTTP) > 128 || output.InFlight != 0 || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("unbounded/private metric: %s", w.Body.String())
	}
	found := false
	for _, row := range output.HTTP {
		if row.Route == "GET /api/runs/{id}/events" {
			found = true
			if row.Requests != 2 || row.Flushes != 1 || row.Bytes != 30 {
				t.Fatalf("wrong metrics: %+v", row)
			}
		}
	}
	if !found {
		t.Fatal("route template missing")
	}
}

func TestRuntimeDiagnosticsAPIReadOnlyAndExplicitLimits(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("diagnostic API")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	_, err = a.Store().Update(id, func(state *agent.Snapshot) error {
		state.Requests = []agent.ModelRequest{{ID: "r1", ProviderModel: "requested"}}
		state.Responses = []agent.ResponseRecord{{RequestID: "r1", Response: agent.ModelResponse{ReportedModel: "reported", Content: "private body"}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := a.Store().Observe(id)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	w := request(t, h, "GET", "/api/runs/"+id+"/diagnostics?limit=1", nil, 200)
	var report agent.RunDiagnostics
	if err := json.Unmarshal(w.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Requests) != 1 || report.Requests[0].RequestedModel != "requested" || report.Requests[0].ReportedModel != "reported" || strings.Contains(w.Body.String(), "private body") {
		t.Fatalf("wrong diagnostic projection: %s", w.Body.String())
	}
	for _, limit := range []string{"0", "101", "bad"} {
		request(t, h, "GET", "/api/runs/"+id+"/diagnostics?limit="+limit, nil, 400)
	}
	request(t, h, "GET", "/api/runs/missing/diagnostics", nil, 404)
	after, err := a.Store().Observe(id)
	if err != nil || before.Run.EventSeq != after.Run.EventSeq || before.Run.UpdatedAt != after.Run.UpdatedAt {
		t.Fatal("diagnostic API mutated run")
	}
	w = request(t, h, "GET", "/api/diagnostics", nil, 200)
	if strings.Contains(w.Body.String(), id) || strings.Contains(w.Body.String(), "missing/diagnostics") || strings.Contains(w.Body.String(), "private body") {
		t.Fatal("HTTP diagnostics contain content or raw path")
	}
	var runtime struct {
		Store agent.StoreDiagnostics `json:"store"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &runtime); err != nil {
		t.Fatal(err)
	}
	if runtime.Store.Backend != "json" || len(runtime.Store.Stages) == 0 || runtime.Store.Since.IsZero() {
		t.Fatal("Store timing not connected to diagnostics API")
	}
}

func TestRuntimeDiagnosticsRealSSEFlushesBeforeClose(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("SSE diagnostics")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/runs/"+snap.Run.ID+"/events", nil)
	response, err := server.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || line != ": connected\n" || response.StatusCode != 200 {
		t.Fatalf("SSE buffering introduced: line=%q err=%v", line, err)
	}
	cancel()
}
