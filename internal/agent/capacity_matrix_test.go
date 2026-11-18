package agent

// Opt-in synthetic capacity measurements, never production state. Dataset scale
// follows pi 8a7b0c03 packages/agent/.../benchmark/datasets.ts; correctness is
// checked before reporting timings as in benchmark/session/benchmark.ts.
// Muteki 144df766 swarm/coordinator_dispatch.py:501-518 counts workers until
// settlement; the N+1 check below retains Trace's existing lifecycle boundary.
import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"trace/internal/worker"
)

type capacityLatency struct {
	Count int     `json:"count"`
	P50   float64 `json:"p50_ms"`
	P95   float64 `json:"p95_ms"`
	Max   float64 `json:"max_ms"`
}

func capacityStats(samples []float64) capacityLatency {
	sort.Float64s(samples)
	if len(samples) == 0 {
		return capacityLatency{}
	}
	return capacityLatency{len(samples), samples[(len(samples)-1)/2], samples[(95*len(samples)+99)/100-1], samples[len(samples)-1]}
}
func capacityHash(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
func capacityReport(t *testing.T, report map[string]any) {
	t.Helper()
	report["passed"] = !t.Failed()
	report["go"] = runtime.Version()
	report["os"] = runtime.GOOS
	report["cpu_count"] = runtime.NumCPU()
	data, err := json.Marshal(report)
	if err != nil {
		t.Error(err)
		return
	}
	t.Logf("CAPACITY_JSON %s", data)
	if root := os.Getenv("TRACE_CAPACITY_REPORT_DIR"); root != "" {
		// A fresh child prevents overwriting reports or treating the path as state.
		dir, err := os.MkdirTemp(root, "trace-capacity-agent-")
		if err != nil {
			t.Error(err)
			return
		}
		path := filepath.Join(dir, "report.json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Error(err)
			return
		}
		t.Logf("capacity report: %s", path)
	}
}

func TestCapacityMatrix(t *testing.T) {
	if os.Getenv("TRACE_CAPACITY_MATRIX") != "1" {
		t.Skip("set TRACE_CAPACITY_MATRIX=1 for isolated capacity measurements")
	}
	soak := time.Duration(0)
	if value := os.Getenv("TRACE_CAPACITY_SOAK"); value != "" {
		var err error
		soak, err = time.ParseDuration(value)
		if err != nil || soak < 0 || soak > time.Hour {
			t.Fatal("TRACE_CAPACITY_SOAK must be a duration from 0 to 1h")
		}
	}
	started := time.Now()
	report := map[string]any{"started_at": started.UTC(), "measurement": "synthetic Store append+committed suffix; separate Worker/capture scenarios; no browser or external model latency", "soak_requested": soak.String()}
	defer func() { report["elapsed_seconds"] = time.Since(started).Seconds(); capacityReport(t, report) }()
	var rows []map[string]any
	for _, size := range []struct{ events, runs int }{{20000, 1}, {20000, 3}, {50000, 3}} {
		name := fmt.Sprintf("events_%d_runs_%d", size.events, size.runs)
		t.Run(name, func(t *testing.T) { rows = append(rows, capacityStoreCase(t, size.events, size.runs, 0)) })
	}
	if soak > 0 {
		t.Run("soak", func(t *testing.T) { rows = append(rows, capacityStoreCase(t, 20000, 3, soak)) })
	}
	report["store"] = rows
	var lifecycle []map[string]any
	for _, n := range []int{5, 8} {
		t.Run(fmt.Sprintf("workers_%d", n), func(t *testing.T) { lifecycle = append(lifecycle, capacityWorkers(t, n)) })
	}
	report["workers"] = lifecycle
	t.Run("capture", func(t *testing.T) { report["capture"] = capacityCapture(t) })
	t.Run("negative_controls", func(t *testing.T) {
		TestModelStreamBatchPropagatesPersistenceFailure(t)
		TestCompleteFailsWhenFinalStreamFlushFails(t)
		TestPreviewPersistenceDoesNotBlockProcessCapture(t)
	})
	report["negative_controls"] = []string{"persistence failure cancels stream", "failed final flush blocks dispatch", "blocked preview does not block complete output capture", "worker N+1 admission rejected"}
}

func capacityStoreCase(t *testing.T, events, runs int, soak time.Duration) map[string]any {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, runs)
	cursors := make([]int, runs)
	history := make([]string, runs)
	bytesBefore := make([]int64, runs)
	for n := range ids {
		snap, err := store.Create("isolated capacity fixture", DefaultPrompt, promptHash(DefaultPrompt))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < events; i++ {
			addEvent(&snap, "model_stream", "capacity", map[string]string{"content": fmt.Sprintf("%08d", i) + strings.Repeat("x", 700)})
		}
		if err := store.saveLocked(snap); err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(snap.Events)
		history[n] = capacityHash(data)
		ids[n], cursors[n] = snap.Run.ID, snap.Run.EventSeq
		info, err := os.Stat(filepath.Join(store.root, snap.Run.ID+".json"))
		if err != nil {
			t.Fatal(err)
		}
		bytesBefore[n] = info.Size()
		if events == 20000 && info.Size() < 15_000_000 {
			t.Fatal("baseline fixture below 15 MB")
		}
	}
	var samples []float64
	var mu sync.Mutex
	errorsOut := make(chan error, runs)
	var wg sync.WaitGroup
	started := time.Now()
	deadline := started.Add(soak)
	for n, id := range ids {
		wg.Add(1)
		go func(n int, id string) {
			defer wg.Done()
			cursor := cursors[n]
			for iteration := 0; iteration < 10 || time.Now().Before(deadline); iteration++ {
				begin := time.Now()
				if err := store.AppendEvents(id, []Event{{Type: "capacity_tick", Data: map[string]int{"iteration": iteration, "run": n}}}); err != nil {
					errorsOut <- err
					return
				}
				run, batch, err := store.EventBatch(id, cursor)
				if err != nil || len(batch) != 1 || run.EventSeq != cursor+1 {
					errorsOut <- fmt.Errorf("committed cursor lost/duplicated: run=%d cursor=%d batch=%d: %v", n, cursor, len(batch), err)
					return
				}
				var event Event
				if err := json.Unmarshal(batch[0], &event); err != nil || event.Sequence != cursor+1 || event.Type != "capacity_tick" {
					errorsOut <- fmt.Errorf("invalid committed event: %v", err)
					return
				}
				cursor = run.EventSeq
				if _, err := store.List(); err != nil {
					errorsOut <- err
					return
				}
				elapsed := float64(time.Since(begin)) / float64(time.Millisecond)
				mu.Lock()
				samples = append(samples, elapsed)
				mu.Unlock()
			}
			cursors[n] = cursor
		}(n, id)
	}
	wg.Wait()
	close(errorsOut)
	for err := range errorsOut {
		t.Error(err)
	}
	hashes := make([]string, runs)
	bytesAfter := make([]int, runs)
	for n, id := range ids {
		snap, err := store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Events) != cursors[n] || len(snap.Events) < events {
			t.Fatal("final event ledger size mismatch")
		}
		data, _ := json.Marshal(snap.Events[:events])
		if capacityHash(data) != history[n] {
			t.Fatal("historical event content changed")
		}
		raw, err := os.ReadFile(filepath.Join(store.root, id+".json"))
		if err != nil {
			t.Fatal(err)
		}
		hashes[n], bytesAfter[n] = capacityHash(raw), len(raw)
	}
	return map[string]any{"initial_events_per_run": events, "runs": runs, "initial_bytes": bytesBefore, "final_bytes": bytesAfter, "final_sha256": hashes, "history_sha256": history, "historical_prefix_preserved": true, "append_suffix_list_latency": capacityStats(samples), "elapsed_seconds": time.Since(started).Seconds(), "soak_seconds": soak.Seconds()}
}

