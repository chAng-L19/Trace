package agent

import (
	"errors"
	"fmt"
	"time"

	"trace/internal/worker"
)

// RecoverExecResources includes hidden runs: hiding history is not proof that
// an execution owner exited. Every observation requires the original Run lock.
func (a *Agent) RecoverExecResources() error {
	runs, err := a.store.ListWithDeleted()
	if err != nil {
		return err
	}
	var result error
	for _, run := range runs {
		release, err := a.store.AcquireRunLock(run.ID)
		if err != nil {
			if !errors.Is(err, ErrRunLockBusy) {
				result = errors.Join(result, fmt.Errorf("recover exec resources %s: %w", run.ID, err))
			}
			continue
		}
		_, err = a.reconcileExecResourcesLocked(run.ID, worker.ReconcileResourceOwner)
		release()
		result = errors.Join(result, err)
	}
	return result
}

type execResourceObservation struct {
	request, call, owner, root, rootIdentity string
	err                                      error
}

// The caller holds this Run's execution lock throughout observation and commit.
func (a *Agent) reconcileExecResourcesLocked(id string, cleanup func(string, string, string, string) error) (Snapshot, error) {
	return a.reconcileExecResourcesWithIdentityLocked(id, worker.ResourceRootIdentity, cleanup)
}

func (a *Agent) reconcileExecResourcesWithIdentityLocked(id string, rootIdentity func(string) (string, error), cleanup func(string, string, string, string) error) (Snapshot, error) {
	snap, err := a.store.Load(id)
	if err != nil {
		return Snapshot{}, err
	}
	var observed []execResourceObservation
	missingResult := false
	for _, action := range snap.Actions {
		if execResultMissing(action) {
			missingResult = true
		}
		if !execResourceUnresolved(action) {
			continue
		}
		var cleanupErr error
		actual, identityErr := rootIdentity(action.ResourceRoot)
		if action.ResourceOwner != worker.ResourceOwner(id, action.RequestID, action.CallID) {
			cleanupErr = fmt.Errorf("resource owner identity mismatch")
		} else if identityErr != nil || action.ResourceRootIdentity == "" || actual != action.ResourceRootIdentity {
			cleanupErr = fmt.Errorf("persisted resource root unavailable or identity changed; cleanup remains uncertain: %v", identityErr)
		} else {
			cleanupErr = cleanup(action.ResourceRoot, id, action.RequestID, action.CallID)
		}
		observed = append(observed, execResourceObservation{action.RequestID, action.CallID, action.ResourceOwner, action.ResourceRoot, action.ResourceRootIdentity, cleanupErr})
	}
	if len(observed) == 0 && !missingResult && (snap.WorkerSettlement == nil || snap.WorkerSettlement.CleanupVersion != 1 || snap.WorkerSettlement.CleanupStatus == "completed" && !snap.WorkerSettlement.DescendantCleanupPending) {
		return snap, nil
	}
	return a.commitExecResourceObservations(id, observed)
}

func execResultMissing(action Action) bool {
	return action.Adapter == "exec" && action.Status == "started" && action.Result == nil
}

func execResourceUnresolved(action Action) bool {
	if action.Adapter != "exec" || action.ResourceOwner == "" || action.ResourceCleanupStatus == "completed" {
		return false
	}
	if action.Status != "started" && action.Status != "uncertain" && action.Status != "resolved_manually" {
		return false
	}
	return action.Result == nil || action.Result.WorkerCleanupError != ""
}

// This narrow writer deliberately bypasses Update's Deleted guard only for
// cleanup or missing-result observations. It never undeletes a Run or accepts an arbitrary edit.
func (a *Agent) commitExecResourceObservations(id string, observed []execResourceObservation) (Snapshot, error) {
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	snap, err := s.loadLocked(id)
	if err != nil {
		return Snapshot{}, err
	}
	changed, unresolved := false, false
	for i := range snap.Actions {
		action := &snap.Actions[i]
		if !execResultMissing(*action) {
			continue
		}
		action.Status = "uncertain"
		changed, unresolved = true, true
		// The run lock excludes a current Trace executor, not an escaped process.
		// Event time is the recovery observation; FinishedAt stays unknown.
		addEvent(&snap, "action_result_missing", "恢复时未找到工具持久结果；保留未知状态并交回模型处理，未重放", map[string]string{"request_id": action.RequestID, "call_id": action.CallID, "previous_status": "started", "cleanup_status": action.ResourceCleanupStatus})
	}
	for _, observation := range observed {
		for i := range snap.Actions {
			action := &snap.Actions[i]
			if action.RequestID != observation.request || action.CallID != observation.call || action.ResourceOwner != observation.owner || action.ResourceRoot != observation.root || action.ResourceRootIdentity != observation.rootIdentity || !execResourceUnresolved(*action) {
				continue
			}
			if action.Status != "resolved_manually" {
				action.Status = "uncertain"
				unresolved = true
			}
			action.ResourceCleanupStatus, action.ResourceCleanupError = "completed", ""
			if observation.err != nil {
				action.ResourceCleanupStatus, action.ResourceCleanupError = "uncertain", observation.err.Error()
				unresolved = true
			}
			changed = true
			addEvent(&snap, "exec_resource_reconciled", "执行资源已核对；业务观测保持原状态，未重放", map[string]string{"request_id": observation.request, "call_id": observation.call, "resource_owner": observation.owner, "cleanup_status": action.ResourceCleanupStatus, "cleanup_error": action.ResourceCleanupError})
		}
	}
	if unresolved && snap.Run.Status == StatusCompleted {
		// Cleanup is not business completion. Leave explicit control pauses intact;
		// pending calls are delivered by the existing model loop without replay.
		snap.Run.Status = StatusWaitingTool
	}
	changed = a.refreshWorkerCleanupReceiptLocked(&snap) || changed
	if !changed {
		return snap, nil
	}
	snap.Run.UpdatedAt = time.Now().UTC()
	s.refreshDerivedLocked(&snap, map[string]bool{id: true})
	if err := s.saveLocked(snap); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}
