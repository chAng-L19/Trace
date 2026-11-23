package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Deterministic model decisions traverse the installed pi loop, real dispatch,
// Worker lifecycle, file operations, CAS, receipts and the existing Judge.
func TestRunResponsibilityPiRootDispatchWorkerReceiptAcceptance(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			const marker = "role-loop-worker-proof"
			path := filepath.Join(t.TempDir(), "worker-output.txt")
			var a *Agent
			var store *Store
			var rootID string
			var mu sync.Mutex
			turns := map[string]int{}
			tool := func(id, name string, args any) (ModelResponse, error) {
				raw, err := json.Marshal(args)
				return ModelResponse{ToolCalls: []ToolCall{{ID: id, Name: name, Arguments: string(raw)}}}, err
			}
			claim := func(evidenceID, summary string) (ModelResponse, error) {
				raw, err := json.Marshal(map[string]any{"trace_result": Result{Status: "success", Summary: summary, EvidenceIDs: []string{evidenceID}, Answers: []Answer{{Kind: "marker", Value: marker, EvidenceIDs: []string{evidenceID}}}}})
				return ModelResponse{Content: fixtureReport(string(raw))}, err
			}
			provider := settlementProviderFunc(func(ctx context.Context, request ModelRequest) (ModelResponse, error) {
				at := strings.LastIndex(request.ID, "-")
				if at < 0 {
					return ModelResponse{}, fmt.Errorf("missing request identity")
				}
				runID := request.ID[:at]
				mu.Lock()
				turns[runID]++
				n := turns[runID]
				mu.Unlock()
				current, err := store.Load(runID)
				if err != nil {
					return ModelResponse{}, err
				}
				if runID != rootID {
					if current.Run.Responsibility != ResponsibilityWorker || !responsibilityHasTool(request, "read") || !responsibilityHasTool(request, "worker_dispatch") {
						return ModelResponse{}, fmt.Errorf("dispatched Worker lost execution/subdispatch capability")
					}
					switch n {
					case 1:
						return tool("write-output", "write", map[string]string{"path": path, "content": marker})
					case 2:
						return tool("read-output", "read", map[string]string{"path": path})
					case 3:
						return tool("archive-output", "artifact_register", map[string]string{"path": path, "purpose": "parent acceptance"})
					case 4:
						for _, action := range current.Actions {
							if action.Name == "read" && action.Result != nil {
								return claim(action.Result.EvidenceID, "Worker wrote, read and archived the requested marker.")
							}
						}
					}
					return ModelResponse{}, fmt.Errorf("unexpected Worker turn %d", n)
				}
				for _, name := range []string{"exec", "read", "write", "source_search", "task_claim", "task_complete"} {
					if responsibilityHasTool(request, name) {
						return ModelResponse{}, fmt.Errorf("root exposed %s on turn %d", name, n)
					}
				}
				switch n {
				case 1:
					if current.Run.Responsibility != ResponsibilityCoordinator || len(current.Workers) != 0 || !strings.Contains(requestRuntimeObservations(rootID, request), "from the first turn") {
						return ModelResponse{}, fmt.Errorf("first root turn was not coordinator")
					}
					return tool("plan-work", "plan", map[string]any{"capacity": 2, "tasks": []PlanTask{{ID: "produce", Goal: "Write, read and archive " + path, SuccessContains: marker, ResourceKeys: []string{"file:" + path}}}})
				case 2:
					return tool("dispatch-work", "worker_dispatch", map[string]string{"task_id": "produce"})
				case 3:
					return tool("wait-work", "worker_wait", map[string]int{"seconds": 10})
				case 4:
					// Worker wait wakes on the first event. A test-only settlement
					// barrier makes the next scripted decision deterministic.
					settled, err := waitAllTestWorkers(a, ctx, rootID, 15)
					if err != nil {
						return ModelResponse{}, err
					}
					if len(settled.ChildReports) == 0 || !taskDone(settled.Plan.Tasks[0]) || settled.Workers[0].CleanupStatus != "completed" {
						return ModelResponse{}, fmt.Errorf("missing completed Worker receipt")
					}
					return tool("inspect-task-receipt", "worker_history", map[string]any{"kind": "tasks"})
				case 5:
					for _, m := range current.Messages {
						if strings.Contains(m.Content, "Child settlement report") {
							return tool("read-receipt", "session_read", map[string]string{"message_id": m.ID})
						}
					}
					return ModelResponse{}, fmt.Errorf("settlement receipt did not reach parent conversation")
				case 6:
					if len(current.Workers) != 1 {
						return ModelResponse{}, fmt.Errorf("unexpected Worker count")
					}
					child, err := store.Load(current.Workers[0].RunID)
					if err != nil {
						return ModelResponse{}, err
					}
					for _, action := range child.Actions {
						if action.Name == "artifact_register" && action.Result != nil && len(action.Result.Artifacts) == 1 {
							return tool("accept-artifact", "artifact_read", map[string]string{"hash": action.Result.Artifacts[0].Hash})
						}
					}
					return ModelResponse{}, fmt.Errorf("Worker did not archive deliverable")
				case 7:
					for _, action := range current.Actions {
						if action.Name == "artifact_read" && action.Result != nil {
							return claim(action.Result.EvidenceID, "Accepted the saved Worker settlement receipt and the exact archived output; marker delivered.")
						}
					}
				}
				return ModelResponse{}, fmt.Errorf("unexpected root turn %d", n)
			})
			a, store = responsibilityAgent(t, backend, provider)
			a.SetRequireVerifiedCompletion(true)
			root, err := a.Create("Produce the marker using a Worker, then read its receipt and archived output for acceptance.")
			if err != nil {
				t.Fatal(err)
			}
			rootID = root.Run.ID
			s, err := a.Run(piMigrationContext(t), rootID)
			if err != nil || s.Run.Status != StatusCompleted || s.Result.Status != "success" || len(s.Workers) != 1 || len(s.ChildReports) == 0 {
				t.Fatalf("orchestration loop failed: run=%+v result=%+v err=%v", s.Run, s.Result, err)
			}
			child, err := store.Load(s.Workers[0].RunID)
			if err != nil || child.Run.Responsibility != ResponsibilityWorker || child.Run.Status != StatusCompleted || child.Result.Status != "success" || len(child.Actions) != 3 {
				t.Fatalf("Worker did not execute exactly once: actions=%+v result=%+v err=%v", child.Actions, child.Result, err)
			}
			if child.WorkerSettlement == nil || child.WorkerSettlement.CleanupStatus != "completed" || s.Plan.Tasks[0].Status != "verified" {
				t.Fatal("missing durable settlement/acceptance")
			}
			content, err := os.ReadFile(path)
			if err != nil || string(content) != marker {
				t.Fatal("actual Worker output differs")
			}
			seen := map[string]bool{}
			for _, action := range s.Actions {
				seen[action.Name] = true
				if action.Adapter != "builtin" || !a.registry.permitsRunTool(s.Run, action.Name, action.Adapter) {
					t.Fatalf("root executed forbidden action: %+v", action)
				}
			}
			for _, name := range []string{"plan", "worker_dispatch", "worker_wait", "worker_history", "session_read", "artifact_read"} {
				if !seen[name] {
					t.Fatalf("root skipped %s", name)
				}
			}
			mu.Lock()
			rootTurns, workerTurns := turns[rootID], turns[child.Run.ID]
			mu.Unlock()
			if rootTurns != 7 || workerTurns != 4 {
				t.Fatalf("unexpected model/replay turns: root=%d worker=%d", rootTurns, workerTurns)
			}
		})
	}
}

