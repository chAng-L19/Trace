package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

type nestedResumeProvider struct {
	mu            sync.Mutex
	middleBlocked bool
	middleEntered chan struct{}
	root          resumeWorkerProvider
	leaf          resumeWorkerProvider
}

func (p *nestedResumeProvider) Complete(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	goal := ""
	for _, m := range req.Messages {
		if strings.HasPrefix(m.Content, "Worker task context (") {
			_, raw, _ := strings.Cut(m.Content, "\n")
			var manifest WorkerContext
			if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
				return ModelResponse{}, err
			}
			goal = manifest.Goal
		}
	}
	if goal == "" {
		return p.root.Complete(ctx, req)
	}
	if goal == "leaf" {
		return p.leaf.Complete(ctx, req)
	}
	seen := map[string]bool{}
	for _, m := range req.Messages {
		if m.Role == "tool" {
			seen[m.ToolCallID] = true
		}
	}
	if !seen["nested-plan"] {
		return ModelResponse{ToolCalls: []ToolCall{{ID: "nested-plan", Name: "plan", Arguments: `{"tasks":[{"id":"leaf","goal":"leaf"}]}`}}}, nil
	}
	if !seen["nested-dispatch"] {
		return ModelResponse{ToolCalls: []ToolCall{{ID: "nested-dispatch", Name: "worker_dispatch", Arguments: `{"task_id":"leaf"}`}}}, nil
	}
	p.mu.Lock()
	block := !p.middleBlocked
	if block {
		p.middleBlocked = true
		close(p.middleEntered)
	}
	p.mu.Unlock()
	if block {
		<-ctx.Done()
		return ModelResponse{}, ctx.Err()
	}
	if !seen["nested-wait"] {
		return ModelResponse{ToolCalls: []ToolCall{{ID: "nested-wait", Name: "worker_wait", Arguments: `{"seconds":5}`}}}, nil
	}
	return ModelResponse{Content: fixtureReport("nested worker complete")}, nil
}

func TestWorkerResumeNestedParentPausePreservesEveryIdentity(t *testing.T) {
	p := &nestedResumeProvider{middleEntered: make(chan struct{}), leaf: resumeWorkerProvider{entered: make(chan struct{}), blockAfterAction: true}}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	p.root.pauseParent = func() error { _, err := a.Pause(id); return err }
	if _, err := a.SetPlan(id, []PlanTask{{ID: "middle", Goal: "middle"}, {ID: "next", Goal: "next", Dependencies: []string{"middle"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), id, "middle")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.StopWorkers(context.Background()); a.CloseMCP() })
	for _, entered := range []chan struct{}{p.middleEntered, p.leaf.entered} {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("nested worker did not reach simultaneous active boundary")
		}
	}
	middle, err := store.Load(w.RunID)
	if err != nil || len(middle.Workers) != 1 {
		t.Fatalf("nested identity missing: %v %+v", err, middle.Workers)
	}
	leafWorker := middle.Workers[0]
	if _, err := a.Pause(id); err != nil {
		t.Fatal(err)
	}
	if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	for _, childID := range []string{w.RunID, leafWorker.RunID} {
		child, err := store.Load(childID)
		if err != nil || child.Run.Status != StatusPaused || child.Run.PauseReason != "parent_pause" {
			t.Fatalf("nested pause lost: %+v %v", child.Run, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := a.Run(ctx, id); err != nil {
		t.Fatal(err)
	}
	root, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	middle, err = store.Load(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := store.Load(leafWorker.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Workers) != 1 || root.Workers[0].ID != w.ID || root.Workers[0].RunID != w.RunID || root.Plan.Tasks[0].Status != "completed" || !hasString(root.Plan.Frontier, "next") {
		t.Fatalf("root identity/settlement: %+v %+v", root.Workers, root.Plan)
	}
	if len(middle.Workers) != 1 || middle.Workers[0].ID != leafWorker.ID || middle.Workers[0].RunID != leafWorker.RunID || middle.Plan.Tasks[0].Status != "completed" || middle.Run.Status != StatusCompleted {
		t.Fatalf("middle identity/settlement: %+v %+v", middle.Workers, middle.Plan)
	}
	if leaf.Run.Status != StatusCompleted || len(leaf.Actions) != 1 || leaf.Actions[0].CallID != "once" {
		t.Fatalf("leaf action replayed: %+v %+v", leaf.Run, leaf.Actions)
	}
	for _, s := range []Snapshot{root, middle} {
		if s.Workers[0].CleanupStatus != "completed" {
			t.Fatal("nested cleanup not settled")
		}
	}
}
