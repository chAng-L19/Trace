package agent

// Muteki a73535a1 pentest/judgement.py binds tool-backed facts to their Worker
// and contract; ARTEX e6ec5699 traffic/evidence.go reads original bytes under
// its writer lock. Adapted to Trace's existing recursive single-Store verifier.
import (
	"sort"
	"strings"
)

// Only atomic acceptance data survives sourceFor's one-child archive window.
// No child Evidence/Action is inserted into the coordinator's execution ledger.
type delegatedObservation struct {
	Reference           EvidenceReference
	Output              string
	ToolID              string
	ArgumentsHash       string
	ArtifactOnly        bool
	HasArtifacts        bool
	Artifacts           []ArtifactRef
	CleanupRequirements map[string]bool
}

func delegatedAcceptanceEnabled(s *Snapshot) bool {
	return s.Run.Responsibility == ResponsibilityCoordinator && s.Run.ParentTaskID == ""
}

// All acceptance/review/result callers consume this same atomic source view.
// Resolution never performs Store I/O, including inside Update callbacks.
func delegatedObservationFor(s *Snapshot, id string) (delegatedObservation, bool) {
	if !delegatedAcceptanceEnabled(s) {
		return delegatedObservation{}, false
	}
	observation, ok := s.delegatedAcceptance[id]
	return observation, ok
}

func acceptanceEvidenceIDs(s *Snapshot) []string {
	ids := make([]string, 0, len(s.Evidence)+len(s.delegatedAcceptance))
	for _, e := range s.Evidence {
		ids = append(ids, e.ID)
	}
	if !delegatedAcceptanceEnabled(s) {
		return ids
	}
	qualified := make([]string, 0, len(s.delegatedAcceptance))
	for id := range s.delegatedAcceptance {
		qualified = append(qualified, id)
	}
	sort.Strings(qualified)
	return append(ids, qualified...)
}

func delegatedEvidenceLocators(s *Snapshot) []map[string]any {
	locators := []map[string]any{}
	for _, id := range acceptanceEvidenceIDs(s) {
		if o, ok := delegatedObservationFor(s, id); ok {
			locators = append(locators, map[string]any{"evidence_id": id, "reference": o.Reference, "tool_id": o.ToolID, "arguments_hash": o.ArgumentsHash, "artifact_only": o.ArtifactOnly})
		}
	}
	return locators
}

// source is already recursively verified by provenance.go, under its Store
// lock. Restrict acceptance further to the current, actually retired visit.
func delegatedWorkerRetired(root *Snapshot, task PlanTask, w AgentWorker, source *Snapshot, prior, zeroEffectsRetry bool) bool {
	// Match task_retry: cancellation can retire an uncertain failed attempt,
	// but never make it an acceptance evidence provider.
	cancelledFailure := prior && source != nil && source.Run.Status == StatusCancelled &&
		(task.Status == "blocked" || task.Status == "abandoned") && strings.TrimSpace(task.DispositionReason) != ""
	if source == nil || source.Run.Deleted || source.Run.Responsibility != ResponsibilityWorker ||
		source.Run.ParentRunID != root.Run.ID || source.Run.ParentTaskID != task.ID ||
		task.ChildRunID != source.Run.ID || task.EvidenceRunID != source.Run.ID && (!prior || task.EvidenceRunID != "") ||
		task.WorkerID != w.ID || task.ID != w.TaskID || w.RunID != source.Run.ID ||
		!taskScopeMatches(source, task) || !source.delegatedDescendantsRetired || toolCleanupError(*source) != nil ||
		len(pendingToolCalls(source)) > 0 && !cancelledFailure {
		return false
	}
	context := source.WorkerContext
	if context == nil || context.RunID != source.Run.ID || context.ParentRunID != root.Run.ID || context.TaskID != task.ID || context.WorkerID != w.ID {
		return false
	}
	if w.CleanupStatus != "completed" || w.SettledVisit == nil || *w.SettledVisit != w.Visit || workerUnsettled(root, w) {
		return false
	}
	receipt := source.WorkerSettlement
	if !receipt.matches(root.Run.ID, w) || receipt.CleanupStatus != "completed" || receipt.DescendantCleanupPending || receipt.DescendantSettlementPending {
		return false
	}
	if source.Run.Status == StatusRunning || source.Run.Status == StatusWaitingTool || source.Run.Status == StatusCreated {
		return false
	}
	for _, action := range source.Actions {
		if action.Status == "started" || (action.Status == "uncertain" || action.Status == "result_ready") && !cancelledFailure {
			return false
		}
	}
	// Only the paired, zero-effect RetryTask seal exempts the old attempt's
	// business probe. Resource cleanup and every identity/receipt guard remain.
	if !zeroEffectsRetry {
		for _, requirement := range source.Requirements {
			if requirement.Category == "cleanup" {
				check := latestCheck(source, requirement.ID)
				if check == nil || check.Status != "pass" {
					return false
				}
			}
		}
	}
	if receipt.BusinessSourceHash != "" && receipt.BusinessSourceHash != workerBusinessSourceHash(source) {
		return false
	}
	// Reject ambiguous registration rather than picking one binding by order.
	workers, tasks := 0, 0
	for _, candidate := range root.Workers {
		if candidate.RunID == source.Run.ID {
			workers++
		}
	}
	for _, candidate := range root.Plan.Tasks {
		if candidate.ID == task.ID {
			tasks++
		}
	}
	return workers == 1 && tasks == 1
}

