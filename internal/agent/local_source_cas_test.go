package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func localSourceCASFixture(t *testing.T, backend, parentKind string) (*Agent, *Store, string, AgentWorker, string) {
	t.Helper()
	a, store := treeCapacityAgent(t, backend, MockProvider{})
	parentID := createTestRun(t, a)
	owner := "manual"
	if parentKind == "worker" {
		if _, err := a.SetPlan(parentID, capacityTasks(1)); err != nil {
			t.Fatal(err)
		}
		local, w := capacityFixtureChild(t, a, parentID, "0")
		a, parentID, owner = local, w.RunID, w.ID
	}
	source := PlanTask{ID: "A", Goal: "original local work", SuccessContains: "A verified"}
	if _, err := a.SetPlan(parentID, []PlanTask{source}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(parentID, source.ID, owner, 3600); err != nil {
		t.Fatal(err)
	}
	ref, err := store.PutArtifact("stdout", []byte("A verified"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(parentID, func(s *Snapshot) error {
		resultValidationSource(t, s, "a", "exec", "exec", "", ref)
		s.Actions[len(s.Actions)-1].PlanTaskID = source.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	completed, err := a.CompleteTask(parentID, source.ID, owner, "verified", []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := planTask(&completed, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "verified" || task.ChildRunID != "" || task.EvidenceRunID != parentID || task.LocalExecution != (parentKind == "worker") {
		t.Fatalf("source did not complete in its parent executor: %+v", task)
	}
	if _, err := store.Update(parentID, func(s *Snapshot) error {
		s.Plan.Tasks[0].Remaining = []RemainingWork{{ID: "B", Goal: "remaining B", NextAction: "inspect B"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	next := PlanTask{
		ID: "C", Goal: "remaining B", SuccessContains: "B verified",
		Dependencies: []string{source.ID},
		Continues:    []WorkReference{{TaskID: source.ID, ItemID: "B"}},
	}
	if _, err := a.SetPlan(parentID, []PlanTask{source, next}); err != nil {
		t.Fatal(err)
	}
	local, w := capacityFixtureChild(t, a, parentID, next.ID)
	return local, store, parentID, w, filepath.Join(store.root, "artifacts", ref.Hash)
}

func TestExecutorOwnershipRechecksLocalSourceCAS(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, parentKind := range []string{"root", "worker"} {
			for _, phase := range []string{"admission", "receipt"} {
				t.Run(backend+"/"+parentKind+"/"+phase, func(t *testing.T) {
					local, store, parentID, w, blobPath := localSourceCASFixture(t, backend, parentKind)
					original := ToolResult{
						Stdout: "remote accepted B", Stderr: "remote warning", ExitCode: 7,
						Error: "remote observation", MCPResult: json.RawMessage(`{"accepted":true}`),
					}
					responsibilityPending(t, local, w.RunID, ToolCall{ID: "intact-cas", Name: "exec", Arguments: `{}`})
					sent := 0
					control, err := local.executeAction(context.Background(), w.RunID, "intact-cas", "mcp", nil, func() ToolResult {
						sent++
						return original
					})
					if err != nil || sent != 1 || len(control.Actions) != 1 {
						t.Fatalf("intact CAS did not reach the successor executor: sent=%d actions=%+v err=%v", sent, control.Actions, err)
					}
					valid := control.Actions[0]
					if valid.Status != "completed" || valid.Result == nil || valid.Result.OutcomeUncertain || valid.ExecutorOwnershipError != "" {
						t.Fatalf("intact local source rejected: %+v", valid)
					}

					responsibilityPending(t, local, w.RunID, ToolCall{ID: "missing-cas", Name: "exec", Arguments: `{}`})
					parent, err := store.Load(parentID)
					if err != nil {
						t.Fatal(err)
					}
					source, err := planTask(&parent, "A")
					if err != nil {
						t.Fatal(err)
					}
					output, verified := verifiedOutput(&parent, "a")
					if source.Status != "verified" || !verified || output != "A verified" {
						t.Fatalf("parent context did not verify the original CAS observation: task=%+v output=%q verified=%v", source, output, verified)
					}
					invalidate := func() {
						t.Helper()
						if err := os.Remove(blobPath); err != nil {
							t.Fatal(err)
						}
						// A verified Load/Update here would revoke A before the Action's own fence.
						unlock, err := store.acquireStoreLock()
						if err != nil {
							t.Fatal(err)
						}
						defer unlock()
						raw, err := store.loadRawLocked(parentID)
						if err != nil {
							t.Fatal(err)
						}
						task, err := planTask(&raw, "A")
						if err != nil {
							t.Fatal(err)
						}
						if task.Status != "verified" || task.ChildRunID != "" {
							t.Fatalf("local source metadata was revoked before the executor fence: %+v", task)
						}
						if _, verified := verifiedOutput(&raw, "a"); verified {
							t.Fatal("deleted CAS blob still supplied verified evidence")
						}
					}
					if phase == "admission" {
						invalidate()
					}
					sent = 0
					_, err = local.executeAction(context.Background(), w.RunID, "missing-cas", "mcp", nil, func() ToolResult {
						sent++
						if phase == "receipt" {
							invalidate()
						}
						return original
					})
					const sourceChanged = "original local evidence changed before Action"
					if phase == "admission" {
						if err == nil || sent != 0 || !strings.Contains(err.Error(), sourceChanged) {
							t.Fatalf("missing local CAS bypassed admission: sent=%d err=%v", sent, err)
						}
					} else if err != nil || sent != 1 {
						t.Fatalf("in-flight observation lost: sent=%d err=%v", sent, err)
					}
					persisted, err := store.Load(w.RunID)
					if err != nil {
						t.Fatal(err)
					}
					if phase == "admission" {
						if len(persisted.Actions) != 1 {
							t.Fatalf("denied Action was persisted: %+v", persisted.Actions)
						}
						return
					}
					if len(persisted.Actions) != 2 {
						t.Fatalf("in-flight receipt was not persisted: %+v", persisted.Actions)
					}
					action := persisted.Actions[1]
					if action.CallID != "missing-cas" || action.Status != "uncertain" || action.Result == nil || !action.Result.OutcomeUncertain || !strings.Contains(action.ExecutorOwnershipError, sourceChanged) {
						t.Fatalf("missing local CAS promoted an in-flight receipt: %+v", action)
					}
					got := *action.Result
					got.OutcomeUncertain, got.EvidenceID = false, ""
					if !reflect.DeepEqual(got, original) {
						t.Fatalf("raw observation changed: got=%+v want=%+v", got, original)
					}
				})
			}
		}
	}
}
