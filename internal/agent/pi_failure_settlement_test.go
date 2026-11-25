package agent

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestPiFailureSettlementPreservesCommittedSummary(t *testing.T) {
	for _, budget := range []bool{false, true} {
		name := "operator"
		if budget {
			name = "budget"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			providerFailure := errors.New("permanent summary provider failure")
			a, store := newTestAgent(t, successProvider{})
			id, original := longHistory(t, a, store)
			seconds := int64(60)
			a.ConfigureBudget(BudgetOverride{MaxSeconds: &seconds})
			var cause error = errors.New("late summary cancellation")
			if budget {
				cause = &timeBudgetCause{RunID: id}
			}
			persisted := false
			a.provider = settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
				if request.Purpose != "context_summary" {
					t.Fatalf("expected admitted summary, got %q", request.Purpose)
				}
				// Response settlement has already chosen failed when this hook runs.
				store.mu.Lock()
				store.beforePersist = func(got string) {
					if got == id && !persisted {
						persisted = true
						cancel(cause)
					}
				}
				store.mu.Unlock()
				return ModelResponse{}, providerFailure
			})
			defer func() { store.mu.Lock(); store.beforePersist = nil; store.mu.Unlock() }()
			snap, err := a.Run(ctx, id)
			if !persisted || snap.Run.Status != StatusFailed || !strings.Contains(snap.Run.Error, providerFailure.Error()) || !errors.Is(err, providerFailure) {
				t.Fatalf("committed summary failure lost: persisted=%v status=%s error=%q returned=%v", persisted, snap.Run.Status, snap.Run.Error, err)
			}
			if len(snap.Responses) != 1 || len(snap.Compactions) != 1 || snap.Compactions[0].Status != "failed" || messageHash(snap.Messages) != messageHash(original) || len(snap.Actions) != 0 {
				t.Fatal("summary failure changed its source or executed a tool")
			}
			for _, event := range snap.Events {
				if event.Type == "budget_paused" {
					t.Fatal("late budget replaced committed summary failure")
				}
			}
		})
	}
}

func TestPiRecoverySummaryFailureKeepsIndependentCause(t *testing.T) {
	for _, budget := range []bool{false, true} {
		name := "operator"
		if budget {
			name = "budget"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			overflow := &ProviderHTTPError{StatusCode: 400, Body: "context_length_exceeded"}
			summaryFailure := errors.New("independent recovery summary failure")
			a, store := newTestAgent(t, successProvider{})
			id, original := recoveryHistory(t, a, store)
			seconds := int64(60)
			a.ConfigureBudget(BudgetOverride{MaxSeconds: &seconds})
			calls := 0
			a.provider = settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
				calls++
				if calls == 1 {
					return ModelResponse{}, overflow
				}
				if calls != 2 || request.Purpose != "context_summary" {
					t.Fatalf("unexpected recovery request: call=%d purpose=%q", calls, request.Purpose)
				}
				if budget {
					cancel(&timeBudgetCause{RunID: id})
				} else {
					cancel(errors.New("recovery owner cancelled"))
				}
				return ModelResponse{}, summaryFailure
			})
			snap, err := a.Run(ctx, id)
			want := StatusFailed
			if budget {
				want = StatusPaused
			}
			if calls != 2 || snap.Run.Status != want || !errors.Is(err, summaryFailure) {
				t.Fatalf("summary cause lost: calls=%d status=%s error=%v", calls, snap.Run.Status, err)
			}
			if len(snap.Responses) != 2 || !strings.Contains(snap.Responses[1].Error, summaryFailure.Error()) || len(snap.Compactions) != 1 || snap.Compactions[0].Status != "failed" || messageHash(snap.Messages) != messageHash(original) || len(snap.Actions) != 0 {
				t.Fatal("recovery failure changed source, receipt, or execution")
			}
			if budget && snap.Run.PauseReason != "time budget exhausted" {
				t.Fatalf("budget state lost: %+v", snap.Run)
			}
		})
	}
}

