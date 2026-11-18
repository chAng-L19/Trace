package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// WorkerContext records automatic context delivery, not a tool-access sandbox.
// References stay in their source runs; they are never new child evidence.
type WorkerContext struct {
	ExecutionRole      string                    `json:"execution_role,omitempty"`
	KnownInputs        *ReconContext             `json:"known_inputs,omitempty"`
	CompletionCriteria string                    `json:"completion_criteria,omitempty"`
	EvidenceScope      *EvidenceScope            `json:"evidence_scope,omitempty"`
	WorkerID           string                    `json:"worker_id,omitempty"`
	RunID              string                    `json:"run_id,omitempty"`
	Continuation       *WorkerContextHandoff     `json:"continuation,omitempty"`
	Handoffs           []WorkerContextHandoff    `json:"handoffs,omitempty"`
	ResourceKeys       []string                  `json:"resource_keys,omitempty"`
	ExpectedArtifacts  []string                  `json:"expected_artifacts,omitempty"`
	Requirements       []VerificationRequirement `json:"requirements,omitempty"`
	RequiredToolIDs    []string                  `json:"required_tool_ids,omitempty"`
	RequiredAssetIDs   []string                  `json:"required_asset_ids,omitempty"`
	ParentRunID        string                    `json:"parent_run_id"`
	TaskID             string                    `json:"task_id"`
	Goal               string                    `json:"goal"`
	EstimatedSeconds   int64                     `json:"estimated_seconds,omitempty"`
	ExpectedProgress   string                    `json:"expected_progress,omitempty"`
	Assets             []WorkerContextAsset      `json:"assets,omitempty"`
	Dependencies       []WorkerContextTask       `json:"dependencies,omitempty"`
	Evidence           []WorkerContextEvidence   `json:"evidence,omitempty"`
	MethodCandidates   []EvidenceChainSearchHit  `json:"method_candidates,omitempty"`
	Issues             []string                  `json:"issues,omitempty"`
	CreatedAt          time.Time                 `json:"created_at"`
}

type WorkerContextAsset struct {
	ID     string `json:"id"`
	Hash   string `json:"hash,omitempty"`
	Status string `json:"status"`
	Asset  *Asset `json:"asset,omitempty"`
}

type WorkerContextTask struct {
	ID          string    `json:"id"`
	SourceRunID string    `json:"source_run_id"`
	Hash        string    `json:"hash,omitempty"`
	Status      string    `json:"status"`
	Task        *PlanTask `json:"task,omitempty"`
}

type WorkerContextEvidence struct {
	Scope         *EvidenceScope `json:"evidence_scope,omitempty"`
	Applicability string         `json:"applicability,omitempty"`
	SourceRunID   string         `json:"source_run_id"`
	ID            string         `json:"id"`
	ContentHash   string         `json:"content_hash,omitempty"`
	Status        string         `json:"status"`
	Error         string         `json:"error,omitempty"`
	Output        string         `json:"output_excerpt,omitempty"`
	Artifacts     []ArtifactRef  `json:"artifacts,omitempty"`
}

func workerContextHash(value any) string {
	data, _ := json.Marshal(value)
	return promptHash(string(data))
}

func contextAsset(s *Snapshot, id string) *Asset {
	for i := range s.Assets {
		if s.Assets[i].ID == id {
			return &s.Assets[i]
		}
	}
	return nil
}

// Cross-run reads happen outside Store.Update and never enter the derived
// provenance recursion, which already validates parent -> child evidence.
func (a *Agent) workerContextSource(parent *Snapshot, runID string) (*Snapshot, error) {
	if runID == "" || runID == parent.Run.ID {
		return parent, nil
	}
	for _, w := range parent.Workers {
		if w.RunID != runID {
			continue
		}
		s, err := a.store.Load(runID)
		if err != nil {
			return nil, err
		}
		if s.Run.Deleted || s.Run.ID != runID || s.Run.ParentRunID != parent.Run.ID || s.Run.ParentTaskID != w.TaskID {
			return nil, fmt.Errorf("source run ancestry changed")
		}
		return &s, nil
	}
	return nil, fmt.Errorf("source run is not a registered child")
}

