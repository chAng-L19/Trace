package agent

// References: Harness subagent/activation.ts ActivationPool + manager.rootPool,
// and DSH executor/dsh.ts WorkerSlots: share a root pool and reserve before
// startup. Trace uses its durable claims under the existing Store transaction.
import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type WorkerCapacityState struct {
	RootRunID string `json:"root_run_id"`
	Active    int    `json:"active"`
	Capacity  int    `json:"capacity"`
}

type WorkerCapacityError struct {
	WorkerCapacityState
	LocalWorkerID string `json:"local_worker_id,omitempty"`
	NextAction    string `json:"next_action"`
}

func (e *WorkerCapacityError) Error() string {
	data, _ := json.Marshal(e)
	return errWorkerCapacity.Error() + ": " + string(data)
}

func (e *WorkerCapacityError) Unwrap() error { return errWorkerCapacity }

func sameWorkerModel(a, b *ModelSelection) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// WorkerCapacity is a read-only tree projection; it does not reconcile workers
// or release unknown claims. The same Store lock serializes admission writes.
func (a *Agent) WorkerCapacity(id string) (WorkerCapacityState, error) {
	return a.WorkerCapacityContext(context.Background(), id)
}

// WorkerCapacityContext reads only scheduling and cleanup fields. Its detached
// projection is never passed to admission, reconciliation, or persistence.
func (a *Agent) WorkerCapacityContext(ctx context.Context, id string) (WorkerCapacityState, error) {
	// Share cold display admission before acquiring the Store fence. SQLite's
	// row-owned BLOB must not overlap another large display decode in this Store.
	if err := a.store.lockDisplayRead(ctx); err != nil {
		return WorkerCapacityState{}, err
	}
	defer a.store.activityReadMu.Unlock()
	unlock, err := a.store.acquireStoreLockContext(ctx)
	if err != nil {
		return WorkerCapacityState{}, err
	}
	defer unlock()
	root, nodes, err := a.store.capacityTreeLocked(ctx, id)
	if err != nil {
		return WorkerCapacityState{}, err
	}
	state, err := workerCapacityState(root)
	if err != nil {
		return state, err
	}
	if err = ctx.Err(); err != nil {
		return WorkerCapacityState{}, err
	}
	state = countWorkerCapacity(state, nodes)
	if err = ctx.Err(); err != nil {
		return WorkerCapacityState{}, err
	}
	return state, nil
}

func (a *Agent) workerCapacityLocked(current *Snapshot) (WorkerCapacityState, error) {
	root, _, err := a.resolveTreeRootLocked(current)
	if err != nil {
		return WorkerCapacityState{}, err
	}
	state, err := workerCapacityState(root)
	if err != nil {
		return state, err
	}
	nodes, err := a.workerTreeLocked(root, current)
	if err != nil {
		return state, err
	}
	return countWorkerCapacity(state, nodes), nil
}

func workerCapacityState(root *Snapshot) (WorkerCapacityState, error) {
	state := WorkerCapacityState{RootRunID: root.Run.ID, Capacity: root.Plan.Capacity}
	if state.Capacity == 0 {
		state.Capacity = 5
	}
	if state.Capacity < 1 || state.Capacity > 32 {
		return state, fmt.Errorf("root worker capacity must be 1..32")
	}
	return state, nil
}

func countWorkerCapacity(state WorkerCapacityState, nodes []*Snapshot) WorkerCapacityState {
	byID := make(map[string]*Snapshot, len(nodes))
	for _, s := range nodes {
		byID[s.Run.ID] = s
		state.Active += occupiedWorkerSlots(s)
	}
	for _, s := range nodes {
		parent := byID[s.Run.ParentRunID]
		if parent == nil {
			continue
		}
		// Deduplicate at most one proven local continuation, including the
		// settled-to-running handoff. Unknown effects retain separate slots.
		for _, task := range s.Plan.Tasks {
			if !task.LocalExecution || task.Status != "running" || task.ChildRunID != "" {
				continue
			}
			if localTaskSharesWorkerSlot(parent, s, task) {
				state.Active--
				break // Even malformed history may reuse at most one slot.
			}
		}
	}
	return state
}

