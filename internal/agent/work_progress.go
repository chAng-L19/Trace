package agent

import (
	"fmt"
	"strings"
)

// WorkItem declares finite goal coverage independently of a stage's acceptance.
// Identity and outcome must both match the same original observation.
type WorkItem struct {
	ID          string                  `json:"id"`
	Goal        string                  `json:"goal"`
	Identity    EvidencePredicate       `json:"identity"`
	Requirement VerificationRequirement `json:"requirement"`
	NextAction  string                  `json:"next_action"`
}

type RemainingWork struct {
	ID          string   `json:"id"`
	Goal        string   `json:"goal"`
	NextAction  string   `json:"next_action"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
}

type WorkReference struct {
	TaskID string `json:"task_id"`
	ItemID string `json:"item_id"`
}

// WorkProgress is a derived view of existing tasks, never another work queue.
type WorkProgress struct {
	WorkReference
	Goal        string   `json:"goal"`
	NextAction  string   `json:"next_action"`
	Status      string   `json:"status"`
	TaskIDs     []string `json:"task_ids,omitempty"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	Reason      string   `json:"reason,omitempty"`
}

func validateTaskWork(t PlanTask) error {
	ids := map[string]bool{}
	identities := map[string]bool{}
	for _, item := range t.WorkItems {
		if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.Goal) == "" || ids[item.ID] {
			return fmt.Errorf("task %s needs unique work item IDs and goals", t.ID)
		}
		ids[item.ID] = true
		identity := valueHash(struct {
			Identity        EvidencePredicate
			Predicate       EvidencePredicate
			Tool, Arguments string
		}{item.Identity, item.Requirement.Predicate, item.Requirement.ToolID, item.Requirement.ArgumentsHash})
		if identities[identity] {
			return fmt.Errorf("task %s repeats a work identity", t.ID)
		}
		identities[identity] = true
		if err := validatePredicate(item.Identity); err != nil {
			return fmt.Errorf("work %s identity: %w", item.ID, err)
		}
		if err := ValidateRequirements([]VerificationRequirement{item.Requirement}); err != nil {
			return fmt.Errorf("work %s: %w", item.ID, err)
		}
		if item.Requirement.Category != "goal" || item.Requirement.RequireReview {
			return fmt.Errorf("work %s needs a machine goal predicate; retain reviewed acceptance in task requirements", item.ID)
		}
		if item.Requirement.ToolID == "" {
			return fmt.Errorf("work %s requires an observation tool", item.ID)
		}
	}
	return nil
}

func validateRemainingShape(items []RemainingWork) error {
	ids := map[string]bool{}
	for _, item := range items {
		if strings.TrimSpace(item.ID) == "" || strings.TrimSpace(item.Goal) == "" || strings.TrimSpace(item.NextAction) == "" || ids[item.ID] {
			return fmt.Errorf("remaining work needs unique IDs, goals and next actions")
		}
		ids[item.ID] = true
	}
	return nil
}
func validateRemainingWork(s *Snapshot, items []RemainingWork) error {
	if err := validateRemainingShape(items); err != nil {
		return err
	}
	for _, item := range items {
		if len(item.EvidenceIDs) > 0 {
			if _, err := evidenceReferences(s, item.EvidenceIDs); err != nil {
				return fmt.Errorf("remaining %s: %w", item.ID, err)
			}
		}
	}
	return nil
}

func workReference(t PlanTask, itemID string) WorkReference {
	return WorkReference{TaskID: t.ID, ItemID: itemID}
}

func validateWorkLinks(tasks []PlanTask) error {
	sources := map[string]*PlanTask{}
	for i := range tasks {
		sources[tasks[i].ID] = &tasks[i]
	}
	owners := map[WorkReference]string{}
	for _, t := range tasks {
		seen := map[WorkReference]bool{}
		for _, ref := range t.Continues {
			if ref.TaskID == t.ID || seen[ref] {
				return fmt.Errorf("task %s has invalid or duplicate continuation", t.ID)
			}
			seen[ref] = true
			source := sources[ref.TaskID]
			if source == nil || ref.TaskID != rootWorkTaskID && !hasString(t.Dependencies, ref.TaskID) && !(source.Stage && taskDisposed(*source) && source.Status != "superseded" && source.DispositionReason != "") {
				return fmt.Errorf("task %s continuation requires its source dependency", t.ID)
			}
			found := false
			for _, item := range source.WorkItems {
				if item.ID != ref.ItemID {
					continue
				}
				found = true
				if t.Goal != item.Goal || !sameEvidenceScope(t.EvidenceScope, source.EvidenceScope) || !sameTaskRequirements(t.Requirements, []VerificationRequirement{item.Requirement}) {
					return fmt.Errorf("task %s must preserve continuation goal, scope and acceptance", t.ID)
				}
			}
			for _, item := range source.Remaining {
				if item.ID != ref.ItemID {
					continue
				}
				found = true
				if t.Goal != item.Goal || !sameEvidenceScope(t.EvidenceScope, source.EvidenceScope) {
					return fmt.Errorf("task %s changed remaining goal or scope", t.ID)
				}
				if len(t.Requirements) == 0 && t.SuccessContains == "" {
					return fmt.Errorf("task %s continuation requires observable acceptance", t.ID)
				}
			}
			if !found {
				return fmt.Errorf("task %s references unknown remaining work %s/%s", t.ID, ref.TaskID, ref.ItemID)
			}
			for _, key := range source.ResourceKeys {
				if !hasString(t.ResourceKeys, key) {
					return fmt.Errorf("task %s lost continuation resource %s", t.ID, key)
				}
			}
			if !taskDisposed(t) {
				if other, exists := owners[ref]; exists {
					return fmt.Errorf("work %s/%s already linked to task %s", ref.TaskID, ref.ItemID, other)
				}
				owners[ref] = t.ID
			}
		}
	}
	return nil
}