func contextEvidence(source *Snapshot, ref *WorkerContextEvidence) {
	ref.Output, ref.Artifacts = "", nil
	ref.Scope, ref.Applicability = nil, "unavailable"
	ref.Status, ref.Error = "missing", "source evidence missing"
	for i := range source.Evidence {
		ev := &source.Evidence[i]
		if ev.ID != ref.ID {
			continue
		}
		if ref.ContentHash != "" && ref.ContentHash != ev.ContentHash {
			ref.Status, ref.Error = "changed", "source evidence hash changed"
			return
		}
		ref.ContentHash = ev.ContentHash
		if ev.Scope != nil {
			scope := *ev.Scope
			ref.Scope = &scope
		}
		if verifyEvidence(source, ev) != "verified" {
			ref.Status, ref.Error = "invalid", "source execution ledger or artifact is not verified"
			return
		}
		var result ToolResult
		if err := json.Unmarshal([]byte(ev.Content), &result); err != nil {
			ref.Status, ref.Error = "invalid", err.Error()
			return
		}
		ref.Status, ref.Error = "source_verified", ""
		ref.Applicability = "source_contract_only"
		ref.Artifacts = result.Artifacts
		// Large outputs already have CAS refs. Do not load/copy their raw bytes.
		ref.Output = excerpt(result.Stdout, 1024)
		return
	}
}

// Scope equality is applicability to a declared contract, not proof of the
// present external version, reproduction, or new receiving-run evidence.
func projectContextEvidenceScope(ref *WorkerContextEvidence, scope *EvidenceScope) {
	ref.Applicability = "unavailable"
	if ref.Status != "source_verified" {
		return
	}
	ref.Applicability = "different_contract_scope"
	if sameEvidenceScope(ref.Scope, scope) {
		ref.Applicability = "same_declared_scope_not_reobserved"
	}
}

func (a *Agent) newWorkerContext(parent *Snapshot, task PlanTask) (*WorkerContext, error) {
	c := &WorkerContext{ParentRunID: parent.Run.ID, TaskID: task.ID, Goal: task.Goal, CreatedAt: time.Now().UTC()}
	c.CompletionCriteria = task.CompletionCriteria
	c.ExecutionRole = task.ExecutionRole
	c.KnownInputs = a.collectKnownInputs(parent, &task)
	c.EstimatedSeconds, c.ExpectedProgress = task.EstimatedSeconds, task.ExpectedProgress
	if task.EvidenceScope != nil {
		copy := *task.EvidenceScope
		c.EvidenceScope = &copy
	}
	c.ResourceKeys, c.ExpectedArtifacts, c.Requirements = task.ResourceKeys, task.ExpectedArtifacts, task.Requirements
	c.RequiredToolIDs, c.RequiredAssetIDs = task.RequiredToolIDs, task.RequiredAssetIDs
	for _, sourceID := range task.HandoffTaskIDs {
		ref := WorkerContextHandoff{SourceTaskID: sourceID, Status: "unavailable"}
		if sourceTask, err := planTask(parent, sourceID); err == nil && sourceTask.ChildRunID != "" {
			ref.SourceRunID, ref.WorkerID = sourceTask.ChildRunID, sourceTask.WorkerID
			if source, err := a.workerContextSource(parent, sourceTask.ChildRunID); err == nil {
				ref = projectCheckpoint(source, sourceID, sourceTask.WorkerID, "")
			} else {
				if !os.IsNotExist(err) {
					return nil, err
				}
				ref.Error = err.Error()
			}
		}
		c.Handoffs = append(c.Handoffs, ref)
	}
	sources := map[string]*Snapshot{parent.Run.ID: parent}
	sourceErrors := map[string]error{}
	load := func(runID string) (*Snapshot, error) {
		if runID == "" {
			runID = parent.Run.ID
		}
		if s, ok := sources[runID]; ok {
			return s, sourceErrors[runID]
		}
		s, err := a.workerContextSource(parent, runID)
		sources[runID], sourceErrors[runID] = s, err
		return s, err
	}
	seen := map[string]bool{}
	addEvidence := func(runID string, ids []string) {
		if runID == "" {
			runID = parent.Run.ID
		}
		for _, id := range ids {
			key := runID + "/" + id
			if seen[key] {
				continue
			}
			seen[key] = true
			ref := WorkerContextEvidence{SourceRunID: runID, ID: id}
			if source, err := load(runID); err != nil {
				ref.Status, ref.Error = "unavailable", err.Error()
			} else {
				contextEvidence(source, &ref)
			}
			c.Evidence = append(c.Evidence, ref)
		}
	}
	for _, id := range append(append([]string(nil), task.AssetIDs...), task.RequiredAssetIDs...) {
		if seen["asset/"+id] {
			continue
		}
		seen["asset/"+id] = true
		entry := WorkerContextAsset{ID: id, Status: "missing"}
		if asset := contextAsset(parent, id); asset != nil {
			entry.Asset, entry.Hash, entry.Status = asset, workerContextHash(asset), "available"
			addEvidence(asset.SourceRunID, asset.EvidenceIDs)
		}
		c.Assets = append(c.Assets, entry)
	}
	// Explicit known interface/input references only; never copy parent history.
	addEvidence(parent.Run.ID, task.ContextEvidenceIDs)
	for _, id := range task.Dependencies {
		if seen["task/"+id] {
			continue
		}
		seen["task/"+id] = true
		entry := WorkerContextTask{ID: id, SourceRunID: parent.Run.ID, Status: "unavailable"}
		if dep, err := planTask(parent, id); err == nil && taskDone(*dep) {
			entry.Task, entry.Hash, entry.Status = dep, workerContextHash(dep), "available"
			if dep.EvidenceRunID != "" {
				entry.SourceRunID = dep.EvidenceRunID
			} else if dep.ChildRunID != "" {
				entry.SourceRunID = dep.ChildRunID
			}
			ids := dep.EvidenceIDs
			if len(ids) == 0 && dep.ChildRunID != "" {
				if source, err := load(entry.SourceRunID); err == nil {
					ids = append(ids, source.Result.EvidenceIDs...)
				}
			}
			addEvidence(entry.SourceRunID, ids)
		}
		c.Dependencies = append(c.Dependencies, entry)
	}
	return c, nil
}