func TestPiFinishPersistenceFailurePreservesCommittedFailure(t *testing.T) {
	providerFailure := errors.New("permanent provider failure before finish I/O")
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	writes := 0
	a.provider = settlementProviderFunc(func(context.Context, ModelRequest) (ModelResponse, error) {
		store.mu.Lock()
		store.beforePersist = func(got string) {
			if got != id {
				return
			}
			writes++
			// The first write commits failed; only Finish's subsequent write fails.
			mode := os.FileMode(0700)
			if writes == 2 {
				mode = 0500
			}
			if err := os.Chmod(store.root, mode); err != nil {
				t.Fatal(err)
			}
		}
		store.mu.Unlock()
		return ModelResponse{}, providerFailure
	})
	defer func() {
		store.mu.Lock()
		defer store.mu.Unlock()
		store.beforePersist = nil
		if err := os.Chmod(store.root, 0700); err != nil {
			t.Error(err)
		}
	}()
	snap, err := a.Run(context.Background(), id)
	var persistenceFailure *os.PathError
	if writes < 2 || !errors.As(err, &persistenceFailure) || !errors.Is(err, providerFailure) || snap.Run.Status != StatusFailed || snap.Run.Error != providerFailure.Error() {
		t.Fatalf("finish replaced committed failure: writes=%d status=%s saved=%q returned=%v", writes, snap.Run.Status, snap.Run.Error, err)
	}
	if len(snap.Responses) != 1 || snap.Responses[0].Error != providerFailure.Error() || len(snap.Actions) != 0 {
		t.Fatal("finish I/O failure altered provider receipt or executed tools")
	}
}
func TestPiSummaryRetryBudgetRemainsControlHalt(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		name := "preparation"
		if recovery {
			name = "recovery"
		}
		t.Run(name, func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			var id string
			requests := 1
			if recovery {
				id, _ = recoveryHistory(t, a, store)
				requests = 2
			} else {
				id, _ = longHistory(t, a, store)
			}
			a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(requests)})
			calls := 0
			a.provider = settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
				calls++
				if recovery && calls == 1 {
					return ModelResponse{}, &ProviderHTTPError{StatusCode: 400, Body: "context_length_exceeded"}
				}
				if request.Purpose != "context_summary" {
					t.Fatalf("expected summary: %+v", request)
				}
				return ModelResponse{}, &ProviderHTTPError{StatusCode: 503}
			})
			snap, err := a.Run(context.Background(), id)
			if err != nil || calls != requests || snap.Run.Status != StatusPaused || snap.Run.PauseReason != "model request budget exhausted" || len(snap.Responses) != requests {
				t.Fatalf("scheduled retry budget changed: calls=%d status=%s reason=%s err=%v", calls, snap.Run.Status, snap.Run.PauseReason, err)
			}
		})
	}
}

func TestPiStoppedModelPreservesIndependentErrorBeforeNotification(t *testing.T) {
	for _, status := range []Status{StatusPaused, StatusCancelled} {
		for _, notified := range []bool{false, true} {
			name := string(status) + "/pending"
			if notified {
				name = string(status) + "/notified"
			}
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				providerFailure := errors.New("provider failure while operator stop is committed")
				a, store := newTestAgent(t, successProvider{})
				id := createTestRun(t, a)
				calls := 0
				a.provider = settlementProviderFunc(func(context.Context, ModelRequest) (ModelResponse, error) {
					calls++
					var err error
					if status == StatusPaused {
						_, err = a.Pause(id)
					} else {
						_, err = a.Cancel(id)
					}
					if err != nil {
						t.Fatal(err)
					}
					if notified {
						cancel(errors.New("operator stop notification"))
					}
					return ModelResponse{}, providerFailure
				})
				// Drive the adapter without the scheduling subscriber to fix notification order.
				snap, err := a.runPi(ctx, id)
				if calls != 1 || snap.Run.Status != status || !errors.Is(err, providerFailure) {
					t.Fatalf("stopped request lost independent error: calls=%d status=%s err=%v", calls, snap.Run.Status, err)
				}
				saved, loadErr := store.Load(id)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if len(saved.Responses) != 1 || !strings.Contains(saved.Responses[0].Error, providerFailure.Error()) || len(saved.Actions) != 0 || saved.Run.Error != "" {
					t.Fatalf("stopped response changed control state or executed a tool: %+v", saved.Run)
				}
			})
		}
	}
}