func delegatedSourceBound(root *Snapshot, task PlanTask, w AgentWorker, source *Snapshot) bool {
	return delegatedWorkerRetired(root, task, w, source, false, false) && taskDone(task) && businessResultReady(source) &&
		source.WorkerSettlement.BusinessSourceHash != "" &&
		(w.Status == "completed" && source.Run.Status == StatusCompleted || reportBusinessSettled(root.Run.ID, w, source))
}

func collectDelegatedAcceptance(root *Snapshot, sourceFor func(string) *Snapshot) map[string]delegatedObservation {
	if !delegatedAcceptanceEnabled(root) {
		return nil
	}
	observations := map[string]delegatedObservation{}
	cleanupRequired := false
	for _, r := range root.Requirements {
		cleanupRequired = cleanupRequired || r.Category == "cleanup"
	}
	for _, task := range root.Plan.Tasks {
		if !taskDone(task) || task.EvidenceRunID == "" {
			continue
		}
		for _, w := range root.Workers {
			if w.ID != task.WorkerID || w.RunID != task.EvidenceRunID {
				continue
			}
			source := sourceFor(task.EvidenceRunID)
			if !delegatedSourceBound(root, task, w, source) {
				continue
			}
			ids := task.EvidenceIDs
			// Ordinary settled tasks have no machine predicate and therefore
			// no task evidence list. Use only the child's verified outcome refs,
			// still bound to this registered task/visit, never its whole ledger.
			if task.Status == "completed" && len(ids) == 0 && len(task.Requirements) == 0 && task.SuccessContains == "" {
				ids = source.Result.EvidenceIDs
			}
			outcomeIDs := append([]string(nil), ids...)
			ids = append(append([]string(nil), ids...), source.Result.ArtifactEvidenceIDs...)
			seen := map[string]bool{}
			for _, id := range ids {
				if seen[id] {
					continue
				}
				seen[id] = true
				// Direct original observations only; no transitive ID laundering.
				if strings.Contains(id, "/") {
					continue
				}
				artifactOnly := !hasString(outcomeIDs, id)
				output, issue := diagnoseResultEvidence(source, id, artifactOnly)
				if issue != nil {
					continue
				}
				for _, e := range source.Evidence {
					if e.ID != id {
						continue
					}
					for _, action := range source.Actions {
						if action.RequestID != e.RequestID || action.CallID != e.CallID {
							continue
						}
						qualified := source.Run.ID + "/" + id
						binding := valueHash(struct {
							Task                              PlanTask
							AcceptedIDs                       []string
							Worker                            AgentWorker
							ParentID, TaskID, WorkerID, RunID string
							Receipt                           *WorkerSettlement
							Evidence                          Evidence
							Action                            Action
						}{task, ids, w, source.Run.ParentRunID, source.Run.ParentTaskID, source.WorkerContext.WorkerID, source.Run.ID, source.WorkerSettlement, e, action})
						o := delegatedObservation{Reference: EvidenceReference{ID: qualified, Hash: e.ContentHash, SourceHash: binding}, Output: output, ToolID: action.Name, ArgumentsHash: promptHash(action.Arguments), ArtifactOnly: artifactOnly, HasArtifacts: action.Result != nil && len(action.Result.Artifacts) > 0, CleanupRequirements: map[string]bool{}}
						if action.Result != nil {
							o.Artifacts = append([]ArtifactRef(nil), action.Result.Artifacts...)
						}
						// Parent boundaries order visits; child boundaries locate the
						// original probe. A report-only resume cannot refresh it.
						probeVisit, visitProof := delegatedProbeVisit(root, w, source, e, action)
						if cleanupRequired && !artifactOnly && len(source.Workers) == 0 &&
							delegatedCleanupOrdered(root, task, w, probeVisit, sourceFor) {
							for _, requirement := range root.Requirements {
								if requirement.Category != "cleanup" {
									continue
								}
								// Apply the frozen root probes as the exclusions in the
								// child's own sequence domain. This also works when an
								// ordinary task had no child-level requirements.
								floorView := Snapshot{Run: source.Run, Actions: source.Actions, Events: source.Events, Evidence: source.Evidence, Requirements: root.Requirements}
								status, _ := checkPredicate(source, requirement.Predicate, []string{id}, requirement.ToolID, requirement.ArgumentsHash, cleanupFreshnessFloor(&floorView))
								o.CleanupRequirements[valueHash(requirement)] = status != "unknown"
							}
						}
						o.Reference.SourceHash = valueHash([]any{binding, artifactOnly, visitProof})
						observations[qualified] = o
						break
					}
					break
				}
			}
		}
	}
	return observations
}

