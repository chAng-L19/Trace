package agentloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fake implements Hooks with per-test overrides.
type fake struct {
	mu       sync.Mutex
	recover  func() (*Turn, error)
	prepare  func() error
	model    func() (Turn, error)
	execute  func(Call, bool, bool) (Outcome, error)
	finish   func() (bool, error)
	log      []string
	results  []string
	outcomes map[string]Outcome
}

func (f *fake) record(entry string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, entry)
}

func (f *fake) Recover(context.Context) (*Turn, error) {
	if f.recover != nil {
		return f.recover()
	}
	return nil, nil
}

func (f *fake) Prepare(context.Context) error {
	f.record("prepare")
	if f.prepare != nil {
		return f.prepare()
	}
	return nil
}

func (f *fake) Model(context.Context) (Turn, error) {
	f.record("model")
	return f.model()
}

func (f *fake) Execute(_ context.Context, call Call, manual, parallel bool) (Outcome, error) {
	f.record("execute:" + call.ID)
	if f.execute != nil {
		return f.execute(call, manual, parallel)
	}
	return Outcome{Content: "observed"}, nil
}

func (f *fake) Result(_ context.Context, call Call, outcome Outcome) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results = append(f.results, call.ID)
	if f.outcomes == nil {
		f.outcomes = map[string]Outcome{}
	}
	f.outcomes[call.ID] = outcome
	return nil
}

func (f *fake) Finish(context.Context) (bool, error) {
	f.record("finish")
	if f.finish != nil {
		return f.finish()
	}
	return true, nil
}

var readTool = Tool{Name: "read", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}, Parallel: true}

