package agent

// Muteki c60c8fee coordinator_progress.py:92-130,261-322 (AGPL-3.0)
// separates cooperative conclusion from the hard execution deadline. Trace
// sends durable worker messages and reuses worker_yield / the original child.
import (
	"context"
	"fmt"
	"time"
)

type BudgetCheckpoint struct {
	BudgetRunID string    `json:"budget_run_id"`
	RequestedAt time.Time `json:"requested_at"`
	Deadline    time.Time `json:"deadline"`
}
type CheckpointWindowError struct{ RunID string }

func (e *CheckpointWindowError) Error() string {
	return "run " + e.RunID + " is in its time-budget checkpoint window; new dispatch is closed"
}

func checkpointReserve(b Budget) time.Duration {
	if b.MaxSeconds <= 0 {
		return 0
	}
	seconds := b.CheckpointSeconds
	if seconds == 0 {
		return min(10*time.Second, time.Duration(b.MaxSeconds)*time.Second/10)
	}
	return time.Duration(min(seconds, b.MaxSeconds)) * time.Second
}
func remainingBudget(s *Snapshot) time.Duration {
	elapsed := s.Run.ElapsedMillis
	if !s.Run.BudgetActiveSince.IsZero() {
		elapsed += max(int64(0), time.Since(s.Run.BudgetActiveSince).Milliseconds())
	}
	return time.Duration(s.Run.Budget.MaxSeconds*1000-elapsed) * time.Millisecond
}
func (a *Agent) checkDispatchWindowLocked(s *Snapshot) error {
	return a.checkTaskDispatchWindowLocked(s, PlanTask{})
}

func (a *Agent) checkTaskDispatchWindowLocked(s *Snapshot, task PlanTask) error {
	seen := map[string]bool{}
	for {
		if seen[s.Run.ID] {
			return fmt.Errorf("checkpoint ancestry contains a cycle")
		}
		seen[s.Run.ID] = true
		if s.Run.Budget.MaxSeconds > 0 && remainingBudget(s) <= checkpointReserve(s.Run.Budget) {
			return &CheckpointWindowError{s.Run.ID}
		}
		if s.Run.Budget.MaxSeconds > 0 && task.EstimatedSeconds > 0 {
			available := remainingBudget(s) - checkpointReserve(s.Run.Budget)
			if time.Duration(task.EstimatedSeconds)*time.Second > available {
				return &TaskWindowError{TaskID: task.ID, BudgetRunID: s.Run.ID, EstimatedSeconds: task.EstimatedSeconds, AvailableMillis: max(int64(0), available.Milliseconds())}
			}
		}
		if s.Run.ParentTaskID == "" || s.Run.ParentRunID == "" {
			return nil
		}
		parent, err := a.store.loadRawLocked(s.Run.ParentRunID)
		if err != nil {
			return err
		}
		s = &parent
	}
}

// Only this Run's explicit time budget creates a timer. Parent notices travel
// through the existing durable worker inbox, not a second runtime queue.
func (a *Agent) startBudgetCheckpoint(ctx context.Context, snap Snapshot, fail context.CancelCauseFunc) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	if snap.Run.Budget.MaxSeconds <= 0 {
		close(done)
		return func() {}
	}
	remaining := remainingBudget(&snap)
	deadline := time.Now().UTC().Add(remaining)
	timer := time.NewTimer(max(time.Duration(0), remaining-checkpointReserve(snap.Run.Budget)))
	go func() {
		defer close(done)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-timer.C:
		}
		if err := a.requestBudgetCheckpoint(ctx, snap.Run.ID, BudgetCheckpoint{BudgetRunID: snap.Run.ID, RequestedAt: time.Now().UTC(), Deadline: deadline}, map[string]bool{}); err != nil {
			fail(fmt.Errorf("persist budget checkpoint: %w", err))
		}
	}()
	return func() { close(stop); <-done }
}
func (a *Agent) requestBudgetCheckpoint(ctx context.Context, id string, notice BudgetCheckpoint, seen map[string]bool) error {
	if seen[id] {
		return fmt.Errorf("checkpoint worker tree contains a cycle")
	}
	seen[id] = true
	var children []string
	_, err := a.store.Update(id, func(s *Snapshot) error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if isStopped(s.Run.Status) || s.Run.Status == StatusCompleted {
			return nil
		}
		if previous := s.Run.BudgetCheckpoint; previous != nil && !previous.Deadline.After(notice.Deadline) {
			return nil
		}
		s.Run.BudgetCheckpoint = &notice
		addEvent(s, "budget_checkpoint_requested", "显式预算即将到期：停止新派发并保存续跑 checkpoint；硬截止不延长", notice)
		for _, w := range s.Workers {
			if w.Status != "running" {
				continue
			}
			content := fmt.Sprintf("Runtime time-budget checkpoint request. Deadline: %s. Stop dispatching new work. Finish the current safe boundary and call worker_yield with a concrete checkpoint and next action. This is not task completion or cleanup proof; the hard deadline is unchanged.", notice.Deadline.Format(time.RFC3339Nano))
			if err := appendWorkerMessage(s, "parent", w.ID, content); err != nil {
				return err
			}
			children = append(children, w.RunID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, child := range children {
		if err := a.requestBudgetCheckpoint(ctx, child, notice, seen); err != nil {
			return err
		}
	}
	return nil
}