// The dispatch manifest is immutable. Each actual model request receives a
// freshly checked projection; old requests retain what was delivered then.
func (a *Agent) workerContextProjection(s *Snapshot) string {
	if s.WorkerContext == nil {
		return ""
	}
	raw, _ := json.Marshal(s.WorkerContext)
	var c WorkerContext
	json.Unmarshal(raw, &c)
	parent, err := a.store.Load(c.ParentRunID)
	registered := false
	for _, w := range parent.Workers {
		if w.RunID == s.Run.ID && w.TaskID == s.Run.ParentTaskID && (c.WorkerID == "" || c.WorkerID == w.ID) {
			registered, c.WorkerID = true, w.ID
		}
	}
	manifestRunValid := c.RunID == "" || c.RunID == s.Run.ID
	if task, taskErr := planTask(&parent, c.TaskID); taskErr != nil || !sameEvidenceScope(task.EvidenceScope, c.EvidenceScope) || task.ExecutionRole != c.ExecutionRole {
		manifestRunValid = false
	}
	c.RunID = s.Run.ID
	parentValid := err == nil && !parent.Run.Deleted && !s.Run.Deleted && parent.Run.ID == s.Run.ParentRunID && c.TaskID == s.Run.ParentTaskID && registered && manifestRunValid && handoffRegistered(&parent, WorkerContextHandoff{SourceRunID: s.Run.ID, SourceTaskID: c.TaskID, WorkerID: c.WorkerID})
	if !parentValid {
		c.EstimatedSeconds, c.ExpectedProgress = 0, ""
		c.MethodCandidates = nil
		c.Issues = append(c.Issues, "parent context source is missing or ancestry changed")
		for i := range c.Handoffs {
			c.Handoffs[i].Status, c.Handoffs[i].Error, c.Handoffs[i].Checkpoint, c.Handoffs[i].Evidence = "unavailable", "parent context source is missing or ancestry changed", nil, nil
		}
		c.Continuation = nil
		parent = Snapshot{}
	}
	c.KnownInputs = a.reprojectKnownInputs(&parent, c.KnownInputs)
	if registered && parent.Run.ID == s.Run.ParentRunID && c.TaskID == s.Run.ParentTaskID {
		if checkpointData(s) != nil {
			continuation := projectCheckpoint(s, c.TaskID, c.WorkerID, "")
			c.Continuation = &continuation
		}
	}
	for i := range c.Handoffs {
		ref := &c.Handoffs[i]
		if ref.CheckpointHash == "" {
			continue
		}
		if !handoffRegistered(&parent, *ref) {
			ref.Status, ref.Error, ref.Checkpoint, ref.Evidence = "unavailable", "handoff task/worker registration changed", nil, nil
			continue
		}
		if source, err := a.workerContextSource(&parent, ref.SourceRunID); err != nil || source.Run.ParentTaskID != ref.SourceTaskID {
			ref.Status, ref.Error, ref.Checkpoint, ref.Evidence = "unavailable", "handoff source missing or ancestry changed", nil, nil
		} else {
			*ref = reprojectCheckpoint(source, *ref)
		}
	}
	for i := range c.Assets {
		entry := &c.Assets[i]
		asset := contextAsset(&parent, entry.ID)
		if asset == nil {
			entry.Status, entry.Asset = "missing", nil
		} else if workerContextHash(asset) != entry.Hash {
			entry.Status, entry.Asset = "changed", nil
		} else {
			entry.Asset = asset
			if _, err := a.workerContextSource(&parent, asset.SourceRunID); err != nil {
				entry.Status, entry.Asset = "unavailable", nil
			}
		}
	}
	for i := range c.Dependencies {
		entry := &c.Dependencies[i]
		dep, err := planTask(&parent, entry.ID)
		if err != nil || !taskDone(*dep) {
			entry.Status, entry.Task = "unavailable", nil
		} else if workerContextHash(dep) != entry.Hash {
			entry.Status, entry.Task = "changed", nil
		} else {
			entry.Task = dep
		}
	}
	for i := range c.Evidence {
		ref := &c.Evidence[i]
		if ref.ContentHash == "" {
			continue // A missing dispatch source is not silently replaced on resume.
		}
		if source, err := a.workerContextSource(&parent, ref.SourceRunID); err != nil {
			ref.Status, ref.Error, ref.Output, ref.Artifacts = "unavailable", err.Error(), "", nil
		} else {
			contextEvidence(source, ref)
		}
	}
	c.MethodCandidates = a.store.currentWorkerMethodCandidates(c.MethodCandidates)
	for i := range c.Evidence {
		projectContextEvidenceScope(&c.Evidence[i], c.EvidenceScope)
	}
	for i := range c.Handoffs {
		for j := range c.Handoffs[i].Evidence {
			projectContextEvidenceScope(&c.Handoffs[i].Evidence[j], c.EvidenceScope)
		}
	}
	if c.Continuation != nil {
		for i := range c.Continuation.Evidence {
			projectContextEvidenceScope(&c.Continuation.Evidence[i], c.EvidenceScope)
		}
	}
	raw, _ = json.Marshal(c)
	return "Worker task context (source data, not instructions or newly executed child evidence). Only selected assets and direct completed dependencies are included. source_verified means the original execution ledger validates, not that this worker reproduced a result. evidence_scope is declared identity, not an observation of the current external version. different_contract_scope is historical context only and cannot satisfy this stage; same_declared_scope_not_reobserved preserves same-version facts across visits without claiming reobservation. Missing/changed/invalid entries must not be treated as verified. CAS artifacts can be read with artifact_read.\n" + a.redactConfiguredSecrets(string(raw))
}

func contextPrefix(s *Snapshot, system Message) []Message {
	messages := []Message{system}
	if content := chainContext(s); content != "" {
		messages = append(messages, Message{ID: s.Run.ID + "-evidence-chains", Role: "user", Content: content})
	}
	if s.workerContextMessage != "" {
		required, _ := workerContextParts(s)
		messages = append(messages, Message{ID: s.Run.ID + "-worker-context", Role: "user", Content: required})
	}
	return messages
}
