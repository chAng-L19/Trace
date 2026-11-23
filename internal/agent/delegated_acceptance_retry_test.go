package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The failed visit has genuine production cleanup/settlement and is sealed by
// RetryTask itself. The replacement then executes the original exact probes.
func delegatedRetryFixture(t *testing.T, backend string) (delegatedFixture, delegatedFixture) {
	t.Helper()
	a, store := responsibilityAgent(t, backend, manualProvider(
		ToolCall{ID: "goal", Name: "exec", Arguments: `{"command":"printf delegated-original-proof"}`},
		ToolCall{ID: "cleanup", Name: "exec", Arguments: `{"command":"printf clean"}`},
	))
	predicate := delegatedProof
	root, err := a.CreateConfigured("retry zero-effect failure, satisfy original contract", RunConfiguration{Requirements: delegatedRequirements(), SuccessContains: &predicate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{{ID: "work", Goal: "execute the original goal and cleanup probes"}}); err != nil {
		t.Fatal(err)
	}
	const previousWorker = "retry-zero-effect-worker"
	if _, err = a.ClaimTask(root.Run.ID, "work", previousWorker, 3600); err != nil {
		t.Fatal(err)
	}
	child, err := a.CreateConfigured("zero-effect failed attempt", RunConfiguration{role: "worker", Requirements: []VerificationRequirement{delegatedRequirements()[1]}})
	if err != nil {
		t.Fatal(err)
	}
	w := AgentWorker{ID: previousWorker, TaskID: "work", RunID: child.Run.ID, Visit: 1, Status: "running", CleanupStatus: "pending"}
	if _, err = store.Update(child.Run.ID, func(s *Snapshot) error {
		s.Run.ParentRunID, s.Run.ParentTaskID, s.Run.Status = root.Run.ID, "work", StatusRunning
		s.WorkerContext = &WorkerContext{RunID: child.Run.ID, ParentRunID: root.Run.ID, TaskID: "work", WorkerID: w.ID}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(root.Run.ID, func(s *Snapshot) error {
		s.Run.Status = StatusRunning
		s.Plan.Tasks[0].ChildRunID = child.Run.ID
		s.Workers = append(s.Workers, w)
		addEvent(s, "worker_started", "original zero-effect visit", w)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	unlock, err := store.AcquireRunLock(child.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.runChildOperation(context.Background(), root.Run.ID, w, func(_ *Agent, _ context.Context, id string) (Snapshot, error) {
		return store.Update(id, func(s *Snapshot) error {
			s.Run.Status, s.Run.Error = StatusFailed, "failed before executing any action"
			return nil
		})
	})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	retried, err := a.RetryTask(root.Run.ID, "work")
	if err != nil {
		t.Fatal(err)
	}
	if retried.Workers[0].Status != "superseded" || retried.Plan.Tasks[0].ChildRunID != "" || retried.Plan.Tasks[0].WorkerID != "" {
		t.Fatal("RetryTask did not seal and unbind the old attempt")
	}
	old, err := store.Load(w.RunID)
	if err != nil || old.Run.Status != StatusCancelled || len(old.Actions)+len(old.Evidence)+len(old.Workers)+len(pendingToolCalls(&old)) != 0 {
		t.Fatalf("retry fixture has potential effects: %+v %v", old.Run, err)
	}
	last := delegatedFixtureSettle(t, a, store, root.Run.ID, "work", false)
	return delegatedFixture{a: a, store: store, root: root.Run.ID, worker: w}, last
}

func TestDelegatedAcceptanceRetryTaskZeroEffectsJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			old, last := delegatedRetryFixture(t, backend)
			s := delegatedRootCheck(t, last)
			delegatedRootClaim(t, last)
			sealed, err := last.store.Load(old.worker.RunID)
			if err != nil {
				t.Fatal(err)
			}
			cleanup := delegatedRequirements()[1]
			if len(sealed.Requirements) != 1 || valueHash(sealed.Requirements[0]) != valueHash(cleanup) {
				t.Fatal("old cleanup requirement was removed or weakened")
			}
			if check := latestCheck(&sealed, cleanup.ID); check != nil && check.Status == "pass" {
				t.Fatal("zero-action attempt gained a fabricated business cleanup pass")
			}
			historical := s.Plan.Tasks[0]
			historical.WorkerID, historical.ChildRunID, historical.EvidenceRunID = old.worker.ID, old.worker.RunID, old.worker.RunID
			if delegatedWorkerRetired(&s, historical, s.Workers[0], &sealed, true, false) {
				t.Fatal("ordinary retirement waived the old business cleanup requirement")
			}
			if len(s.Workers) != 2 || s.Workers[0].Status != "superseded" || s.Plan.Tasks[0].ChildRunID != last.worker.RunID || s.Plan.Tasks[0].WorkerID != last.worker.ID || last.worker.ID == old.worker.ID {
				t.Fatal("replacement did not retain distinct original registration")
			}
			for id := range s.delegatedAcceptance {
				if strings.HasPrefix(id, old.worker.RunID+"/") {
					t.Fatal("sealed failed attempt became outcome evidence")
				}
			}
			if len(s.Actions) != 0 || len(s.Evidence) != 0 {
				t.Fatal("retry acceptance fabricated root execution")
			}
			reader, err := NewStore(last.store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			reopened, err := reader.Load(last.root)
			if err != nil || reopened.Result.Status != "success" {
				t.Fatalf("retry acceptance failed after reopen: %+v %v", reopened.Result, err)
			}
		})
	}
}

func TestDelegatedAcceptanceRetrySealRevocationJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, mutation := range []string{"action", "pending", "evidence", "descendant", "receipt_cleanup", "worker_cleanup", "receipt_visit", "settled_visit", "ancestry", "context", "root_retry", "child_seal", "fake_superseded", "missing_parent_event", "missing_child_event", "late_finish"} {
			t.Run(backend+"/"+mutation, func(t *testing.T) {
				old, last := delegatedRetryFixture(t, backend)
				delegatedRootCheck(t, last)
				delegatedRootClaim(t, last)
				target := old.worker.RunID
				if mutation == "worker_cleanup" || mutation == "settled_visit" || mutation == "root_retry" || mutation == "fake_superseded" || mutation == "missing_parent_event" || mutation == "late_finish" {
					target = last.root
				}
				if mutation == "fake_superseded" {
					delegatedRawMutation(t, last, old.worker.RunID, func(s *Snapshot) {
						for i := range s.Events {
							if s.Events[i].Type == "worker_retry_superseded" {
								s.Events[i].Type = "unproven_superseded"
							}
						}
					})
				}
				delegatedRawMutation(t, last, target, func(s *Snapshot) {
					switch mutation {
					case "action":
						s.Actions = append(s.Actions, Action{RequestID: "later", CallID: "effect", Name: "exec", Adapter: "exec", Status: "uncertain", ResourceCleanupStatus: "completed"})
					case "pending":
						s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "pending", Name: "exec", Arguments: "{}"}}})
					case "evidence":
						s.Evidence = append(s.Evidence, Evidence{ID: "late-evidence"})
					case "descendant":
						s.Workers = append(s.Workers, AgentWorker{ID: "late-worker", TaskID: "nested", RunID: "missing", Status: "running", CleanupStatus: "pending"})
					case "receipt_cleanup":
						s.WorkerSettlement.CleanupStatus = "uncertain"
					case "worker_cleanup":
						s.Workers[0].CleanupStatus = "uncertain"
					case "receipt_visit":
						s.WorkerSettlement.Visit++
					case "settled_visit":
						s.Workers[0].SettledVisit = nil
					case "ancestry":
						s.Run.ParentRunID = "other-root"
					case "context":
						s.WorkerContext.WorkerID = "other-worker"
					case "late_finish":
						addEvent(s, "worker_finished", "old attempt settled after the original probe visit started", map[string]string{"worker_id": old.worker.ID, "run_id": old.worker.RunID, "cleanup_status": "completed"})
					case "fake_superseded", "missing_parent_event", "missing_child_event":
						for i := range s.Events {
							if s.Events[i].Type == "task_retry_requested" || s.Events[i].Type == "worker_retry_superseded" {
								s.Events[i].Type = "missing_retry_seal"
							}
						}
					case "root_retry", "child_seal":
						for i := range s.Events {
							if s.Events[i].Type != "task_retry_requested" && s.Events[i].Type != "worker_retry_superseded" {
								continue
							}
							raw, _ := json.Marshal(s.Events[i].Data)
							var fields map[string]string
							if err := json.Unmarshal(raw, &fields); err != nil {
								t.Fatal(err)
							}
							fields["task_id"] = "other-task"
							s.Events[i].Data = fields
						}
					}
				})
				s, err := last.store.Load(last.root)
				if err != nil {
					t.Fatal(err)
				}
				if s.Result.Status == "success" || latestCheck(&s, delegatedRequirements()[1].ID).Status == "pass" {
					t.Fatal("changed retry seal retained root success or cleanup verification")
				}
				s, err = last.a.CheckEvidence(context.Background(), last.root, delegatedRequirements()[1].ID, []string{last.qualified(1)})
				if err == nil && latestCheck(&s, delegatedRequirements()[1].ID).Status == "pass" {
					t.Fatal("fresh evidence_check accepted an invalid retry seal")
				}
			})
		}
	}
}
