package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The root makes its own real probe and checks both goal and post-worker cleanup.
// Child proof is never copied into the root's evidence ledger.
type dispositionRootProvider struct{}

func (dispositionRootProvider) Complete(ctx context.Context, request ModelRequest) (ModelResponse, error) {
	last := lastConversationMessage(request)
	if last.Role == "tool" && last.ToolCallID == "check" {
		for _, message := range request.Messages {
			if message.Role != "tool" || message.ToolCallID != "probe" {
				continue
			}
			var result ToolResult
			if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
				return ModelResponse{}, err
			}
			arguments, _ := json.Marshal(map[string]any{"requirement_id": "cleanup-proof", "evidence_ids": []string{result.EvidenceID}})
			return ModelResponse{ToolCalls: []ToolCall{{ID: "cleanup-check", Name: "evidence_check", Arguments: string(arguments)}}}, nil
		}
		return ModelResponse{}, fmt.Errorf("root probe evidence missing")
	}
	return (taskContractProvider{}).Complete(ctx, request)
}

func TestTreeWorkerCapacityDisposedUncertainRootCompletion(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, disposition := range []string{"superseded", "abandoned"} {
			t.Run(backend+"/"+disposition, func(t *testing.T) {
				a, store := treeCapacityAgent(t, backend, taskContractProvider{})
				defer a.CloseMCP()
				goal := taskRequirement("first-proof")
				cleanup := taskRequirement("cleanup-proof")
				cleanup.Category = "cleanup"
				cleanup.ArgumentsHash = promptHash(`{"command":"printf '{\"ok\":true}'"}`)
				root, err := a.CreateConfigured("complete independently verified root work", RunConfiguration{
					Requirements: []VerificationRequirement{goal, cleanup},
					RootContract: &RootContract{Version: 1, RequirementIDs: []string{goal.ID, cleanup.ID}},
				})
				if err != nil {
					t.Fatal(err)
				}
				old := PlanTask{ID: "old", Goal: "inspect source", Stage: true, Requirements: []VerificationRequirement{goal}}
				tasks := []PlanTask{old}
				if disposition == "superseded" {
					replacement := old
					replacement.ID = "replacement"
					tasks = append(tasks, replacement)
				}
				if _, err = a.SetPlan(root.Run.ID, tasks); err != nil {
					t.Fatal(err)
				}
				if _, err = a.SetPlanCapacity(root.Run.ID, 1); err != nil {
					t.Fatal(err)
				}
				_, w := capacityFixtureChild(t, a, root.Run.ID, old.ID)
				if err = settlementOperation(t, a, store, root.Run.ID, w, func(s *Snapshot) { s.Run.Status, s.Run.PauseReason = StatusPaused, "parent_pause" }); err != nil {
					t.Fatal(err)
				}
				if _, err = store.Update(root.Run.ID, func(s *Snapshot) error {
					s.Workers[0].Status, s.Plan.Tasks[0].Status = "uncertain", "uncertain"
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				replacementID := ""
				if disposition == "superseded" {
					replacementID = "replacement"
				}
				parent, err := a.DisposeTask(root.Run.ID, old.ID, disposition, "original settled attempt inspected; retire this path", replacementID)
				if err != nil {
					t.Fatal(err)
				}
				if parent.Workers[0].Status != "uncertain" || workerUnsettled(&parent, parent.Workers[0]) {
					t.Fatal("disposition rewrote history or kept retired worker unsettled")
				}
				// Reconciliation observes the original receipt repeatedly; it must
				// preserve both the explicit disposition and uncertain history.
				for range 2 {
					if err = a.reconcileWorkerSettlements(root.Run.ID); err != nil {
						t.Fatal(err)
					}
				}
				parent, err = store.Load(root.Run.ID)
				if err != nil || parent.Plan.Tasks[0].Status != disposition || parent.Workers[0].Status != "uncertain" {
					t.Fatalf("late reconciliation rewrote disposition: %+v %v", parent.Plan, err)
				}
				if disposition == "superseded" {
					if _, err = a.DispatchTask(context.Background(), root.Run.ID, replacementID); err != nil {
						t.Fatal(err)
					}
					parent, err = waitAllTestWorkers(a, context.Background(), root.Run.ID, 10)
					if err != nil || parent.Plan.Tasks[1].Status != "verified" {
						t.Fatalf("replacement proof: %+v %v", parent.Plan, err)
					}
				}
				if len(rootContractGaps(&parent)) == 0 || parent.Result.Status == "success" || len(parent.Evidence) != 0 {
					t.Fatal("retired/replacement child bypassed root-local proof")
				}
				a.provider = dispositionRootProvider{}
				finished, err := a.Run(context.Background(), root.Run.ID)
				if err != nil || finished.Result.Status != "success" || finished.Result.CleanupStatus != "completed" {
					t.Fatalf("root stayed blocked after lawful retirement: %+v %v", finished.Result, err)
				}
				if len(finished.Evidence) == 0 || len(rootContractGaps(&finished)) != 0 || finished.Workers[0].Status != "uncertain" {
					t.Fatal("root evidence or worker history changed")
				}
				if cleanupFreshnessFloor(&finished) >= finished.Run.EventSeq {
					t.Fatal("retired worker kept cleanup freshness permanently open")
				}
				if continueForEvidence(&finished) {
					t.Fatalf("completed root still requests continuation: %+v", finished.Result)
				}
				a, store = reopenCapacityAgent(t, store)
				finished, err = store.Load(root.Run.ID)
				if err != nil || finished.Result.Status != "success" {
					t.Fatalf("restart lost root completion: %+v %v", finished.Result, err)
				}
				originalWorker, originalTask := finished.Workers[0], finished.Plan.Tasks[0]
				originalEvidence := append([]Evidence(nil), finished.Evidence...)
				for _, negative := range []string{"cleanup_unknown", "running", "starting", "cancelling", "old_identity", "stale_visit", "missing_reason", "blank_reason", "root_proof_changed"} {
					bad, err := store.Update(root.Run.ID, func(s *Snapshot) error {
						s.Workers[0], s.Plan.Tasks[0] = originalWorker, originalTask
						s.Evidence = append([]Evidence(nil), originalEvidence...)
						switch negative {
						case "cleanup_unknown":
							s.Workers[0].CleanupStatus = "unknown"
						case "running", "starting", "cancelling":
							s.Workers[0].Status = negative
						case "old_identity":
							s.Plan.Tasks[0].WorkerID = "unrelated-worker"
						case "stale_visit":
							s.Workers[0].Visit++
						case "missing_reason":
							s.Plan.Tasks[0].DispositionReason = ""
						case "blank_reason":
							s.Plan.Tasks[0].DispositionReason = " \t"
						case "root_proof_changed":
							s.Evidence[0].Content += "invalid source"
						}
						return nil
					})
					if err != nil || bad.Result.Status == "success" {
						t.Fatalf("%s bypassed root acceptance: %+v %v", negative, bad.Result, err)
					}
					if negative != "root_proof_changed" {
						if !workerUnsettled(&bad, bad.Workers[0]) || cleanupFreshnessFloor(&bad) != bad.Run.EventSeq {
							t.Fatalf("%s completion/capacity gate drift", negative)
						}
						if disposition == "abandoned" && settledStage(&bad, bad.Plan.Tasks[0]) {
							t.Fatalf("%s admitted retired stage", negative)
						}
					}
					if !continueForEvidence(&bad) {
						t.Fatalf("%s silently ended root", negative)
					}
				}
			})
		}
	}
}

func TestTreeWorkerCapacityDisposedUncertainDoesNotYieldAcceptedWork(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			id := createTestRun(t, a)
			s := stageScopeSnapshot(t, nil)
			s.Run.ID, s.Run.ParentRunID, s.Run.ParentTaskID, s.Run.Status = id, "parent", "task", StatusRunning
			s.Run.SuccessContains = "true"
			visit := uint64(1)
			s.Workers = []AgentWorker{{ID: "retired", TaskID: "old", RunID: "retired-child", Visit: visit, SettledVisit: &visit, Status: "uncertain", CleanupStatus: "completed"}}
			s.Plan.Tasks = []PlanTask{{ID: "old", WorkerID: "retired", ChildRunID: "retired-child", Status: "superseded", DispositionReason: "original attempt inspected", ReplacementTaskID: "replacement", Goal: "same"}, {ID: "replacement", Goal: "same", Status: "completed"}}
			if _, err := store.Update(id, func(dst *Snapshot) error { *dst = s; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := a.YieldWorker(id, "done", "accepted work complete"); err == nil || !strings.Contains(err.Error(), "acceptance already passed") {
				t.Fatalf("retired uncertain worker allowed yielding accepted work: %v", err)
			}
		})
	}
}
