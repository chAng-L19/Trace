package agent

import (
	"context"
	"errors"
	"testing"
)

func TestContinuationLocalUnknownActionFence(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			root := createTestRun(t, a)
			if _, err := a.SetPlan(root, capacityTasks(1)); err != nil {
				t.Fatal(err)
			}
			local, w := capacityFixtureChild(t, a, root, "0")
			source := PlanTask{ID: "source", Goal: "stage A", SuccessContains: "A verified", ResourceKeys: []string{"object"}}
			if _, err := local.SetPlan(w.RunID, []PlanTask{source}); err != nil {
				t.Fatal(err)
			}
			if _, err := local.ClaimTask(w.RunID, "source", w.ID, 3600); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(w.RunID, func(s *Snapshot) error {
				resultValidationSource(t, s, "a", "exec", "exec", "A verified")
				s.Actions[len(s.Actions)-1].PlanTaskID = "source"
				resultValidationSource(t, s, "b-unknown", "remote", "mcp", "B response unknown")
				action := &s.Actions[len(s.Actions)-1]
				action.PlanTaskID = "source"
				action.Status = "uncertain"
				action.Result.OutcomeUncertain = true
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := local.CompleteTask(w.RunID, "source", w.ID, "verified", []string{"a"}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(w.RunID, func(s *Snapshot) error {
				s.Plan.Tasks[0].Remaining = []RemainingWork{{ID: "B", Goal: "remaining B", NextAction: "inspect saved B"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			next := PlanTask{ID: "next", Goal: "remaining B", SuccessContains: "B verified", ResourceKeys: source.ResourceKeys, Dependencies: []string{"source"}, Continues: []WorkReference{{TaskID: "source", ItemID: "B"}}}
			other := PlanTask{ID: "other", Goal: "independent B write", ResourceKeys: source.ResourceKeys}
			if _, err := local.SetPlan(w.RunID, []PlanTask{source, next, other}); err != nil {
				t.Fatal(err)
			}
			if _, err := local.ClaimTask(w.RunID, "next", w.ID, 3600); err == nil {
				t.Fatal("local continuation bypassed unknown original effect")
			}
			var busy *ResourceBusyError
			if _, err := local.ClaimTask(w.RunID, "other", w.ID, 3600); !errors.As(err, &busy) {
				t.Fatalf("unknown local effect released same resource: %v", err)
			}
			sourceState, _ := store.Load(w.RunID)
			// Control Actions never make their own claim/completion source unknown.
			sourceState.Actions = []Action{{Name: "task_claim", Adapter: "builtin", Status: "started"}, {Name: "task_complete", Adapter: "builtin", Status: "started"}}
			if localWorkSourceUnresolved(&sourceState, sourceState.Plan.Tasks[0]) {
				t.Fatal("control Action blocked its own transition")
			}
		})
	}
}

func TestExecutorOwnershipRechecksContinuationSources(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, phase := range []string{"admission", "receipt"} {
			t.Run(backend+"/"+phase, func(t *testing.T) {
				a, store := treeCapacityAgent(t, backend, taskContractProvider{})
				defer a.CloseMCP()
				root := createTestRun(t, a)
				source := PlanTask{ID: "source", Goal: "first target", Requirements: []VerificationRequirement{taskRequirement("first-proof")}}
				if _, err := a.SetPlan(root, []PlanTask{source}); err != nil {
					t.Fatal(err)
				}
				workers, err := a.DispatchFrontier(context.Background(), root)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := waitAllTestWorkers(a, context.Background(), root, 10); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Update(root, func(s *Snapshot) error {
					s.Plan.Tasks[0].Remaining = []RemainingWork{{ID: "B", Goal: "remaining B", NextAction: "inspect original B"}}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				next := PlanTask{ID: "next", Goal: "remaining B", SuccessContains: "B verified", Dependencies: []string{"source"}, Continues: []WorkReference{{TaskID: "source", ItemID: "B"}}}
				if _, err := a.SetPlan(root, []PlanTask{source, next}); err != nil {
					t.Fatal(err)
				}
				local, w := capacityFixtureChild(t, a, root, "next")
				responsibilityPending(t, local, w.RunID, ToolCall{ID: "new-write", Name: "exec", Arguments: `{}`})
				invalidate := func() {
					if _, err := store.Update(workers[0].RunID, func(s *Snapshot) error {
						resultValidationSource(t, s, "unknown", "remote", "mcp", "new unknown external effect")
						action := &s.Actions[len(s.Actions)-1]
						action.Status = "uncertain"
						action.Result.OutcomeUncertain = true
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				if phase == "admission" {
					invalidate()
				}
				sent := 0
				got, err := local.executeAction(context.Background(), w.RunID, "new-write", "mcp", nil, func() ToolResult {
					sent++
					if phase == "receipt" {
						invalidate()
					}
					return ToolResult{Stdout: "sent receipt"}
				})
				if phase == "admission" {
					if err == nil || sent != 0 {
						t.Fatalf("revoked source reached external I/O: %d %v", sent, err)
					}
				} else if err != nil || sent != 1 || len(got.Actions) != 1 || got.Actions[0].ExecutorOwnershipError == "" || !got.Actions[0].Result.OutcomeUncertain {
					t.Fatalf("source changed during I/O promoted receipt: %+v %v", got.Actions, err)
				}
			})
		}
	}
}

func TestContinuationLocalCleanupUsesOriginalContract(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, cleaned := range []bool{false, true} {
			name := backend + "/unknown"
			if cleaned {
				name = backend + "/cleaned"
			}
			t.Run(name, func(t *testing.T) {
				a, store := treeCapacityAgent(t, backend, MockProvider{})
				id := createTestRun(t, a)
				source := PlanTask{ID: "source", Goal: "stage", SuccessContains: "A verified", ResourceKeys: []string{"object"}}
				if _, err := a.SetPlan(id, []PlanTask{source}); err != nil {
					t.Fatal(err)
				}
				if _, err := a.ClaimTask(id, "source", "manual", 3600); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Update(id, func(s *Snapshot) error {
					resultValidationSource(t, s, "a", "exec", "exec", "A verified")
					action := Action{PlanTaskID: "source", Adapter: "exec", Status: "resolved_manually"}
					if cleaned {
						action.Result = &ToolResult{WorkerCleanupError: "historical failed cleanup"}
						action.ResourceCleanupStatus = "completed"
					}
					s.Actions = append(s.Actions, action)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := a.CompleteTask(id, "source", "manual", "verified", []string{"a"}); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Update(id, func(s *Snapshot) error {
					s.Plan.Tasks[0].Remaining = []RemainingWork{{ID: "B", Goal: "remaining B", NextAction: "inspect B"}}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				next := PlanTask{ID: "next", Goal: "remaining B", SuccessContains: "B verified", ResourceKeys: source.ResourceKeys, Continues: []WorkReference{{TaskID: "source", ItemID: "B"}}, Dependencies: []string{"source"}}
				other := PlanTask{ID: "other", Goal: "same key operation", ResourceKeys: source.ResourceKeys}
				if _, err := a.SetPlan(id, []PlanTask{source, next, other}); err != nil {
					t.Fatal(err)
				}
				_, otherErr := a.ClaimTask(id, "other", "manual-other", 3600)
				if otherErr == nil {
					if _, err := a.CompleteTask(id, "other", "manual-other", "completed", nil); err != nil {
						t.Fatal(err)
					}
				}
				_, nextErr := a.ClaimTask(id, "next", "manual-next", 3600)
				if cleaned && (nextErr != nil || otherErr != nil) {
					t.Fatalf("old cleanup error overrode completed proof: %v %v", nextErr, otherErr)
				}
				if !cleaned && (nextErr == nil || otherErr == nil) {
					t.Fatalf("manual business observation waived missing process cleanup: %v %v", nextErr, otherErr)
				}
			})
		}
	}
}

func TestRootRemainingLocalActionDoesNotInvalidateItself(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			id := createTestRun(t, a)
			if _, err := store.Update(id, func(s *Snapshot) error {
				resultValidationClaim(t, s, Result{Status: "incomplete", Remaining: []RemainingWork{{ID: "B", Goal: "remaining B", NextAction: "inspect B"}}})
				s.Run.Status = StatusCreated
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			task := PlanTask{ID: "next", Goal: "remaining B", SuccessContains: "B verified", Continues: []WorkReference{{TaskID: rootWorkTaskID, ItemID: "B"}}}
			if _, err := a.SetPlan(id, []PlanTask{task}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ClaimTask(id, "next", "manual", 3600); err != nil {
				t.Fatal(err)
			}
			responsibilityPending(t, a, id, ToolCall{ID: "probe", Name: "exec", Arguments: `{}`})
			snap, err := a.executeAction(context.Background(), id, "probe", "mcp", nil, func() ToolResult {
				current, err := store.Load(id)
				if err != nil || current.Plan.Tasks[0].Status != "running" {
					t.Fatalf("own admitted Action revoked continuation: %v %+v", err, current.Plan)
				}
				return ToolResult{Stdout: "B verified"}
			})
			if err != nil || len(snap.Actions) != 1 || snap.Actions[0].Status != "completed" || snap.Actions[0].PlanTaskID != "next" {
				t.Fatalf("local root action: %+v %v", snap.Actions, err)
			}
			// A later unresolved Action is never waived merely because it belongs to this successor.
			if _, err := store.Update(id, func(s *Snapshot) error { s.Actions[0].Status = "uncertain"; return nil }); err != nil {
				t.Fatal(err)
			}
			current, _ := store.Load(id)
			if !localWorkSourceUnresolved(&current, PlanTask{ID: rootWorkTaskID}) {
				t.Fatal("root skipped historical unknown successor effect")
			}
			// Another root obligation may not borrow next's in-flight exemption.
			current.Actions[0].Status = "started"
			current.Plan.Tasks[0].Status = "running"
			if !localWorkSourceUnresolved(&current, PlanTask{ID: rootWorkTaskID}, "other") {
				t.Fatal("another successor borrowed admitted Action exemption")
			}
			if localWorkSourceUnresolved(&current, PlanTask{ID: rootWorkTaskID}, "next") {
				t.Fatal("own running Action lost its bounded exemption")
			}
		})
	}
}
