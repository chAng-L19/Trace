package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func failedParentSettlementFixture(t *testing.T) (*Agent, *Store, string, AgentWorker) {
	t.Helper()
	a, store, id, w := settlementFixture(t)
	restore, failed := settlementRenameFailure(t, store, id)
	err := settlementOperation(t, a, store, id, w, func(s *Snapshot) { s.Run.Status = StatusCompleted })
	restore()
	if err == nil || !*failed {
		t.Fatal("missing actual parent rename failure")
	}
	return a, store, id, w
}

func TestWorkerSettlementWaitAutomaticallyRecovers(t *testing.T) {
	a, store, id, _ := failedParentSettlementFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	recovered, err := a.WaitWorkers(ctx, id, 30)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Plan.Tasks[0].Status != "completed" || occupiedWorkerSlots(&recovered) != 0 {
		t.Fatal("wait did not repair durable parent settlement")
	}
	if err = a.StopWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	child, err := store.Load(recovered.Workers[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(child.Actions) != 0 || len(child.Requests) != 0 {
		t.Fatal("reconciliation replayed model or action")
	}
}

func TestWorkerSettlementParentLoopAutomaticallyRecovers(t *testing.T) {
	a, store, id, w := failedParentSettlementFixture(t)
	calls := 0
	a.provider = settlementProviderFunc(func(_ context.Context, req ModelRequest) (ModelResponse, error) {
		calls++
		parent, err := store.Load(id)
		if err != nil {
			return ModelResponse{}, err
		}
		if parent.Plan.Tasks[0].Status != "completed" || parent.Workers[0].CleanupStatus != "completed" {
			t.Fatal("parent provider saw stale running worker")
		}
		_, err = a.Pause(id)
		return ModelResponse{}, err
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := a.Run(ctx, id); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("unexpected parent requests: %d", calls)
	}
	child, err := store.Load(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(child.Actions) != 0 || len(child.Requests) != 0 {
		t.Fatal("parent checkpoint replayed child")
	}
}

func TestWorkerSettlementAutoRecoveryFailureIsObservable(t *testing.T) {
	a, store, id, _ := failedParentSettlementFixture(t)
	restore, failed := settlementRenameFailure(t, store, id)
	_, err := a.WaitWorkers(context.Background(), id, 30)
	restore()
	if err == nil || !*failed {
		t.Fatal("wait swallowed actual recovery persistence failure")
	}
	if err = a.StopWorkers(context.Background()); err == nil {
		t.Fatal("pending fault disappeared before commit")
	}
	if _, err = a.WaitWorkers(context.Background(), id, 30); err != nil {
		t.Fatal(err)
	}
	if err = a.StopWorkers(context.Background()); err != nil {
		t.Fatal("successful automatic commit retained pending", err)
	}
}

func TestWorkerSettlementAutoRecoveryMissingReceiptStaysUnknown(t *testing.T) {
	a, store, id, w := settlementFixture(t)
	cause := errors.New("receipt fsync failure")
	a.recordWorkerSettlementError(id, w, cause)
	_, err := a.WaitWorkers(context.Background(), id, 30)
	var pending *workerSettlementPendingError
	if !errors.As(err, &pending) || !errors.Is(err, cause) {
		t.Fatalf("missing receipt failure hidden: %v", err)
	}
	if _, err = store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Second); return nil }); err != nil {
		t.Fatal(err)
	}
	a.workers = newWorkerLifecycle() // Restart: no in-memory receipt error survives.
	recovered, err := a.WaitWorkers(context.Background(), id, 30)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Workers[0].CleanupStatus != "unknown" || occupiedWorkerSlots(&recovered) != 1 {
		t.Fatal("idle lock was substituted for cleanup proof")
	}
}

func TestWorkerSettlementCheckpointDoesNotWriteActiveWorkers(t *testing.T) {
	a, store, id, w := settlementFixture(t)
	unlock, err := store.AcquireRunLock(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	writes := 0
	store.mu.Lock()
	store.beforePersist = func(string) { writes++ }
	store.mu.Unlock()
	for i := 0; i < 4; i++ {
		if err = a.reconcileWorkerSettlements(id); err != nil {
			t.Fatal(err)
		}
	}
	store.mu.Lock()
	store.beforePersist = nil
	store.mu.Unlock()
	if writes != 0 {
		t.Fatalf("normal active checkpoint rewrote state: %d", writes)
	}
}
