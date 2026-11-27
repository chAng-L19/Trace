package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"sync"
	"time"
)

// Limits are opt-in. Long tasks must not stop at an implicit model-round cap.
func DefaultBudget() Budget { return Budget{} }

const maxBudgetSeconds = int64(math.MaxInt64) / int64(time.Second)

// ValidateBudget bounds durations by their actual representation, not by an
// implicit task limit. Zero remains unlimited.
func ValidateBudget(b Budget) error {
	if b.MaxModelRequests < 0 || b.MaxActions < 0 || b.MaxSeconds < 0 || b.CheckpointSeconds < 0 {
		return errors.New("budget limits must be non-negative; 0 means unlimited")
	}
	if b.MaxSeconds > maxBudgetSeconds {
		return fmt.Errorf("max_seconds exceeds duration range (%d)", maxBudgetSeconds)
	}
	if b.CheckpointSeconds > maxBudgetSeconds {
		return fmt.Errorf("checkpoint_seconds exceeds duration range")
	}
	return nil
}

// SetRunBudget changes only this Run. The run lock also excludes execution
// owned by another CLI process; changing an active timer requires a pause.
func (a *Agent) SetRunBudget(id string, budget Budget) (Snapshot, error) {
	if err := ValidateBudget(budget); err != nil {
		return Snapshot{}, err
	}
	release, err := a.store.AcquireRunLock(id)
	if err != nil {
		return Snapshot{}, err
	}
	defer release()
	return a.store.Update(id, func(s *Snapshot) error {
		if s.Run.Deleted {
			return os.ErrNotExist
		}
		if s.Run.Status == StatusRunning || s.Run.Status == StatusWaitingTool {
			return errors.New("pause the run before changing its budget")
		}
		if s.Run.BudgetConfigured && s.Run.Budget == budget {
			return nil
		}
		s.Run.Budget = budget
		s.Run.BudgetConfigured = true
		addEvent(s, "budget_updated", "此运行的预算已更新；0 表示不限", budget)
		return nil
	})
}

type timeBudgetCause struct{ RunID string }

func (e *timeBudgetCause) Error() string { return "trace time budget exhausted" }

func BudgetFromEnv() (BudgetOverride, error) {
	var override BudgetOverride
	for _, field := range []struct {
		name   string
		target **int
	}{{"TRACE_MAX_ROUNDS", &override.MaxModelRequests}, {"TRACE_MAX_ACTIONS", &override.MaxActions}} {
		if text, ok := os.LookupEnv(field.name); ok {
			value, err := strconv.Atoi(text)
			if err != nil || value < 0 {
				return override, fmt.Errorf("%s must be a non-negative integer", field.name)
			}
			*field.target = &value
		}
	}
	if text, ok := os.LookupEnv("TRACE_MAX_SECONDS"); ok {
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil || value < 0 || value > maxBudgetSeconds {
			return override, fmt.Errorf("TRACE_MAX_SECONDS must be 0..%d", maxBudgetSeconds)
		}
		override.MaxSeconds = &value
	}
	if text, ok := os.LookupEnv("TRACE_CHECKPOINT_SECONDS"); ok {
		value, err := strconv.ParseInt(text, 10, 64)
		if err != nil || value < 0 || value > maxBudgetSeconds {
			return override, fmt.Errorf("TRACE_CHECKPOINT_SECONDS must be 0..%d", maxBudgetSeconds)
		}
		override.CheckpointSeconds = &value
	}
	return override, nil
}

func (a *Agent) ConfigureBudget(override BudgetOverride) { a.budget = override }
func (a *Agent) SetSuccessContains(value string)         { a.successContains = value }

func applyBudget(s *Snapshot, override BudgetOverride) {
	if s.Run.BudgetConfigured {
		return
	}
	before := s.Run.Budget
	if override.MaxModelRequests != nil {
		s.Run.Budget.MaxModelRequests = *override.MaxModelRequests
	}
	if override.MaxActions != nil {
		s.Run.Budget.MaxActions = *override.MaxActions
	}
	if override.MaxSeconds != nil {
		s.Run.Budget.MaxSeconds = *override.MaxSeconds
	}
	if override.CheckpointSeconds != nil {
		s.Run.Budget.CheckpointSeconds = *override.CheckpointSeconds
	}
	if before != s.Run.Budget {
		addEvent(s, "budget_updated", "运行预算已更新", s.Run.Budget)
	}
}

func refreshUsage(s *Snapshot) {
	usage := UsageStats{ModelRequests: len(s.Requests), ModelResponses: len(s.Responses), ToolActions: len(s.Actions)}
	for _, record := range s.Responses {
		tokens := record.Response.Usage
		usage.PromptCache.add(tokens)
		if tokens == nil {
			usage.MissingUsageResponses++
			continue
		}
		if tokens.InputTokens == nil || tokens.OutputTokens == nil || tokens.TotalTokens == nil {
			usage.MissingUsageResponses++
		}
		sumToken(&usage.KnownInputTokens, tokens.InputTokens)
		sumToken(&usage.KnownOutputTokens, tokens.OutputTokens)
		sumToken(&usage.KnownTotalTokens, tokens.TotalTokens)
	}
	s.Run.Usage = usage
}
func sumToken(sum **int64, value *int64) {
	if value == nil {
		return
	}
	if *sum == nil {
		v := int64(0)
		*sum = &v
	}
	**sum += *value
}

