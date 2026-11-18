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
	"strings"
	"time"
)

func evidenceDependency(other Snapshot, source string, selected map[string]Evidence) string {
	if dependency := chainEvidenceDependency(other, source, selected); dependency != "" {
		return dependency
	}
	if other.Run.ParentRunID == source {
		return "parent_run_id"
	}
	for _, worker := range other.Workers {
		if worker.RunID == source {
			return "worker " + worker.ID
		}
	}
	for _, asset := range other.Assets {
		if asset.SourceRunID == source {
			for _, id := range asset.EvidenceIDs {
				if _, ok := selected[id]; ok {
					return "asset " + asset.ID
				}
			}
		}
	}
	for _, task := range other.Plan.Tasks {
		if task.EvidenceRunID == source || task.ChildRunID == source {
			return "task " + task.ID
		}
	}
	if other.WorkerContext != nil {
		for _, e := range other.WorkerContext.Evidence {
			if _, ok := selected[e.ID]; ok && e.SourceRunID == source {
				return "worker_context evidence " + e.ID
			}
		}
	}
	return ""
}

// Tool call IDs are response-scoped. Pair the ordered source events with local
// tool messages once, then erase only those persistent message identities in
// the transcript and request snapshots. Identical repeated results stay apart.
func evidenceMessageIDs(s *Snapshot, selected map[string]Evidence) map[string]bool {
	assignMessageIDs(s)
	ordered := append([]Evidence(nil), s.Evidence...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].SourceEventSequence < ordered[j].SourceEventSequence })
	used, removed := map[string]bool{}, map[string]bool{}
	for _, e := range ordered {
		contents := map[string]bool{e.Content: true}
		for _, a := range s.Actions {
			if a.RequestID == e.RequestID && a.CallID == e.CallID && a.Result != nil {
				raw, _ := json.Marshal(a.Result)
				contents[string(raw)] = true
			}
		}
		for _, message := range s.Messages {
			if used[message.ID] || !strings.HasPrefix(message.ID, s.Run.ID+"-m") || message.Role != "tool" || message.ToolCallID != e.CallID || !contents[message.Content] {
				continue
			}
			used[message.ID] = true
			if _, ok := selected[e.ID]; ok {
				removed[message.ID] = true
			}
			break
		}
	}
	return removed
}

