package agent

import "encoding/json"

// These are parent-domain dispatch/settlement fences. SourceFloor alone is in
// the child's sequence domain; it is compared only with original child events.
type delegatedVisitBoundary struct {
	Visit                      uint64
	Start, Finish, SourceFloor int
}

func delegatedVisitBoundaries(root *Snapshot, w AgentWorker) ([]delegatedVisitBoundary, bool) {
	var visits []delegatedVisitBoundary
	lastSequence := 0
	for _, event := range root.Events {
		if event.Sequence <= lastSequence {
			return nil, false
		}
		lastSequence = event.Sequence
		raw, _ := json.Marshal(event.Data)
		switch event.Type {
		case "worker_started", "worker_resumed":
			var registered AgentWorker
			if json.Unmarshal(raw, &registered) != nil || registered.ID != w.ID || registered.RunID != w.RunID || registered.TaskID != w.TaskID {
				continue
			}
			if registered.Visit != uint64(len(visits)+1) {
				return nil, false
			}
			floor := 0
			if registered.SourceEventFloor != nil {
				floor = *registered.SourceEventFloor
			} else if registered.Visit != 1 {
				return nil, false
			}
			if floor < 0 || registered.Visit == 1 && (floor != 0 || event.Type != "worker_started") {
				return nil, false
			}
			if len(visits) > 0 {
				previous := visits[len(visits)-1]
				if event.Type != "worker_resumed" || previous.Finish <= previous.Start || floor < previous.SourceFloor {
					return nil, false
				}
			}
			visits = append(visits, delegatedVisitBoundary{Visit: registered.Visit, Start: event.Sequence, SourceFloor: floor})
		case "worker_finished", "worker_cleanup_reconciled":
			var fields map[string]string
			if json.Unmarshal(raw, &fields) == nil && fields["worker_id"] == w.ID && fields["run_id"] == w.RunID && fields["cleanup_status"] == "completed" && len(visits) > 0 {
				visits[len(visits)-1].Finish = event.Sequence
			}
		}
	}
	if len(visits) == 0 || visits[len(visits)-1].Visit != w.Visit {
		return nil, false
	}
	latest := visits[len(visits)-1]
	if w.SourceEventFloor != nil {
		if *w.SourceEventFloor != latest.SourceFloor {
			return nil, false
		}
	} else if w.Visit != 1 {
		return nil, false
	}
	return visits, true
}

func delegatedProbeVisit(visits []delegatedVisitBoundary, ok bool, source *Snapshot, e Evidence, action Action) (uint64, string) {
	start := actionStartSequence(source, action)
	if !ok || start == 0 || e.SourceEventSequence <= start {
		return 0, ""
	}
	for _, visit := range visits {
		if visit.SourceFloor > source.Run.EventSeq {
			return 0, ""
		}
	}
	for i, visit := range visits {
		if start <= visit.SourceFloor {
			continue
		}
		if i+1 < len(visits) && e.SourceEventSequence > visits[i+1].SourceFloor {
			continue
		}
		if visit.Finish <= visit.Start {
			return 0, ""
		}
		return visit.Visit, valueHash(visits)
	}
	return 0, ""
}

func delegatedWorkerBoundary(root *Snapshot, w AgentWorker) (start, finish int) {
	// Locate historical starts without pretending the current registration has
	// that old visit's watermark. The full current chain is checked by callers.
	for _, event := range root.Events {
		raw, _ := json.Marshal(event.Data)
		switch event.Type {
		case "worker_started", "worker_resumed":
			var registered AgentWorker
			if json.Unmarshal(raw, &registered) == nil && registered.ID == w.ID && registered.RunID == w.RunID && registered.TaskID == w.TaskID {
				if start != 0 {
					return start, finish
				}
				if registered.Visit == w.Visit {
					start = event.Sequence
				}
			}
		case "worker_finished", "worker_cleanup_reconciled":
			var fields map[string]string
			if start > 0 && json.Unmarshal(raw, &fields) == nil && fields["worker_id"] == w.ID && fields["run_id"] == w.RunID && fields["cleanup_status"] == "completed" {
				finish = event.Sequence
			}
		}
	}
	return
}

// sourceFor is provenance.go's recursive, registration-scoped resolver. Keep a
// single boolean on the verified snapshot, never a map of descendant archives.
func delegatedDescendantsRetired(s *Snapshot, sourceFor func(string) *Snapshot) bool {
	if toolCleanupError(*s) != nil {
		return false
	}
	for _, task := range s.Plan.Tasks {
		if task.Status == "running" {
			return false
		}
	}
	for _, w := range s.Workers {
		if workerUnsettled(s, w) || w.SettledVisit == nil || *w.SettledVisit != w.Visit {
			return false
		}
		child := sourceFor(w.RunID)
		if child == nil || child.Run.Deleted || child.Run.ParentRunID != s.Run.ID || child.Run.ParentTaskID != w.TaskID ||
			!child.WorkerSettlement.matches(s.Run.ID, w) || child.WorkerSettlement.CleanupStatus != "completed" ||
			child.WorkerSettlement.DescendantCleanupPending || child.WorkerSettlement.DescendantSettlementPending || !child.delegatedDescendantsRetired {
			return false
		}
	}
	return true
}

// RetryTask preserves a superseded Worker while rebinding the same PlanTask.
// Only its paired zero-effect seal can prove this historical attempt harmless;
// the live task binding and all acceptance observations still belong to the new Worker.
func delegatedRetryNoEffects(root *Snapshot, current PlanTask, w AgentWorker, source *Snapshot, probeStart int) bool {
	if w.Status != "superseded" || source == nil || source.Run.Status != StatusCancelled ||
		!taskDone(current) || current.WorkerID == "" || current.ChildRunID == "" ||
		current.WorkerID == w.ID || current.ChildRunID == w.RunID || current.ID != w.TaskID ||
		len(source.Actions) != 0 || len(source.Evidence) != 0 || len(source.Workers) != 0 || len(pendingToolCalls(source)) != 0 {
		return false
	}
	visits, valid := delegatedVisitBoundaries(root, w)
	if !valid {
		return false
	}
	last := visits[len(visits)-1]
	if last.Finish <= last.Start {
		return false
	}
	retrySequence, retries, seals := 0, 0, 0
	for _, event := range root.Events {
		if event.Type != "task_retry_requested" {
			continue
		}
		raw, _ := json.Marshal(event.Data)
		var fields map[string]string
		if json.Unmarshal(raw, &fields) == nil && fields["task_id"] == w.TaskID && fields["previous_worker_id"] == w.ID && fields["previous_run_id"] == w.RunID {
			retries++
			retrySequence = event.Sequence
		}
	}
	for _, event := range source.Events {
		if event.Type != "worker_retry_superseded" {
			continue
		}
		raw, _ := json.Marshal(event.Data)
		var fields map[string]string
		if json.Unmarshal(raw, &fields) == nil && fields["parent_run_id"] == root.Run.ID && fields["task_id"] == w.TaskID && event.Sequence > 0 && event.Sequence <= source.Run.EventSeq {
			seals++
		}
	}
	if retries != 1 || seals != 1 || retrySequence <= last.Finish || retrySequence >= probeStart {
		return false
	}
	// Reconstruct only the already-proven historical identity for the shared
	// retirement guard. Never install it in the plan or expose it as outcome evidence.
	historical := current
	historical.WorkerID, historical.ChildRunID, historical.EvidenceRunID = w.ID, w.RunID, w.RunID
	return delegatedWorkerRetired(root, historical, w, source, true, true)
}
