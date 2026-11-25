// Package agentloop is Trace's model–tool loop. Its turn semantics follow the
// pi agent loop (packages/agent/src/agent-loop.ts, pi 5a14e69c0 / agent-core
// 1.4.0, MIT): prepare → assistant response → tool batch → finishTurn decision.
// Trace's Agent remains the authority for requests, actions, evidence, controls
// and worker settlement; this package only orders the calls into it.
package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// MaxParallelTools bounds one parallel tool batch.
const MaxParallelTools = 5

// Tool is one executable definition of the request that produced a turn.
// Parallel marks a tool proven safe to run concurrently with other Parallel
// tools; a batch runs in parallel only when every call is one.
type Tool struct {
	Name       string
	Parameters any
	Parallel   bool
}

// Call is one durable tool call exactly as the model returned it.
type Call struct {
	ID        string
	Name      string
	Arguments string
	// Settled calls are revalidated for the original batch budget, but never executed or emitted again.
	Settled bool
}

// Turn is the assistant response of one model request.
// Failed skips tools and asks Finish for a decision (pi stopReason "error").
type Turn struct {
	Calls      []Call
	StopReason string
	Failed     bool
	Tools      []Tool
}

// Outcome is the model-facing result of one call.
type Outcome struct {
	Content string
	IsError bool
	// ValidationError distinguishes current admission policy from an already recorded execution fact.
	ValidationError bool
}

// Hooks are the durable Trace transactions the loop orders. Returning the
// configured halt error from any hook stops the loop at a control boundary.
type Hooks interface {
	// Recover returns a pending tool batch that must settle before a new
	// model request, or nil. The batch always executes sequentially.
	Recover(ctx context.Context) (*Turn, error)
	Prepare(ctx context.Context) error
	Model(ctx context.Context) (Turn, error)
	// Execute runs one admitted call. manual marks a call to a tool that is
	// not in the turn's executable set; it is a handoff, never validated.
	Execute(ctx context.Context, call Call, manual, parallel bool) (Outcome, error)
	// Result commits the model-facing result, in declaration order.
	Result(ctx context.Context, call Call, outcome Outcome) error
	// Finish decides whether the run ends after this turn.
	Finish(ctx context.Context) (end bool, err error)
}

// Run drives the loop until Finish ends it, a hook fails, or ctx is done.
func Run(ctx context.Context, hooks Hooks, halt error) error {
	ended, err := run(ctx, hooks, halt)
	if ended && err == nil {
		// Finish already committed the end; a later cancel must not turn it into a failure.
		return nil
	}
	if ctx.Err() != nil {
		if err != nil && !IsHaltOnly(err, halt) && !errors.Is(err, context.Cause(ctx)) {
			return errors.Join(context.Cause(ctx), err)
		}
		if err != nil && !IsHaltOnly(err, halt) {
			return err
		}
		return context.Cause(ctx)
	}
	return err
}

// run reports ended=true only when Finish committed the end of the run.
func run(ctx context.Context, hooks Hooks, halt error) (bool, error) {
	for {
		for {
			if ctx.Err() != nil {
				return false, context.Cause(ctx)
			}
			recovery, err := hooks.Recover(ctx)
			if err != nil {
				return false, err
			}
			if recovery == nil {
				break
			}
			sequential := *recovery
			sequential.Tools = make([]Tool, len(recovery.Tools))
			for i, tool := range recovery.Tools {
				tool.Parallel = false
				sequential.Tools[i] = tool
			}
			if err = executeBatch(ctx, hooks, sequential, halt); err != nil {
				return false, err
			}
		}
		if err := hooks.Prepare(ctx); err != nil {
			return false, err
		}
		if ctx.Err() != nil {
			return false, context.Cause(ctx)
		}
		turn, err := hooks.Model(ctx)
		if err != nil {
			return false, err
		}
		if !turn.Failed && len(turn.Calls) > 0 {
			if err = executeBatch(ctx, hooks, turn, halt); err != nil {
				return false, err
			}
		}
		if ctx.Err() != nil && !turn.Failed {
			return false, context.Cause(ctx)
		}
		end, err := hooks.Finish(ctx)
		if err != nil || end {
			return end && err == nil, err
		}
	}
}

type prepared struct {
	call    Call
	manual  bool
	outcome *Outcome
	err     error
}

