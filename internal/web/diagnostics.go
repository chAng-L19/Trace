package web

import (
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Diagnostic counters are process-local, bounded and never scheduling state.
// Routes are ServeMux templates, never URLs, run IDs, model names or tokens.
type diagnosticCounters struct {
	mu       sync.Mutex
	http     map[string]*httpDiagnostic
	stages   map[string]*stageDiagnostic
	inFlight int64
	started  time.Time
}

type httpDiagnostic struct {
	Route             string `json:"route"`
	StatusClass       string `json:"status_class"`
	Requests          int64  `json:"requests"`
	Bytes             int64  `json:"bytes"`
	DurationMillis    int64  `json:"duration_ms_total"`
	MaxDurationMillis int64  `json:"duration_ms_max"`
	FirstWriteMillis  int64  `json:"first_write_ms_total"`
	FirstWriteSamples int64  `json:"first_write_samples"`
	Flushes           int64  `json:"flushes"`
}

type stageDiagnostic struct {
	Stage             string `json:"stage"`
	Samples           int64  `json:"samples"`
	DurationMillis    int64  `json:"duration_ms_total"`
	MaxDurationMillis int64  `json:"duration_ms_max"`
}

type diagnosticWriter struct {
	http.ResponseWriter
	started    time.Time
	status     int
	bytes      int64
	firstWrite *int64
	flushes    int64
}

func (w *diagnosticWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *diagnosticWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *diagnosticWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	if n > 0 && w.firstWrite == nil {
		ms := time.Since(w.started).Milliseconds()
		w.firstWrite = &ms
	}
	return n, err
}

type diagnosticFlusher struct{ *diagnosticWriter }

func (w diagnosticFlusher) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.flushes++
	w.ResponseWriter.(http.Flusher).Flush()
}

func (s *Server) diagnosticHandler(mux *http.ServeMux, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		s.diagnostics.mu.Lock()
		if s.diagnostics.started.IsZero() {
			s.diagnostics.started = started.UTC()
		}
		s.diagnostics.inFlight++
		s.diagnostics.mu.Unlock()
		capture := &diagnosticWriter{ResponseWriter: w, started: started}
		var writer http.ResponseWriter = capture
		if _, ok := w.(http.Flusher); ok {
			writer = diagnosticFlusher{capture}
		}
		defer func() {
			duration := time.Since(started).Milliseconds()
			_, route := mux.Handler(r)
			if route == "" {
				route = "unmatched"
			}
			status := capture.status
			if status == 0 {
				status = 200
			}
			class := strconv.Itoa(status/100) + "xx"
			if status < 100 || status > 599 {
				class = "other"
			}
			key := route + "\x00" + class
			s.diagnostics.mu.Lock()
			defer s.diagnostics.mu.Unlock()
			s.diagnostics.inFlight--
			if s.diagnostics.http == nil {
				s.diagnostics.http = map[string]*httpDiagnostic{}
			}
			entry := s.diagnostics.http[key]
			if entry == nil {
				if len(s.diagnostics.http) >= 127 {
					key, route, class = "overflow", "other", "other"
				}
				entry = s.diagnostics.http[key]
				if entry == nil {
					entry = &httpDiagnostic{Route: route, StatusClass: class}
					s.diagnostics.http[key] = entry
				}
			}
			entry.Requests++
			entry.Bytes += capture.bytes
			entry.DurationMillis += duration
			entry.Flushes += capture.flushes
			if duration > entry.MaxDurationMillis {
				entry.MaxDurationMillis = duration
			}
			if capture.firstWrite != nil {
				entry.FirstWriteSamples++
				entry.FirstWriteMillis += *capture.firstWrite
			}
		}()
		next.ServeHTTP(writer, r)
	})
}

func (d *diagnosticCounters) stage(name string, start time.Time) {
	switch name {
	case "sse_event_batch", "sse_write_flush", "run_diagnostics_read":
	default:
		return
	}
	duration := time.Since(start).Milliseconds()
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stages == nil {
		d.stages = map[string]*stageDiagnostic{}
	}
	entry := d.stages[name]
	if entry == nil {
		entry = &stageDiagnostic{Stage: name}
		d.stages[name] = entry
	}
	entry.Samples++
	entry.DurationMillis += duration
	if duration > entry.MaxDurationMillis {
		entry.MaxDurationMillis = duration
	}
}

func (s *Server) getDiagnostics(w http.ResponseWriter, r *http.Request) {
	s.diagnostics.mu.Lock()
	rows, stages := []httpDiagnostic{}, []stageDiagnostic{}
	for _, row := range s.diagnostics.http {
		rows = append(rows, *row)
	}
	for _, stage := range s.diagnostics.stages {
		stages = append(stages, *stage)
	}
	inFlight, since := s.diagnostics.inFlight, s.diagnostics.started
	s.diagnostics.mu.Unlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].Route+rows[i].StatusClass < rows[j].Route+rows[j].StatusClass })
	sort.Slice(stages, func(i, j int) bool { return stages[i].Stage < stages[j].Stage })
	writeJSON(w, 200, map[string]any{"since": since, "in_flight": inFlight, "http": rows, "stages": stages, "max_http_series": 128, "basis": "process_local_monotonic; completed HTTP requests; SSE duration is connection lifetime; not p95"})
}

func (s *Server) getRunDiagnostics(w http.ResponseWriter, r *http.Request) {
	limit := 40
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			http.Error(w, "limit must be 1..100", 400)
			return
		}
		limit = value
	}
	s.mu.Lock()
	currency := s.config.Costs.Currency
	s.mu.Unlock()
	started := time.Now()
	report, err := s.store.Diagnostics(r.PathValue("id"), currency, limit)
	s.diagnostics.stage("run_diagnostics_read", started)
	respond(w, report, err)
}
