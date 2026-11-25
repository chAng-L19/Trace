package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"trace/internal/agentloop"
)

type interruptPiResult struct {
	*piTurns
	interrupt error
}

func (h *interruptPiResult) Result(ctx context.Context, call agentloop.Call, outcome agentloop.Outcome) error {
	if err := h.piTurns.Result(ctx, call, outcome); err != nil {
		return err
	}
	return h.interrupt
}

func TestPiRecoveryBatchBudgetSurvivesPartialSettlement(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, interrupt := range []bool{false, true} {
			t.Run(backend+map[bool]string{false: "/continuous", true: "/reopen"}[interrupt], func(t *testing.T) {
				a, store := newTestAgent(t, &scriptedProvider{responses: []ModelResponse{{Content: fixtureReport("done")}}})
				if backend == "sqlite" {
					store = sqliteTestStore(t)
					var err error
					a, err = New(store, &scriptedProvider{responses: []ModelResponse{{Content: fixtureReport("done")}}}, "")
					if err != nil {
						t.Fatal(err)
					}
				}
				marker := filepath.Join(t.TempDir(), "must-not-write")
				large, _ := json.Marshal(map[string]string{"path": "absent", "legacy": strings.Repeat("x", 1<<20)})
				write, _ := json.Marshal(map[string]string{"path": marker, "content": "unexpected"})
				calls := []ToolCall{{ID: "oversized", Name: "read", Arguments: string(large)}, {ID: "later-write", Name: "write", Arguments: string(write)}}
				id := seedPiBatch(t, a, store, calls, a.registry.Definitions())
				if interrupt {
					stop := errors.New("fixture: interrupted after first durable result")
					err := agentloop.Run(piMigrationContext(t), &interruptPiResult{piTurns: &piTurns{a: a, id: id}, interrupt: stop}, errPiHalt)
					if !errors.Is(err, stop) {
						t.Fatalf("interrupt=%v", err)
					}
					snap, err := store.Load(id)
					if err != nil || len(pendingToolCalls(&snap)) != 1 {
						t.Fatalf("pending after interruption: %v %+v", err, snap.Run)
					}
					root := store.root
					if err := store.Close(); err != nil {
						t.Fatal(err)
					}
					store, err = NewStore(root)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { store.Close() })
					a, err = New(store, &scriptedProvider{responses: []ModelResponse{{Content: fixtureReport("done")}}}, "")
					if err != nil {
						t.Fatal(err)
					}
				}
				snap, err := a.Run(piMigrationContext(t), id)
				if err != nil || len(snap.Actions) != 0 || len(pendingToolCalls(&snap)) != 0 {
					t.Fatalf("admission changed: actions=%d pending=%d err=%v", len(snap.Actions), len(pendingToolCalls(&snap)), err)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("rejected write executed: %v", err)
				}
				messages := 0
				for _, m := range snap.Messages {
					if m.Role == "tool" {
						messages++
					}
				}
				if messages != 2 {
					t.Fatalf("settled result duplicated/lost: %d", messages)
				}
			})
		}
	}
}

func TestPiRecoveryReadyReceiptSurvivesValidationPolicy(t *testing.T) {
	for _, status := range []string{"result_ready", "uncertain", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			a, store := newTestAgent(t, &scriptedProvider{responses: []ModelResponse{{Content: fixtureReport("done")}}})
			args, _ := json.Marshal(map[string]string{"path": filepath.Join(t.TempDir(), "absent"), "legacy": strings.Repeat("x", 1<<20)})
			call := ToolCall{ID: "historical", Name: "read", Arguments: string(args)}
			id := seedPiBatch(t, a, store, []ToolCall{call}, a.registry.Definitions())
			_, err := store.Update(id, func(s *Snapshot) error {
				s.Actions = []Action{{RequestID: id + "-0001", CallID: call.ID, Name: call.Name, Arguments: call.Arguments, Status: status, Result: &ToolResult{ExitCode: 0, Stdout: "saved historical fact"}}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			snap, err := a.Run(piMigrationContext(t), id)
			if err != nil || len(pendingToolCalls(&snap)) != 0 || len(snap.Actions) != 1 || snap.Actions[0].Result.Stdout != "saved historical fact" {
				t.Fatalf("receipt stuck/lost: pending=%d actions=%d err=%v", len(pendingToolCalls(&snap)), len(snap.Actions), err)
			}
			if status != "result_ready" {
				if snap.Actions[0].Status != status {
					t.Fatal("historical uncertainty promoted")
				}
				for _, e := range snap.Evidence {
					if e.Verification == "verified" {
						t.Fatal("uncertain fact verified")
					}
				}
			}
		})
	}
}

func TestPiFinishCancellationBeforeStoreTransaction(t *testing.T) {
	a, store := newTestAgent(t, &scriptedProvider{responses: []ModelResponse{{Content: fixtureReport("done")}}})
	id := createTestRun(t, a)
	turn := &piTurns{a: a, id: id}
	if err := turn.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Model(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	store.mu.Lock()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		end, err := turn.Finish(ctx)
		if end {
			done <- errors.New("cancelled finish reported end")
			return
		}
		done <- err
	}()
	<-started
	cancel()
	store.mu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v", err)
	}
	after, err := store.Load(id)
	if err != nil || valueHash(before) != valueHash(after) {
		t.Fatalf("cancelled Finish committed: %v %+v", err, after.Run)
	}
}

