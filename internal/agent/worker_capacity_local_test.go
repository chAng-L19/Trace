package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTreeWorkerCapacityLocalClaimIdentityAndContracts(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			root := createTestRun(t, a)
			if _, err := a.SetPlan(root, capacityTasks(1)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.SetPlanCapacity(root, 1); err != nil {
				t.Fatal(err)
			}
			local, w := capacityFixtureChild(t, a, root, "0")
			if _, err := store.Update(root, func(s *Snapshot) error {
				s.Run.TreeBudget = TreeBudget{MaxWorkers: 1, MaxDepth: 1}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			forged := capacityTasks(1)
			forged[0].LocalExecution, forged[0].LocalWorkerVisit = true, w.Visit
			if _, err := a.SetPlan(w.RunID, forged); err == nil {
				t.Fatal("plan input forged a local slot exemption")
			}
			if _, err := a.SetPlan(w.RunID, capacityTasks(2)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ClaimTask(w.RunID, "0", "manual-owner", 3600); !errors.Is(err, errWorkerCapacity) {
				t.Fatalf("unrelated manual claim borrowed current worker slot: %v", err)
			}
			if _, err := a.ClaimTask(w.RunID, "0", w.ID, 3600); err == nil {
				t.Fatal("non-owner Agent borrowed live worker identity")
			}
			if _, err := store.Update(w.RunID, func(s *Snapshot) error {
				s.Run.ModelSelection = &ModelSelection{Provider: "different-provider", Model: "different-model"}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := local.ClaimTask(w.RunID, "0", w.ID, 3600); err == nil || !strings.Contains(err.Error(), "model route") {
				t.Fatalf("local work silently changed model route: %v", err)
			}
			if _, err := store.Update(w.RunID, func(s *Snapshot) error { s.Run.ModelSelection = nil; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := local.ClaimTask(w.RunID, "0", w.ID, 3600); err != nil {
				t.Fatalf("local work needed another slot or cumulative admission: %v", err)
			}
			if _, err := store.Update(w.RunID, func(s *Snapshot) error {
				s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Hour)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.RecoverWorkers(w.RunID); err != nil {
				t.Fatal(err)
			}
			if _, err := local.ClaimTask(w.RunID, "1", w.ID, 3600); err == nil {
				t.Fatal("multiple local tasks borrowed one executor concurrently")
			}
			state, err := a.WorkerCapacity(w.RunID)
			if err != nil || state.Active != 1 {
				t.Fatalf("local work double-counted executor: %+v %v", state, err)
			}
			if _, err := local.CompleteTask(w.RunID, "0", w.ID, "completed", nil); err != nil {
				t.Fatal(err)
			}
			if _, err := local.ClaimTask(w.RunID, "1", w.ID, 3600); err != nil {
				t.Fatalf("completed local task did not release its local claim: %v", err)
			}
			if _, err := store.Update(root, func(s *Snapshot) error { s.Workers[0].Visit++; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := local.ClaimTask(w.RunID, "1", w.ID, 3600); err == nil {
				t.Fatal("old worker visit renewed local execution")
			}
			if _, err := local.CompleteTask(w.RunID, "1", w.ID, "completed", nil); err == nil {
				t.Fatal("old worker visit completed local execution")
			}
			state, err = a.WorkerCapacity(root)
			if err != nil || state.Active != 2 {
				t.Fatalf("stale local claim lost its unresolved reservation: %+v %v", state, err)
			}
			if _, err := store.Update(root, func(s *Snapshot) error {
				s.Workers[0].Visit = w.Visit
				s.Workers[0].Status, s.Workers[0].CleanupStatus = "uncertain", "unknown"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := local.ClaimTask(w.RunID, "1", w.ID, 3600); err == nil {
				t.Fatal("unknown cleanup lent a local slot")
			}
		})
	}
}

func TestTreeWorkerCapacityLocalPauseResumesSameTaskAtCapacity(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			root := createTestRun(t, a)
			if _, err := a.SetPlan(root, capacityTasks(1)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.SetPlanCapacity(root, 1); err != nil {
				t.Fatal(err)
			}
			local, w := capacityFixtureChild(t, a, root, "0")
			if _, err := a.SetPlan(w.RunID, capacityTasks(1)); err != nil {
				t.Fatal(err)
			}
			if _, err := local.ClaimTask(w.RunID, "0", w.ID, 30); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(w.RunID, func(s *Snapshot) error {
				s.Run.Status, s.Run.PauseReason = StatusPaused, "parent_pause"
				s.WorkerSettlement = &WorkerSettlement{ParentRunID: root, TaskID: w.TaskID, WorkerID: w.ID, RunID: w.RunID, Visit: w.Visit, CleanupStatus: "completed", ObservedAt: time.Now().UTC()}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.RecoverWorkers(root); err != nil {
				t.Fatal(err)
			}
			state, err := a.WorkerCapacity(root)
			if err != nil || state.Active != 1 {
				t.Fatalf("unfinished local task released its reservation: %+v %v", state, err)
			}
			child, err := store.Load(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			unlock, err := store.AcquireRunLock(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			_, release, resumed, err := a.prepareWorkerResume(context.Background(), child)
			if err != nil {
				t.Fatalf("retained local reservation blocked same worker: %v", err)
			}
			defer release()
			if err := a.resumeLocalTaskVisit(w.RunID, resumed); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(w.RunID, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := local.CompleteTask(w.RunID, "0", w.ID, "completed", nil); err == nil {
				t.Fatal("previous executor settled resumed task")
			}
			local.workerVisit = resumed.Visit
			if _, err := local.CompleteTask(w.RunID, "0", w.ID, "completed", nil); err != nil {
				t.Fatal(err)
			}
			state, err = a.WorkerCapacity(root)
			if err != nil || state.Active != 1 {
				t.Fatalf("resumed worker lost its original slot: %+v %v", state, err)
			}
		})
	}
}

type capacityNestedProvider struct {
	a        *Agent
	root     string
	path     string
	entered  chan struct{}
	start    chan struct{}
	allBusy  chan struct{}
	rejected atomic.Int32
	peak     atomic.Int32
}

func (p *capacityNestedProvider) Complete(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	var manifest WorkerContext
	seen := map[string]ToolResult{}
	for _, m := range req.Messages {
		if strings.HasPrefix(m.Content, "Worker task context (") {
			_, raw, _ := strings.Cut(m.Content, "\n")
			if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
				return ModelResponse{}, err
			}
		}
		if m.Role == "tool" {
			var result ToolResult
			if err := json.Unmarshal([]byte(m.Content), &result); err != nil {
				return ModelResponse{}, err
			}
			seen[m.ToolCallID] = result
			if result.Error != "" && m.ToolCallID != "nested-dispatch" {
				return ModelResponse{}, fmt.Errorf("local progress tool %s: %s", m.ToolCallID, result.Error)
			}
		}
	}
	if manifest.WorkerID == "" {
		return ModelResponse{}, fmt.Errorf("nested worker context missing")
	}
	state, err := p.a.WorkerCapacity(p.root)
	if err != nil {
		return ModelResponse{}, err
	}
	for old := p.peak.Load(); int32(state.Active) > old; old = p.peak.Load() {
		if p.peak.CompareAndSwap(old, int32(state.Active)) {
			break
		}
	}
	if state.Active > state.Capacity {
		return ModelResponse{}, fmt.Errorf("nested execution exceeded tree capacity: %+v", state)
	}
	call := func(id, name string, args any) (ModelResponse, error) {
		raw, err := json.Marshal(args)
		return ModelResponse{ToolCalls: []ToolCall{{ID: id, Name: name, Arguments: string(raw)}}}, err
	}
	if _, ok := seen["local-plan"]; !ok {
		p.entered <- struct{}{}
		select {
		case <-p.start:
		case <-ctx.Done():
			return ModelResponse{}, ctx.Err()
		}
		return call("local-plan", "plan", map[string]any{"tasks": []PlanTask{{ID: "nested", Goal: "read local proof", SuccessContains: "local-capacity-proof"}}})
	}
	if _, ok := seen["nested-dispatch"]; !ok {
		return call("nested-dispatch", "worker_dispatch", map[string]string{"task_id": "nested"})
	}
	if _, ok := seen["local-wait"]; !ok {
		failure := seen["nested-dispatch"].Error
		if !strings.Contains(failure, "worker capacity reached") || !strings.Contains(failure, manifest.WorkerID) {
			return ModelResponse{}, fmt.Errorf("missing capacity/local-progress response: %s", failure)
		}
		if p.rejected.Add(1) == 5 {
			close(p.allBusy)
		}
		select {
		case <-p.allBusy:
		case <-ctx.Done():
			return ModelResponse{}, ctx.Err()
		}
		// No own child started: this must return immediately, not wait 300s.
		return call("local-wait", "worker_wait", map[string]int{"seconds": 300})
	}
	if _, ok := seen["local-claim"]; !ok {
		return call("local-claim", "task_claim", map[string]any{"task_id": "nested", "worker_id": manifest.WorkerID, "lease_seconds": 3600})
	}
	if _, ok := seen["local-read"]; !ok {
		return call("local-read", "read", FileRequest{Path: p.path})
	}
	evidence := seen["local-read"].EvidenceID
	if evidence == "" {
		return ModelResponse{}, fmt.Errorf("local read did not produce execution evidence")
	}
	if _, ok := seen["local-complete"]; !ok {
		return call("local-complete", "task_complete", map[string]any{"task_id": "nested", "worker_id": manifest.WorkerID, "status": "verified", "evidence_ids": []string{evidence}})
	}
	raw, _ := json.Marshal(map[string]any{"trace_result": Result{Status: "success", EvidenceIDs: []string{evidence}, Answers: []Answer{{Value: "local-capacity-proof", EvidenceIDs: []string{evidence}}}}})
	return ModelResponse{Content: fixtureReport(string(raw))}, nil
}

func TestTreeWorkerCapacityFiveNestedWorkersProgressLocally(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			p := &capacityNestedProvider{entered: make(chan struct{}, 5), start: make(chan struct{}), allBusy: make(chan struct{})}
			a, store := treeCapacityAgent(t, backend, p)
			p.a, p.root, p.path = a, createTestRun(t, a), filepath.Join(t.TempDir(), "proof.txt")
			if err := os.WriteFile(p.path, []byte("local-capacity-proof"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := a.SetPlan(p.root, capacityTasks(5)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			defer a.StopWorkers(context.Background())
			workers, err := a.DispatchFrontier(ctx, p.root)
			if err != nil || len(workers) != 5 {
				t.Fatalf("five-child startup: %d %v", len(workers), err)
			}
			for range 5 {
				select {
				case <-p.entered:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			close(p.start)
			parent, err := waitAllTestWorkers(a, ctx, p.root, 90)
			if err != nil {
				t.Fatal(err)
			}
			if p.rejected.Load() != 5 || p.peak.Load() != 5 {
				t.Fatalf("saturation proof: rejected=%d peak=%d", p.rejected.Load(), p.peak.Load())
			}
			for _, w := range parent.Workers {
				child, err := store.Load(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if w.Status != "completed" || w.CleanupStatus != "completed" || child.Run.Status != StatusCompleted || child.Result.Status != "success" || len(child.Workers) != 0 {
					t.Fatalf("nested local completion failed: worker=%+v run=%+v result=%+v", w, child.Run, child.Result)
				}
				if len(child.Plan.Tasks) != 1 || child.Plan.Tasks[0].Status != "verified" || !child.Plan.Tasks[0].LocalExecution || child.Plan.Tasks[0].ChildRunID != "" {
					t.Fatalf("local plan not legally settled: %+v", child.Plan)
				}
			}
			state, err := a.WorkerCapacity(p.root)
			if err != nil || state.Active != 0 {
				t.Fatalf("settled tree leaked capacity: %+v %v", state, err)
			}
		})
	}
}

func TestOccupiedWorkerSlotsRetainsUncertainWorkerAfterCleanup(t *testing.T) {
	s := Snapshot{
		Workers: []AgentWorker{{ID: "w", RunID: "child", TaskID: "task", Status: "uncertain", CleanupStatus: "completed"}},
		Plan:    PlanState{Tasks: []PlanTask{{ID: "task", Status: "uncertain", WorkerID: "w", ChildRunID: "child"}}},
	}
	if got := occupiedWorkerSlots(&s); got != 1 {
		t.Fatalf("uncertain worker released capacity after cleanup: got %d", got)
	}
	s.Workers[0].Status = "completed"
	if got := occupiedWorkerSlots(&s); got != 1 {
		t.Fatalf("uncertain task was incorrectly deduplicated against settled worker: got %d", got)
	}
	s.Workers[0].Status = "uncertain"
	s.Plan.Tasks[0].Status = "abandoned"
	s.Plan.Tasks[0].DispositionReason = "inspected and retired"
	if got := occupiedWorkerSlots(&s); got != 0 {
		t.Fatalf("disposed worker retained capacity: got %d", got)
	}
	for _, status := range []string{"running", "starting", "cancelling"} {
		s.Workers[0].Status = status
		if got := occupiedWorkerSlots(&s); got != 1 {
			t.Fatalf("disposition erased %s execution: %d", status, got)
		}
	}
	s.Workers[0].Status, s.Workers[0].CleanupStatus = "uncertain", "unknown"
	if got := occupiedWorkerSlots(&s); got != 1 {
		t.Fatalf("disposition erased unknown cleanup: %d", got)
	}
}
