package agent

import (
	"fmt"
	"time"
)

// Delete hides a conversation from the archive. Keep its snapshot at the same
// path so related runs can still verify source messages, evidence and CAS data.
func (s *Store) Delete(id string) error {
	_, err := s.DeleteWithEvidence(id, true)
	return err
}

type EvidenceDeleteResult struct {
	Permanent         bool     `json:"permanent,omitempty"`
	RunIDs            []string `json:"run_ids,omitempty"`
	Deleted           bool     `json:"deleted"`
	ArtifactsDeleted  int      `json:"artifacts_deleted"`
	ArtifactsRetained int      `json:"artifacts_retained"`
	CleanupWarning    string   `json:"cleanup_warning,omitempty"`
}

func (s *Store) DeleteWithEvidence(id string, retain bool) (EvidenceDeleteResult, error) {
	return s.deleteEvidenceRecords(id, "", true, retain)
}

func (s *Store) DeleteEvidence(id, evidenceID string) (EvidenceDeleteResult, error) {
	return s.deleteEvidenceRecords(id, evidenceID, false, false)
}

// Muteki run_bindings.py (144df766, AGPL-3.0) settles owners before deletion;
// ARTEX manager.go (e6ec5699, AGPL-3.0) stages
// external files before committing metadata. Adapted independently to Trace's
// run/store locks and CAS, with no cancellation or shared-input deletion.
func (s *Store) deleteEvidenceRecords(id, evidenceID string, conversation, retain bool) (EvidenceDeleteResult, error) {
	result := EvidenceDeleteResult{}
	release, err := s.AcquireRunLock(id)
	if err != nil {
		return result, err
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return result, err
	}
	defer unlock()
	snap, err := s.loadRawLocked(id)
	if err != nil {
		return result, err
	}
	if conversation && snap.Run.ParentTaskID != "" {
		return result, fmt.Errorf("Worker 子运行请从所属会话管理")
	}
	if snap.Run.Status == StatusRunning || snap.Run.Status == StatusWaitingTool {
		return result, fmt.Errorf("请先暂停会话，等待执行结束后再删除")
	}
	if err := toolCleanupError(snap); err != nil {
		return result, fmt.Errorf("工具资源清理尚未确认，保留执行账本: %w", err)
	}
	if conversation {
		releaseChildren, err := s.checkDescendantCleanupLocked(id)
		defer releaseChildren()
		if err != nil {
			return result, err
		}
	} else {
		for _, worker := range snap.Workers {
			if worker.Status == "running" || worker.CleanupStatus != "completed" {
				return result, fmt.Errorf("请先结束 Worker 并确认清理，再删除会话")
			}
		}
	}
	if !retain {
		return s.purgeEvidenceLocked(snap, evidenceID, conversation)
	}
	snap.Run.Deleted = conversation || snap.Run.Deleted
	snap.Run.UpdatedAt = time.Now().UTC()
	err = s.saveLocked(snap)
	result.Deleted = err == nil
	return result, err
}

// Caller holds the Store lock and root Run lock. Probe descendant Run locks
// without waiting, so a running child can never disappear behind its archive.
func (s *Store) checkDescendantCleanupLocked(id string) (func(), error) {
	var releases []func()
	releaseAll := func() {
		for _, release := range releases {
			release()
		}
	}
	ids, err := s.snapshotIDsLocked()
	if err != nil {
		return releaseAll, err
	}
	all := make(map[string]Snapshot, len(ids))
	for _, rid := range ids {
		child, err := s.loadRawLocked(rid)
		if err != nil {
			return releaseAll, err
		}
		all[rid] = child
	}
	closure := map[string]bool{id: true}
	for changed := true; changed; {
		changed = false
		for rid, child := range all {
			if closure[rid] || child.Run.ParentTaskID == "" || !closure[child.Run.ParentRunID] {
				continue
			}
			release, err := s.AcquireRunLock(rid)
			if err != nil {
				return releaseAll, err
			}
			releases = append(releases, release)
			closure[rid] = true
			changed = true
		}
	}
	// Retain every descendant lock through the caller's deletion transaction.
	for rid := range closure {
		child := all[rid]
		if child.Run.Status == StatusRunning || child.Run.Status == StatusWaitingTool && !settledChildForDeletion(child, all) {
			return releaseAll, fmt.Errorf("子运行 %s 尚在执行，请先完成清理", rid)
		}
		if err := toolCleanupError(child); err != nil {
			return releaseAll, fmt.Errorf("子运行 %s 工具资源清理尚未确认: %w", rid, err)
		}
		if err := deletionWorkerCleanupError(child, all); err != nil {
			return releaseAll, err
		}
	}
	return releaseAll, nil
}

