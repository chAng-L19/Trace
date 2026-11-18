package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBudgetCheckpointPersistsNoticeAndClosesDispatch(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetRunBudget(id, Budget{MaxSeconds: 10, CheckpointSeconds: 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlan(id, []PlanTask{{ID: "active", Goal: "active child"}, {ID: "late", Goal: "must wait"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "active", "w", 30); err != nil {
		t.Fatal(err)
	}
	child := registerBudgetChild(t, a, id, "active", "w")
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusRunning
		s.Run.ElapsedMillis = 8500
		s.Run.BudgetActiveSince = time.Now()
		s.Workers[0].Status = "running"
		s.Plan.Tasks[0].Status = "running"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(child, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	notice := BudgetCheckpoint{BudgetRunID: id, RequestedAt: time.Now().UTC(), Deadline: time.Now().UTC().Add(1500 * time.Millisecond)}
	if err := a.requestBudgetCheckpoint(context.Background(), id, notice, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	s, err := store.Load(id)
	if err != nil || s.Run.BudgetCheckpoint == nil || len(s.WorkerMessages) != 1 || s.WorkerMessages[0].To != "w" || s.Run.Status != StatusRunning {
		t.Fatalf("checkpoint claimed completion: %+v %v", s, err)
	}
	c, err := store.Load(child)
	if err != nil || c.Run.BudgetCheckpoint == nil || c.Run.Status != StatusRunning {
		t.Fatalf("child notice: %+v %v", c.Run, err)
	}
	if err := a.requestBudgetCheckpoint(context.Background(), id, notice, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	s, _ = store.Load(id)
	if len(s.WorkerMessages) != 1 {
		t.Fatal("duplicate checkpoint notification")
	}
	var window *CheckpointWindowError
	if _, err := a.DispatchTask(context.Background(), id, "late"); !errors.As(err, &window) {
		t.Fatalf("deadline window admitted child: %v", err)
	}
	if _, err := a.YieldWorker(child, "budget checkpoint", "resume existing child after budget increase"); err != nil {
		t.Fatal(err)
	}
	c, _ = store.Load(child)
	if c.Run.Status != StatusPaused || !strings.HasPrefix(c.Run.PauseReason, "worker_yield:") {
		t.Fatal("cooperative checkpoint lost")
	}
}

func TestBudgetCheckpointHardDeadlineAndImmediateCancellation(t *testing.T) {
	for _, manual := range []bool{false, true} {
		name := "hard-deadline"
		if manual {
			name = "manual-cancel"
		}
		t.Run(name, func(t *testing.T) {
			a, store := newTestAgent(t, MockProvider{})
			id := createTestRun(t, a)
			if _, err := a.SetRunBudget(id, Budget{MaxSeconds: 1}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; s.Run.ElapsedMillis = 600; return nil }); err != nil {
				t.Fatal(err)
			}
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := time.Now()
			ctx, stop, err := a.beginBudgetContext(parent, id)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			if manual {
				cancel()
			}
			select {
			case <-ctx.Done():
			case <-time.After(1500 * time.Millisecond):
				t.Fatal("deadline was extended")
			}
			if manual {
				if time.Since(start) > 250*time.Millisecond {
					t.Fatal("manual cancellation waited for checkpoint window")
				}
			} else {
				var cause *timeBudgetCause
				if !errors.As(context.Cause(ctx), &cause) {
					t.Fatalf("deadline cause: %v", context.Cause(ctx))
				}
				s, err := store.Load(id)
				if err != nil || s.Run.BudgetCheckpoint == nil {
					t.Fatalf("notice missing before hard deadline: %+v %v", s.Run, err)
				}
			}
		})
	}
}

func TestBudgetCheckpointInheritedDeadlineAndNoImplicitTimer(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	ctx, stop, err := a.beginBudgetContext(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, set := ctx.Deadline(); set {
		t.Fatal("unlimited budget created implicit deadline")
	}
	stop()
	if _, err := a.SetRunBudget(id, Budget{MaxSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlan(id, []PlanTask{{ID: "child", Goal: "inherits root deadline"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "child", "worker", 30); err != nil {
		t.Fatal(err)
	}
	child := registerBudgetChild(t, a, id, "child", "worker")
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusRunning
		s.Run.ElapsedMillis = 700
		s.Run.BudgetActiveSince = time.Now().UTC()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, stop, err = a.beginBudgetContext(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("background-dispatched child lost root hard deadline")
	}
	var cause *timeBudgetCause
	if !errors.As(context.Cause(ctx), &cause) || cause.RunID != id {
		t.Fatalf("wrong ancestor deadline: %v", context.Cause(ctx))
	}
}

type checkpointCooperativeProvider struct {
	mu               sync.Mutex
	turns            int
	entered, release chan struct{}
}

func (p *checkpointCooperativeProvider) Complete(ctx context.Context, r ModelRequest) (ModelResponse, error) {
	p.mu.Lock()
	p.turns++
	turn := p.turns
	p.mu.Unlock()
	if turn == 1 {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return ModelResponse{}, ctx.Err()
		}
		return ModelResponse{ToolCalls: []ToolCall{{ID: "wait-checkpoint", Name: "worker_wait", Arguments: "{}"}}}, nil
	}
	if turn == 2 {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, "Runtime time-budget checkpoint request") {
				checkpoint, _ := json.Marshal(WorkerHandoff{Report: &u7Text, NextAction: "retain same child and inspect persisted output on resume"})
				raw, _ := json.Marshal(map[string]string{"reason": "explicit budget conclusion", "checkpoint": string(checkpoint)})
				return ModelResponse{ToolCalls: []ToolCall{{ID: "yield-checkpoint", Name: "worker_yield", Arguments: string(raw)}}}, nil
			}
		}
		return ModelResponse{}, errors.New("cooperative worker lost checkpoint message")
	}
	return ModelResponse{Content: fixtureReport("continued original child")}, nil
}

func TestBudgetCheckpointCooperativeWorkerAndSameChildResume(t *testing.T) {
	p := &checkpointCooperativeProvider{entered: make(chan struct{}), release: make(chan struct{})}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.StopWorkers(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := a.SetPlan(id, []PlanTask{{ID: "child", Goal: "cooperate with checkpoint"}}); err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), id, "child")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not enter provider")
	}
	notice := BudgetCheckpoint{BudgetRunID: id, RequestedAt: time.Now().UTC(), Deadline: time.Now().UTC().Add(time.Second)}
	if err := a.requestBudgetCheckpoint(context.Background(), id, notice, map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	close(p.release)
	if _, err = waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	child, err := store.Load(w.RunID)
	if err != nil || child.Run.Status != StatusPaused || !strings.HasPrefix(child.Run.PauseReason, "worker_yield:") {
		t.Fatalf("did not cooperate: %+v %v", child.Run, err)
	}
	if _, err = a.ResumeWorker(context.Background(), id, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	s, err := store.Load(id)
	if err != nil || len(s.Workers) != 1 || s.Workers[0].RunID != w.RunID || s.Workers[0].Visit != 2 {
		t.Fatalf("checkpoint replay created new child: %+v %v", s.Workers, err)
	}
	child, err = store.Load(w.RunID)
	if err != nil || child.Run.Status != StatusCompleted || len(child.Actions) != 2 || child.Actions[0].CallID != "wait-checkpoint" || child.Actions[1].CallID != "yield-checkpoint" || child.WorkerCheckpoint == nil || child.WorkerCheckpoint.Report == nil {
		t.Fatalf("checkpoint resume did not finish the original actions and report: %+v %v", child.Run, err)
	}
}

func TestBudgetCheckpointDuringToolKeepsHardDeadline(t *testing.T) {
	a, store := newTestAgent(t, manualProvider(ToolCall{ID: "one", Name: "external", Arguments: "{}"}))
	id := createTestRun(t, a)
	waitTestRun(t, a, id)
	a.ConfigureBudget(BudgetOverride{MaxSeconds: int64Pointer(1)})
	t.Setenv("TRACE_TEST_CHILD", "blocking_tool")
	t.Setenv("TRACE_TEST_READY", filepath.Join(t.TempDir(), "ready"))
	start := time.Now()
	s, err := a.ExecuteTool(context.Background(), id, "one", []string{os.Args[0], "-test.run=^TestProcessFixture$"})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 3*time.Second || s.Run.Status != StatusPaused || s.Run.BudgetCheckpoint == nil || len(s.Actions) != 1 || s.Actions[0].Result == nil || !s.Actions[0].Result.Interrupted {
		t.Fatalf("checkpoint delayed tool interruption: %+v", s.Run)
	}
	persisted, err := store.Load(id)
	if err != nil || persisted.Run.BudgetCheckpoint == nil || len(persisted.Evidence) != 1 || persisted.Evidence[0].Verification == "verified" || len(persisted.Actions) != 1 || persisted.Actions[0].Status != "uncertain" || persisted.Actions[0].Result == nil || !persisted.Actions[0].Result.OutcomeUncertain || !persisted.Actions[0].Result.Interrupted {
		t.Fatalf("interrupted checkpoint promoted evidence: %+v %v", persisted.Run, err)
	}
}

func TestBudgetCheckpointCommitFailureDoesNotPublishNotice(t *testing.T) {
	a, store, id, _ := dispatchFailureFixture(t)
	var restore func()
	store.beforePersist = func(got string) {
		if got == id && restore == nil {
			restore = dispatchBlockRename(t, store, id)
		}
	}
	err := a.requestBudgetCheckpoint(context.Background(), id, BudgetCheckpoint{BudgetRunID: id, RequestedAt: time.Now(), Deadline: time.Now().Add(time.Second)}, map[string]bool{})
	store.beforePersist = nil
	if restore != nil {
		restore()
	}
	if err == nil {
		t.Fatal("checkpoint persistence failure swallowed")
	}
	s, loadErr := store.Load(id)
	if loadErr != nil || s.Run.BudgetCheckpoint != nil || len(s.WorkerMessages) != 0 {
		t.Fatalf("failed commit published checkpoint: %+v %v", s.Run, loadErr)
	}
}

func TestBudgetCheckpointOperatorCancelDoesNotWaitForWindow(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetRunBudget(id, Budget{MaxSeconds: 60, CheckpointSeconds: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, stop, err := a.beginBudgetContext(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	start := time.Now()
	if _, err := a.Cancel(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("operator cancel waited for checkpoint window")
	}
	if time.Since(start) > time.Second {
		t.Fatal("manual cancel deadline delayed")
	}
	s, err := store.Load(id)
	if err != nil || s.Run.Status != StatusCancelled || s.Run.BudgetCheckpoint != nil {
		t.Fatalf("cancel became checkpoint completion: %+v %v", s.Run, err)
	}
}

func TestBudgetCheckpointCommitLatencyDoesNotExtendDeadline(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetRunBudget(id, Budget{MaxSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; s.Run.ElapsedMillis = 900; return nil }); err != nil {
		t.Fatal(err)
	}
	store.beforePersist = func(got string) {
		if got == id {
			time.Sleep(200 * time.Millisecond)
		}
	}
	ctx, stop, err := a.beginBudgetContext(context.Background(), id)
	store.beforePersist = nil
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	deadline, ok := ctx.Deadline()
	if !ok || deadline.After(time.Now()) {
		t.Fatalf("persistence extended absolute deadline: %v %v", deadline, ok)
	}
	select {
	case <-ctx.Done():
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expired deadline remained live after commit")
	}
}
