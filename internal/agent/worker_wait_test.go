package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Lifecycle tests need a settlement barrier, not worker_wait's new first-event
// semantics. Do not consume queued input just to force a wait: delivery itself
// is part of many resume/provenance tests. This is deliberately test-only.
func waitAllTestWorkers(a *Agent, ctx context.Context, id string, seconds int) (Snapshot, error) {
	if seconds <= 0 {
		seconds = 30
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		view, err := a.store.Observe(id)
		if err != nil {
			return Snapshot{}, err
		}
		settled := true
		for _, w := range view.Workers {
			if w.Status == "running" {
				settled = false
				break
			}
			unlock, err := a.store.AcquireRunLock(w.RunID)
			if err != nil {
				settled = false
				break
			}
			unlock()
		}
		if settled {
			return a.store.Load(id)
		}
		select {
		case <-ctx.Done():
			cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
			err := a.StopWorkers(cleanup)
			stop()
			return Snapshot{}, errors.Join(ctx.Err(), err)
		case <-ticker.C:
		}
	}
}

func TestFiveRealChildrenCapacityOnLargeParent(t *testing.T) {
	p := &blockingProvider{entered: make(chan struct{}, 6), release: make(chan struct{}, 6)}
	a, store := newTestAgent(t, p)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := a.StopWorkers(ctx); err != nil {
			t.Error(err)
		}
	})
	id := createTestRun(t, a)
	tasks := make([]PlanTask, 6)
	for i := range tasks {
		tasks[i] = PlanTask{ID: fmt.Sprint(i), Goal: "hold independent child"}
	}
	if _, err := a.SetPlan(id, tasks); err != nil {
		t.Fatal(err)
	}
	events := make([]Event, 20000)
	for i := range events {
		events[i] = Event{Type: "model_stream", Data: map[string]any{"request_id": "historical-fixture", "delta": StreamDelta{Kind: "thinking", Text: fmt.Sprintf("%0600d", i)}}}
	}
	if err := store.AppendEvents(id, events); err != nil {
		t.Fatal(err)
	}
	// Race instrumentation multiplies full-ledger work. This is a lifecycle
	// assertion with a bounded cleanup, not a throughput deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	started := time.Now()
	workers, err := a.DispatchFrontier(ctx, id)
	if err != nil || len(workers) != 5 {
		t.Fatalf("five-child dispatch: %d %v", len(workers), err)
	}
	for i := 0; i < 5; i++ {
		select {
		case <-p.entered:
		case <-ctx.Done():
			t.Fatal("five children did not reach their model requests")
		}
	}
	seen := map[string]bool{}
	for _, w := range workers {
		child, err := store.Load(w.RunID)
		if err != nil || seen[w.RunID] || child.Run.ParentRunID != id || len(child.Requests) != 1 || child.Run.Status != StatusRunning {
			t.Fatalf("not five independent live children: %+v %v", child.Run, err)
		}
		seen[w.RunID] = true
	}
	if _, err := a.DispatchTask(ctx, id, "5"); !errors.Is(err, errWorkerCapacity) {
		t.Fatalf("sixth child admitted: %v", err)
	}
	view, err := store.Observe(id)
	if err != nil || len(view.Workers) != 5 || view.Plan.Tasks[5].Status != "pending" {
		t.Fatalf("sixth admission mutated parent: %+v %v", view.Plan, err)
	}
	p.release <- struct{}{}
	partial, err := a.WaitWorkers(ctx, id, 120)
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, w := range partial.Workers {
		if w.Status == "running" {
			active++
		}
	}
	if active != 4 {
		t.Fatalf("first child did not wake parent with four continuing: %d", active)
	}
	next, err := a.DispatchFrontier(ctx, id)
	if err != nil || len(next) != 1 || next[0].TaskID != "5" || seen[next[0].RunID] {
		t.Fatalf("sixth child did not acquire released slot: %+v %v", next, err)
	}
	select {
	case <-p.entered:
	case <-ctx.Done():
		t.Fatal("sixth child did not reach model")
	}
	close(p.release)
	final, err := waitAllTestWorkers(a, ctx, id, 120)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Workers) != 6 {
		t.Fatalf("duplicate child count: %d", len(final.Workers))
	}
	for _, task := range final.Plan.Tasks {
		if task.Status != "completed" {
			t.Fatalf("child not settled: %+v", task)
		}
	}
	t.Logf("large parent: events=%d children=%d dispatch/settle=%s", len(final.Events), len(final.Workers), time.Since(started))
}
