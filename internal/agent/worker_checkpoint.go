package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// A checkpoint is a worker-authored observation, never acceptance evidence.
// Keep it in the existing snapshot so compaction cannot discard the handoff.
type WorkerHandoff struct {
	Report          *ReportText       `json:"report,omitempty"`
	Subject         string            `json:"subject,omitempty"`
	Instance        string            `json:"instance,omitempty"`
	Progress        string            `json:"progress,omitempty"`
	Findings        []string          `json:"findings,omitempty"`
	NegativeResults []string          `json:"negative_results,omitempty"`
	NextAction      string            `json:"next_action"`
	EvidenceIDs     []string          `json:"evidence_ids,omitempty"`
	EvidenceHashes  map[string]string `json:"evidence_hashes,omitempty"`
}

type WorkerCheckpoint struct {
	WorkerHandoff
	Reason    string    `json:"reason"`
	Summary   string    `json:"summary"`
	CreatedAt time.Time `json:"created_at"`
}

type WorkerContextHandoff struct {
	SourceRunID    string                  `json:"source_run_id"`
	SourceTaskID   string                  `json:"source_task_id"`
	WorkerID       string                  `json:"worker_id,omitempty"`
	CheckpointHash string                  `json:"checkpoint_hash,omitempty"`
	Status         string                  `json:"status"`
	Error          string                  `json:"error,omitempty"`
	Checkpoint     *WorkerCheckpoint       `json:"checkpoint,omitempty"`
	Evidence       []WorkerContextEvidence `json:"evidence,omitempty"`
}

type HandoffNotReadyError struct {
	TaskID  string
	Sources []string
}

func (e *HandoffNotReadyError) Error() string {
	return fmt.Sprintf("task %s awaits explicit handoff checkpoints from %v", e.TaskID, e.Sources)
}

func checkpointData(s *Snapshot) *WorkerCheckpoint {
	if s.WorkerCheckpoint != nil {
		return s.WorkerCheckpoint
	}
	// Legacy yields already recorded the complete checkpoint in their event.
	for i := len(s.Events) - 1; i >= 0; i-- {
		if s.Events[i].Type != "worker_yielded" {
			continue
		}
		raw, _ := json.Marshal(s.Events[i].Data)
		var v struct{ Reason, Checkpoint string }
		if json.Unmarshal(raw, &v) == nil && v.Checkpoint != "" {
			var handoff WorkerHandoff
			if json.Unmarshal([]byte(v.Checkpoint), &handoff) != nil || handoff.NextAction == "" {
				handoff = WorkerHandoff{NextAction: v.Checkpoint}
			}
			return &WorkerCheckpoint{WorkerHandoff: handoff, Reason: v.Reason, Summary: v.Checkpoint, CreatedAt: s.Events[i].CreatedAt}
		}
	}
	return nil
}

func validateCheckpoint(s *Snapshot, checkpoint *WorkerCheckpoint) error {
	if len(checkpoint.EvidenceIDs) > 16 || len(checkpoint.Findings) > 16 || len(checkpoint.NegativeResults) > 16 {
		return fmt.Errorf("checkpoint exceeds 16 KiB or 16 entries per list; retain artifact references and concise observations")
	}
	if strings.TrimSpace(checkpoint.NextAction) == "" {
		return fmt.Errorf("handoff next_action is required")
	}
	// Hashes are server-owned and limited to the selected source evidence.
	checkpoint.EvidenceHashes = nil
	seen := map[string]bool{}
	for _, id := range checkpoint.EvidenceIDs {
		if id == "" || seen[id] {
			return fmt.Errorf("checkpoint evidence IDs must be nonempty and unique")
		}
		seen[id] = true
		ref := WorkerContextEvidence{ID: id, SourceRunID: s.Run.ID}
		contextEvidence(s, &ref)
		if ref.Status != "source_verified" {
			return fmt.Errorf("checkpoint evidence %s is not verified in this worker ledger: %s", id, ref.Error)
		}
		if checkpoint.EvidenceHashes == nil {
			checkpoint.EvidenceHashes = map[string]string{}
		}
		checkpoint.EvidenceHashes[id] = ref.ContentHash
	}
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	if len(raw) > 16384 {
		return fmt.Errorf("checkpoint exceeds 16 KiB after source evidence hashes")
	}
	return nil
}