func localTaskSharesWorkerSlot(parent, child *Snapshot, task PlanTask) bool {
	if _, ok := localWorkerOwner(parent, child, task.WorkerID, task.LocalWorkerVisit); ok {
		return true
	}
	owner, err := planTask(parent, child.Run.ParentTaskID)
	if err != nil || owner.WorkerID != task.WorkerID || owner.ChildRunID != child.Run.ID {
		return false
	}
	for _, w := range parent.Workers {
		if w.ID != task.WorkerID || w.RunID != child.Run.ID || w.TaskID != owner.ID || w.SettledVisit == nil {
			continue
		}
		if !workerUnsettled(parent, w) && owner.Status != "running" && owner.Status != "uncertain" {
			continue // A retired parent has no slot for a local task to reuse.
		}
		previous := w
		if w.Status == "running" && w.Visit > 1 && task.LocalWorkerVisit == w.Visit-1 {
			// Parent admitted the new visit; the child has not yet committed its
			// identity-only handoff. The old cleanup receipt proves this one slot.
			if _, ok := localWorkerOwner(parent, child, w.ID, w.Visit); !ok {
				continue
			}
			previous.Visit--
		} else if w.Status == "running" || w.Status == "starting" || w.Status == "cancelling" || w.CleanupStatus != "completed" || w.Status != "uncertain" && owner.Status != "uncertain" {
			continue
		}
		if *w.SettledVisit == previous.Visit {
			local := resumableLocalTask(child, parent.Run.ID, previous)
			if local != nil && local.ID == task.ID {
				return true
			}
		}
	}
	return false
}

func (a *Agent) localTaskActiveLocked(s *Snapshot, task PlanTask) (bool, error) {
	if !task.LocalExecution || task.Status != "running" || task.ChildRunID != "" || s.Run.ParentRunID == "" {
		return false, nil
	}
	parent, err := a.store.loadRawLocked(s.Run.ParentRunID)
	if err != nil {
		return false, err
	}
	_, active := localWorkerOwner(&parent, s, task.WorkerID, task.LocalWorkerVisit)
	return active, nil
}

// A paused executor's unfinished local task retains its reservation. After
// proven cleanup the same worker may reuse it; an uncertain task never does.
func resumableLocalTask(child *Snapshot, parentID string, w AgentWorker) *PlanTask {
	if child.Run.ParentRunID != parentID || child.Run.ParentTaskID != w.TaskID || child.WorkerContext == nil || child.WorkerContext.WorkerID != w.ID || child.WorkerContext.RunID != child.Run.ID || child.WorkerContext.ParentRunID != parentID || child.WorkerContext.TaskID != w.TaskID || !child.WorkerSettlement.matches(parentID, w) || child.WorkerSettlement.CleanupStatus != "completed" || toolCleanupError(*child) != nil {
		return nil
	}
	for _, action := range child.Actions {
		if action.Status == "started" || action.Status == "uncertain" {
			return nil
		}
	}
	var found *PlanTask
	for i := range child.Plan.Tasks {
		task := &child.Plan.Tasks[i]
		if !task.LocalExecution || task.Status != "running" && task.Status != "uncertain" {
			continue
		}
		if found != nil || task.Status != "running" || task.WorkerID != w.ID || task.LocalWorkerVisit != w.Visit || task.ChildRunID != "" {
			return nil
		}
		found = task
	}
	return found
}