// Cleanup is deliberately limited to a leaf Worker causally dispatched after
// every other registered Worker settled. No wall clocks or cross-Run seq are
// ordered. A concurrent cleanup probe remains unknown and must be recollected
// by a subsequent dependent Worker using the original exact cleanup contract.
func delegatedCleanupOrdered(root *Snapshot, task PlanTask, w AgentWorker, probeVisit uint64, sourceFor func(string) *Snapshot) bool {
	if probeVisit == 0 {
		return false
	}
	probeWorker := w
	probeWorker.Visit = probeVisit
	start, finish := delegatedWorkerBoundary(root, probeWorker)
	if start == 0 || finish <= start {
		return false
	}
	for _, action := range root.Actions {
		if observationAction(action) && !(action.Adapter == "builtin" && (action.Name == "read" || action.Name == "artifact_read")) || action.Name == "write" || action.Name == "edit" {
			return false // New coordinators perform target effects through Workers.
		}
	}
	for _, other := range root.Workers {
		if other.ID == w.ID && other.RunID == w.RunID {
			continue
		}
		var dependency *PlanTask
		for i := range root.Plan.Tasks {
			if root.Plan.Tasks[i].ID == other.TaskID {
				dependency = &root.Plan.Tasks[i]
				break
			}
		}
		otherSource := sourceFor(other.RunID)
		if dependency != nil && delegatedRetryNoEffects(root, *dependency, other, otherSource, start) {
			continue
		}
		if dependency == nil || !taskSatisfiedForRoot(root, *dependency) ||
			!(delegatedTaskDependsOn(root, task, other.TaskID, map[string]bool{}) || taskDisposed(*dependency) || settledStage(root, *dependency)) ||
			!delegatedWorkerRetired(root, *dependency, other, otherSource, true, false) {
			return false
		}
		otherStart, otherFinish := delegatedWorkerBoundary(root, other)
		if otherStart == 0 || otherFinish <= otherStart || otherFinish >= start {
			return false
		}
	}
	return true
}

func delegatedTaskDependsOn(root *Snapshot, task PlanTask, id string, visiting map[string]bool) bool {
	if visiting[task.ID] {
		return false
	}
	visiting[task.ID] = true
	defer delete(visiting, task.ID)
	for _, dep := range task.Dependencies {
		if dep == id {
			return true
		}
		for _, candidate := range root.Plan.Tasks {
			if candidate.ID == dep && delegatedTaskDependsOn(root, candidate, id, visiting) {
				return true
			}
		}
	}
	return false
}

func checkRequirement(s *Snapshot, r VerificationRequirement, ids []string) (string, string) {
	if r.Category == "cleanup" {
		for _, id := range ids {
			if o, ok := delegatedObservationFor(s, id); ok {
				if !o.CleanupRequirements[valueHash(r)] {
					return "unknown", "delegated cleanup needs the original fresh leaf probe after all other Workers settled and before its own settlement"
				}
			} else if status, reason := checkPredicate(s, r.Predicate, []string{id}, r.ToolID, r.ArgumentsHash, cleanupFreshnessFloor(s)); status == "unknown" {
				return status, reason
			}
		}
	}
	return checkPredicate(s, r.Predicate, ids, r.ToolID, r.ArgumentsHash, 0)
}