func sameTaskWork(a, b PlanTask) bool {
	return valueHash(a.WorkItems) == valueHash(b.WorkItems) && valueHash(a.Continues) == valueHash(b.Continues)
}

// Merge only source-bound work declarations. Missing later claims cannot erase
// already received obligations, including across report-only visits.
func mergeRemainingWork(t *PlanTask, child *Snapshot) bool {
	if child == nil || child.Run.ParentTaskID != t.ID || validateRemainingShape(child.Result.Remaining) != nil {
		return false
	}
	changed := false
	for _, item := range child.Result.Remaining {
		known := false
		for _, declared := range t.WorkItems {
			known = known || declared.ID == item.ID
		}
		for _, previous := range t.Remaining {
			known = known || previous.ID == item.ID
		}
		if !known {
			t.Remaining = append(t.Remaining, item)
			changed = true
		}
	}
	return changed
}

func progressTasks(s *Snapshot, ref WorkReference, includeOrigin bool) []PlanTask {
	out := []PlanTask{}
	seen := map[string]bool{}
	for _, t := range s.Plan.Tasks {
		linked := includeOrigin && t.ID == ref.TaskID
		for _, link := range t.Continues {
			linked = linked || link == ref
		}
		if linked {
			out = append(out, t)
			seen[t.ID] = true
		}
	}
	// Existing same-contract replacements carry proof, but the retired ID
	// remains unsuccessful as a scheduling dependency.
	for i := 0; i < len(out); i++ {
		previous := out[i]
		if previous.Status != "superseded" {
			continue
		}
		for _, replacement := range s.Plan.Tasks {
			if replacement.ID == previous.ReplacementTaskID && !seen[replacement.ID] && taskContractCovers(replacement, previous) {
				seen[replacement.ID] = true
				out = append(out, replacement)
			}
		}
	}
	return out
}

func workItemProof(source *Snapshot, t PlanTask, item WorkItem) []string {
	return workItemProofs(source, t, []WorkItem{item})[valueHash(item)]
}
func workItemProofs(source *Snapshot, t PlanTask, items []WorkItem) map[string][]string {
	if source == nil || source.Run.Deleted || !taskDone(t) || !taskScopeMatches(source, t) {
		return nil
	}
	if t.ChildRunID != "" {
		if source.Run.ID != t.ChildRunID || source.Run.ParentTaskID != t.ID {
			return nil
		}
	} else if t.EvidenceRunID != "" && t.EvidenceRunID != source.Run.ID {
		return nil
	}
	proofs := map[string][]string{}
	for _, id := range acceptanceEvidenceIDs(source) {
		output, valid := verifiedOutput(source, id)
		if !valid {
			continue
		}
		for _, item := range items {
			identity, err := predicateMatches(item.Identity, output)
			if err != nil || !identity {
				continue
			}
			if status, _ := checkRequirement(source, item.Requirement, []string{id}); status == "pass" {
				key := valueHash(item)
				proofs[key] = append(proofs[key], source.Run.ID+"/"+id)
			}
		}
	}
	return proofs
}

