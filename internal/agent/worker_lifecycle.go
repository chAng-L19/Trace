package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// A pending durable transition is not evidence that a resource failed cleanup.
// Unwrap preserves errors.Is/As for the original persistence failure.
type workerSettlementPendingError struct{ err error }

func (e *workerSettlementPendingError) Error() string {
	return "worker settlement pending: " + e.err.Error()
}
func (e *workerSettlementPendingError) Unwrap() error { return e.err }

func splitWorkerSettlementErrors(err error) (cleanup, pending error) {
	if err == nil {
		return nil, nil
	}
	if _, ok := err.(*workerSettlementPendingError); ok {
		return nil, err
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, part := range joined.Unwrap() {
			c, p := splitWorkerSettlementErrors(part)
			cleanup, pending = errors.Join(cleanup, c), errors.Join(pending, p)
		}
		return cleanup, pending
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		c, p := splitWorkerSettlementErrors(wrapped.Unwrap())
		if c == nil && p != nil {
			return nil, err
		}
	}
	return err, nil
}

// This typed cause is the only lifecycle error that a later Action ledger
// observation may discharge. MCP, transport and legacy errors remain opaque.
type workerToolCleanupError struct {
	runID string
	err   error
}

func (e *workerToolCleanupError) Error() string { return e.err.Error() }
func (e *workerToolCleanupError) Unwrap() error { return e.err }

func splitWorkerToolCleanupErrors(err error) (other, tools error) {
	if err == nil {
		return nil, nil
	}
	if _, ok := err.(*workerToolCleanupError); ok {
		return nil, err
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, part := range joined.Unwrap() {
			o, t := splitWorkerToolCleanupErrors(part)
			other, tools = errors.Join(other, o), errors.Join(tools, t)
		}
		return other, tools
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		o, t := splitWorkerToolCleanupErrors(wrapped.Unwrap())
		if o == nil && t != nil {
			return nil, err
		}
	}
	return err, nil
}

func (a *Agent) currentWorkerToolCleanupError(err error) error {
	if err == nil {
		return nil
	}
	if resource, ok := err.(*workerToolCleanupError); ok {
		snap, loadErr := a.store.Load(resource.runID)
		if loadErr != nil {
			return errors.Join(err, loadErr)
		}
		if current := toolCleanupError(snap); current != nil {
			return &workerToolCleanupError{runID: resource.runID, err: current}
		}
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var current error
		for _, part := range joined.Unwrap() {
			current = errors.Join(current, a.currentWorkerToolCleanupError(part))
		}
		return current
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		other, tools := splitWorkerToolCleanupErrors(wrapped.Unwrap())
		if other == nil && tools != nil && a.currentWorkerToolCleanupError(tools) == nil {
			return nil
		}
	}
	return err
}

// Registration spans dispatch setup through child cleanup, closing the window
// between persisted claims and goroutine start during server shutdown.
type workerLifecycle struct {
	mu               sync.Mutex
	closed           bool
	next             int
	cancel           map[int]context.CancelFunc
	wg               sync.WaitGroup
	err              error
	settlementErrors map[string]error
}

func newWorkerLifecycle() *workerLifecycle {
	return &workerLifecycle{cancel: map[int]context.CancelFunc{}, settlementErrors: map[string]error{}}
}
func (w *workerLifecycle) begin(parent context.Context) (context.Context, func(), error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil, nil, fmt.Errorf("worker dispatcher is closed")
	}
	ctx, cancel := context.WithCancel(parent)
	w.next++
	id := w.next
	w.cancel[id] = cancel
	w.wg.Add(1)
	return ctx, func() { cancel(); w.mu.Lock(); delete(w.cancel, id); w.mu.Unlock(); w.wg.Done() }, nil
}

func (a *Agent) HasActiveWorkers() bool {
	a.workers.mu.Lock()
	defer a.workers.mu.Unlock()
	return len(a.workers.cancel) > 0
}

func (a *Agent) recordWorkerCleanupError(err error) {
	if err == nil {
		return
	}
	a.workers.mu.Lock()
	defer a.workers.mu.Unlock()
	a.workers.err = errors.Join(a.workers.err, err)
}

// StopWorkers is idempotent. Deadline expiry means cleanup is still pending;
// callers must not release shared resources until a later wait succeeds.
func (a *Agent) StopWorkers(ctx context.Context) error {
	w := a.workers
	w.mu.Lock()
	w.closed = true
	for _, cancel := range w.cancel {
		cancel()
	}
	w.mu.Unlock()
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
		w.mu.Lock()
		var pending error
		for _, err := range w.settlementErrors {
			pending = errors.Join(pending, err)
		}
		if pending != nil {
			pending = &workerSettlementPendingError{err: pending}
		}
		cleanupErr := w.err
		w.mu.Unlock()
		// Store reads must not run under the lifecycle mutex: settlement writes
		// may take the same locks in the opposite direction.
		return errors.Join(a.currentWorkerToolCleanupError(cleanupErr), pending)
	case <-ctx.Done():
		return ctx.Err()
	}
}
