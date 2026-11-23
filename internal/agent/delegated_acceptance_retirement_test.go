package agent

import (
	"context"
	"testing"
)

func delegatedCancelledStage(t *testing.T, backend, disposition string) (delegatedFixture, delegatedFixture) {
	t.Helper()
	a, store := responsibilityAgent(t, backend, manualProvider(ToolCall{ID: "cancelled-call", Name: "exec", Arguments: "{}"}))
	predicate := delegatedProof
	root, err := a.CreateConfigured("retire cancelled effects, then run original final probes", RunConfiguration{Requirements: delegatedRequirements(), SuccessContains: &predicate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{{ID: "retired", Goal: "attempt retired after cancellation", Stage: true}}); err != nil {
		t.Fatal(err)
	}
	_, w := capacityFixtureChild(t, a, root.Run.ID, "retired")
	if _, err = store.Update(root.Run.ID, func(s *Snapshot) error { addEvent(s, "worker_started", "cancelled attempt visit", w); return nil }); err != nil {
		t.Fatal(err)
	}
	unlock, err := store.AcquireRunLock(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.runChildOperation(context.Background(), root.Run.ID, w, func(child *Agent, _ context.Context, id string) (Snapshot, error) {
		waitTestRun(t, child, id)
		return store.Update(id, func(s *Snapshot) error {
			requestID := s.Requests[len(s.Requests)-1].ID
			s.Actions = append(s.Actions, Action{RequestID: requestID, CallID: "cancelled-call", Name: "exec", Adapter: "exec", Arguments: "{}", Status: "uncertain", ResourceCleanupStatus: "completed"})
			s.Run.Status = StatusCancelled
			return nil
		})
	})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	// Exercise the same real disposition API that admits uncertain cancelled
	// actions after committed cleanup, with no success/pending-call waiver.
	if _, err = a.DisposeTask(root.Run.ID, "retired", disposition, "cancelled attempt retired; later leaf independently checks cleanup", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(root.Run.ID, func(s *Snapshot) error { s.Workers[0].Status = "uncertain"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = a.UpdatePlan(root.Run.ID, []PlanTask{{ID: "last-cleanup", Goal: "run original probes after retired stage"}}, "append"); err != nil {
		t.Fatal(err)
	}
	a.provider = manualProvider(ToolCall{ID: "goal", Name: "exec", Arguments: `{"command":"printf delegated-original-proof"}`}, ToolCall{ID: "cleanup", Name: "exec", Arguments: `{"command":"printf clean"}`})
	last := delegatedFixtureSettle(t, a, store, root.Run.ID, "last-cleanup", false)
	return delegatedFixture{a: a, store: store, root: root.Run.ID, worker: w}, last
}

func TestDelegatedAcceptanceCleanupAllowsCancelledDisposedStageJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, disposition := range []string{"blocked", "abandoned"} {
			t.Run(backend+"/"+disposition, func(t *testing.T) {
				old, last := delegatedCancelledStage(t, backend, disposition)
				s := delegatedRootCheck(t, last)
				delegatedRootClaim(t, last)
				child, err := last.store.Load(old.worker.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if len(pendingToolCalls(&child)) == 0 || child.Actions[0].Status != "uncertain" {
					t.Fatal("fixture lost cancelled uncertainty")
				}
				if delegatedSourceBound(&s, s.Plan.Tasks[0], s.Workers[0], &child) {
					t.Fatal("cancelled retirement became a source provider")
				}
			})
		}
	}
}

func TestDelegatedAcceptanceCancelledRetirementRequiresCurrentCleanupJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, mutation := range []string{"started", "not_cancelled", "receipt_visit", "settled_visit", "worker_unsettled", "receipt_cleanup", "tool_cleanup", "descendant_cleanup", "descendant_settlement", "live_descendant"} {
			t.Run(backend+"/"+mutation, func(t *testing.T) {
				old, last := delegatedCancelledStage(t, backend, "abandoned")
				delegatedRootCheck(t, last)
				delegatedRootClaim(t, last)
				target := old.worker.RunID
				if mutation == "settled_visit" || mutation == "worker_unsettled" {
					target = last.root
				}
				delegatedRawMutation(t, last, target, func(s *Snapshot) {
					switch mutation {
					case "started":
						s.Actions[0].Status = "started"
					case "not_cancelled":
						s.Run.Status = StatusPaused
					case "receipt_visit":
						s.WorkerSettlement.Visit++
					case "settled_visit":
						s.Workers[0].SettledVisit = nil
					case "worker_unsettled":
						s.Workers[0].Status = "running"
					case "receipt_cleanup":
						s.WorkerSettlement.CleanupStatus = "uncertain"
					case "tool_cleanup":
						s.Actions[0].ResourceCleanupStatus = "uncertain"
					case "descendant_cleanup":
						s.WorkerSettlement.DescendantCleanupPending = true
					case "descendant_settlement":
						s.WorkerSettlement.DescendantSettlementPending = true
					case "live_descendant":
						s.Workers = append(s.Workers, AgentWorker{ID: "unsettled-descendant", TaskID: "nested", RunID: "missing", Status: "running", CleanupStatus: "pending"})
					}
				})
				s, err := last.a.CheckEvidence(context.Background(), last.root, delegatedRequirements()[1].ID, []string{last.qualified(1)})
				if err == nil && latestCheck(&s, delegatedRequirements()[1].ID).Status == "pass" {
					t.Fatal("unsettled cancelled predecessor admitted")
				}
				if s.Result.Status == "success" {
					t.Fatal("root result survived invalid predecessor cleanup")
				}
			})
		}
	}
}
