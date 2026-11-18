package agent

import (
	"fmt"
	"strings"
	"time"
)

// YieldWorker ends a visit, not the task. Reuse pause/resume and the same
// execution ledger; never replay an attempt to release an occupied slot.
func (a *Agent) YieldWorker(id, reason, checkpoint string) (Snapshot, error) {
	return a.YieldWorkerWithHandoff(id, reason, checkpoint, nil)
}

func (a *Agent) YieldWorkerWithHandoff(id, reason, checkpoint string, handoff *WorkerHandoff) (Snapshot, error) {
	if strings.TrimSpace(reason) == "" || strings.TrimSpace(checkpoint) == "" {
		return Snapshot{}, fmt.Errorf("yield requires a concrete reason and next-action checkpoint")
	}
	return a.store.Update(id, func(s *Snapshot) error {
		if s.Run.ParentRunID == "" || s.Run.ParentTaskID == "" {
			return fmt.Errorf("only a dispatched child can yield to its parent")
		}
		if isStopped(s.Run.Status) || s.Run.Status == StatusCompleted {
			return fmt.Errorf("worker run is not active")
		}
		if len(pendingToolCalls(s)) > 1 {
			return fmt.Errorf("worker_yield must be the last unresolved call; finish the tool batch first")
		}
		refreshVerification(s)
		accepted := len(s.Requirements) > 0 && len(unmetRequirements(s)) == 0
		if s.Run.SuccessContains != "" {
			matched := false
			for _, ev := range s.Evidence {
				output, ok := verifiedOutput(s, ev.ID)
				if ok && strings.Contains(output, s.Run.SuccessContains) {
					matched = true
					break
				}
			}
			accepted = matched && (len(s.Requirements) == 0 || len(unmetRequirements(s)) == 0)
		}
		for _, task := range s.Plan.Tasks {
			accepted = accepted && taskSatisfiedForRoot(s, task)
		}
		for _, w := range s.Workers {
			accepted = accepted && !workerUnsettled(s, w)
		}
		accepted = accepted && toolCleanupError(*s) == nil
		if accepted {
			return fmt.Errorf("declared acceptance already passed: finish this child with a final result, not worker_yield; runtime still checks plan, evidence and cleanup")
		}
		cp := &WorkerCheckpoint{Reason: reason, Summary: checkpoint, CreatedAt: time.Now().UTC(), WorkerHandoff: WorkerHandoff{NextAction: checkpoint}}
		if handoff != nil {
			cp.WorkerHandoff = *handoff
		}
		if err := validateCheckpoint(s, cp); err != nil {
			return err
		}
		s.WorkerCheckpoint = cp
		s.Run.Status = StatusPaused
		s.Run.PauseReason = "worker_yield: " + reason + "; checkpoint: " + checkpoint
		addEvent(s, "worker_yielded", "Worker visit yielded; task remains incomplete", map[string]string{"reason": reason, "checkpoint": checkpoint})
		return nil
	})
}