// Preserve dispatch-time evidence pins even for checkpoints predating hashes.
func reprojectCheckpoint(source *Snapshot, pinned WorkerContextHandoff) WorkerContextHandoff {
	current := projectCheckpoint(source, pinned.SourceTaskID, pinned.WorkerID, pinned.CheckpointHash)
	if current.Status != "available" {
		return current
	}
	valid := pinned.SourceRunID == source.Run.ID && pinned.CheckpointHash != "" && len(pinned.Evidence) == len(current.Evidence)
	for i, ev := range current.Evidence {
		if i >= len(pinned.Evidence) {
			valid = false
			break
		}
		old := pinned.Evidence[i]
		valid = valid && old.SourceRunID == ev.SourceRunID && old.ID == ev.ID && old.ContentHash != "" && old.ContentHash == ev.ContentHash
	}
	if !valid {
		current.Status, current.Error, current.Checkpoint, current.Evidence = "changed", "source-qualified handoff evidence changed; request a new explicit handoff", nil, nil
	}
	return current
}

// The registration transaction must recheck the fixed dispatch manifest.
func handoffRegistered(parent *Snapshot, ref WorkerContextHandoff) bool {
	task, err := planTask(parent, ref.SourceTaskID)
	if err != nil || task.WorkerID != ref.WorkerID || task.ChildRunID != ref.SourceRunID {
		return false
	}
	for _, w := range parent.Workers {
		if w.ID == ref.WorkerID && w.TaskID == ref.SourceTaskID && w.RunID == ref.SourceRunID {
			return true
		}
	}
	return false
}

func (a *Agent) validateHandoffManifestLocked(parent *Snapshot, task PlanTask, manifest []WorkerContextHandoff) error {
	if len(task.HandoffTaskIDs) != len(manifest) {
		return fmt.Errorf("handoff manifest does not match task contract")
	}
	for i, sourceID := range task.HandoffTaskIDs {
		ref := manifest[i]
		if ref.Status != "available" || ref.SourceTaskID != sourceID || !handoffRegistered(parent, ref) {
			return &HandoffNotReadyError{TaskID: task.ID, Sources: []string{sourceID}}
		}
		source, err := a.store.loadLocked(ref.SourceRunID)
		if err != nil {
			return err
		}
		if source.Run.Deleted || source.Run.ParentRunID != parent.Run.ID || source.Run.ParentTaskID != sourceID || reprojectCheckpoint(&source, ref).Status != "available" {
			return &HandoffNotReadyError{TaskID: task.ID, Sources: []string{sourceID}}
		}
	}
	return nil
}

func projectCheckpoint(source *Snapshot, taskID, workerID, pinned string) WorkerContextHandoff {
	ref := WorkerContextHandoff{SourceRunID: source.Run.ID, SourceTaskID: taskID, WorkerID: workerID, CheckpointHash: pinned, Status: "missing"}
	if source.Run.Deleted {
		ref.Status, ref.Error = "unavailable", "source run is deleted"
		return ref
	}
	cp := checkpointData(source)
	if cp == nil {
		ref.Error = "source has no saved checkpoint"
		return ref
	}
	hash := workerContextHash(cp)
	if pinned != "" && pinned != hash {
		ref.Status, ref.Error = "changed", "source checkpoint changed; request a new explicit handoff"
		return ref
	}
	ref.CheckpointHash, ref.Checkpoint, ref.Status = hash, cp, "available"
	for _, id := range cp.EvidenceIDs {
		ev := WorkerContextEvidence{SourceRunID: source.Run.ID, ID: id, ContentHash: cp.EvidenceHashes[id]}
		contextEvidence(source, &ev)
		ref.Evidence = append(ref.Evidence, ev)
		if ev.Status != "source_verified" {
			ref.Status, ref.Error = "invalid", "checkpoint source evidence is no longer verified"
		}
	}
	return ref
}

// Explicit handoffs are scheduling prerequisites: do not launch a receiver
// before each source has a durable checkpoint to project into its context.
func (a *Agent) taskHandoffPrerequisitesLocked(parent *Snapshot, task PlanTask) ([]string, error) {
	missing := []string{}
	for _, sourceID := range task.HandoffTaskIDs {
		sourceTask, err := planTask(parent, sourceID)
		if err != nil || sourceTask.ChildRunID == "" || sourceTask.WorkerID == "" {
			missing = append(missing, sourceID)
			continue
		}
		registered := false
		for _, worker := range parent.Workers {
			if worker.ID == sourceTask.WorkerID && worker.TaskID == sourceID && worker.RunID == sourceTask.ChildRunID {
				registered = true
				break
			}
		}
		if !registered {
			missing = append(missing, sourceID)
			continue
		}
		source, err := a.store.loadLocked(sourceTask.ChildRunID)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				missing = append(missing, sourceID)
				continue
			}
			return nil, err
		}
		if source.Run.Deleted || source.Run.ParentRunID != parent.Run.ID || source.Run.ParentTaskID != sourceID || checkpointData(&source) == nil || projectCheckpoint(&source, sourceID, sourceTask.WorkerID, "").Status != "available" {
			missing = append(missing, sourceID)
		}
	}
	return missing, nil
}
