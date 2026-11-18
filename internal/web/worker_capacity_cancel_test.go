package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"trace/internal/agent"
)

type capacityRequestContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *capacityRequestContext) Err() error {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Err()
}

func TestWorkerCapacityCancellationDoesNotHoldServerLock(t *testing.T) {
	s, a := fixture(t, nil)
	run, err := a.Create("capacity cancellation")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	stopWriter := func() { releaseOnce.Do(func() { close(release) }) }
	defer stopWriter()
	writer := make(chan error, 1)
	go func() {
		_, err := a.Store().Update(run.Run.ID, func(*agent.Snapshot) error {
			close(entered)
			<-release
			return nil
		})
		writer <- err
	}()
	select {
	case <-entered:
	case err := <-writer:
		t.Fatal("writer did not enter its Store transaction", err)
	case <-time.After(time.Second):
		t.Fatal("writer did not acquire its Store transaction")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := &capacityRequestContext{Context: ctx, observed: make(chan struct{})}
	mux := http.NewServeMux()
	s.registerPlannerRoutes(mux)
	r := httptest.NewRequest(http.MethodGet, "/api/runs/"+run.Run.ID+"/worker-capacity", nil).WithContext(observed)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { mux.ServeHTTP(w, r); close(done) }()
	select {
	case <-observed.observed:
	case <-time.After(time.Second):
		t.Fatal("capacity request did not reach cancellable Store acquisition")
	}
	if !s.mu.TryLock() {
		t.Fatal("capacity Store wait monopolized the Web lock")
	}
	s.mu.Unlock()
	cancel()
	select {
	case <-done:
		if w.Code == http.StatusOK {
			t.Fatal("cancelled read returned a successful capacity")
		}
	case <-time.After(time.Second):
		t.Fatal("GET cancellation did not release the request while Store writer remained active")
	}
	stopWriter()
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	request(t, mux, http.MethodGet, "/api/runs/"+run.Run.ID+"/worker-capacity", nil, http.StatusOK)
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	request(t, mux, http.MethodGet, "/api/runs/"+run.Run.ID+"/worker-capacity", nil, http.StatusServiceUnavailable)
	s.mu.Lock()
	s.closed = false
	s.mu.Unlock()
}