func refreshWorkProgress(s *Snapshot, sourceFor func(string) *Snapshot) {
	s.Plan.WorkProgress = nil
	// Keep only atomic proof IDs, not another copy of every child's history.
	// Validate each source ledger once for all its declared finite items.
	proofs := map[string]map[string][]string{}
	bindings := map[string]bool{}
	for _, task := range s.Plan.Tasks {
		if !taskDone(task) && !settledStage(s, task) {
			continue
		}
		if taskDisposed(task) && s.Run.Responsibility != ResponsibilityCoordinator && s.Run.Responsibility != ResponsibilityWorker && len(task.WorkItems) == 0 && len(task.Remaining) == 0 && len(task.Continues) == 0 {
			continue
		}
		sourceID := task.ChildRunID
		if sourceID == "" {
			sourceID = task.EvidenceRunID
		}
		source := sourceFor(sourceID)
		if task.ChildRunID != "" && source != nil {
			_, issues := collectRemainingDeclarations(source)
			var reasons []string
			for _, issue := range issues {
				reasons = append(reasons, issue.Repair)
			}
			if len(reasons) > 0 {
				s.Plan.WorkProgress = append(s.Plan.WorkProgress, WorkProgress{WorkReference: WorkReference{TaskID: task.ID}, Status: "invalid", Reason: strings.Join(reasons, "; ")})
			}
		}
		if !taskDone(task) {
			continue
		}
		bindings[task.ID] = workProgressSourceBound(s, task, source)
		if !bindings[task.ID] {
			continue
		}
		items := append([]WorkItem(nil), task.WorkItems...)
		for _, ref := range task.Continues {
			if origin, err := planTask(s, ref.TaskID); err == nil {
				for _, item := range origin.WorkItems {
					if item.ID == ref.ItemID {
						items = append(items, item)
					}
				}
			}
		}
		if len(items) > 0 {
			proofs[task.ID] = workItemProofs(source, task, items)
		}
	}
	sources := s.Plan.Tasks
	if root, exists := rootWorkSource(s); exists {
		sources = append(append([]PlanTask(nil), sources...), root)
	}
	for _, source := range sources {
		for _, item := range source.WorkItems {
			p := WorkProgress{WorkReference: workReference(source, item.ID), Goal: item.Goal, NextAction: item.NextAction, Status: "pending"}
			for _, candidate := range progressTasks(s, p.WorkReference, true) {
				if candidate.ID != source.ID {
					p.TaskIDs = append(p.TaskIDs, candidate.ID)
					p.Status = "scheduled"
				}
				if proof := proofs[candidate.ID][valueHash(item)]; len(proof) > 0 {
					p.Status, p.EvidenceIDs = "verified", proof
					break
				}
			}
			s.Plan.WorkProgress = append(s.Plan.WorkProgress, p)
		}
		for _, item := range source.Remaining {
			p := WorkProgress{WorkReference: workReference(source, item.ID), Goal: item.Goal, NextAction: item.NextAction, Status: "pending"}
			for _, candidate := range progressTasks(s, p.WorkReference, false) {
				if candidate.ID == source.ID {
					continue
				}
				p.TaskIDs = append(p.TaskIDs, candidate.ID)
				if taskDone(candidate) && bindings[candidate.ID] {
					p.Status, p.Reason = "verified", ""
					break
				}
				if settledStage(s, candidate) {
					if p.Status != "scheduled" {
						p.Status, p.Reason = candidate.Status, candidate.DispositionReason
					}
				} else {
					p.Status, p.Reason = "scheduled", ""
				}
			}
			s.Plan.WorkProgress = append(s.Plan.WorkProgress, p)
		}
	}
}

func workProgressGaps(s *Snapshot) []string {
	gaps := []string{}
	for _, progress := range s.Plan.WorkProgress {
		if progress.Status == "invalid" {
			gaps = append(gaps, "invalid remaining declaration from "+progress.TaskID+": "+progress.Reason)
		}
	}
	for _, t := range s.Plan.Tasks {
		for _, item := range t.WorkItems {
			if !workProgressResolved(s.Plan, workReference(t, item.ID), false) {
				gaps = append(gaps, "unmet finite work coverage: "+t.ID+"/"+item.ID)
			}
		}
		for _, item := range t.Remaining {
			if !workProgressResolved(s.Plan, workReference(t, item.ID), true) {
				gaps = append(gaps, "remaining work requires a settled continuation or explicit stage disposition: "+t.ID+"/"+item.ID)
			}
		}
	}
	return append(gaps, rootWorkProgressGaps(s)...)
}