func TestPiRecoveryTruncatedPrefixDoesNotAuthorizePending(t *testing.T) {
	a, store := newTestAgent(t, &scriptedProvider{responses: []ModelResponse{{Content: fixtureReport("done")}}})
	marker := filepath.Join(t.TempDir(), "must-not-write")
	write, _ := json.Marshal(map[string]string{"path": marker, "content": "unexpected"})
	calls := []ToolCall{{ID: "prefix", Name: "read", Arguments: `{"path":"absent"}`}, {ID: "pending", Name: "write", Arguments: string(write)}}
	id := seedPiBatch(t, a, store, calls, a.registry.Definitions())
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Responses = []ResponseRecord{{RequestID: id + "-0001", Response: ModelResponse{StopReason: "length", ToolCalls: calls}}}
		appendToolResult(s, calls[0].ID, `{"exit_code":-1,"error":"truncated"}`, "runtime_observation")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Run(piMigrationContext(t), id)
	if err != nil || len(snap.Actions) != 0 || len(pendingToolCalls(&snap)) != 0 {
		t.Fatalf("truncated recovery executed: actions=%d err=%v", len(snap.Actions), err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("truncated write executed: %v", err)
	}
}

func TestPiProviderFailureAfterCancellationKeepsBothCauses(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	providerFailure := errors.New("independent provider failure")
	control := errors.New("parent cancelled")
	a, _ := newTestAgent(t, settlementProviderFunc(func(context.Context, ModelRequest) (ModelResponse, error) {
		cancel(control)
		return ModelResponse{}, providerFailure
	}))
	_, err := a.Run(ctx, createTestRun(t, a))
	if !errors.Is(err, control) || !errors.Is(err, providerFailure) {
		t.Fatalf("failure hidden by cancellation: %v", err)
	}
}

type cancelAfterFailedPiModel struct {
	*piTurns
	cancel    context.CancelCauseFunc
	cause     error
	committed Snapshot
}

func (h *cancelAfterFailedPiModel) Model(ctx context.Context) (agentloop.Turn, error) {
	turn, err := h.piTurns.Model(ctx)
	if err != nil {
		return turn, err
	}
	if turn.Failed {
		h.committed, err = h.a.store.Load(h.id)
		if err != nil {
			return agentloop.Turn{}, err
		}
		h.cancel(h.cause)
	}
	return turn, nil
}

func TestPiCommittedModelFailureSurvivesLateCancellation(t *testing.T) {
	for _, budget := range []bool{false, true} {
		t.Run(map[bool]string{false: "operator", true: "budget"}[budget], func(t *testing.T) {
			providerFailure := errors.New("committed provider failure")
			a, store := newTestAgent(t, settlementProviderFunc(func(context.Context, ModelRequest) (ModelResponse, error) {
				return ModelResponse{}, providerFailure
			}))
			id := createTestRun(t, a)
			if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Budget.MaxSeconds = 60; return nil }); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			var cause error = errors.New("late operator cancellation")
			if budget {
				cause = &timeBudgetCause{RunID: id}
			}
			turns := &piTurns{a: a, id: id}
			hooks := &cancelAfterFailedPiModel{piTurns: turns, cancel: cancel, cause: cause}
			err := agentloop.Run(ctx, hooks, errPiHalt)
			if hooks.committed.Run.Status != StatusFailed || hooks.committed.Run.Error != providerFailure.Error() {
				t.Fatalf("fixture not committed: %+v", hooks.committed.Run)
			}
			var snap Snapshot
			if err != nil {
				snap, err = a.settlePiLoopError(ctx, id, err)
			} else {
				var loadErr error
				snap, loadErr = store.Load(id)
				err = errors.Join(loadErr, turns.modelErr)
			}
			if snap.Run.Status != StatusFailed || snap.Run.Error != providerFailure.Error() || !errors.Is(err, providerFailure) {
				t.Fatalf("committed failure overwritten: status=%s error=%q returned=%v", snap.Run.Status, snap.Run.Error, err)
			}
			for _, event := range snap.Events {
				if event.Type == "budget_paused" {
					t.Fatal("late budget replaced committed failure")
				}
			}
		})
	}
}
