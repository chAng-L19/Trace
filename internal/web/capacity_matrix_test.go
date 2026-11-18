package web

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"trace/internal/agent"
)

// Uses the real HTTP/SSE handler and its 150ms polling, not a synthetic SSE
// surrogate. Store construction and all snapshots remain in t.TempDir().
func TestCapacitySSEMatrix(t *testing.T) {
	if os.Getenv("TRACE_CAPACITY_MATRIX") != "1" {
		t.Skip("set TRACE_CAPACITY_MATRIX=1 for isolated SSE measurements")
	}
	soak := time.Duration(0)
	if text := os.Getenv("TRACE_CAPACITY_SOAK"); text != "" {
		var err error
		soak, err = time.ParseDuration(text)
		if err != nil || soak < 0 || soak > time.Hour {
			t.Fatal("TRACE_CAPACITY_SOAK must be 0..1h")
		}
	}
	report := map[string]any{"measurement": "append start through complete HTTP SSE data frame, including Store commit and SSE polling; no browser paint or provider latency", "started_at": time.Now().UTC()}
	defer func() {
		report["passed"] = !t.Failed()
		data, err := json.Marshal(report)
		if err != nil {
			t.Error(err)
			return
		}
		t.Logf("CAPACITY_JSON %s", data)
		if root := os.Getenv("TRACE_CAPACITY_REPORT_DIR"); root != "" {
			dir, err := os.MkdirTemp(root, "trace-capacity-sse-")
			if err != nil {
				t.Error(err)
				return
			}
			path := filepath.Join(dir, "report.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Error(err)
			}
			t.Logf("capacity report: %s", path)
		}
	}()
	var rows []map[string]any
	for _, count := range []int{20000, 50000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) { rows = append(rows, capacitySSECase(t, count, 0)) })
	}
	if soak > 0 {
		t.Run("soak", func(t *testing.T) { rows = append(rows, capacitySSECase(t, 20000, soak)) })
	}
	report["scenarios"] = rows
}

