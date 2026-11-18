package agent

import (
	"context"
	"math"
	"testing"
)

func TestExplicitRunBudgetSurvivesResumeAndKeepsOtherRunIndependent(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(3), MaxActions: intPointer(4)})
	first, second := createTestRun(t, a), createTestRun(t, a)
	if _, err := a.SetRunBudget(first, Budget{MaxModelRequests: 1}); err != nil {
		t.Fatal(err)
	}
	// A service rebuild/CLI resume may carry different global defaults.
	restarted, err := New(store, successProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	restarted.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(8), MaxActions: intPointer(9)})
	snap, err := restarted.Run(context.Background(), first)
	if err != nil || !snap.Run.BudgetConfigured || snap.Run.Budget != (Budget{MaxModelRequests: 1}) {
		t.Fatalf("explicit budget overwritten on resume: %+v %v", snap.Run, err)
	}
	other, err := store.Load(second)
	if err != nil || other.Run.BudgetConfigured || other.Run.Budget != (Budget{MaxModelRequests: 3, MaxActions: 4}) {
		t.Fatalf("other run changed: %+v %v", other.Run, err)
	}
	if _, err = restarted.SetRunBudget(second, Budget{}); err != nil {
		t.Fatal(err)
	}
	other, err = restarted.Run(context.Background(), second)
	if err != nil || other.Run.Budget != (Budget{}) || !other.Run.BudgetConfigured {
		t.Fatalf("explicit unlimited overwritten: %+v %v", other.Run, err)
	}
}

func TestRunBudgetRejectsInvalidAndActiveUpdates(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	for _, budget := range []Budget{{MaxActions: -1}, {MaxModelRequests: -1}, {MaxSeconds: -1}, {MaxSeconds: math.MaxInt64}} {
		if _, err := a.SetRunBudget(id, budget); err == nil {
			t.Fatalf("invalid budget accepted: %+v", budget)
		}
	}
	if err := ValidateBudget(Budget{MaxSeconds: maxBudgetSeconds}); err != nil {
		t.Fatal(err)
	}
	release, err := store.AcquireRunLock(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetRunBudget(id, Budget{MaxActions: 1}); err == nil {
		t.Error("active run lock was ignored")
	}
	release()
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetRunBudget(id, Budget{MaxActions: 1}); err == nil {
		t.Fatal("running snapshot changed without pause")
	}
	snap, err := store.Load(id)
	if err != nil || snap.Run.BudgetConfigured || snap.Run.Budget != (Budget{}) {
		t.Fatalf("rejected update changed state: %+v %v", snap.Run, err)
	}
}
