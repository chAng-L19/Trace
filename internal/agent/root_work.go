package agent

import "fmt"

// $run names declarations in the root's original assistant messages. This
// source exists only while deriving checks; it is never a persisted PlanTask.
const rootWorkTaskID = "$run"

func rootWorkSource(parent *Snapshot) (PlanTask, bool) {
	if parent.Run.ParentRunID != "" || parent.Run.ParentTaskID != "" {
		return PlanTask{}, false
	}
	items := remainingDeclarations(parent)
	if len(items) == 0 {
		return PlanTask{}, false
	}
	return PlanTask{ID: rootWorkTaskID, Status: "completed", EvidenceRunID: parent.Run.ID,
		EvidenceScope: executionEvidenceScope(parent), Remaining: items}, true
}

func continuationSourceTask(parent *Snapshot, ref WorkReference) (PlanTask, error) {
	if ref.TaskID != rootWorkTaskID {
		task, err := planTask(parent, ref.TaskID)
		if err != nil {
			return PlanTask{}, err
		}
		return *task, nil
	}
	if source, exists := rootWorkSource(parent); exists {
		for _, item := range source.Remaining {
			if item.ID == ref.ItemID {
				return source, nil
			}
		}
	}
	return PlanTask{}, fmt.Errorf("unknown root remaining work %s/%s", ref.TaskID, ref.ItemID)
}

func validateRootWorkLinks(parent *Snapshot, tasks []PlanTask) error {
	sources := tasks
	if root, exists := rootWorkSource(parent); exists {
		sources = append(append([]PlanTask(nil), tasks...), root)
	}
	return validateWorkLinks(sources)
}

func rootWorkProgressGaps(s *Snapshot) []string {
	if s.Run.ParentRunID != "" || s.Run.ParentTaskID != "" {
		return nil
	}
	declarations, issues := collectRemainingDeclarations(s)
	var gaps []string
	for _, issue := range issues {
		gaps = append(gaps, issue.Repair)
	}
	for _, item := range declarations {
		ref := WorkReference{TaskID: rootWorkTaskID, ItemID: item.ID}
		if !workProgressResolved(s.Plan, ref, true) {
			gaps = append(gaps, "remaining work requires a settled continuation or explicit stage disposition: "+ref.TaskID+"/"+ref.ItemID)
		}
	}
	return gaps
}

func pendingRemainingDeclarations(s *Snapshot, items []RemainingWork) []RemainingWork {
	if s.Run.ParentRunID != "" || s.Run.ParentTaskID != "" {
		return items
	}
	var pending []RemainingWork
	for _, item := range items {
		if !workProgressResolved(s.Plan, WorkReference{TaskID: rootWorkTaskID, ItemID: item.ID}, true) {
			pending = append(pending, item)
		}
	}
	return pending
}
