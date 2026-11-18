package agent

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Muteki run_bindings.py:147,269 (144df766, AGPL-3.0) separates archive
// visibility from permanent removal after execution owners have settled.
// Trace keeps shared workspace/uploads and lock inodes; only owned snapshots
// and unshared CAS files are removed under its existing store/run locks.
func (s *Store) PurgeRun(id string) (EvidenceDeleteResult, error) {
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
	root, err := s.loadRawLocked(id)
	if err != nil {
		return result, err
	}
	if root.Run.ParentTaskID != "" {
		return result, fmt.Errorf("Worker 子运行请从所属会话永久删除")
	}
	ids, err := s.snapshotIDsLocked()
	if err != nil {
		return result, err
	}
	all := map[string]Snapshot{}
	for _, rid := range ids {
		snap, err := s.loadRawLocked(rid)
		if err != nil {
			return result, err
		}
		all[rid] = snap
	}
	removed := map[string]bool{id: true}
	for changed := true; changed; {
		changed = false
		for rid, snap := range all {
			if !removed[rid] && snap.Run.ParentTaskID != "" && removed[snap.Run.ParentRunID] {
				removed[rid], changed = true, true
			}
		}
	}
	var deleting []string
	for rid := range removed {
		deleting = append(deleting, rid)
	}
	sort.Strings(deleting)
	for _, rid := range deleting {
		if rid != id {
			release, err := s.AcquireRunLock(rid)
			if err != nil {
				return result, err
			}
			defer release()
		}
	}
	for _, rid := range deleting {
		snap := all[rid]
		if err := toolCleanupError(snap); err != nil {
			return result, fmt.Errorf("会话 %s 工具资源清理尚未确认，保留执行账本: %w", rid, err)
		}
		if snap.Run.Status == StatusRunning || snap.Run.Status == StatusWaitingTool && !settledChildForDeletion(snap, all) {
			return result, fmt.Errorf("请先暂停会话 %s，等待执行结束后再永久删除", rid)
		}
		if err := deletionWorkerCleanupError(snap, all); err != nil {
			return result, err
		}
		for _, worker := range snap.Workers {
			if worker.RunID != "" && !removed[worker.RunID] {
				if _, exists := all[worker.RunID]; exists {
					return result, fmt.Errorf("Worker %s 的子运行归属不一致，请先核对", worker.ID)
				}
			}
		}
	}
	var others [][]byte
	canCollect := true
	for rid, snap := range all {
		if removed[rid] {
			continue
		}
		if ref := runPurgeDependency(snap, removed); ref != "" {
			return result, fmt.Errorf("会话 %s (%s) 仍通过 %s 引用待删除记录；请先处理引用，或选择仅隐藏", rid, snap.Run.Goal, ref)
		}
		release, lockErr := s.AcquireRunLock(rid)
		if lockErr != nil {
			canCollect = false
		} else {
			defer release()
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			return result, err
		}
		others = append(others, raw)
	}
	inputBytes, err := s.readInputReferenceBytes()
	if err != nil {
		return result, err
	}
	others = append(others, inputBytes...)
	hashes := map[string]bool{}
	for _, rid := range deleting {
		raw, err := json.Marshal(all[rid])
		if err != nil {
			return result, err
		}
		var value any
		if err = json.Unmarshal(raw, &value); err != nil {
			return result, err
		}
		purgeArtifactHashes(value, hashes)
	}
	var collect []string
	for hash := range hashes {
		// Input indexes also contain hashes of inline sources. Only existing
		// CAS entries count as retained files, just as deletion counts files.
		if _, err := os.Lstat(filepath.Join(s.root, "artifacts", hash)); errors.Is(err, os.ErrNotExist) {
			delete(hashes, hash)
			continue
		} else if err != nil {
			return result, err
		}
		shared := !canCollect
		for _, raw := range others {
			if bytes.Contains(raw, []byte(hash)) {
				shared = true
				break
			}
		}
		if shared {
			result.ArtifactsRetained++
		} else {
			collect = append(collect, hash)
		}
	}
	stage, err := os.MkdirTemp(s.root, ".run-delete-")
	if err != nil {
		return result, err
	}
	type movedFile struct {
		source, staged string
		artifact       bool
	}
	var moved []movedFile
	rollback := func(cause error) (EvidenceDeleteResult, error) {
		for i := len(moved) - 1; i >= 0; i-- {
			if err := os.Rename(moved[i].staged, moved[i].source); err != nil {
				cause = errors.Join(cause, err)
			}
		}
		if err := os.Remove(stage); err != nil {
			cause = errors.Join(cause, err)
		}
		return EvidenceDeleteResult{}, cause
	}
	for _, hash := range collect {
		file := movedFile{filepath.Join(s.root, "artifacts", hash), filepath.Join(stage, "cas-"+hash), true}
		if err = os.Rename(file.source, file.staged); errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return rollback(err)
		}
		moved = append(moved, file)
	}
	for _, rid := range deleting {
		for _, suffix := range []string{".json", ".display"} {
			if s.sqlite && suffix == ".json" {
				continue
			}
			file := movedFile{filepath.Join(s.root, rid+suffix), filepath.Join(stage, "run-"+rid+suffix), false}
			if err = os.Rename(file.source, file.staged); err != nil {
				if suffix == ".display" && os.IsNotExist(err) {
					continue
				}
				return rollback(err)
			}
			moved = append(moved, file)
		}
	}
	if s.sqlite {
		err = syncStoreDirectory(stage)
		if err == nil {
			err = syncStoreDirectory(s.root)
		}
		if err == nil && len(moved) > 0 {
			err = syncStoreDirectory(filepath.Join(s.root, "artifacts"))
		}
		if err == nil {
			err = s.sqliteCommitPurge(stage, deleting)
		}
	} else {
		err = commitRunPurge(stage)
	}
	if err != nil {
		return rollback(err)
	}
	// No reader holding the store lock can observe a partial closure removal.
	s.eventViewMu.Lock()
	s.eventViewEpoch++
	for _, rid := range deleting {
		s.observations.Delete(rid)
		s.eventViews.Delete(rid)
	}
	s.eventViewMu.Unlock()
	result.Deleted, result.Permanent, result.RunIDs = true, true, deleting
	if !canCollect && len(hashes) > 0 {
		result.CleanupWarning = fmt.Sprintf("会话记录已永久删除；其他运行仍在活动，本次未回收 %d 个产物引用对应的文件", len(hashes))
	}
	cleanupFailed := false
	for _, file := range moved {
		if err = os.Remove(file.staged); err != nil {
			cleanupFailed = true
			result.CleanupWarning += fmt.Sprintf("记录已移除，暂存文件清理未完成: %v; ", err)
		} else if file.artifact {
			result.ArtifactsDeleted++
			s.artifactVerified.Delete(filepath.Base(file.source))
		}
	}
	if !cleanupFailed {
		err = nil
		if !s.sqlite {
			err = os.Remove(filepath.Join(stage, "committed"))
		}
		if err == nil {
			err = os.Remove(stage)
		}
		if err != nil {
			result.CleanupWarning += fmt.Sprintf("暂存目录清理未完成: %v", err)
		}
	}
	if s.sqlite {
		if e := syncStoreDirectory(s.root); e != nil {
			result.CleanupWarning += fmt.Sprintf("purge directory sync: %v", e)
		}
	}
	return result, nil
}

func purgeArtifactHashes(value any, hashes map[string]bool) {
	switch v := value.(type) {
	case map[string]any:
		if hash, ok := v["hash"].(string); ok {
			decoded, err := hex.DecodeString(hash)
			if err == nil && len(decoded) == 32 {
				hashes[hash] = true
			}
		}
		for _, child := range v {
			purgeArtifactHashes(child, hashes)
		}
	case []any:
		for _, child := range v {
			purgeArtifactHashes(child, hashes)
		}
	}
}

// Keep raw input indexes: unknown fields can also reference shared CAS files.
// Callers retain their own locks and commit/rollback boundaries.
func (s *Store) readInputReferenceBytes() ([][]byte, error) {
	inputs, err := os.ReadDir(filepath.Join(s.root, "inputs"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var references [][]byte
	for _, input := range inputs {
		if input.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.root, "inputs", input.Name()))
		if err != nil {
			return nil, err
		}
		references = append(references, raw)
	}
	return references, nil
}