func budgetReason(s *Snapshot, kind string) string {
	elapsed := s.Run.ElapsedMillis
	if !s.Run.BudgetActiveSince.IsZero() {
		elapsed += time.Since(s.Run.BudgetActiveSince).Milliseconds()
	}
	if s.Run.Budget.MaxSeconds > 0 && elapsed >= s.Run.Budget.MaxSeconds*1000 {
		return "time budget exhausted"
	}
	if kind == "model" && s.Run.Budget.MaxModelRequests > 0 && len(s.Requests) >= s.Run.Budget.MaxModelRequests {
		return "model request budget exhausted"
	}
	if kind == "action" && s.Run.Budget.MaxActions > 0 && len(s.Actions) >= s.Run.Budget.MaxActions {
		return "tool action budget exhausted"
	}
	return ""
}

func settleBudgetTime(s *Snapshot) {
	if s.Run.BudgetActiveSince.IsZero() {
		return
	}
	delta := time.Since(s.Run.BudgetActiveSince).Milliseconds()
	if delta > 0 {
		s.Run.ElapsedMillis += delta
	}
	s.Run.BudgetActiveSince = time.Time{}
}
func pauseBudget(s *Snapshot, reason string) {
	s.Run.Status = StatusPaused
	s.Run.PauseReason = reason
	s.Run.Error = ""
	addEvent(s, "budget_paused", "运行预算耗尽，已暂停", map[string]string{"reason": reason})
}

// Account cumulative active wall time, including interruptions; operator pauses do not reset it.
func (a *Agent) beginBudgetContext(parent context.Context, id string) (context.Context, func(), error) {
	var deadline time.Time
	var deadlineCause *timeBudgetCause
	snap, err := a.store.Update(id, func(s *Snapshot) error {
		s.Run.BudgetActiveSince = time.Now().UTC()
		s.Run.BudgetCheckpoint = nil
		current := s
		seen := map[string]bool{}
		for {
			if seen[current.Run.ID] {
				return fmt.Errorf("budget deadline ancestry contains a cycle")
			}
			seen[current.Run.ID] = true
			if current.Run.Budget.MaxSeconds > 0 {
				left := remainingBudget(current)
				candidate := time.Now().Add(left)
				if deadlineCause == nil || candidate.Before(deadline) {
					deadline, deadlineCause = candidate, &timeBudgetCause{RunID: current.Run.ID}
				}
			}
			if current.Run.ParentTaskID == "" || current.Run.ParentRunID == "" {
				break
			}
			ancestor, err := a.store.loadRawLocked(current.Run.ParentRunID)
			if err != nil {
				return err
			}
			current = &ancestor
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	ctx, checkpointFailure := context.WithCancelCause(parent)
	cancel := func() {}
	if deadlineCause != nil {
		ctx, cancel = context.WithDeadlineCause(ctx, deadline, deadlineCause)
	}
	ctx = context.WithValue(ctx, runTimeBudgetKey{}, selectedTimeBudget{deadline: deadline, cause: deadlineCause})
	controlled, stop := a.controlledContext(ctx, id)
	stopCheckpoint := a.startBudgetCheckpoint(controlled, snap, checkpointFailure)
	var once sync.Once
	return controlled, func() {
		once.Do(func() {
			stopCheckpoint()
			stop()
			cancel()
			checkpointFailure(nil)
			a.store.Update(id, func(s *Snapshot) error { settleBudgetTime(s); return nil })
		})
	}, nil
}

func (a *Agent) pauseIfBudgetTime(id string, ctx context.Context) (bool, error) {
	var cause *timeBudgetCause
	if !errors.As(context.Cause(ctx), &cause) {
		return false, nil
	}
	snap, loadErr := a.store.Load(id)
	if loadErr != nil {
		return false, loadErr
	}
	if cause.RunID == id && snap.Run.Budget.MaxSeconds == 0 {
		return false, nil
	}
	_, err := a.store.Update(id, func(s *Snapshot) error {
		pauseTimeBudget(s, cause)
		return nil
	})
	return err == nil, err
}

func pauseTimeBudget(s *Snapshot, cause *timeBudgetCause) {
	if isStopped(s.Run.Status) || cause.RunID == s.Run.ID && s.Run.Budget.MaxSeconds == 0 {
		return
	}
	if cause.RunID == s.Run.ID {
		pauseBudget(s, "time budget exhausted")
	} else {
		s.Run.Status, s.Run.PauseReason, s.Run.Error = StatusPaused, "parent_pause", ""
		addEvent(s, "run_paused", "祖先运行预算耗尽，原 Worker 等待父任务恢复", map[string]string{"budget_run_id": cause.RunID})
	}
}
