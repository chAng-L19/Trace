package agent

// Sources: Muteki c60c8fee coordinator_schedule_dispatch.py:30-45,89-140,185-201
// (AGPL-3.0). Trace keeps its own Store transaction and explicit resource keys;
// missing prerequisites never create a child, and storage failures are not gaps.
import (
	"errors"
	"fmt"
	"os"
	"slices"
)

// ARTEX community 6997a466 task_control.go keeps terminal lifecycle changes
// behind explicit admission. Metadata edits never implicitly reopen execution.
func (a *Agent) checkTaskAdmissionLocked(s *Snapshot) error {
	if workerParentStopped(s.Run) {
		return fmt.Errorf("parent run is stopped: %s", s.Run.Status)
	}
	return a.checkWorkerAncestors(s)
}

type TaskBlockedError struct {
	TaskID          string   `json:"task_id"`
	MissingToolIDs  []string `json:"missing_tool_ids,omitempty"`
	MissingAssetIDs []string `json:"missing_asset_ids,omitempty"`
}

func (e *TaskBlockedError) Error() string {
	return fmt.Sprintf("task %s blocked: required tools %v; required assets %v", e.TaskID, e.MissingToolIDs, e.MissingAssetIDs)
}

type ResourceBusyError struct{ TaskID, OwnerTaskID, Key string }

func (e *ResourceBusyError) Error() string {
	return fmt.Sprintf("task %s waits for resource %s held by %s", e.TaskID, e.Key, e.OwnerTaskID)
}

func sameTaskSchedulingContract(a, b PlanTask) bool {
	if !slices.Equal(a.ContextEvidenceIDs, b.ContextEvidenceIDs) {
		return false
	}
	return a.EstimatedSeconds == b.EstimatedSeconds && a.ExpectedProgress == b.ExpectedProgress &&
		slices.Equal(a.HandoffTaskIDs, b.HandoffTaskIDs) && slices.Equal(a.ResourceKeys, b.ResourceKeys) &&
		slices.Equal(a.RequiredToolIDs, b.RequiredToolIDs) &&
		slices.Equal(a.RequiredAssetIDs, b.RequiredAssetIDs) &&
		slices.Equal(a.ExpectedArtifacts, b.ExpectedArtifacts)
}

func taskOwnsResources(s *Snapshot, task PlanTask) bool {
	if task.Status == "running" || task.Status == "uncertain" {
		return true
	}
	for _, w := range s.Workers {
		if w.TaskID == task.ID && (w.Status == "running" || w.CleanupStatus != "completed") {
			return true
		}
	}
	return false
}

// Explicit write keys apply throughout the worker tree, including ancestors.
// A running owner is not implicitly made read-only by delegating another task.
func (a *Agent) taskResourcesLocked(s *Snapshot, candidate PlanTask) error {
	if len(candidate.ResourceKeys) == 0 {
		return nil
	}
	root, _, err := a.resolveTreeRootLocked(s)
	if err != nil {
		return err
	}
	nodes, err := a.workerTreeLocked(root, s)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		for _, owner := range node.Plan.Tasks {
			if node.Run.ID == s.Run.ID && owner.ID == candidate.ID || !taskOwnsResources(node, owner) {
				continue
			}
			for _, key := range candidate.ResourceKeys {
				if hasString(owner.ResourceKeys, key) {
					return &ResourceBusyError{candidate.ID, node.Run.ID + "/" + owner.ID, key}
				}
			}
		}
	}
	return nil
}

// Must run under Store.Update; loadLocked avoids a second lock acquisition.
func (a *Agent) taskPrerequisitesLocked(s *Snapshot, task PlanTask) (*TaskBlockedError, error) {
	gap := &TaskBlockedError{TaskID: task.ID}
	for _, id := range task.RequiredToolIDs {
		if _, ok := a.registry.Lookup(id); !ok {
			gap.MissingToolIDs = append(gap.MissingToolIDs, id)
		}
	}
	for _, id := range task.RequiredAssetIDs {
		asset := contextAsset(s, id)
		if asset == nil || asset.Scope == "excluded" {
			gap.MissingAssetIDs = append(gap.MissingAssetIDs, id)
			continue
		}
		if asset.SourceRunID == "" || asset.SourceRunID == s.Run.ID {
			continue
		}
		registered := false
		for _, w := range s.Workers {
			registered = registered || w.RunID == asset.SourceRunID
		}
		if !registered {
			gap.MissingAssetIDs = append(gap.MissingAssetIDs, id)
			continue
		}
		source, err := a.store.loadLocked(asset.SourceRunID)
		if errors.Is(err, os.ErrNotExist) {
			gap.MissingAssetIDs = append(gap.MissingAssetIDs, id)
			continue
		}
		if err != nil {
			return nil, err
		}
		if source.Run.Deleted || source.Run.ParentRunID != s.Run.ID {
			gap.MissingAssetIDs = append(gap.MissingAssetIDs, id)
		}
	}
	if len(gap.MissingToolIDs)+len(gap.MissingAssetIDs) == 0 {
		return nil, nil
	}
	return gap, nil
}
