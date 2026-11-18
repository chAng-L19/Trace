package agent

import (
	"context"
	"errors"
	"sync"
	"time"
)

type mcpLifecycle struct {
	mu       sync.Mutex
	closed   bool
	finished bool
	err      error
}

// CloseMCP first settles worker cleanup, then closes parent-owned transports.
// A timeout leaves transports available for a subsequent cleanup attempt.
func (a *Agent) CloseMCP() error {
	a.mcpState.mu.Lock()
	a.mcpState.closed = true
	a.mcpState.mu.Unlock()
	// Waiting may recheck typed tool cleanup against the Store. Keep Store
	// reads outside the MCP mutex; concurrent closes serialize transports below.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stopErr := a.StopWorkers(ctx)
	cleanupErr, pendingErr := splitWorkerSettlementErrors(stopErr)
	if stopErr != nil {
		if ctx.Err() != nil || a.HasActiveWorkers() {
			return stopErr
		}
	}
	a.mcpState.mu.Lock()
	defer a.mcpState.mu.Unlock()
	if a.mcpState.finished {
		return errors.Join(a.mcpState.err, cleanupErr, pendingErr)
	}
	// Cache only this Agent's transport errors. Descendant cleanup is read
	// afresh by StopWorkers and can converge after a durable resource check.
	for _, client := range a.mcpClients {
		a.mcpState.err = errors.Join(a.mcpState.err, client.Close())
	}
	a.mcpClients = nil
	a.mcpState.finished = true
	return errors.Join(a.mcpState.err, cleanupErr, pendingErr)
}
