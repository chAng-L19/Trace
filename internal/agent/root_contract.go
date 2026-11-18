package agent

import (
	"fmt"
	"strings"
)

// RootContract opts into recoverable execution stages. Requirements remain
// the acceptance authority; assets are an optional, fixed finite scope.
type RootContract struct {
	// AutoIntent is server-owned and permits one pre-execution scope expansion.
	AutoIntent         bool              `json:"auto_intent,omitempty"`
	Goal               string            `json:"goal,omitempty"`
	CompletionCriteria string            `json:"completion_criteria,omitempty"`
	IntentHash         string            `json:"intent_hash,omitempty"`
	Version            int               `json:"version"`
	RequirementIDs     []string          `json:"requirement_ids"`
	AssetIDs           []string          `json:"asset_ids,omitempty"`
	RequirementHashes  map[string]string `json:"requirement_hashes,omitempty"`
	AssetHashes        map[string]string `json:"asset_hashes,omitempty"`
}

// EvidenceScope is declared object/version identity, not an observation that
// external state still has that version. New versions require new stages.
type EvidenceScope struct {
	ObjectID string `json:"object_id"`
	Version  string `json:"version"`
}

func sameEvidenceScope(a, b *EvidenceScope) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func validateEvidenceScope(scope *EvidenceScope) error {
	if scope != nil && (strings.TrimSpace(scope.ObjectID) == "" || strings.TrimSpace(scope.Version) == "") {
		return fmt.Errorf("evidence scope requires object_id and version")
	}
	return nil
}
func executionEvidenceScope(s *Snapshot) *EvidenceScope {
	if s == nil || s.WorkerContext == nil || s.WorkerContext.EvidenceScope == nil {
		return nil
	}
	copy := *s.WorkerContext.EvidenceScope
	return &copy
}
func evidenceApplicable(s *Snapshot, e Evidence) bool {
	return sameEvidenceScope(executionEvidenceScope(s), e.Scope)
}
func taskScopeMatches(s *Snapshot, t PlanTask) bool {
	return s != nil && sameEvidenceScope(t.EvidenceScope, executionEvidenceScope(s))
}
func rootAssetIdentity(a Asset) string {
	return valueHash(struct{ ID, ParentID, Kind, Value, SourceRunID, SourceTaskID string }{a.ID, a.ParentID, a.Kind, a.Value, a.SourceRunID, a.SourceTaskID})
}
func validateRootContract(s *Snapshot, c RootContract) error {
	if c.Version != 1 && c.Version != 2 {
		return fmt.Errorf("unsupported root contract version")
	}
	if c.Version == 1 && len(c.RequirementIDs) == 0 {
		return fmt.Errorf("root contract version 1 requires acceptance requirement IDs")
	}
	if c.Version == 2 && (strings.TrimSpace(c.Goal) == "" || c.Goal != s.Run.Goal) {
		return fmt.Errorf("root intent must preserve the original goal")
	}
	if s.Run.ParentRunID != "" {
		return fmt.Errorf("root contract belongs to a root run")
	}
	seen := map[string]bool{}
	for _, id := range c.RequirementIDs {
		if id == "" || seen[id] || requirementByID(s, id) == nil {
			return fmt.Errorf("root contract requirement missing or duplicated: %s", id)
		}
		seen[id] = true
	}
	seen = map[string]bool{}
	for _, id := range c.AssetIDs {
		if id == "" || seen[id] {
			return fmt.Errorf("root contract asset missing or duplicated: %s", id)
		}
		seen[id] = true
		found := false
		for _, a := range s.Assets {
			if a.ID == id && a.Scope != "excluded" {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("root contract asset missing or excluded: %s", id)
		}
	}
	return nil
}

// pinRootContract ignores caller hashes; source identities are server-derived.
func pinRootContract(s *Snapshot, c RootContract) RootContract {
	c.AutoIntent = false
	if c.Version == 2 {
		c.IntentHash = valueHash([]string{c.Goal, c.CompletionCriteria})
	}
	c.RequirementIDs = append([]string(nil), c.RequirementIDs...)
	c.AssetIDs = append([]string(nil), c.AssetIDs...)
	c.RequirementHashes = map[string]string{}
	c.AssetHashes = map[string]string{}
	for _, id := range c.RequirementIDs {
		c.RequirementHashes[id] = valueHash(requirementByID(s, id))
	}
	for _, id := range c.AssetIDs {
		for _, a := range s.Assets {
			if a.ID == id {
				c.AssetHashes[id] = rootAssetIdentity(a)
			}
		}
	}
	return c
}
func (a *Agent) SetRootContract(id string, c RootContract) (Snapshot, error) {
	release, err := a.store.AcquireRunLock(id)
	if err != nil {
		return Snapshot{}, err
	}
	defer release()
	return a.store.Update(id, func(s *Snapshot) error {
		if s.RootContract != nil && (s.RootContract.Version != 2 || !s.RootContract.AutoIntent) {
			return fmt.Errorf("root contract is immutable")
		}
		if len(s.Requests) > 0 || len(s.Actions) > 0 || s.Run.Status == StatusRunning || s.Run.Status == StatusWaitingTool {
			return fmt.Errorf("root contract must be declared before execution")
		}
		if len(s.Workers) > 0 {
			return fmt.Errorf("root contract must be declared before worker execution")
		}
		for _, task := range s.Plan.Tasks {
			if taskStarted(s, task) {
				return fmt.Errorf("root contract must be declared before task execution")
			}
		}
		if s.RootContract != nil {
			original := s.RootContract
			if c.Version != 1 && c.Version != 2 {
				return fmt.Errorf("unsupported root contract version")
			}
			if c.Goal != "" && c.Goal != original.Goal || c.CompletionCriteria != "" && c.CompletionCriteria != original.CompletionCriteria {
				return fmt.Errorf("root intent is immutable")
			}
			for _, id := range original.RequirementIDs {
				if !hasString(c.RequirementIDs, id) || original.RequirementHashes[id] != valueHash(requirementByID(s, id)) {
					return fmt.Errorf("root requirement cannot be removed or changed: %s", id)
				}
			}
			for _, id := range original.AssetIDs {
				preserved := false
				for _, asset := range s.Assets {
					if asset.ID == id && asset.Scope != "excluded" && original.AssetHashes[id] == rootAssetIdentity(asset) {
						preserved = true
						break
					}
				}
				if !hasString(c.AssetIDs, id) || !preserved {
					return fmt.Errorf("root asset cannot be removed or changed: %s", id)
				}
			}
			c.Goal = original.Goal
			c.CompletionCriteria = original.CompletionCriteria
			c.Version = 2
		}
		if err := validateRootContract(s, c); err != nil {
			return err
		}
		c = pinRootContract(s, c)
		s.RootContract = &c
		addEvent(s, "root_contract_set", "Root acceptance scope fixed", c)
		return nil
	})
}
func rootContractGaps(s *Snapshot) []string {
	c := s.RootContract
	if c == nil {
		return nil
	}
	gaps := []string{}
	if err := validateRootContract(s, *c); err != nil {
		gaps = append(gaps, err.Error())
	}
	if c.Version == 2 && c.IntentHash != valueHash([]string{s.Run.Goal, c.CompletionCriteria}) {
		gaps = append(gaps, "root intent changed")
	}
	for _, id := range c.RequirementIDs {
		r := requirementByID(s, id)
		check := latestCheck(s, id)
		if r == nil || c.RequirementHashes[id] != valueHash(r) || check == nil || check.Status != "pass" {
			gaps = append(gaps, "root acceptance unmet or changed: "+id)
		}
	}
	for _, id := range c.AssetIDs {
		found := false
		for _, asset := range s.Assets {
			if asset.ID == id && asset.Scope != "excluded" && c.AssetHashes[id] == rootAssetIdentity(asset) {
				found = true
			}
		}
		if !found {
			gaps = append(gaps, "root scope asset missing, excluded or changed: "+id)
		}
	}
	return gaps
}
func settledStage(s *Snapshot, t PlanTask) bool {
	if s.RootContract == nil || !t.Stage || (t.Status != "abandoned" && t.Status != "blocked") {
		return false
	}
	for _, w := range s.Workers {
		if w.TaskID == t.ID && workerUnsettled(s, w) {
			return false
		}
	}
	return true
}
func taskSatisfiedForRoot(s *Snapshot, t PlanTask) bool {
	return settledStage(s, t) || planTaskSatisfied(&s.Plan, t, map[string]bool{})
}
