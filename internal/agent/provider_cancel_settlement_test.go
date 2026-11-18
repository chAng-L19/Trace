package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type cancelledSuccessProvider struct {
	cancel context.CancelCauseFunc
	cause  error
}

func (p cancelledSuccessProvider) Complete(context.Context, ModelRequest) (ModelResponse, error) {
	p.cancel(p.cause)
	return ModelResponse{Content: "late success", StopReason: "stop"}, nil
}

func TestProviderSuccessAfterCancellationIsArchivedWithoutCompletion(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cause := errors.New("fixture owner stopped")
	a, store := newTestAgent(t, cancelledSuccessProvider{cancel: cancel, cause: cause})
	id := createTestRun(t, a)
	_, _ = a.Run(ctx, id)
	snap, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusFailed || !strings.Contains(snap.Run.Error, cause.Error()) {
		t.Fatalf("late cancellation lost: %+v", snap.Run)
	}
	if len(snap.Responses) != 1 || snap.Responses[0].Response.Content != "late success" || !strings.Contains(snap.Responses[0].Error, cause.Error()) {
		t.Fatal("cancelled response not faithfully archived")
	}
	for _, m := range snap.Messages {
		if m.Role == "assistant" {
			t.Fatal("cancelled response advanced transcript")
		}
	}
	for _, e := range snap.Events {
		if e.Type == "run_completed" {
			t.Fatal("cancelled request marked completed")
		}
	}
}

type settlementProviderFunc func(context.Context, ModelRequest) (ModelResponse, error)

func (f settlementProviderFunc) Complete(ctx context.Context, request ModelRequest) (ModelResponse, error) {
	return f(ctx, request)
}

func TestProviderCancellationWhileWaitingForStoreLock(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	locked := make(chan func(), 1)
	a.provider = settlementProviderFunc(func(context.Context, ModelRequest) (ModelResponse, error) {
		release, err := store.acquireStoreLock()
		if err != nil {
			return ModelResponse{}, err
		}
		locked <- release
		return ModelResponse{Content: "late success", StopReason: "stop"}, nil
	})
	done := make(chan error, 1)
	go func() { _, err := a.Run(ctx, id); done <- err }()
	var release func()
	select {
	case release = <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("provider did not acquire fixture lock")
	}
	defer release()
	// Update takes mu before the file lock. Observe uses the immutable cache,
	// so only response settlement can hold this mutex at this point.
	deadline := time.Now().Add(5 * time.Second)
	for store.mu.TryLock() {
		store.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("response did not wait for Store lock")
		}
		time.Sleep(time.Millisecond)
	}
	cause := errors.New("cancelled during settlement lock wait")
	cancel(cause)
	release()
	select {
	case err := <-done:
		if !errors.Is(err, cause) {
			t.Fatalf("cancellation cause lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not settle after release")
	}
	snap, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusFailed || len(snap.Responses) != 1 || snap.Responses[0].Error != cause.Error() {
		t.Fatalf("cancelled response advanced run: %+v", snap)
	}
	for _, m := range snap.Messages {
		if m.Role == "assistant" {
			t.Fatal("cancelled response appended assistant")
		}
	}
}

func TestCompletedSummaryRemainsReusableAcrossCancellationPause(t *testing.T) {
	for _, reason := range []string{"operator", "budget"} {
		t.Run(reason, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			a, store := newTestAgent(t, successProvider{})
			id, original := longHistory(t, a, store)
			if reason == "budget" {
				seconds := int64(60)
				a.ConfigureBudget(BudgetOverride{MaxSeconds: &seconds})
			}
			a.provider = settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
				if request.Purpose != "context_summary" {
					return ModelResponse{}, errors.New("expected summary request")
				}
				if reason == "operator" {
					if _, err := a.Pause(id); err != nil {
						return ModelResponse{}, err
					}
					cancel(errors.New("operator stopped provider"))
				} else {
					cancel(&timeBudgetCause{RunID: id})
				}
				return ModelResponse{Content: "Complete compact summary.", StopReason: "stop"}, nil
			})
			snap, err := a.Run(ctx, id)
			if err != nil || snap.Run.Status != StatusPaused || len(snap.Compactions) != 1 || snap.Compactions[0].Status != "completed" {
				t.Fatalf("complete summary lost across pause: %+v %v", snap, err)
			}
			if messageHash(snap.Messages) != messageHash(original) {
				t.Fatal("summary rewrote original history")
			}
			if reason == "budget" && snap.Run.PauseReason != "time budget exhausted" {
				t.Fatalf("budget pause lost: %+v", snap.Run)
			}
			firstSummary := snap.Compactions[0]
			// The 7k fixture window compacts history incrementally. Give the
			// saved summary plus remaining tail room so this check isolates reuse,
			// rather than requiring a second valid compaction on the next batch.
			resumeContext := snap.Run.Context
			resumeContext.WindowTokens = 32000
			if err := a.ConfigureContext(resumeContext); err != nil {
				t.Fatal(err)
			}
			a.provider = successProvider{}
			resumed, err := a.Run(context.Background(), id)
			if err != nil || resumed.Run.Status != StatusCompleted || len(resumed.Compactions) != 1 || len(resumed.Requests) != 2 {
				t.Fatalf("resume did not reuse saved summary: %+v %v", resumed, err)
			}
			last := resumed.Requests[len(resumed.Requests)-1]
			if last.Purpose == "context_summary" || last.CompactionID != firstSummary.ID || resumed.Compactions[0].Summary != firstSummary.Summary || resumed.Compactions[0].SourceHash != firstSummary.SourceHash {
				t.Fatalf("resume replaced rather than reused the saved summary: %+v", last)
			}
		})
	}
}