func calls(n int, name string) []Call {
	out := []Call{}
	for i := 0; i < n; i++ {
		out = append(out, Call{ID: fmt.Sprint(i), Name: name, Arguments: "{}"})
	}
	return out
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestParallelBoundedAndOrdered(t *testing.T) {
	var mu sync.Mutex
	active, peak := 0, 0
	f := &fake{model: func() (Turn, error) {
		return Turn{Calls: calls(7, "read"), Tools: []Tool{readTool}}, nil
	}}
	f.execute = func(call Call, manual, parallel bool) (Outcome, error) {
		if manual || !parallel {
			t.Errorf("read batch not parallel: manual=%v parallel=%v", manual, parallel)
		}
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		time.Sleep(120 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return Outcome{Content: "observed"}, nil
	}
	started := time.Now()
	if err := Run(testContext(t), f, errors.New("halt")); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if peak != MaxParallelTools || fmt.Sprint(f.results) != "[0 1 2 3 4 5 6]" {
		t.Fatalf("peak=%d order=%v", peak, f.results)
	}
	if elapsed > 600*time.Millisecond {
		t.Fatalf("batch stayed sequential: %s", elapsed)
	}
}

func TestMixedBatchRunsSequentially(t *testing.T) {
	f := &fake{model: func() (Turn, error) {
		return Turn{Calls: []Call{{ID: "a", Name: "read", Arguments: "{}"}, {ID: "b", Name: "exec", Arguments: "{}"}},
			Tools: []Tool{readTool, {Name: "exec", Parameters: map[string]any{"type": "object"}}}}, nil
	}}
	f.execute = func(_ Call, _ bool, parallel bool) (Outcome, error) {
		if parallel {
			t.Error("mixed batch executed in parallel")
		}
		return Outcome{Content: "ok"}, nil
	}
	if err := Run(testContext(t), f, errors.New("halt")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.log, ","); got != "prepare,model,execute:a,execute:b,finish" {
		t.Fatalf("sequence=%s", got)
	}
}

func TestParallelHaltDoesNotHidePersistenceFailure(t *testing.T) {
	halt, disk := errors.New("controlled pause"), errors.New("fixture CAS commit failed")
	entered := make(chan struct{}, 2)
	f := &fake{model: func() (Turn, error) {
		return Turn{Calls: []Call{{ID: "pause", Name: "read", Arguments: "{}"}, {ID: "disk", Name: "read", Arguments: "{}"}}, Tools: []Tool{readTool}}, nil
	}}
	f.execute = func(call Call, _, _ bool) (Outcome, error) {
		entered <- struct{}{}
		for len(entered) < 2 {
			time.Sleep(time.Millisecond)
		}
		if call.ID == "pause" {
			return Outcome{}, halt
		}
		time.Sleep(30 * time.Millisecond)
		return Outcome{}, disk
	}
	err := Run(testContext(t), f, halt)
	if !errors.Is(err, disk) || IsHaltOnly(err, halt) || len(f.results) != 0 {
		t.Fatalf("real persistence error masked: %v results=%v", err, f.results)
	}
}

func TestParallelPureHaltAllowsLateSuccessfulCallback(t *testing.T) {
	halt := errors.New("controlled pause")
	f := &fake{model: func() (Turn, error) {
		return Turn{Calls: []Call{{ID: "pause", Name: "read", Arguments: "{}"}, {ID: "late", Name: "read", Arguments: "{}"}}, Tools: []Tool{readTool}}, nil
	}}
	f.execute = func(call Call, _, _ bool) (Outcome, error) {
		if call.ID == "pause" {
			return Outcome{}, fmt.Errorf("boundary: %w", halt)
		}
		time.Sleep(50 * time.Millisecond)
		return Outcome{Content: "read completed"}, nil
	}
	err := Run(testContext(t), f, halt)
	if !IsHaltOnly(err, halt) || len(f.results) != 0 {
		t.Fatalf("normal halt/late callback became failure: %v results=%v", err, f.results)
	}
	for _, entry := range f.log {
		if entry == "finish" {
			t.Fatal("finish ran after halt")
		}
	}
}

func TestHaltStopsBeforeProvider(t *testing.T) {
	halt := errors.New("fixture durable pause")
	f := &fake{prepare: func() error { return halt }, model: func() (Turn, error) {
		t.Error("provider ran after halt")
		return Turn{}, nil
	}}
	if err := Run(testContext(t), f, halt); !IsHaltOnly(err, halt) {
		t.Fatalf("halt lost: %v", err)
	}
}

func TestCancellationPreservesCause(t *testing.T) {
	cause := errors.New("fixture caller stopped")
	ctx, cancel := context.WithCancelCause(testContext(t))
	defer cancel(nil)
	f := &fake{prepare: func() error {
		cancel(cause)
		return context.Cause(ctx)
	}, model: func() (Turn, error) { return Turn{}, errors.New("unexpected model call") }}
	if err := Run(ctx, f, errors.New("halt")); !errors.Is(err, cause) {
		t.Fatalf("cancel cause lost: %v", err)
	}
}

func TestJoinedHaltPreservesFailure(t *testing.T) {
	halt, disk, cancelCause := errors.New("controlled pause"), errors.New("fixture CAS commit failed"), errors.New("operator cancelled")
	for _, tc := range []struct {
		name   string
		err    error
		cancel bool
	}{
		{"mixed", errors.Join(halt, disk), false},
		{"nested-mixed", fmt.Errorf("store: %w", errors.Join(halt, disk)), false},
		{"mixed-with-cancel", errors.Join(halt, disk), true},
		{"pure-wrapped", fmt.Errorf("boundary: %w", halt), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(testContext(t))
			defer cancel(nil)
			f := &fake{prepare: func() error {
				if tc.cancel {
					cancel(cancelCause)
				}
				return tc.err
			}, model: func() (Turn, error) { return Turn{}, errors.New("unexpected model call") }}
			err := Run(ctx, f, halt)
			if tc.name == "pure-wrapped" {
				if !IsHaltOnly(err, halt) {
					t.Fatalf("normal boundary became failure: %v", err)
				}
				return
			}
			if !errors.Is(err, disk) || IsHaltOnly(err, halt) {
				t.Fatalf("joined persistence failure hidden: %v", err)
			}
			if tc.cancel && !errors.Is(err, cancelCause) {
				t.Fatalf("cancel cause lost: %v", err)
			}
		})
	}
}

func TestIsHaltOnlyRejectsMixedCauses(t *testing.T) {
	halt, disk := errors.New("halt"), errors.New("disk")
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false}, {"plain", halt, true}, {"wrapped", fmt.Errorf("boundary: %w", halt), true},
		{"joined-only", errors.Join(halt, fmt.Errorf("again: %w", halt)), true},
		{"mixed", errors.Join(halt, disk), false}, {"nested-mixed", fmt.Errorf("outer: %w", errors.Join(halt, disk)), false},
		{"reverse-mixed", errors.Join(disk, halt), false}, {"real", disk, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHaltOnly(tc.err, halt); got != tc.want {
				t.Fatalf("IsHaltOnly(%v)=%v", tc.err, got)
			}
		})
	}
}