// Deleting a stopped tree does not require successful worker settlement. An
// interrupted controller may have saved the pauses but no lifecycle receipt.
// Callers hold every tree Run lock and the Store lock, and separately check
// every tool cleanup ledger. Never use this exception to complete/resume work.
func pausedWorkerForDeletion(parent Snapshot, w AgentWorker, all map[string]Snapshot) bool {
	child, ok := all[w.RunID]
	if !ok || parent.Run.Status != StatusPaused || child.Run.Status != StatusPaused || w.ID == "" || w.TaskID == "" || child.Run.ID != w.RunID || child.Run.ParentRunID != parent.Run.ID || child.Run.ParentTaskID != w.TaskID {
		return false
	}
	task, err := planTask(&parent, w.TaskID)
	if err != nil || task.WorkerID != w.ID || task.ChildRunID != w.RunID {
		return false
	}
	if c := child.WorkerContext; c != nil && (c.ParentRunID != parent.Run.ID || c.TaskID != w.TaskID || c.WorkerID != "" && c.WorkerID != w.ID || c.RunID != "" && c.RunID != child.Run.ID) {
		return false
	}
	if w.Error != "" && w.CleanupStatus != "completed" && (child.WorkerSettlement == nil || child.WorkerSettlement.Visit != w.Visit) {
		return false // An absent/currently missing receipt cannot erase a recorded failure.
	}
	if r := child.WorkerSettlement; r != nil {
		// A resumed visit retains the previous receipt. Its visit is not proof
		// of current completion, nor a reason to prevent deleting stopped work.
		previous := w
		previous.Visit = r.Visit
		if r.Visit > w.Visit || !r.matches(parent.Run.ID, previous) || r.CleanupStatus != "completed" || r.CleanupError != "" || r.LifecycleCleanupError != "" || r.ToolCleanupError != "" || r.DescendantCleanupPending {
			return false
		}
	}
	return true
}

func deletionWorkerCleanupError(snap Snapshot, all map[string]Snapshot) error {
	for _, task := range snap.Plan.Tasks {
		if task.Status != "running" {
			continue
		}
		stopped := false
		for _, w := range snap.Workers {
			if w.ID == task.WorkerID && w.TaskID == task.ID && w.RunID == task.ChildRunID && pausedWorkerForDeletion(snap, w, all) {
				stopped = true
				break
			}
		}
		if !stopped {
			return fmt.Errorf("计划 %s 仍在派发或执行，请先等待 Worker 结束", task.ID)
		}
	}
	for _, w := range snap.Workers {
		if (w.Status == "running" || w.Status == "starting" || w.Status == "cancelling" || w.CleanupStatus != "completed") && !pausedWorkerForDeletion(snap, w, all) {
			return fmt.Errorf("请先结束 Worker %s 并确认清理，再删除会话", w.ID)
		}
	}
	return nil
}

// A stopped Worker may retain waiting_tool for an uncertain business outcome.
// Deletion requires its current, identity-bound lifecycle receipt, not success.
// Callers also hold the child Run lock and validate every tool cleanup ledger.
func settledChildForDeletion(child Snapshot, all map[string]Snapshot) bool {
	parent, ok := all[child.Run.ParentRunID]
	if !ok || child.Run.ParentTaskID == "" || child.WorkerSettlement == nil {
		return false
	}
	for _, w := range parent.Workers {
		if w.RunID == child.Run.ID && w.TaskID == child.Run.ParentTaskID && w.Status != "running" && w.Status != "starting" && w.Status != "cancelling" && w.CleanupStatus == "completed" && w.SettledVisit != nil && *w.SettledVisit == w.Visit && child.WorkerSettlement.matches(parent.Run.ID, w) && child.WorkerSettlement.CleanupStatus == "completed" && child.WorkerSettlement.CleanupError == "" {
			return true
		}
	}
	return false
}