func TestPiModelRetryBudgetIgnoresNotificationOrder(t *testing.T) {
	for _, notified := range []bool{false, true} {
		name := "pending"
		if notified {
			name = "notified"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			providerFailure := &ProviderHTTPError{StatusCode: 503, Body: "fixture retry"}
			a, store := newTestAgent(t, successProvider{})
			a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(1)})
			id := createTestRun(t, a)
			calls := 0
			persisted := false
			a.provider = settlementProviderFunc(func(context.Context, ModelRequest) (ModelResponse, error) {
				calls++
				if notified {
					store.mu.Lock()
					store.beforePersist = func(got string) {
						if got == id && !persisted {
							persisted = true
							cancel(context.Canceled)
						}
					}
					store.mu.Unlock()
				}
				return ModelResponse{}, providerFailure
			})
			defer func() { store.mu.Lock(); store.beforePersist = nil; store.mu.Unlock() }()
			snap, err := a.runPi(ctx, id)
			if err != nil || calls != 1 || snap.Run.Status != StatusPaused || snap.Run.PauseReason != "model request budget exhausted" || snap.Run.Error != "" || notified && !persisted {
				t.Fatalf("retry budget depends on notification: calls=%d persisted=%v status=%s pause=%q err=%v", calls, persisted, snap.Run.Status, snap.Run.PauseReason, err)
			}
			if len(snap.Responses) != 1 || !strings.Contains(snap.Responses[0].Error, providerFailure.Error()) || len(snap.Actions) != 0 {
				t.Fatal("retry budget lost provider receipt or executed a tool")
			}
		})
	}
}

func TestPiRecoveryAdmissionBudgetIgnoresNotificationOrder(t *testing.T) {
	for _, budget := range []string{"run", "tree"} {
		for _, failure := range []string{"overflow", "length"} {
			for _, notified := range []bool{false, true} {
				name := budget + "/" + failure + "/pending"
				if notified {
					name = budget + "/" + failure + "/notified"
				}
				t.Run(name, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					a, store := newTestAgent(t, successProvider{})
					id, original := recoveryHistory(t, a, store)
					if budget == "run" {
						if _, err := a.SetRunBudget(id, Budget{MaxModelRequests: 1}); err != nil {
							t.Fatal(err)
						}
					} else if _, err := a.SetTreeBudget(id, TreeBudget{MaxModelRequests: 1}); err != nil {
						t.Fatal(err)
					}
					calls, commits := 0, 0
					a.provider = settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
						calls++
						if request.Purpose != "" {
							t.Fatalf("summary admitted beyond budget: %q", request.Purpose)
						}
						if notified {
							store.mu.Lock()
							store.beforePersist = func(got string) {
								if got == id {
									commits++
									cancel()
								}
							}
							store.mu.Unlock()
						}
						if failure == "length" {
							return ModelResponse{Content: "truncated observation", StopReason: "length"}, nil
						}
						return ModelResponse{}, &ProviderHTTPError{StatusCode: 400, Body: "context_length_exceeded"}
					})
					defer func() { store.mu.Lock(); store.beforePersist = nil; store.mu.Unlock() }()
					snap, err := a.runPi(ctx, id)
					if err != nil || calls != 1 || snap.Run.Status != StatusPaused || snap.Run.Error != "" || notified && commits == 0 {
						t.Fatalf("recovery admission budget changed with notification: calls=%d commits=%d status=%s reason=%q err=%v", calls, commits, snap.Run.Status, snap.Run.PauseReason, err)
					}
					wantReason := "model request budget exhausted"
					if budget == "tree" {
						wantReason = "tree budget "
					}
					if !strings.HasPrefix(snap.Run.PauseReason, wantReason) || len(snap.Requests) != 1 || len(snap.Responses) != 1 || len(snap.Actions) != 0 || len(snap.Compactions) != 0 || messageHash(snap.Messages) != messageHash(original) {
						t.Fatalf("recovery budget changed history or admitted work: %+v", snap.Run)
					}
				})
			}
		}
	}
}
