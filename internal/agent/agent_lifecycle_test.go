package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"trace/internal/mcp"
)

func TestCloseMCPWaitsForWorkerCleanupAndIsConcurrentIdempotent(t *testing.T) {
	a, _ := newTestAgent(t, successProvider{})
	ctx, release, err := a.workers.begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cleaned := make(chan struct{})
	go func() { <-ctx.Done(); close(cleaned); release() }()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.CloseMCP(); err != nil {
				t.Error(err)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("concurrent cleanup hung")
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("cleanup returned before worker stopped")
	}
	if _, _, err := a.workers.begin(context.Background()); err == nil {
		t.Fatal("worker started after close")
	}
	if _, err := a.AddMCP(context.Background(), mcp.Config{}); err == nil {
		t.Fatal("transport reopened after close")
	}
}

func TestCloseMCPRechecksTypedToolCleanupWithoutDroppingTransportError(t *testing.T) {
	for _, withTransport := range []bool{false, true} {
		a, store := newTestAgent(t, MockProvider{})
		id := createTestRun(t, a)
		_, err := store.Update(id, func(s *Snapshot) error {
			s.Actions = []Action{{Adapter: "exec", RequestID: "r", CallID: "c", Status: "resolved_manually"}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		a.recordWorkerCleanupError(&workerToolCleanupError{runID: id, err: errors.New("owned process still live")})
		transport := errors.New("MCP DELETE 500")
		if withTransport {
			a.mcpState.err = transport
		}
		if err = a.CloseMCP(); err == nil {
			t.Fatal("pending cleanup hidden")
		}
		_, err = store.Update(id, func(s *Snapshot) error { s.Actions[0].ResourceCleanupStatus = "completed"; return nil })
		if err != nil {
			t.Fatal(err)
		}
		err = a.CloseMCP()
		if withTransport && !errors.Is(err, transport) {
			t.Fatalf("transport failure cleared by resource observation: %v", err)
		}
		if !withTransport && err != nil {
			t.Fatalf("typed historical cleanup cached after settlement: %v", err)
		}
	}
}
