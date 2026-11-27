package agent

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"
)

// Only ordinary Store loads commit revocations. Report reads apply exactly the
// same verification to detached snapshots, including all recursive sources.
type sourceReadMode bool

const (
	readSourceOnly      sourceReadMode = false
	commitSourceChanges sourceReadMode = true
)

func (store *Store) loadVerifiedLocked(id string, visiting map[string]bool) (Snapshot, error) {
	return store.verifySourceLocked(id, visiting, commitSourceChanges)
}

func (store *Store) verifySourceLocked(id string, visiting map[string]bool, mode sourceReadMode) (Snapshot, error) {
	if visiting[id] {
		return Snapshot{}, fmt.Errorf("cyclic evidence provenance")
	}
	visiting[id] = true
	defer delete(visiting, id)
	snap, err := store.loadRawLocked(id)
	if err != nil {
		return Snapshot{}, err
	}
	if store.verifyDerivedLocked(&snap, visiting, mode) && mode == commitSourceChanges {
		snap.Run.UpdatedAt = time.Now().UTC()
		if err = store.archiveReports(&snap); err != nil {
			return Snapshot{}, err
		}
		if err = store.savePreparedLocked(snap); err != nil {
			return Snapshot{}, err
		}
	}
	return snap, nil
}

func evidenceSupports(source *Snapshot, ids []string, predicate string) bool {
	if source == nil || len(ids) == 0 {
		return false
	}
	matched := predicate == ""
	for _, id := range ids {
		output, valid := verifiedOutput(source, id)
		if !valid {
			return false
		}
		matched = matched || strings.Contains(output, predicate)
	}
	return matched
}

func taskDone(t PlanTask) bool {
	return t.Status == "verified" || (t.Status == "completed" && t.SuccessContains == "" && len(t.Requirements) == 0)
}

func (store *Store) refreshDerivedLocked(s *Snapshot, visiting map[string]bool) bool {
	return store.verifyDerivedLocked(s, visiting, commitSourceChanges)
}