func workProgressResolved(p PlanState, ref WorkReference, disposition bool) bool {
	for _, progress := range p.WorkProgress {
		if progress.WorkReference == ref {
			return progress.Status == "verified" || disposition && (progress.Status == "blocked" || progress.Status == "abandoned") && progress.Reason != ""
		}
	}
	return false
}
func workItemsSchema() map[string]any {
	predicate := map[string]any{"type": "object", "properties": map[string]any{
		"kind":  map[string]any{"type": "string", "enum": []string{"contains", "equals", "regex", "json_equals"}},
		"value": map[string]string{"type": "string"}, "pointer": map[string]string{"type": "string"}, "expected": map[string]any{},
	}, "required": []string{"kind"}, "additionalProperties": false}
	requirement := map[string]any{"type": "object", "properties": map[string]any{
		"id": map[string]string{"type": "string"}, "description": map[string]string{"type": "string"}, "category": map[string]any{"type": "string", "enum": []string{"goal"}},
		"predicate": predicate, "tool_id": map[string]string{"type": "string"}, "arguments_hash": map[string]string{"type": "string"},
	}, "required": []string{"id", "description", "category", "predicate", "tool_id"}, "additionalProperties": false}
	return map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
		"id": map[string]string{"type": "string"}, "goal": map[string]string{"type": "string"}, "identity": predicate, "requirement": requirement, "next_action": map[string]string{"type": "string"},
	}, "required": []string{"id", "goal", "identity", "requirement"}, "additionalProperties": false}}
}

func workReferencesSchema() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
		"task_id": map[string]string{"type": "string"}, "item_id": map[string]string{"type": "string"},
	}, "required": []string{"task_id", "item_id"}, "additionalProperties": false}}
}
func cloneRemainingWork(items []RemainingWork) []RemainingWork {
	out := append([]RemainingWork(nil), items...)
	for i := range out {
		out[i].EvidenceIDs = append([]string(nil), out[i].EvidenceIDs...)
	}
	return out
}

// Reuse the established complete task/worker/context/visit/settlement binding.
func workProgressSourceBound(parent *Snapshot, task PlanTask, source *Snapshot) bool {
	if source == nil || source.Run.Deleted {
		return false
	}
	if task.ChildRunID == "" {
		return source.Run.ID == parent.Run.ID && (task.EvidenceRunID == "" || task.EvidenceRunID == parent.Run.ID)
	}
	matches := 0
	bound := false
	for _, w := range parent.Workers {
		if w.RunID != task.ChildRunID {
			continue
		}
		matches++
		if delegatedSourceBound(parent, task, w, source) {
			bound = true
			continue
		}
		manifest := source.WorkerContext
		if manifest != nil && manifest.RunID == source.Run.ID && manifest.ParentRunID == parent.Run.ID && manifest.TaskID == task.ID && manifest.WorkerID == w.ID &&
			task.WorkerID == w.ID && task.ChildRunID == w.RunID && taskScopeMatches(source, task) && reportBusinessContinuing(parent.Run.ID, w, source) {
			bound = true
		}
	}
	return matches == 1 && bound
}
func unresolvedWorkActions(source *Snapshot) bool {
	if source == nil || source.Run.Deleted || toolCleanupError(*source) != nil {
		return true
	}
	for _, action := range source.Actions {
		if action.Status == "started" || action.Status == "uncertain" || action.Status == "result_ready" {
			return true
		}
	}
	return false
}

func continuationSourceReady(parent *Snapshot, task PlanTask, source *Snapshot, successor ...string) bool {
	if task.ChildRunID == "" {
		if source == nil || source.Run.ID != parent.Run.ID {
			return false
		}
		return (taskDone(task) || settledStage(parent, task)) && !localWorkSourceUnresolved(source, task, successor...)
	}
	if unresolvedWorkActions(source) {
		return false
	}
	if taskDone(task) {
		return workProgressSourceBound(parent, task, source)
	}
	if !settledStage(parent, task) {
		return false
	}
	for _, w := range parent.Workers {
		if w.ID == task.WorkerID && w.RunID == task.ChildRunID && delegatedWorkerRetired(parent, task, w, source, true, false) {
			return true
		}
	}
	return false
}
func (a *Agent) checkContinuationsLocked(parent *Snapshot, task PlanTask) error {
	for _, ref := range task.Continues {
		if workProgressResolved(parent.Plan, ref, true) {
			return &WorkAlreadyResolvedError{Reference: ref}
		}
		sourceTask, err := continuationSourceTask(parent, ref)
		if err != nil {
			return err
		}
		source := parent
		if sourceTask.ChildRunID != "" {
			loaded, err := a.store.loadLocked(sourceTask.ChildRunID)
			if err != nil {
				return err
			}
			source = &loaded
		}
		if !continuationSourceReady(parent, sourceTask, source, task.ID) {
			return fmt.Errorf("continuation %s/%s needs the original settled identity and reconciled Actions; do not replay unknown effects", ref.TaskID, ref.ItemID)
		}
	}
	return nil
}

// WorkAlreadyResolvedError leaves an obsolete continuation unexecuted.
type WorkAlreadyResolvedError struct{ Reference WorkReference }

func (e *WorkAlreadyResolvedError) Error() string {
	return fmt.Sprintf("work %s/%s is already resolved; declare a new explicit obligation for new work", e.Reference.TaskID, e.Reference.ItemID)
}
