package picore

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestNativePiHaltStopsBeforeProvider(t *testing.T) {
	halt := errors.New("fixture durable pause")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	calls := 0
	err := Run(ctx, func(method string, _ json.RawMessage) (any, error) {
		calls++
		if method != "prepare" {
			t.Errorf("provider/tool ran after halt: %s", method)
		}
		return nil, halt
	}, halt)
	if !errors.Is(err, halt) || calls != 1 {
		t.Fatalf("halt lost: calls=%d error=%v", calls, err)
	}
}

func TestNativePiCancellationPreservesCauseAndReapsHost(t *testing.T) {
	cause := errors.New("fixture caller stopped")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	finished := make(chan error, 1)
	go func() {
		finished <- Run(ctx, func(method string, _ json.RawMessage) (any, error) {
			if method != "prepare" {
				return nil, errors.New("unexpected callback")
			}
			cancel(cause)
			return nil, context.Cause(ctx)
		}, errors.New("halt"))
	}()
	select {
	case err := <-finished:
		if !errors.Is(err, cause) {
			t.Fatalf("cancel cause lost: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pi child was not reaped after cancellation")
	}
}
