package agent

import (
	"fmt"
	"slices"
)

type executorOwner struct {
	RunID       string `json:"run_id"`
	ParentRunID string `json:"parent_run_id"`
	TaskID      string `json:"task_id"`
	WorkerID    string `json:"worker_id"`
	Visit       uint64 `json:"visit"`
}

// Called only inside the Action's Store.Update, never around external I/O.
func (a *Agent) executorOwnershipLocked(s *Snapshot) ([]executorOwner, error) {
	if s.Run.ParentTaskID == "" {
		if a.workerRunID != "" || a.workerVisit != 0 {
			return nil, fmt.Errorf("executor worker lifecycle does not own run %s", s.Run.ID)
		}
		for _, task := range s.Plan.Tasks {
			if task.ChildRunID == "" && task.Status == "running" {
				if err := a.executorContinuationSourcesLocked(s, task, s.Run.ID); err != nil {
					return nil, err
				}
			}
		}
		return nil, nil
	}
	if a.workerRunID != s.Run.ID || a.workerVisit == 0 {
		return nil, fmt.Errorf("executor requires the current worker lifecycle identity")
	}
	var owners []executorOwner
	seen := map[string]bool{s.Run.ID: true}
	child := s
	for child.Run.ParentTaskID != "" {
		manifest := child.WorkerContext
		if manifest == nil || manifest.WorkerID == "" || manifest.RunID != child.Run.ID || manifest.ParentRunID != child.Run.ParentRunID || manifest.TaskID != child.Run.ParentTaskID {
			return nil, fmt.Errorf("executor worker context does not match run %s", child.Run.ID)
		}
		if seen[child.Run.ParentRunID] {
			return nil, fmt.Errorf("executor worker ancestry contains a cycle")
		}
		seen[child.Run.ParentRunID] = true
		parent, err := a.store.loadRawLocked(child.Run.ParentRunID)
		if err != nil {
			return nil, fmt.Errorf("load executor ancestor: %w", err)
		}
		if workerParentStopped(parent.Run) || child.Run.Deleted {
			return nil, fmt.Errorf("executor worker ancestor or run %s is stopped", child.Run.ID)
		}
		var current AgentWorker
		for _, w := range parent.Workers {
			if w.ID == manifest.WorkerID && w.RunID == child.Run.ID && w.TaskID == child.Run.ParentTaskID {
				current = w
				break
			}
		}
		visit := current.Visit
		if child == s {
			visit = a.workerVisit
		}
		_, active := localWorkerOwner(&parent, child, manifest.WorkerID, visit)
		if !active {
			task, err := planTask(&parent, child.Run.ParentTaskID)
			if err != nil || !taskDone(*task) || task.WorkerID != manifest.WorkerID || task.ChildRunID != child.Run.ID || current.Visit != visit || !reportBusinessContinuing(parent.Run.ID, current, child) {
				return nil, fmt.Errorf("executor worker visit or ownership changed for run %s", child.Run.ID)
			}
		}
		parentTask, err := planTask(&parent, child.Run.ParentTaskID)
		if err != nil {
			return nil, err
		}
		if err := a.executorContinuationSourcesLocked(&parent, *parentTask, s.Run.ID); err != nil {
			return nil, err
		}
		for _, task := range child.Plan.Tasks {
			if task.LocalExecution && (task.Status == "running" || task.Status == "uncertain") {
				if err := a.executorContinuationSourcesLocked(child, task, s.Run.ID); err != nil {
					return nil, err
				}
			}
			if task.LocalExecution && (task.Status == "running" || task.Status == "uncertain") && (task.WorkerID != manifest.WorkerID || task.LocalWorkerVisit != visit) {
				return nil, fmt.Errorf("executor local task %s belongs to a previous worker visit", task.ID)
			}
		}
		owners = append(owners, executorOwner{child.Run.ID, parent.Run.ID, child.Run.ParentTaskID, manifest.WorkerID, visit})
		child = &parent
	}
	return owners, nil
}

func (a *Agent) revalidateExecutorOwnershipLocked(s *Snapshot, action *Action, admitted []executorOwner, result *ToolResult) {
	current, err := a.executorOwnershipLocked(s)
	if err == nil && !slices.Equal(admitted, current) {
		err = fmt.Errorf("executor worker ancestry changed during execution")
	}
	if err == nil {
		return
	}
	// A local fence cannot recall a sent request. Keep its raw receipt and
	// record the ownership hold independently of process cleanup.
	result.OutcomeUncertain = true
	action.ExecutorOwnershipError = "executor ownership changed; in-flight external effects require verification: " + err.Error()
	addEvent(s, "action_executor_changed", "Worker 执行代次已变化；保留外部观察与占用，待核对", map[string]any{
		"call_id": action.CallID, "request_id": action.RequestID, "owners": admitted, "error": err.Error(),
	})
}
