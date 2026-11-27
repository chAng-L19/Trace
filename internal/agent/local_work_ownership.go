package agent

import (
	"encoding/json"
	"fmt"
	"time"
)

// New Actions pin the local Plan task. Legacy Actions use its claim interval;
// an unknown interval remains conservative without counting control Actions.
func localWorkSourceUnresolved(s *Snapshot, task PlanTask, successor ...string) bool {
	if s == nil || s.Run.Deleted {
		return true
	}
	var claimed, finished time.Time
	for _, event := range s.Events {
		if event.Type != "task_claimed" && event.Type != "task_completed" {
			continue
		}
		raw, err := json.Marshal(event.Data)
		var recorded PlanTask
		if err != nil || json.Unmarshal(raw, &recorded) != nil || recorded.ID != task.ID {
			continue
		}
		if event.Type == "task_claimed" {
			claimed, finished = event.CreatedAt, time.Time{}
		}
		if event.Type == "task_completed" && !claimed.IsZero() {
			finished = event.CreatedAt
		}
	}
	for _, action := range s.Actions {
		if task.ID == rootWorkTaskID && len(successor) == 1 && action.PlanTaskID == successor[0] && (action.Status == "started" || action.Status == "result_ready") && action.ExecutorOwnershipError == "" {
			current, err := planTask(s, action.PlanTaskID)
			if err == nil && current.Status == "running" && current.ChildRunID == "" {
				continuing := false
				for _, ref := range current.Continues {
					continuing = continuing || ref.TaskID == rootWorkTaskID
				}
				if continuing {
					continue
				}
			}
		}
		if action.Adapter == "builtin" && (coordinatorBuiltin(action.Name) || action.Name == "task_claim" || action.Name == "task_complete") {
			continue
		}
		if task.ID != rootWorkTaskID && action.PlanTaskID != "" && action.PlanTaskID != task.ID {
			continue
		}
		if action.PlanTaskID == "" && !claimed.IsZero() && !action.StartedAt.IsZero() &&
			(action.StartedAt.Before(claimed) || !finished.IsZero() && action.StartedAt.After(finished)) {
			continue
		}
		if action.Status == "started" || action.Status == "uncertain" || action.Status == "result_ready" || toolCleanupError(Snapshot{Actions: []Action{action}}) != nil {
			return true
		}
	}
	return false
}

// Verify only referenced original sources under the Action's existing Store
// fence. Loading the whole parent here would recurse into the active child.
func (a *Agent) executorContinuationSourcesLocked(parent *Snapshot, task PlanTask, activeRunID string) error {
	for _, ref := range task.Continues {
		sourceTask, err := continuationSourceTask(parent, ref)
		if err != nil {
			return err
		}
		source := parent
		if sourceTask.ChildRunID != "" {
			loaded, err := a.store.verifySourceLocked(sourceTask.ChildRunID, map[string]bool{parent.Run.ID: true, activeRunID: true}, readSourceOnly)
			if err != nil {
				return fmt.Errorf("verify original continuation source: %w", err)
			}
			source = &loaded
		}
		if sourceTask.ChildRunID == "" && sourceTask.ID != rootWorkTaskID && taskDone(sourceTask) {
			if !taskScopeMatches(source, sourceTask) || sourceTask.Status == "verified" &&
				(!evidenceSupports(source, sourceTask.EvidenceIDs, sourceTask.SuccessContains) || !taskRequirementsSatisfied(source, sourceTask)) {
				return fmt.Errorf("continuation %s/%s original local evidence changed before Action", ref.TaskID, ref.ItemID)
			}
		}
		if !continuationSourceReady(parent, sourceTask, source, task.ID) {
			return fmt.Errorf("continuation %s/%s source changed before Action; inspect original effects", ref.TaskID, ref.ItemID)
		}
	}
	return nil
}
