package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func registerBudgetChild(t *testing.T, a *Agent, parent, task, worker string) string {
	t.Helper()
	child, err := a.CreateConfigured("budget child", RunConfiguration{TreeBudget: &TreeBudget{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.Update(child.Run.ID, func(s *Snapshot) error { s.Run.ParentRunID, s.Run.ParentTaskID = parent, task; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.Update(parent, func(s *Snapshot) error {
		taskRef, err := planTask(s, task)
		if err != nil {
			return err
		}
		taskRef.ChildRunID = child.Run.ID
		s.Workers = append(s.Workers, AgentWorker{ID: worker, TaskID: task, RunID: child.Run.ID, Status: "paused", CleanupStatus: "completed"})
		taskRef.Status = "paused"
		refreshFrontier(&s.Plan)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return child.Run.ID
}

func TestTreeBudgetAtomicWorkerClaimsAndRestartNoRecount(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetTreeBudget(id, TreeBudget{MaxWorkers: 5, MaxDepth: 1}); err != nil {
		t.Fatal(err)
	}
	tasks := []PlanTask{}
	for i := 0; i < 6; i++ {
		tasks = append(tasks, PlanTask{ID: fmt.Sprint(i), Goal: "independent"})
	}
	if _, err := a.SetPlan(id, tasks); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlanCapacity(id, 6); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 6)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := a.ClaimTask(id, fmt.Sprint(i), fmt.Sprintf("worker-%d", i), 30)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			var budget *TreeBudgetError
			if !errors.As(err, &budget) {
				t.Fatal(err)
			}
		}
	}
	if accepted != 5 {
		t.Fatalf("concurrent admissions=%d", accepted)
	}
	s, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var admitted PlanTask
	for _, task := range s.Plan.Tasks {
		if task.Status == "running" {
			admitted = task
			break
		}
	}
	child := registerBudgetChild(t, a, id, admitted.ID, admitted.WorkerID)
	if _, err := a.SetPlan(child, []PlanTask{{ID: "deep", Goal: "must hit depth gate"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(child, "deep", "nested", 30); err == nil {
		t.Fatal("depth budget bypassed")
	}
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := New(reopened, MockProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.Update(id, func(s *Snapshot) error { return resumed.checkTreeBudgetLocked(s, "resume") }); err != nil {
		t.Fatalf("resume counted a new worker: %v", err)
	}
	_, err = reopened.Update(id, func(s *Snapshot) error { return resumed.checkTreeBudgetLocked(s, "worker") })
	var budget *TreeBudgetError
	if !errors.As(err, &budget) {
		t.Fatalf("restart reset cumulative admissions: %v", err)
	}
}

func TestTreeBudgetRequestsReserveUnknownAndCosts(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetTreeBudget(id, TreeBudget{MaxModelRequests: 2, MaxTokens: 100, MaxCost: "0.000100", Currency: "USD"}); err != nil {
		t.Fatal(err)
	}
	price := &ModelPrice{Model: "fixture", InputPerMillion: "1", OutputPerMillion: "1", Currency: "USD", Source: "test", EffectiveAt: "2026-10-29"}
	request := func(id string) ModelRequest {
		return ModelRequest{ID: id, ProviderModel: "fixture", Price: price, ContextTokens: 40, MaxOutputTokens: 20}
	}
	first := request("one")
	if _, err := store.Update(id, func(s *Snapshot) error {
		if err := a.admitTreeRequestLocked(s, first); err != nil {
			return err
		}
		s.Requests = append(s.Requests, first)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := store.Update(id, func(s *Snapshot) error { return a.admitTreeRequestLocked(s, request("two")) })
	var budget *TreeBudgetError
	if !errors.As(err, &budget) {
		t.Fatalf("in-flight reservation bypassed: %v", err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Responses = append(s.Responses, ResponseRecord{RequestID: first.ID, Response: ModelResponse{Usage: &TokenUsage{InputTokens: int64Pointer(5), OutputTokens: int64Pointer(5), TotalTokens: int64Pointer(10)}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { return a.admitTreeRequestLocked(s, request("two")) }); err != nil {
		t.Fatalf("reported usage did not release reservation: %v", err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Responses[0].Response.Usage = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(id, func(s *Snapshot) error { return a.admitTreeRequestLocked(s, request("two")) })
	if !errors.As(err, &budget) {
		t.Fatalf("unknown usage treated as zero: %v", err)
	}
	if _, err := a.SetTreeBudget(id, TreeBudget{}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { return a.admitTreeRequestLocked(s, request("two")) }); err != nil {
		t.Fatalf("implicit unknown usage limit when unlimited: %v", err)
	}
}

func TestTreeBudgetAtomicRequestsAcrossFiveChildren(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetTreeBudget(id, TreeBudget{MaxModelRequests: 2}); err != nil {
		t.Fatal(err)
	}
	tasks := []PlanTask{}
	for i := 0; i < 5; i++ {
		tasks = append(tasks, PlanTask{ID: fmt.Sprint(i), Goal: "parallel requests"})
	}
	if _, err := a.SetPlan(id, tasks); err != nil {
		t.Fatal(err)
	}
	children := []string{}
	for i := 0; i < 5; i++ {
		task := fmt.Sprint(i)
		worker := "w" + task
		if _, err := a.ClaimTask(id, task, worker, 30); err != nil {
			t.Fatal(err)
		}
		children = append(children, registerBudgetChild(t, a, id, task, worker))
	}
	results := make(chan error, 5)
	var wg sync.WaitGroup
	for _, child := range children {
		wg.Add(1)
		go func(child string) {
			defer wg.Done()
			_, err := store.Update(child, func(s *Snapshot) error {
				r := ModelRequest{ID: child + "-1", ContextTokens: 10, MaxOutputTokens: 10}
				if err := a.admitTreeRequestLocked(s, r); err != nil {
					return err
				}
				s.Requests = append(s.Requests, r)
				return nil
			})
			results <- err
		}(child)
	}
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			var budget *TreeBudgetError
			if !errors.As(err, &budget) {
				t.Fatal(err)
			}
		}
	}
	if accepted != 2 {
		t.Fatalf("atomic tree requests accepted=%d", accepted)
	}
}

func TestTreeBudgetEnvironmentAndUnlimitedDefaults(t *testing.T) {
	if DefaultBudget() != (Budget{}) {
		t.Fatal("default run budget became bounded")
	}
	t.Setenv("TRACE_TREE_MAX_DEPTH", "3")
	t.Setenv("TRACE_TREE_MAX_WORKERS", "9")
	t.Setenv("TRACE_TREE_MAX_MODEL_REQUESTS", "0")
	t.Setenv("TRACE_TREE_MAX_TOKENS", "1000")
	t.Setenv("TRACE_TREE_MAX_SECONDS", "60")
	t.Setenv("TRACE_TREE_MAX_COST", "0.25")
	t.Setenv("TRACE_TREE_CURRENCY", "USD")
	b, err := TreeBudgetFromEnv()
	if err != nil || b == nil || b.MaxDepth != 3 || b.MaxWorkers != 9 || b.MaxModelRequests != 0 || b.MaxCost != "0.25" {
		t.Fatalf("env: %+v %v", b, err)
	}
	t.Setenv("TRACE_TREE_MAX_DEPTH", "-1")
	if _, err := TreeBudgetFromEnv(); err == nil {
		t.Fatal("negative depth accepted")
	}
}

func TestTreeBudgetResumesOriginalChildAtWorkerCeiling(t *testing.T) {
	p := &yieldProvider{}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	if _, err := a.SetTreeBudget(id, TreeBudget{MaxWorkers: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "yield then continue"}}); err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), id, "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ResumeWorker(context.Background(), id, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	s, err := store.Load(id)
	if err != nil || len(s.Workers) != 1 || s.Workers[0].RunID != w.RunID || s.Workers[0].Visit != 2 {
		t.Fatalf("resume changed identity: %+v %v", s.Workers, err)
	}
}

func TestTreeBudgetCumulativeTimeAndCostNotPerChild(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "child", Goal: "aggregate"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, "child", "w", 30); err != nil {
		t.Fatal(err)
	}
	child := registerBudgetChild(t, a, id, "child", "w")
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Run.ElapsedMillis = 600
		s.Run.TreeBudget = TreeBudget{MaxSeconds: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(child, func(s *Snapshot) error { s.Run.ElapsedMillis = 600; return nil }); err != nil {
		t.Fatal(err)
	}
	_, err := store.Update(child, func(s *Snapshot) error { return a.admitTreeRequestLocked(s, ModelRequest{ID: "new"}) })
	var budget *TreeBudgetError
	if !errors.As(err, &budget) {
		t.Fatalf("tree time ignored descendant: %v", err)
	}
	price := &ModelPrice{Model: "fixture", InputPerMillion: "1", OutputPerMillion: "1", Currency: "USD", Source: "test", EffectiveAt: "2026-10-29"}
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Run.TreeBudget = TreeBudget{MaxCost: "0.00005", Currency: "USD"}
		s.Requests = []ModelRequest{{ID: "root-request", Price: price, ProviderModel: "fixture"}}
		s.Responses = []ResponseRecord{{RequestID: "root-request", Response: ModelResponse{Usage: &TokenUsage{InputTokens: int64Pointer(20), OutputTokens: int64Pointer(20), TotalTokens: int64Pointer(40)}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(child, func(s *Snapshot) error {
		return a.admitTreeRequestLocked(s, ModelRequest{ID: "child-new", ProviderModel: "fixture", Price: price, ContextTokens: 10, MaxOutputTokens: 10})
	})
	if !errors.As(err, &budget) {
		t.Fatalf("tree cost did not combine parent and child: %v", err)
	}
}

func TestTreeBudgetLegacyRepeatedLeaseEventsCountIdentityOnce(t *testing.T) {
	task := PlanTask{ID: "task", WorkerID: "worker", Status: "running"}
	s := &Snapshot{Run: Run{ID: "root"}, Plan: PlanState{Tasks: []PlanTask{task}}, Workers: []AgentWorker{{ID: "worker", TaskID: "task", RunID: "child"}}}
	for i := 0; i < 20; i++ {
		s.Events = append(s.Events, Event{Sequence: i + 1, Type: "task_claimed", Data: task})
	}
	u, err := collectTreeUsage([]*Snapshot{s}, "")
	if err != nil || u.workers != 1 {
		t.Fatalf("legacy lease events counted as spawns: %d %v", u.workers, err)
	}
	s.Events = append(s.Events, Event{Sequence: 21, Type: "task_claimed", Data: PlanTask{ID: "task", WorkerID: "second-attempt"}})
	u, err = collectTreeUsage([]*Snapshot{s}, "")
	if err != nil || u.workers != 2 {
		t.Fatalf("new identity not counted: %d %v", u.workers, err)
	}
}
