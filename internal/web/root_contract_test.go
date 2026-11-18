package web

import (
	"encoding/json"
	"testing"
	"trace/internal/agent"
)

func TestRootContractAPIFiniteAssetsAndImmutability(t *testing.T) {
	s, a := fixture(t, nil)
	h := s.Handler()
	requirement := agent.VerificationRequirement{ID: "report", Description: "report exact content", Category: "goal", Predicate: agent.EvidencePredicate{Kind: "equals", Value: "complete"}}
	created := request(t, h, "POST", "/api/runs", map[string]any{"goal": "finite deliverables", "requirements": []agent.VerificationRequirement{requirement}}, 201)
	var root agent.Snapshot
	if err := json.Unmarshal(created.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	base := "/api/runs/" + root.Run.ID
	contract := agent.RootContract{Version: 1, RequirementIDs: []string{"report"}, AssetIDs: []string{"document"}, RequirementHashes: map[string]string{"report": "forged"}, AssetHashes: map[string]string{"document": "forged"}}
	request(t, h, "POST", base+"/root-contract", contract, 409)
	request(t, h, "POST", base+"/assets", agent.Asset{ID: "document", Kind: "file", Value: "report.txt"}, 200)
	response := request(t, h, "POST", base+"/root-contract", contract, 200)
	var pinned agent.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &pinned); err != nil {
		t.Fatal(err)
	}
	if pinned.RootContract == nil || len(pinned.RootContract.AssetHashes["document"]) != 64 || pinned.RootContract.RequirementHashes["report"] == "forged" {
		t.Fatal("finite root contract not server-pinned")
	}
	request(t, h, "POST", base+"/root-contract", contract, 409)
	request(t, h, "PUT", base+"/requirements", map[string]any{"requirements": []agent.VerificationRequirement{}}, 409)
	weakened := requirement
	weakened.Predicate.Value = "easier"
	request(t, h, "PUT", base+"/requirements", map[string]any{"requirements": []agent.VerificationRequirement{weakened}}, 409)
	request(t, h, "PUT", base+"/plan", map[string]any{"tasks": []agent.PlanTask{{ID: "stage", Goal: "investigate", Stage: true}}}, 200)
	saved, err := a.Store().Load(root.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RootContract == nil || saved.RootContract.AssetHashes["document"] != pinned.RootContract.AssetHashes["document"] {
		t.Fatal("pinned contract not durable")
	}
}

func TestRootContractAPIRejectsRunningChildAndInvalidInputs(t *testing.T) {
	for _, scenario := range []string{"running", "executed", "child", "active", "configuring", "closed", "invalid"} {
		t.Run(scenario, func(t *testing.T) {
			s, a := fixture(t, nil)
			h := s.Handler()
			requirement := agent.VerificationRequirement{ID: "proof", Description: "proof", Category: "goal", Predicate: agent.EvidencePredicate{Kind: "equals", Value: "ok"}}
			root, err := a.CreateConfigured("root", agent.RunConfiguration{Requirements: []agent.VerificationRequirement{requirement}})
			if err != nil {
				t.Fatal(err)
			}
			contract := agent.RootContract{Version: 1, RequirementIDs: []string{"proof"}}
			if scenario == "invalid" {
				contract.RequirementIDs = []string{"proof", "proof"}
			}
			if _, err = a.Store().Update(root.Run.ID, func(snapshot *agent.Snapshot) error {
				switch scenario {
				case "running":
					snapshot.Run.Status = agent.StatusRunning
				case "executed":
					snapshot.Requests = []agent.ModelRequest{{ID: "past"}}
				case "child":
					snapshot.Run.ParentRunID = "parent"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			switch scenario {
			case "active":
				s.active[root.Run.ID] = true
			case "configuring":
				s.configuring = true
			case "closed":
				s.closed = true
			}
			s.mu.Unlock()
			status := 409
			if scenario == "closed" {
				status = 503
			}
			request(t, h, "POST", "/api/runs/"+root.Run.ID+"/root-contract", contract, status)
			s.mu.Lock()
			delete(s.active, root.Run.ID)
			s.configuring = false
			s.closed = false
			s.mu.Unlock()
			saved, err := a.Store().Load(root.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			original, _ := json.Marshal(root.RootContract)
			current, _ := json.Marshal(saved.RootContract)
			if string(current) != string(original) {
				t.Fatal("rejected request changed root contract")
			}
		})
	}
}
