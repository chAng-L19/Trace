package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

type runTimeBudgetKey struct{}

type selectedTimeBudget struct {
	deadline time.Time
	cause    *timeBudgetCause
}

const workerBudgetHeading = "\nWorker time budget (JSON):\n"

func workerBudgetObservation(ctx context.Context, run Run, projectedContext string, now time.Time) (string, error) {
	budget, ok := ctx.Value(runTimeBudgetKey{}).(selectedTimeBudget)
	if !ok || run.ParentRunID == "" || run.ParentTaskID == "" {
		return "", nil
	}
	// Only the revalidated request projection can supply an estimate. A missing
	// or unreadable estimate stays unknown (0), without hiding the real budget.
	var seconds int64
	if _, body, found := strings.Cut(projectedContext, "\n"); found {
		var stage struct {
			EstimatedSeconds int64 `json:"estimated_seconds"`
		}
		if json.Unmarshal([]byte(body), &stage) == nil {
			seconds = stage.EstimatedSeconds
		}
	}
	data, err := json.Marshal(projectWorkerBudget(seconds, run.ID, budget.deadline, budget.cause, now))
	if err != nil {
		return "", err
	}
	return workerBudgetHeading + string(data), nil
}

// Request-only timing data. The deadline and cause are the pair selected by
// beginBudgetContext, not the deadline of a possibly shorter caller context.
type workerBudgetProjection struct {
	EstimatedSeconds   int64               `json:"estimated_seconds"`
	EstimateIsDeadline bool                `json:"estimate_is_deadline"`
	EffectiveBudget    projectedTimeBudget `json:"effective_budget"`
}

type projectedTimeBudget struct {
	Limited         bool       `json:"limited"`
	Source          string     `json:"source"`
	SourceRunID     string     `json:"source_run_id,omitempty"`
	RemainingMillis *int64     `json:"remaining_millis,omitempty"`
	Deadline        *time.Time `json:"deadline,omitempty"`
}

func projectWorkerBudget(estimatedSeconds int64, workerRunID string, deadline time.Time, cause *timeBudgetCause, now time.Time) workerBudgetProjection {
	view := workerBudgetProjection{EstimatedSeconds: estimatedSeconds, EffectiveBudget: projectedTimeBudget{Source: "none"}}
	if cause == nil {
		return view
	}
	source := "own"
	if cause.RunID != workerRunID {
		source = "ancestor"
	}
	remaining := max(int64(0), deadline.Sub(now).Milliseconds())
	utc := deadline.UTC()
	view.EffectiveBudget = projectedTimeBudget{Limited: true, Source: source, SourceRunID: cause.RunID, RemainingMillis: &remaining, Deadline: &utc}
	return view
}
