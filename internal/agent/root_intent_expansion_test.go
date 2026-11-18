package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAutomaticRootIntentAllowsOneMonotonicScopeExpansion(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	requirement := taskRequirement("proof")
	root, err := a.CreateConfigured("original goal", RunConfiguration{CompletionCriteria: "deliver checked output", Requirements: []VerificationRequirement{requirement}})
	if err != nil {
		t.Fatal(err)
	}
	if root.RootContract == nil || !root.RootContract.AutoIntent {
		t.Fatal("automatic intent not marked")
	}
	id := root.Run.ID
	if _, err = a.UpsertAsset(id, Asset{ID: "document", Kind: "file", Value: "report.txt"}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []RootContract{
		{Version: 2, AssetIDs: []string{"document"}},
		{Version: 2, Goal: "different", RequirementIDs: []string{"proof"}},
		{Version: 2, CompletionCriteria: "weaker", RequirementIDs: []string{"proof"}},
		{Version: 3, RequirementIDs: []string{"proof"}},
	} {
		if _, err = a.SetRootContract(id, bad); err == nil {
			t.Fatalf("weakened scope accepted: %+v", bad)
		}
	}
	expanded, err := a.SetRootContract(id, RootContract{Version: 1, RequirementIDs: []string{"proof"}, AssetIDs: []string{"document"}, AutoIntent: true})
	if err != nil {
		t.Fatal(err)
	}
	c := expanded.RootContract
	if c.AutoIntent || c.Version != 2 || c.Goal != root.Run.Goal || c.CompletionCriteria != root.RootContract.CompletionCriteria || c.IntentHash != root.RootContract.IntentHash || c.RequirementHashes["proof"] != root.RootContract.RequirementHashes["proof"] || c.AssetHashes["document"] == "" {
		t.Fatalf("scope expansion changed fixed intent: %+v", c)
	}
	if _, err = a.SetRootContract(id, *c); err == nil {
		t.Fatal("explicit scope was reopened")
	}
	reopened, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := json.Marshal(c)
	persisted, _ := json.Marshal(reopened.RootContract)
	if string(original) != string(persisted) {
		t.Fatal("expanded scope not durable")
	}
}

func TestAutomaticRootIntentExpansionStopsAtEveryExecutionBoundary(t *testing.T) {
	for _, boundary := range []string{"request", "action", "running", "waiting_tool", "worker", "claimed_task"} {
		t.Run(boundary, func(t *testing.T) {
			a, store := newTestAgent(t, MockProvider{})
			root, err := a.CreateConfigured("goal", RunConfiguration{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Update(root.Run.ID, func(s *Snapshot) error {
				switch boundary {
				case "request":
					s.Requests = []ModelRequest{{ID: "started"}}
				case "action":
					s.Actions = []Action{{RequestID: "request", CallID: "call", Status: "started"}}
				case "running":
					s.Run.Status = StatusRunning
				case "waiting_tool":
					s.Run.Status = StatusWaitingTool
				case "worker":
					s.Workers = []AgentWorker{{ID: "worker", Status: "running"}}
				case "claimed_task":
					s.Plan.Tasks = []PlanTask{{ID: "task", Goal: "work", Started: true, Status: "pending"}}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err = a.SetRootContract(root.Run.ID, RootContract{Version: 2}); err == nil {
				t.Fatal("execution reopened intent")
			}
		})
	}
}

func TestExplicitRootContractCannotForgeAutomaticIntent(t *testing.T) {
	a, _ := newTestAgent(t, MockProvider{})
	root, err := a.CreateConfigured("goal", RunConfiguration{RootContract: &RootContract{Version: 2, Goal: "goal", AutoIntent: true}})
	if err != nil {
		t.Fatal(err)
	}
	if root.RootContract.AutoIntent {
		t.Fatal("caller controlled automatic intent marker")
	}
	if _, err = a.SetRootContract(root.Run.ID, RootContract{Version: 2, Goal: "goal"}); err == nil {
		t.Fatal("explicit empty root contract became mutable")
	}
}

func TestRootCleanupBlocksCompletionAfterManualOutcome(t *testing.T) {
	for _, missing := range []bool{false, true} {
		s := stageScopeSnapshot(t, nil)
		s.Run.SuccessContains = "true"
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: "Finished after observed output."})
		pending := Action{Adapter: "exec", RequestID: "other-request", CallID: "other-call", Status: "resolved_manually", Result: &ToolResult{WorkerCleanupError: "owned process still live"}}
		if missing {
			pending.Result = nil
		}
		s.Actions = append(s.Actions, pending)
		refreshResult(&s)
		if s.Result.Status == "success" || !strings.Contains(s.Result.TerminalReason, "cleanup") {
			t.Fatalf("manual outcome bypassed cleanup: %+v", s.Result)
		}
		if !continueForEvidence(&s) || s.Run.Status == StatusCompleted {
			t.Fatal("cleanup did not defer completion")
		}
		s.Actions[len(s.Actions)-1].ResourceCleanupStatus = "completed"
		s.Run.Status = StatusCompleted
		refreshResult(&s)
		if s.Result.Status != "success" {
			t.Fatalf("verified cleanup did not release completion: %+v", s.Result)
		}
	}
}