func TestInvalidArgumentsAreErrorResultsWithoutExecution(t *testing.T) {
	schema := map[string]any{"type": "object", "required": []string{"command"}, "properties": map[string]any{"command": map[string]any{"type": "string"}}}
	f := &fake{model: func() (Turn, error) {
		return Turn{Calls: []Call{{ID: "missing", Name: "exec", Arguments: `{}`}, {ID: "broken", Name: "exec", Arguments: `{"command":`},
			{ID: "array", Name: "exec", Arguments: `[]`}, {ID: "coerce", Name: "exec", Arguments: `{"command":5}`}, {ID: "ok", Name: "exec", Arguments: `{"command":"id"}`}},
			Tools: []Tool{{Name: "exec", Parameters: schema}}}, nil
	}}
	if err := Run(testContext(t), f, errors.New("halt")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.log, ","); got != "prepare,model,execute:ok,finish" {
		t.Fatalf("invalid call crossed execution boundary: %s", got)
	}
	for id, want := range map[string]string{"missing": "must have required property 'command'", "broken": `Invalid arguments for tool "exec"`, "array": "must be a JSON object", "coerce": "command: must be string"} {
		if outcome := f.outcomes[id]; !outcome.IsError || !strings.Contains(outcome.Content, want) {
			t.Fatalf("%s outcome=%+v want %q", id, outcome, want)
		}
	}
	if f.outcomes["ok"].IsError || fmt.Sprint(f.results) != "[missing broken array coerce ok]" {
		t.Fatalf("results=%v ok=%+v", f.results, f.outcomes["ok"])
	}
}

func TestUnknownToolIsManualHandoff(t *testing.T) {
	f := &fake{model: func() (Turn, error) {
		return Turn{Calls: []Call{{ID: "m", Name: "missing_tool", Arguments: "not json"}}}, nil
	}}
	f.execute = func(_ Call, manual, _ bool) (Outcome, error) {
		if !manual {
			t.Error("unknown tool was not a manual handoff")
		}
		return Outcome{Content: "unavailable"}, nil
	}
	if err := Run(testContext(t), f, errors.New("halt")); err != nil || len(f.results) != 1 {
		t.Fatalf("err=%v results=%v", err, f.results)
	}
}

func TestLengthStopFailsWholeBatch(t *testing.T) {
	f := &fake{model: func() (Turn, error) {
		return Turn{StopReason: "length", Calls: calls(2, "read"), Tools: []Tool{readTool}}, nil
	}}
	if err := Run(testContext(t), f, errors.New("halt")); err != nil {
		t.Fatal(err)
	}
	for _, entry := range f.log {
		if strings.HasPrefix(entry, "execute:") {
			t.Fatal("truncated call executed")
		}
	}
	if len(f.results) != 2 || !f.outcomes["0"].IsError || !strings.Contains(f.outcomes["0"].Content, "output token limit") {
		t.Fatalf("results=%v outcomes=%+v", f.results, f.outcomes)
	}
}

func TestInBandFailureMarksError(t *testing.T) {
	f := &fake{model: func() (Turn, error) { return Turn{Calls: calls(1, "read"), Tools: []Tool{readTool}}, nil }}
	f.execute = func(Call, bool, bool) (Outcome, error) { return Outcome{Content: `{"exit_code":2}`}, nil }
	if err := Run(testContext(t), f, errors.New("halt")); err != nil || !f.outcomes["0"].IsError {
		t.Fatalf("err=%v outcome=%+v", err, f.outcomes["0"])
	}
}

func TestFinishDecisionControlsContinuation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failed   bool
		finishAt int
	}{
		{"failed_end", true, 1},
		{"success_continue", false, 3},
		{"failed_continue", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finishes := 0
			f := &fake{model: func() (Turn, error) { return Turn{Failed: tc.failed}, nil }}
			f.finish = func() (bool, error) { finishes++; return finishes == tc.finishAt, nil }
			if err := Run(testContext(t), f, errors.New("halt")); err != nil || finishes != tc.finishAt {
				t.Fatalf("finishes=%d want=%d err=%v", finishes, tc.finishAt, err)
			}
			if got, want := strings.Join(f.log, ","), strings.TrimSuffix(strings.Repeat("prepare,model,finish,", tc.finishAt), ","); got != want {
				t.Fatalf("sequence=%s want=%s", got, want)
			}
		})
	}
}

func TestRecoveryBatchSequentialBeforePrepare(t *testing.T) {
	recovered := false
	f := &fake{model: func() (Turn, error) { return Turn{}, nil }}
	f.recover = func() (*Turn, error) {
		if recovered {
			return nil, nil
		}
		recovered = true
		return &Turn{Calls: calls(2, "read"), Tools: []Tool{readTool}}, nil
	}
	f.execute = func(_ Call, _ bool, parallel bool) (Outcome, error) {
		if parallel {
			t.Error("recovery batch ran in parallel")
		}
		return Outcome{Content: "saved"}, nil
	}
	if err := Run(testContext(t), f, errors.New("halt")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.log, ","); got != "execute:0,execute:1,prepare,model,finish" {
		t.Fatalf("sequence=%s", got)
	}
}

