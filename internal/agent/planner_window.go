package agent

import "fmt"

// Estimates are admission metadata, never a new budget or success predicate.
// Unknown (zero) estimates retain the existing checkpoint-only behavior.
type TaskWindowError struct {
	TaskID           string `json:"task_id"`
	BudgetRunID      string `json:"budget_run_id"`
	EstimatedSeconds int64  `json:"estimated_seconds"`
	AvailableMillis  int64  `json:"available_millis"`
}

func (e *TaskWindowError) Error() string {
	return fmt.Sprintf("task %s needs %ds of useful execution window; run %s has %dms after checkpoint reserve; keep pending or explicitly replan a smaller observable stage", e.TaskID, e.EstimatedSeconds, e.BudgetRunID, e.AvailableMillis)
}

// This is a local observation, not a cached admission decision. Claim and
// registration recheck all ancestor budgets under the existing Store lock.
func schedulingWindow(s *Snapshot) map[string]any {
	view := map[string]any{
		"scope":    "local run; dispatch also checks every worker ancestor",
		"limited":  s.Run.Budget.MaxSeconds > 0,
		"guidance": "Respect operator priority. At each available slot or new observation, reconsider unvisited work and checkpointed work using observed progress, dependencies and the remaining global window; do not keep a stale initial ranking or let unproductive revisits starve executable first visits. Prefer a concrete next step over an unsupported success estimate. Estimate seconds for the next observable stage, not a new worker solving limit. Do not start work without a useful window. expected_progress is not business success; acceptance requirements and cleanup remain independently verified.",
	}
	if s.Run.Budget.MaxSeconds > 0 {
		remaining := remainingBudget(s)
		reserve := checkpointReserve(s.Run.Budget)
		view["remaining_millis"] = max(int64(0), remaining.Milliseconds())
		view["checkpoint_reserve_millis"] = reserve.Milliseconds()
		view["available_millis"] = max(int64(0), (remaining - reserve).Milliseconds())
	}
	if s.Run.BudgetCheckpoint != nil {
		view["checkpoint"] = s.Run.BudgetCheckpoint
	}
	return view
}