func TestRunResponsibilityDispatchFailureDoesNotTakeOverExecution(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := responsibilityAgent(t, backend, MockProvider{})
			root, err := a.Create("coordinate unavailable capability")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.SetPlan(root.Run.ID, []PlanTask{{ID: "work", Goal: "needs unavailable tool", RequiredToolIDs: []string{"missing-required-capability"}}}); err != nil {
				t.Fatal(err)
			}
			call := ToolCall{ID: "dispatch", Name: "worker_dispatch", Arguments: string(mustResponsibilityJSON(t, map[string]string{"task_id": "work"}))}
			responsibilityPending(t, a, root.Run.ID, call)
			s, err := a.executePendingTool(context.Background(), root.Run.ID, call)
			if err != nil || len(s.Workers) != 0 || len(s.Actions) != 1 || s.Actions[0].Result == nil || s.Actions[0].Result.Error == "" || s.Plan.Tasks[0].Status != "blocked_capability" {
				t.Fatalf("dispatch did not preserve blocker: %+v %v", s.Plan, err)
			}
			before := len(s.Actions)
			path := filepath.Join(t.TempDir(), "forbidden-takeover")
			call = ToolCall{ID: "takeover", Name: "write", Arguments: string(mustResponsibilityJSON(t, map[string]string{"path": path, "content": "fallback"}))}
			responsibilityPending(t, a, root.Run.ID, call)
			s, err = a.executePendingTool(context.Background(), root.Run.ID, call)
			if err != nil || len(s.Actions) != before || len(s.Workers) != 0 || s.Run.Responsibility != ResponsibilityCoordinator || s.Plan.Tasks[0].Status != "blocked_capability" {
				t.Fatalf("dispatch failure changed role/ownership: %+v %v", s.Run, err)
			}
			var result ToolResult
			if err := json.Unmarshal([]byte(s.Evidence[len(s.Evidence)-1].Content), &result); err != nil || result.Diagnostic == nil || result.Diagnostic.Code != "run_role_restricted" {
				t.Fatal("missing takeover denial")
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("root took over failed dispatch")
			}
			saved, err := store.Load(root.Run.ID)
			if err != nil || saved.Plan.Tasks[0].Status != "blocked_capability" || len(saved.Workers) != 0 {
				t.Fatal("blocker was not durable")
			}
		})
	}
}

// A provider may return a tool hidden from its request schema. pi's validation
// failure must settle as a runtime observation without crossing execution.
func TestRunResponsibilityPiHiddenExecutionToolHasNoSideEffects(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "forbidden-write")
			args := string(mustResponsibilityJSON(t, map[string]string{"path": path, "content": "forbidden"}))
			p := manualProvider(ToolCall{ID: "hidden-write", Name: "write", Arguments: args})
			a, _ := responsibilityAgent(t, backend, p)
			root, err := a.Create("delegate execution")
			if err != nil {
				t.Fatal(err)
			}
			s, err := a.Run(piMigrationContext(t), root.Run.ID)
			responsibilityDenied(t, s, err)
			if s.Run.Status != StatusCompleted || len(s.Requests) != 2 || len(s.Workers) != 0 {
				t.Fatalf("hidden call did not settle once: run=%+v requests=%d", s.Run, len(s.Requests))
			}
			for _, req := range s.Requests {
				if responsibilityHasTool(req, "write") {
					t.Fatal("root request exposed hidden execution tool")
				}
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("pi executed hidden write")
			}
		})
	}
}