func capacityWorkers(t *testing.T, n int) map[string]any {
	t.Helper()
	p := &blockingProvider{entered: make(chan struct{}, n+1), release: make(chan struct{}, n+1)}
	a, store := newTestAgent(t, p)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		if err := a.StopWorkers(cleanup); err != nil {
			t.Error(err)
		}
	}()
	id := createTestRun(t, a)
	tasks := make([]PlanTask, n+1)
	for i := range tasks {
		tasks[i] = PlanTask{ID: fmt.Sprint(i), Goal: "capacity lifecycle child"}
	}
	if _, err := a.SetPlan(id, tasks); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlanCapacity(id, n); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	workers, err := a.DispatchFrontier(ctx, id)
	if err != nil || len(workers) != n {
		t.Fatalf("dispatch %d children: %d %v", n, len(workers), err)
	}
	for i := 0; i < n; i++ {
		select {
		case <-p.entered:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	dispatchMS := float64(time.Since(started)) / float64(time.Millisecond)
	if _, err := a.DispatchTask(ctx, id, fmt.Sprint(n)); !errors.Is(err, errWorkerCapacity) {
		t.Fatalf("N+1 admission: %v", err)
	}
	seen := map[string]bool{}
	for _, w := range workers {
		child, err := store.Load(w.RunID)
		if err != nil || seen[w.RunID] || child.Run.ParentRunID != id || len(child.Requests) != 1 {
			t.Fatal("child identity/request mismatch")
		}
		seen[w.RunID] = true
	}
	close(p.release)
	if _, err := waitAllTestWorkers(a, ctx, id, 90); err != nil {
		t.Fatal(err)
	}
	next, err := a.DispatchFrontier(ctx, id)
	if err != nil || len(next) != 1 || seen[next[0].RunID] {
		t.Fatalf("released slot did not admit distinct child: %v", err)
	}
	final, err := waitAllTestWorkers(a, ctx, id, 90)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range final.Plan.Tasks {
		if task.Status != "completed" {
			t.Fatalf("task not settled: %+v", task)
		}
	}
	if len(final.Workers) != n+1 || a.HasActiveWorkers() {
		t.Fatal("worker lifecycle leaked or duplicated")
	}
	return map[string]any{"capacity": n, "children": len(final.Workers), "to_all_first_requests_ms": dispatchMS, "total_ms": float64(time.Since(started)) / float64(time.Millisecond), "overflow_rejected": true, "all_settled": true}
}

func TestCapacityOutputFixture(t *testing.T) {
	if os.Getenv("TRACE_CAPACITY_OUTPUT_FIXTURE") != "1" {
		return
	}
	done := make(chan struct{})
	go func() { fmt.Fprint(os.Stderr, strings.Repeat("e", 4<<20)); close(done) }()
	fmt.Fprint(os.Stdout, strings.Repeat("o", 4<<20))
	<-done
	os.Exit(0)
}
func capacityCapture(t *testing.T) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	result, err := worker.Run(ctx, worker.Request{Command: []string{os.Args[0], "-test.run=^TestCapacityOutputFixture$"}, Env: []string{"TRACE_CAPACITY_OUTPUT_FIXTURE=1"}, CaptureDir: t.TempDir()})
	if err != nil || result.Status != worker.StatusCompleted || result.OutcomeUncertain {
		t.Fatalf("capture: %+v %v", result, err)
	}
	hashes := map[string]string{}
	for name, path := range map[string]string{"stdout": result.StdoutPath, "stderr": result.StderrPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		char := "o"
		if name == "stderr" {
			char = "e"
		}
		expected := capacityHash([]byte(strings.Repeat(char, 4<<20)))
		if len(data) != 4<<20 || capacityHash(data) != expected {
			t.Fatal("capture byte/hash mismatch")
		}
		hashes[name] = expected
	}
	return map[string]any{"bytes_per_stream": 4 << 20, "sha256": hashes, "elapsed_ms": float64(time.Since(started)) / float64(time.Millisecond), "exit_code": result.ExitCode}
}