func (store *Store) verifyDerivedLocked(s *Snapshot, visiting map[string]bool, mode sourceReadMode) bool {
	s.delegatedAcceptance = nil
	previousFrontier := s.Plan.Frontier
	previousWork := valueHash(s.Plan.WorkProgress)
	previousVerification := valueHash(struct {
		Checks   []VerificationCheck
		Findings []Finding
	}{s.Checks, s.Findings})
	changed := false
	sources := map[string]*Snapshot{s.Run.ID: s}
	lastChild := ""
	sourceFor := func(runID string) *Snapshot {
		if runID == "" {
			return s
		}
		if source, ok := sources[runID]; ok {
			return source
		}
		sources[runID] = nil
		for _, w := range s.Workers {
			if w.RunID != runID {
				continue
			}
			// Keep only the current child's complete verified source. Holding
			// every child's request archive until the parent check finishes
			// multiplies large-history memory. A later reference revalidates
			// through the same Store lock; the root and failed lookups stay put.
			if lastChild != "" {
				delete(sources, lastChild)
				lastChild = ""
			}
			source, err := store.verifySourceLocked(runID, visiting, mode)
			if err == nil && source.Run.ParentRunID == s.Run.ID && source.Run.ParentTaskID == w.TaskID {
				sources[runID] = &source
				lastChild = runID
			}
			break
		}
		return sources[runID]
	}
	invalidate := func(kind, id, runID, reason string) {
		changed = true
		addEvent(s, "verification_invalidated", "原始证据失效，已撤销派生验证", map[string]string{"kind": kind, "id": id, "source_run_id": runID, "reason": reason})
	}
	for i := range s.Plan.Tasks {
		task := &s.Plan.Tasks[i]
		if task.Status != "verified" && task.Status != "completed" {
			continue
		}
		valid := true
		if task.Status == "verified" {
			source := sourceFor(task.EvidenceRunID)
			valid = taskScopeMatches(source, *task) && (task.SuccessContains != "" || len(task.Requirements) > 0) && evidenceSupports(source, task.EvidenceIDs, task.SuccessContains) && taskRequirementsSatisfied(source, *task)
			if task.EvidenceRunID != "" && task.EvidenceRunID != s.Run.ID {
				valid = valid && taskScopeMatches(source, *task) && task.EvidenceRunID == task.ChildRunID && source != nil && source.Result.Status == "success" && sameTaskRequirements(source.Requirements, task.Requirements)
			}
		} else if task.ChildRunID != "" {
			source := sourceFor(task.ChildRunID)
			valid = taskScopeMatches(source, *task) && (source.Run.Status == StatusCompleted && source.Result.Status != "failed" || reportBusinessReady(source)) && len(task.Requirements) == 0
			// A settled turn stays completed in ordinary chat. A strict child
			// still needs the business success required at initial settlement.
			if valid && source.Run.RequireVerifiedCompletion {
				valid = source.Result.Status == "success"
			}
		}
		if valid && task.ChildRunID != "" {
			valid = false
			for _, w := range s.Workers {
				if w.ID == task.WorkerID && w.TaskID == task.ID && w.RunID == task.ChildRunID && (w.Status == "completed" && w.CleanupStatus == "completed" || reportBusinessSettled(s.Run.ID, w, sourceFor(task.ChildRunID)) || reportBusinessContinuing(s.Run.ID, w, sourceFor(task.ChildRunID))) {
					valid = true
					break
				}
			}
		}
		if !valid {
			task.Status = "uncertain"
			task.Error = "source execution evidence or child completion is no longer valid"
			invalidate("task", task.ID, task.EvidenceRunID, task.Error)
		}
	}
	// Revoke descendants to a fixed point regardless of task array order. A
	// running child's heartbeat sees uncertain and cancels, without replay.
	for again := true; again; {
		again = false
		done := map[string]bool{}
		for _, t := range s.Plan.Tasks {
			done[t.ID] = taskDone(t)
		}
		for i := range s.Plan.Tasks {
			task := &s.Plan.Tasks[i]
			if task.Status != "running" && !taskDone(*task) {
				continue
			}
			for _, dep := range task.Dependencies {
				if done[dep] {
					continue
				}
				task.Status = "uncertain"
				task.Error = "prerequisite " + dep + " no longer has a valid completion"
				invalidate("task", task.ID, task.EvidenceRunID, task.Error)
				again = true
				break
			}
			for _, ref := range task.Continues {
				sourceTask, err := continuationSourceTask(s, ref)
				if err != nil || !continuationSourceReady(s, sourceTask, sourceFor(sourceTask.ChildRunID), task.ID) {
					if task.Status != "uncertain" {
						task.Status = "uncertain"
						task.Error = "continuation source identity or Action settlement changed"
						invalidate("task", task.ID, ref.TaskID, task.Error)
						again = true
					}
					break
				}
			}
		}
	}
	for i := range s.Assets {
		asset := &s.Assets[i]
		if asset.Coverage != "observed" && asset.Coverage != "verified" && asset.Coverage != "negative" {
			continue
		}
		predicate := asset.Predicate
		if asset.Coverage == "observed" {
			predicate = ""
		}
		valid := evidenceSupports(sourceFor(asset.SourceRunID), asset.EvidenceIDs, predicate)
		if asset.Coverage != "observed" && predicate == "" {
			valid = false
		}
		if !valid {
			asset.Coverage = "uncertain"
			invalidate("asset", asset.ID, asset.SourceRunID, "source execution evidence no longer supports coverage")
		}
	}
	refreshFrontier(&s.Plan)
	previous := s.Result
	statuses := make([]string, len(s.Evidence))
	for i := range s.Evidence {
		statuses[i] = s.Evidence[i].Verification
	}
	s.delegatedDescendantsRetired = false
	if s.Run.Responsibility == ResponsibilityCoordinator || s.Run.Responsibility == ResponsibilityWorker {
		s.delegatedDescendantsRetired = delegatedDescendantsRetired(s, sourceFor)
	}
	s.delegatedAcceptance = collectDelegatedAcceptance(s, sourceFor)
	modern := s.Run.Responsibility == ResponsibilityCoordinator || s.Run.Responsibility == ResponsibilityWorker
	for i := range s.Plan.Tasks {
		task := &s.Plan.Tasks[i]
		if task.ChildRunID == "" {
			continue
		}
		if taskDisposed(*task) && !modern && len(task.WorkItems) == 0 && len(task.Remaining) == 0 && len(task.Continues) == 0 {
			continue
		}
		if mergeRemainingWork(task, sourceFor(task.ChildRunID)) {
			changed = true
		}
	}
	refreshWorkProgress(s, sourceFor)
	refreshResult(s)
	for i, e := range s.Evidence {
		if statuses[i] == e.Verification {
			continue
		}
		changed = true
		if statuses[i] == "verified" {
			invalidate("evidence", e.ID, s.Run.ID, "execution ledger or artifact changed")
		}
	}
	return changed || previousWork != valueHash(s.Plan.WorkProgress) || !slices.Equal(previousFrontier, s.Plan.Frontier) || !reflect.DeepEqual(previous, s.Result) || previousVerification != valueHash(struct {
		Checks   []VerificationCheck
		Findings []Finding
	}{s.Checks, s.Findings})
}