func executeBatch(ctx context.Context, hooks Hooks, turn Turn, halt error) error {
	tools := map[string]Tool{}
	for _, tool := range turn.Tools {
		tools[tool.Name] = tool
	}
	batch := make([]prepared, len(turn.Calls))
	parallel := len(turn.Calls) > 1
	// One work budget and schema cache for the whole batch: per-call limits
	// alone would scale with the number of calls the model emits.
	budget := &validationBudget{}
	for i, call := range turn.Calls {
		batch[i].call = call
		tool, known := tools[call.Name]
		parallel = parallel && known && tool.Parallel
		if turn.StopReason == "length" {
			// Output hit the token limit: arguments may be truncated.
			batch[i].outcome = &Outcome{IsError: true, ValidationError: true, Content: fmt.Sprintf("Tool call %q was not executed: the response hit the output token limit, so its arguments may be truncated. Re-issue the tool call with complete arguments.", call.Name)}
			continue
		}
		if !known {
			batch[i].manual = true
			continue
		}
		if err := validateArguments(call.Name, tool.Parameters, call.Arguments, budget); err != nil {
			batch[i].outcome = &Outcome{IsError: true, ValidationError: true, Content: err.Error()}
		}
	}
	if !parallel {
		for i := range batch {
			if batch[i].call.Settled {
				continue
			}
			if err := settle(ctx, hooks, &batch[i], false); err != nil {
				return err
			}
			if err := hooks.Result(ctx, batch[i].call, *batch[i].outcome); err != nil {
				return err
			}
		}
		return nil
	}
	var wait sync.WaitGroup
	var failed atomic.Bool
	var next atomic.Int64
	for range min(MaxParallelTools, len(batch)) {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for !failed.Load() {
				i := int(next.Add(1) - 1)
				if i >= len(batch) {
					return
				}
				item := &batch[i]
				if item.call.Settled || item.outcome != nil {
					continue
				}
				if failed.Load() {
					return
				}
				item.err = settle(ctx, hooks, item, true)
				if item.err != nil && !IsHaltOnly(item.err, halt) {
					failed.Store(true)
				}
			}
		}()
	}
	wait.Wait()
	var batchErr error
	for i := range batch {
		batchErr = combine(batchErr, batch[i].err, halt)
	}
	if batchErr != nil {
		return batchErr
	}
	for i := range batch {
		if batch[i].call.Settled {
			continue
		}
		if err := hooks.Result(ctx, batch[i].call, *batch[i].outcome); err != nil {
			return err
		}
	}
	return nil
}

func settle(ctx context.Context, hooks Hooks, item *prepared, parallel bool) error {
	if item.outcome != nil {
		return nil
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	outcome, err := hooks.Execute(ctx, item.call, item.manual, parallel)
	if err != nil {
		return err
	}
	outcome.IsError = outcome.IsError || failedContent(outcome.Content)
	item.outcome = &outcome
	return nil
}

// failedContent recognizes Trace tool results that report failure in-band.
func failedContent(content string) bool {
	var saved struct {
		ExitCode   *int `json:"exit_code"`
		Error      any  `json:"error"`
		MCPIsError bool `json:"mcp_is_error"`
	}
	if json.Unmarshal([]byte(content), &saved) != nil {
		return false
	}
	return saved.ExitCode != nil && *saved.ExitCode != 0 || truthy(saved.Error) || saved.MCPIsError
}

func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case float64:
		return v != 0
	}
	return true
}

// combine keeps real failures visible behind a concurrent control halt.
func combine(current, next, halt error) error {
	switch {
	case next == nil:
		return current
	case current == nil || IsHaltOnly(current, halt) && !IsHaltOnly(next, halt):
		return next
	case !IsHaltOnly(current, halt) && !IsHaltOnly(next, halt):
		return errors.Join(current, next)
	}
	return current
}

// IsHaltOnly recognizes a control boundary only when every leaf cause is halt.
// errors.Is alone also matches errors.Join(halt, persistenceFailure).
func IsHaltOnly(err, halt error) bool {
	if err == nil || halt == nil {
		return false
	}
	switch wrapped := err.(type) {
	case interface{ Unwrap() []error }:
		found := false
		for _, cause := range wrapped.Unwrap() {
			if cause == nil {
				continue
			}
			found = true
			if !IsHaltOnly(cause, halt) {
				return false
			}
		}
		return found
	case interface{ Unwrap() error }:
		return IsHaltOnly(wrapped.Unwrap(), halt)
	default:
		return errors.Is(err, halt)
	}
}