// Caller owns the child's Run lock. The parent resume has already reserved its
// slot; this identity-only handoff neither completes nor replays local work.
func (a *Agent) resumeLocalTaskVisit(id string, w AgentWorker) error {
	if w.Visit < 2 {
		return nil
	}
	_, err := a.store.Update(id, func(child *Snapshot) error {
		previous := w
		previous.Visit--
		task := resumableLocalTask(child, child.Run.ParentRunID, previous)
		if task == nil {
			return nil
		}
		parent, err := a.store.loadRawLocked(child.Run.ParentRunID)
		if err != nil {
			return err
		}
		if _, ok := localWorkerOwner(&parent, child, w.ID, w.Visit); !ok {
			return fmt.Errorf("local task continuation worker ownership changed")
		}
		task.LocalWorkerVisit = w.Visit
		task.LeaseUntil = time.Now().UTC().Add(30 * time.Second)
		addEvent(child, "local_task_resumed", "原 Worker 本地任务已接入当前 visit，未重放动作", map[string]any{"task_id": task.ID, "worker_id": w.ID, "visit": w.Visit})
		return nil
	})
	return err
}

// additional=0 confirms an existing startup reservation; additional=1 admits
// a new claim/resume. Registration never counts both the claim and its worker.
func (a *Agent) checkWorkerCapacityLocked(s *Snapshot, additional int) error {
	state, err := a.workerCapacityLocked(s)
	if err != nil {
		return err
	}
	if state.Active+additional <= state.Capacity {
		return nil
	}
	blocked := &WorkerCapacityError{WorkerCapacityState: state, NextAction: "Wait for an existing worker to settle and retry; worker_wait returns immediately when this run has no active children."}
	if s.WorkerContext != nil && s.WorkerContext.WorkerID != "" {
		blocked.LocalWorkerID = s.WorkerContext.WorkerID
		blocked.NextAction += " To finish pending local plan work without spawning, task_claim one task using local_worker_id, execute it in this worker, then task_complete with the original checks/evidence. Local execution must keep the current model route. Never wait for capacity held by your own ancestors."
	}
	return blocked
}

func localWorkerOwner(parent, child *Snapshot, workerID string, visit uint64) (AgentWorker, bool) {
	if visit == 0 || parent.Run.ID != child.Run.ParentRunID || child.Run.ParentTaskID == "" || isStopped(parent.Run.Status) {
		return AgentWorker{}, false
	}
	task, err := planTask(parent, child.Run.ParentTaskID)
	if err != nil || task.Status != "running" || task.WorkerID != workerID || task.ChildRunID != child.Run.ID || !task.LeaseUntil.After(time.Now()) {
		return AgentWorker{}, false
	}
	for _, w := range parent.Workers {
		if w.ID == workerID && w.TaskID == task.ID && w.RunID == child.Run.ID && w.Visit == visit && w.Status == "running" && w.CleanupStatus == "pending" {
			return w, true
		}
	}
	return AgentWorker{}, false
}

// Only the current child lifecycle may lend its occupied slot to a local task.
// A manual caller cannot borrow another worker's identity or a previous visit.
func (a *Agent) localWorkerClaimLocked(s *Snapshot, taskID, workerID string) (bool, error) {
	if s.WorkerContext == nil || workerID != s.WorkerContext.WorkerID {
		return false, nil
	}
	manifest := s.WorkerContext
	if a.workerRunID != s.Run.ID || a.workerVisit == 0 || (s.Run.Status != StatusRunning && s.Run.Status != StatusWaitingTool) || manifest.RunID != s.Run.ID || manifest.ParentRunID != s.Run.ParentRunID || manifest.TaskID != s.Run.ParentTaskID {
		return false, fmt.Errorf("local task claim requires the current worker lifecycle identity")
	}
	parent, err := a.store.loadRawLocked(s.Run.ParentRunID)
	if err != nil {
		return false, err
	}
	if _, ok := localWorkerOwner(&parent, s, workerID, a.workerVisit); !ok {
		return false, fmt.Errorf("local task claim worker visit or ownership changed")
	}
	for _, t := range s.Plan.Tasks {
		if t.LocalExecution && (t.Status == "running" || t.Status == "uncertain") {
			if t.ID != taskID {
				return false, fmt.Errorf("finish local task %s before claiming another on the same worker", t.ID)
			}
			if t.WorkerID != workerID || t.LocalWorkerVisit != a.workerVisit {
				return false, fmt.Errorf("local task claim belongs to a previous worker visit")
			}
		}
	}
	return true, nil
}