func TestResourceLimitedArgumentsNeverExecute(t *testing.T) {
	for _, same := range []bool{true, false} {
		t.Run(fmt.Sprint("duplicates=", same), func(t *testing.T) {
			values := make([]int, 1000)
			for i := range values {
				if !same {
					values[i] = i
				}
			}
			raw, err := json.Marshal(map[string]any{"values": values})
			if err != nil {
				t.Fatal(err)
			}
			f := &fake{model: func() (Turn, error) {
				return Turn{Calls: []Call{{ID: "bounded", Name: "read", Arguments: string(raw)}},
					Tools: []Tool{{Name: "read", Parameters: map[string]any{
						"properties": map[string]any{"values": map[string]any{"uniqueItems": true}},
					}}}}, nil
			}}
			if err := Run(testContext(t), f, nil); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(f.log, ","); got != "prepare,model,finish" {
				t.Fatalf("invalid arguments crossed execution boundary: %s", got)
			}
			outcome, ok := f.outcomes["bounded"]
			if !ok || !outcome.IsError {
				t.Fatalf("missing validation failure result: %+v", outcome)
			}
		})
	}
}

func TestNestedProbeRejectionNeverExecutes(t *testing.T) {
	f := &fake{model: func() (Turn, error) {
		return Turn{Calls: []Call{{ID: "nested", Name: "read", Arguments: "{}"}},
			Tools: []Tool{{Name: "read", Parameters: map[string]any{
				"not": map[string]any{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "object"}}},
			}}}}, nil
	}}
	if err := Run(testContext(t), f, errors.New("halt")); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.log, ","); got != "prepare,model,finish" {
		t.Fatalf("nested probe crossed execution boundary: %s", got)
	}
	if outcome := f.outcomes["nested"]; !outcome.IsError || !strings.Contains(outcome.Content, "must not be valid") {
		t.Fatalf("missing nested probe rejection: %+v", outcome)
	}
}

func TestCommittedFinishSurvivesLateCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(testContext(t))
	f := &fake{model: func() (Turn, error) { return Turn{}, nil }}
	f.finish = func() (bool, error) {
		cancel() // caller cancels after Finish committed the end
		return true, nil
	}
	if err := Run(ctx, f, errors.New("halt")); err != nil {
		t.Fatalf("committed end reported as failure: %v", err)
	}
}

func TestParallelFailureStopsQueuedCalls(t *testing.T) {
	disk := errors.New("fixture commit failed")
	var mu sync.Mutex
	started := 0
	f := &fake{model: func() (Turn, error) {
		return Turn{Calls: calls(MaxParallelTools+3, "read"), Tools: []Tool{readTool}}, nil
	}}
	f.execute = func(Call, bool, bool) (Outcome, error) {
		mu.Lock()
		started++
		mu.Unlock()
		time.Sleep(40 * time.Millisecond)
		return Outcome{}, disk
	}
	err := Run(testContext(t), f, errors.New("halt"))
	if !errors.Is(err, disk) || len(f.results) != 0 {
		t.Fatalf("failure not returned: %v results=%v", err, f.results)
	}
	if started > MaxParallelTools {
		t.Fatalf("queued calls started after real failure: started=%d", started)
	}
}

func TestBatchSharesValidationBudget(t *testing.T) {
	// Each call alone fits the per-call byte budget; together they exceed the batch budget.
	big := `{"s":"` + strings.Repeat("a", maxValidationInputBytes-16) + `"}`
	n := maxBatchValidationBytes/len(big) + 2
	batch := make([]Call, n)
	for i := range batch {
		batch[i] = Call{ID: fmt.Sprint(i), Name: "read", Arguments: big}
	}
	f := &fake{model: func() (Turn, error) { return Turn{Calls: batch, Tools: []Tool{readTool}}, nil }}
	if err := Run(testContext(t), f, errors.New("halt")); err != nil {
		t.Fatal(err)
	}
	if f.outcomes["0"].IsError {
		t.Fatalf("first call should validate: %.200s", f.outcomes["0"].Content)
	}
	last := f.outcomes[fmt.Sprint(n-1)]
	if !last.IsError || !strings.Contains(last.Content, "batch validation") {
		t.Fatalf("batch budget not shared: %.200s", last.Content)
	}
}