func capacitySSECase(t *testing.T, count int, soak time.Duration) map[string]any {
	t.Helper()
	root := t.TempDir()
	store, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(a, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	ids := make([]string, 3)
	cursors := make([]int, 3)
	sizes := make([]int64, 3)
	for n := range ids {
		snap, err := a.Create("isolated SSE capacity")
		if err != nil {
			t.Fatal(err)
		}
		events := make([]agent.Event, count)
		for i := range events {
			events[i] = agent.Event{Type: "model_stream", Data: map[string]string{"content": fmt.Sprintf("%08d", i) + strings.Repeat("x", 700)}}
		}
		if err := store.AppendEvents(snap.Run.ID, events); err != nil {
			t.Fatal(err)
		}
		view, err := store.Observe(snap.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids[n], cursors[n] = snap.Run.ID, view.Run.EventSeq
		info, err := os.Stat(filepath.Join(root, snap.Run.ID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		sizes[n] = info.Size()
		if count == 20000 && info.Size() < 15_000_000 {
			t.Fatal("SSE baseline below 15 MB")
		}
	}
	bad, err := http.Get(server.URL + "/api/runs/" + ids[0] + "/events?after=-1")
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	if bad.StatusCode != 400 {
		t.Fatal("negative SSE cursor accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), soak+5*time.Minute)
	defer cancel()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var samples []float64
	var connects, reconnectFrames []float64
	hashes := make([]string, 3)
	failures := make(chan error, 3)
	started := time.Now()
	deadline := started.Add(soak)
	for n, id := range ids {
		wg.Add(1)
		go func(n int, id string) {
			defer wg.Done()
			cursor := cursors[n]
			digest := sha256.New()
			var response *http.Response
			var scanner *bufio.Scanner
			defer func() {
				if response != nil {
					response.Body.Close()
				}
			}()
			for i := 0; i < 10 || time.Now().Before(deadline); i++ {
				var reconnectStarted time.Time
				// Reconnect mid-stream with Last-Event-ID overriding stale query cursor.
				if i == 0 || i == 5 {
					reconnectStarted = time.Now()
					if response != nil {
						response.Body.Close()
					}
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/runs/"+id+"/events?after=0", nil)
					if err != nil {
						failures <- err
						return
					}
					req.Header.Set("Last-Event-ID", strconv.Itoa(cursor))
					response, err = server.Client().Do(req)
					if err != nil {
						failures <- err
						return
					}
					if response.StatusCode != 200 {
						failures <- fmt.Errorf("SSE status %d", response.StatusCode)
						return
					}
					mu.Lock()
					connects = append(connects, float64(time.Since(reconnectStarted))/float64(time.Millisecond))
					mu.Unlock()
					scanner = bufio.NewScanner(response.Body)
					scanner.Buffer(make([]byte, 4096), 1<<20)
				}
				begin := time.Now()
				if err := store.AppendEvents(id, []agent.Event{{Type: "capacity_sse", Data: map[string]int{"run": n, "iteration": i}}}); err != nil {
					failures <- err
					return
				}
				received := false
				for scanner.Scan() {
					line := scanner.Text()
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					elapsed := float64(time.Since(begin)) / float64(time.Millisecond)
					connectionElapsed := float64(0)
					if !reconnectStarted.IsZero() {
						connectionElapsed = float64(time.Since(reconnectStarted)) / float64(time.Millisecond)
					}
					raw := []byte(strings.TrimPrefix(line, "data: "))
					var event agent.Event
					if err := json.Unmarshal(raw, &event); err != nil {
						failures <- err
						return
					}
					if event.Sequence != cursor+1 || event.Type != "capacity_sse" {
						failures <- fmt.Errorf("SSE duplicate/cross-run/history frame at %d: %+v", cursor, event)
						return
					}
					_, batch, err := store.EventBatch(id, cursor)
					if err != nil || len(batch) != 1 {
						failures <- fmt.Errorf("committed suffix mismatch: %v", err)
						return
					}
					// Compare decoded event JSON; whitespace in stored raw JSON is immaterial.
					var stored agent.Event
					if err := json.Unmarshal(batch[0], &stored); err != nil {
						failures <- err
						return
					}
					expected, _ := json.Marshal(stored)
					actual, _ := json.Marshal(event)
					if !bytes.Equal(expected, actual) {
						failures <- fmt.Errorf("SSE content differs from committed event")
						return
					}
					digest.Write(actual)
					cursor = event.Sequence
					received = true
					mu.Lock()
					samples = append(samples, elapsed)
					if !reconnectStarted.IsZero() {
						reconnectFrames = append(reconnectFrames, connectionElapsed)
					}
					mu.Unlock()
					break
				}
				if !received {
					failures <- fmt.Errorf("SSE ended before expected event: %v", scanner.Err())
					return
				}
			}
			hashes[n] = fmt.Sprintf("%x", digest.Sum(nil))
			cursors[n] = cursor
		}(n, id)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	sort.Float64s(samples)
	if len(samples) == 0 {
		t.Fatal("no SSE latency samples")
	}
	// Report handshake separately and also start-to-first-frame: moving prefix
	// preparation before connected must not hide cold/reconnect latency.
	latencies := func(values []float64) map[string]any {
		sort.Float64s(values)
		if len(values) == 0 {
			return map[string]any{"count": 0}
		}
		return map[string]any{"count": len(values), "p95_ms": values[(95*len(values)+99)/100-1], "max_ms": values[len(values)-1]}
	}
	return map[string]any{"runs": 3, "initial_events_added_per_run": count, "snapshot_bytes": sizes, "frames": len(samples), "p50_ms": samples[(len(samples)-1)/2], "p95_ms": samples[(95*len(samples)+99)/100-1], "max_ms": samples[len(samples)-1], "elapsed_seconds": time.Since(started).Seconds(), "soak_seconds": soak.Seconds(), "received_event_sha256": hashes, "last_cursors": cursors, "negative_cursor_rejected": true, "reconnect_at_frame": 5, "connect_latency": latencies(connects), "connect_through_first_frame_latency": latencies(reconnectFrames)}
}
