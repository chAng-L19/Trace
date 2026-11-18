package agent

import (
	"testing"
	"time"
)

func TestToolCleanupMissingDurableProcessObservation(t *testing.T) {
	for _, adapter := range []string{"exec", "local"} {
		s := Snapshot{Actions: []Action{{Adapter: adapter, CallID: "lost", Status: "started"}}}
		if toolCleanupError(s) == nil {
			t.Fatalf("%s missing result released process cleanup", adapter)
		}
		s.Actions[0].Status = "resolved_manually"
		if toolCleanupError(s) == nil {
			t.Fatal("manual observation forged process exit")
		}
		s.Actions[0].Result = &ToolResult{}
		if err := toolCleanupError(s); err != nil {
			t.Fatalf("legacy result unexpectedly rejected: %v", err)
		}
	}
}

func TestToolCleanupFailurePreservesWorkerResources(t *testing.T) {
	a, store, id, w := settlementFixture(t)
	err := settlementOperation(t, a, store, id, w, func(s *Snapshot) {
		s.Run.Status = StatusCompleted
		s.Actions = []Action{{CallID: "unclean", Name: "exec", Status: "resolved_manually", FinishedAt: time.Now(), Result: &ToolResult{WorkerCleanupError: "owned process still live", WorkerPID: 123, OutcomeUncertain: true}}}
	})
	if err == nil {
		t.Fatal("cleanup failure hidden")
	}
	parent, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Load(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if child.WorkerSettlement == nil || child.WorkerSettlement.CleanupStatus != "uncertain" || parent.Workers[0].CleanupStatus != "uncertain" || occupiedWorkerSlots(&parent) != 1 || !taskOwnsResources(&parent, parent.Plan.Tasks[0]) || hasString(parent.Plan.Frontier, "next") {
		t.Fatalf("cleanup released ownership: %+v %+v", parent.Workers, child.WorkerSettlement)
	}
	// A bad completion receipt cannot override durable tool cleanup failure.
	_, err = store.Update(w.RunID, func(s *Snapshot) error {
		s.WorkerSettlement.CleanupStatus = "completed"
		s.WorkerSettlement.CleanupError = ""
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(id, func(s *Snapshot) error { s.Workers[0].SettledVisit = nil; return a.applyWorkerSettlementLocked(s, w) })
	if err != nil {
		t.Fatal(err)
	}
	parent, err = store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Workers[0].CleanupStatus != "uncertain" || occupiedWorkerSlots(&parent) != 1 {
		t.Fatal("receipt bypassed tool cleanup failure")
	}
}