// Delete the original source records, not merely their catalog visibility.
// Historical model/user narrative is retained and never counted as source proof.
func removeEvidenceRecords(s *Snapshot, selected map[string]Evidence) map[string]bool {
	hashes := map[string]bool{}
	messageIDs := evidenceMessageIDs(s, selected)
	redact := func(messages []Message) {
		for i := range messages {
			if messageIDs[messages[i].ID] {
				messages[i].Content = `{"evidence_deleted":true}`
			}
		}
	}
	redact(s.Messages)
	for i := range s.Requests {
		redact(s.Requests[i].Messages)
	}
	for _, evidence := range selected {
		var result ToolResult
		if json.Unmarshal([]byte(evidence.Content), &result) == nil {
			for _, ref := range result.Artifacts {
				hashes[ref.Hash] = true
			}
		}
		for i := range s.Actions {
			a := &s.Actions[i]
			if a.RequestID == evidence.RequestID && a.CallID == evidence.CallID {
				if a.Result != nil {
					for _, ref := range a.Result.Artifacts {
						hashes[ref.Hash] = true
					}
				}
				a.Result, a.Status = nil, "deleted"
			}
		}
	}
	keptEvidence := s.Evidence[:0]
	for _, e := range s.Evidence {
		if _, deleted := selected[e.ID]; !deleted {
			keptEvidence = append(keptEvidence, e)
		}
	}
	s.Evidence = keptEvidence
	for i := range s.Findings {
		finding := &s.Findings[i]
		invalidSource := false
		for _, id := range finding.EvidenceIDs {
			if _, ok := selected[id]; ok {
				invalidSource = true
			}
		}
		for j := range finding.Retests {
			retest := &finding.Retests[j]
			invalid := invalidSource
			for _, id := range retest.EvidenceIDs {
				if _, ok := selected[id]; ok {
					invalid = true
				}
			}
			if invalid {
				retest.Status, retest.Reason = "unknown", "source evidence deleted"
			}
		}
	}
	keptEvents := s.Events[:0]
	started := map[string]int{}
	for _, event := range s.Events {
		if event.Type != "action_started" {
			continue
		}
		raw, _ := json.Marshal(event.Data)
		var data struct {
			CallID    string `json:"call_id"`
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(raw, &data)
		for id, e := range selected {
			if data.CallID == e.CallID && data.RequestID == e.RequestID {
				started[id] = event.Sequence
			}
		}
	}
	for _, event := range s.Events {
		remove := false
		if event.Type == "tool_result" || event.Type == "tool_output" {
			raw, _ := json.Marshal(event.Data)
			var data struct {
				CallID     string `json:"call_id"`
				EvidenceID string `json:"evidence_id"`
			}
			_ = json.Unmarshal(raw, &data)
			for id, e := range selected {
				if event.Sequence == e.SourceEventSequence || (event.Type == "tool_output" && data.CallID == e.CallID && event.Sequence > started[id] && event.Sequence < e.SourceEventSequence) {
					remove = true
				}
			}
		}
		if !remove {
			keptEvents = append(keptEvents, event)
		}
	}
	s.Events = keptEvents
	return hashes
}

func (s *Store) purgeEvidenceLocked(snap Snapshot, evidenceID string, conversation bool) (EvidenceDeleteResult, error) {
	result := EvidenceDeleteResult{}
	selected := map[string]Evidence{}
	for _, e := range snap.Evidence {
		if conversation || e.ID == evidenceID {
			selected[e.ID] = e
		}
	}
	if !conversation && len(selected) == 0 {
		return result, os.ErrNotExist
	}
	if dependency := chainEvidenceDependency(snap, snap.Run.ID, selected); dependency != "" {
		return result, fmt.Errorf("原始操作仍被 %s 引用；请保留证据或先处理链路引用", dependency)
	}
	ids, err := s.snapshotIDsLocked()
	if err != nil {
		return result, err
	}
	// All dependency checks precede changes, including a whole-run mixed batch.
	var otherBytes [][]byte
	canCollect := true
	for _, id := range ids {
		if id == snap.Run.ID {
			continue
		}
		other, err := s.loadRawLocked(id)
		if err != nil {
			return result, err
		}
		if ref := evidenceDependency(other, snap.Run.ID, selected); ref != "" {
			return result, fmt.Errorf("证据仍被会话 %s (%s) 的 %s 引用；请先处理该引用", id, other.Run.Goal, ref)
		}
		// Writers own their run lock before publishing CAS references. If another
		// run is active, skip collection rather than racing its pending artifact.
		release, lockErr := s.AcquireRunLock(id)
		if lockErr != nil {
			canCollect = false
		} else {
			defer release()
		}
		raw, err := json.Marshal(other)
		if err != nil {
			return result, err
		}
		otherBytes = append(otherBytes, raw)
	}
	// Completed workers still own independent evidence. Do not silently omit
	// their records from a requested whole-conversation purge.
	if conversation && len(snap.Workers) > 0 {
		return result, fmt.Errorf("会话包含 Worker 子运行；请先在关联证据中处理子运行证据及引用")
	}
	hashes := removeEvidenceRecords(&snap, selected)
	snap.Run.Deleted = conversation || snap.Run.Deleted
	snap.Run.UpdatedAt = time.Now().UTC()
	s.refreshDerivedLocked(&snap, map[string]bool{snap.Run.ID: true})
	remaining, err := json.Marshal(snap)
	if err != nil {
		return result, err
	}
	otherBytes = append(otherBytes, remaining)
	inputBytes, err := s.readInputReferenceBytes()
	if err != nil {
		return result, err
	}
	otherBytes = append(otherBytes, inputBytes...)
	var collect []string
	for hash := range hashes {
		decoded, err := hex.DecodeString(hash)
		if err != nil || len(decoded) != 32 {
			continue
		}
		shared := !canCollect
		for _, raw := range otherBytes {
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
	// Stage only exact CAS files. A failure before snapshot commit restores every
	// staged file and leaves the conversation and all evidence unchanged.
	stage := ""
	var moved []string
	rollback := func(cause error) (EvidenceDeleteResult, error) {
		for _, hash := range moved {
			if e := os.Rename(filepath.Join(stage, hash), filepath.Join(s.root, "artifacts", hash)); e != nil {
				cause = errors.Join(cause, e)
			}
		}
		if stage != "" {
			if e := os.Remove(stage); e != nil {
				cause = errors.Join(cause, e)
			}
		}
		return EvidenceDeleteResult{}, cause
	}
	if len(collect) > 0 {
		stage, err = os.MkdirTemp(s.root, ".evidence-delete-")
		if err != nil {
			return result, err
		}
		for _, hash := range collect {
			err = os.Rename(filepath.Join(s.root, "artifacts", hash), filepath.Join(stage, hash))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return rollback(err)
			}
			moved = append(moved, hash)
		}
	}
	if err = s.saveLocked(snap); err != nil {
		return rollback(err)
	}
	result.Deleted = true
	for _, hash := range moved {
		if err = os.Remove(filepath.Join(stage, hash)); err != nil {
			result.CleanupWarning += fmt.Sprintf("证据记录已删除，产物 %s 清理未完成: %v; ", hash, err)
		} else {
			result.ArtifactsDeleted++
			s.artifactVerified.Delete(hash)
		}
	}
	if stage != "" {
		if err = os.Remove(stage); err != nil {
			result.CleanupWarning += fmt.Sprintf("清理暂存目录未完成: %v", err)
		}
	}
	return result, nil
}
