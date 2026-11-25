package agent

import (
	"context"
	"encoding/json"
	"testing"
)

func TestLateModelResponsePreservesPauseAndDynamicPlan(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	request := ModelRequest{ID: "old-request"}
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusPaused
		s.Run.PauseReason = "operator"
		s.Requests = []ModelRequest{request}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	retries := 0
	response := ModelResponse{Content: "finished", StopReason: "stop"}
	s, _, err := a.settlePiResponse(context.Background(), id, request, response, nil, &retries)
	if err != nil {
		t.Fatal(err)
	}
	if s.Run.Status != StatusPaused || len(s.Responses) != 1 || len(s.Messages) != 0 {
		t.Fatal("late model completion revived pause")
	}
	if _, err = a.finishPiTurn(context.Background(), id, request, response); err != nil {
		t.Fatal(err)
	}
	s, err = store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Run.Status != StatusPaused {
		t.Fatal("turn settlement revived pause")
	}
	if _, err = store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPlan(id, []PlanTask{{ID: "new", Goal: "newly required work"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.finishPiTurn(context.Background(), id, request, response); err != nil {
		t.Fatal(err)
	}
	s, err = store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Run.Status == StatusCompleted || s.Result.Status == "success" || !hasString(s.Plan.Frontier, "new") {
		t.Fatal("old final response discarded new plan")
	}
}

func TestLegacyRetryPinsActualChildModelAndRejectsConflict(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		a, store, id, w := settlementFixture(t)
		actual := ModelSelection{Provider: "default", Model: "original", ProviderID: "endpoint-identity"}
		if err := settlementOperation(t, a, store, id, w, func(s *Snapshot) { s.Run.Status = StatusFailed; s.Run.ModelSelection = &actual }); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Update(id, func(s *Snapshot) error {
			s.Plan.Tasks[0].ModelBinding = nil
			if conflict {
				s.Plan.Tasks[0].ModelBinding = &ModelSelection{Model: "other"}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		s, err := a.RetryTask(id, w.TaskID)
		if conflict {
			if err == nil {
				t.Fatal("conflicting legacy child route accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if s.Plan.Tasks[0].ModelBinding == nil || *s.Plan.Tasks[0].ModelBinding != actual {
			t.Fatal("legacy retry lost actual child route")
		}
	}
}

func TestRootContractRejectsWeakenedRequirementsAndClonesHashes(t *testing.T) {
	a, _ := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	requirement := taskRequirement("root-proof")
	if _, err := a.SetVerificationRequirements(id, []VerificationRequirement{requirement}); err != nil {
		t.Fatal(err)
	}
	supplied := RootContract{Version: 1, RequirementIDs: []string{requirement.ID}, RequirementHashes: map[string]string{requirement.ID: "forged"}}
	s, err := a.SetRootContract(id, supplied)
	if err != nil {
		t.Fatal(err)
	}
	if s.RootContract.RequirementHashes[requirement.ID] == "forged" {
		t.Fatal("caller supplied pinned hash")
	}
	weakened := requirement
	weakened.Predicate.Expected = false
	if _, err = a.SetVerificationRequirements(id, []VerificationRequirement{weakened}); err == nil {
		t.Fatal("root acceptance weakened")
	}
	if _, err = a.SetVerificationRequirements(id, []VerificationRequirement{requirement}); err != nil {
		t.Fatal(err)
	}
}

func stageScopeSnapshot(t *testing.T, scope *EvidenceScope) Snapshot {
	t.Helper()
	result := ToolResult{Stdout: `{"ok":true}`, ExitCode: 0}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "call", Name: "exec", Arguments: "{}"}
	s := Snapshot{WorkerContext: &WorkerContext{EvidenceScope: scope}, Requests: []ModelRequest{{ID: "request"}}, Responses: []ResponseRecord{{RequestID: "request", Response: ModelResponse{ToolCalls: []ToolCall{call}}}}, Actions: []Action{{RequestID: "request", CallID: call.ID, Name: call.Name, Arguments: call.Arguments, Adapter: "exec", Status: "completed", Result: &result}}}
	appendToolResult(&s, call.ID, string(raw), "executed")
	s.Run.Status = StatusCompleted
	return s
}

func TestStageScopeAppliesToVerificationResultAndContext(t *testing.T) {
	s := stageScopeSnapshot(t, &EvidenceScope{ObjectID: "document", Version: "1"})
	id := s.Evidence[0].ID
	r := taskRequirement("proof")
	s.Requirements = []VerificationRequirement{r}
	refs, err := evidenceReferences(&s, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	s.Checks = []VerificationCheck{{RequirementID: r.ID, RequirementHash: valueHash(r), References: refs, EvidenceIDs: []string{id}}}
	refreshResult(&s)
	if s.Result.Status != "success" {
		t.Fatalf("same declared scope: %+v", s.Result)
	}
	ref := WorkerContextEvidence{ID: id}
	contextEvidence(&s, &ref)
	projectContextEvidenceScope(&ref, &EvidenceScope{ObjectID: "document", Version: "2"})
	if ref.Status != "source_verified" || ref.Applicability != "different_contract_scope" {
		t.Fatalf("historical context: %+v", ref)
	}
	s.WorkerContext.EvidenceScope = &EvidenceScope{ObjectID: "document", Version: "2"}
	refreshResult(&s)
	if s.Result.Status == "success" || s.Checks[0].Status == "pass" {
		t.Fatal("old evidence satisfied new stage")
	}
	if _, err = evidenceReferences(&s, []string{id}); err == nil {
		t.Fatal("stale evidence reference accepted")
	}
	s.WorkerContext.EvidenceScope = &EvidenceScope{ObjectID: "document", Version: "1"}
	refreshResult(&s)
	if s.Result.Status != "success" {
		t.Fatal("same-version later visit lost verified fact")
	}
	projectContextEvidenceScope(&ref, s.WorkerContext.EvidenceScope)
	if ref.Applicability != "same_declared_scope_not_reobserved" {
		t.Fatal("scope implied fresh observation")
	}
	s.Evidence[0].Scope.Version = "2"
	if verifyEvidence(&s, &s.Evidence[0]) != "invalid" {
		t.Fatal("scope relabel accepted without original event identity")
	}
}

func TestLegacyEvidenceScopeStampCompatibility(t *testing.T) {
	s := stageScopeSnapshot(t, nil)
	for i := range s.Events {
		if s.Events[i].Type == "tool_result" {
			delete(s.Events[i].Data.(map[string]string), "evidence_scope_hash")
		}
	}
	if _, ok := verifiedOutput(&s, s.Evidence[0].ID); !ok {
		t.Fatal("legacy unscoped evidence rejected")
	}
	s.Evidence[0].Scope = &EvidenceScope{ObjectID: "document", Version: "1"}
	if verifyEvidence(&s, &s.Evidence[0]) != "invalid" {
		t.Fatal("legacy evidence relabeled as scoped")
	}
}

func TestReplacementBusinessInputsAndPinnedHandoff(t *testing.T) {
	old := PlanTask{ID: "old", Goal: "deliver report", AssetIDs: []string{"source"}, Dependencies: []string{"input"}, ResourceKeys: []string{"workspace"}, RequiredToolIDs: []string{"read"}, RequiredAssetIDs: []string{"source"}, ExpectedArtifacts: []string{"report"}, InputAttachmentIDs: []string{"attachment"}, HandoffTaskIDs: []string{"previous"}, Requirements: []VerificationRequirement{taskRequirement("proof")}}
	next := old
	next.ID = "next"
	next.ModelSelection = &ModelSelection{Model: "different"}
	next.HandoffTaskIDs = []string{"previous", "old"}
	next.EstimatedSeconds = 123
	if !taskContractCovers(next, old) {
		t.Fatal("valid new route and prior-stage handoff rejected")
	}
	for _, field := range []string{"asset", "dependency", "resource", "tool", "required_asset", "artifact", "attachment", "handoff"} {
		changed := next
		switch field {
		case "asset":
			changed.AssetIDs = nil
		case "dependency":
			changed.Dependencies = nil
		case "resource":
			changed.ResourceKeys = nil
		case "tool":
			changed.RequiredToolIDs = nil
		case "required_asset":
			changed.RequiredAssetIDs = nil
		case "artifact":
			changed.ExpectedArtifacts = nil
		case "attachment":
			changed.InputAttachmentIDs = nil
		case "handoff":
			changed.HandoffTaskIDs = []string{"old", "unrelated"}
		}
		if taskContractCovers(changed, old) {
			t.Fatalf("weakened %s accepted", field)
		}
	}
}

func TestSupersededStageHandoffPlanRefreshAndDependencyRewire(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	old := PlanTask{ID: "old", Goal: "deliver"}
	next := PlanTask{ID: "next", Goal: "deliver", HandoffTaskIDs: []string{"old"}}
	dependent := PlanTask{ID: "dependent", Goal: "consume", Dependencies: []string{"old"}}
	if _, err := a.SetPlan(id, []PlanTask{old, next, dependent}); err != nil {
		t.Fatal(err)
	}
	s, err := a.DisposeTask(id, "old", "superseded", "new execution route", "next")
	if err != nil {
		t.Fatal(err)
	}
	if hasString(s.Plan.Frontier, "dependent") {
		t.Fatal("superseded unlocked old dependency")
	}
	if _, err = a.SetPlan(id, s.Plan.Tasks); err != nil {
		t.Fatalf("historical handoff forms no execution cycle: %v", err)
	}
	s.Plan.Tasks[2].Dependencies = []string{"next"}
	if _, err = a.SetPlan(id, s.Plan.Tasks); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[1].Status = "completed"; return nil }); err != nil {
		t.Fatal(err)
	}
	s, err = store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !hasString(s.Plan.Frontier, "dependent") || !planTaskSatisfied(&s.Plan, s.Plan.Tasks[0], map[string]bool{}) {
		t.Fatal("replacement did not satisfy aggregate/rewired dependency")
	}
}

func TestWorkerSettlementScopeMismatchPreservesCleanupReceipt(t *testing.T) {
	a, store, id, w := settlementFixture(t)
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Plan.Tasks[0].EvidenceScope = &EvidenceScope{ObjectID: "document", Version: "2"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := settlementOperation(t, a, store, id, w, func(s *Snapshot) {
		s.WorkerContext = &WorkerContext{EvidenceScope: &EvidenceScope{ObjectID: "document", Version: "1"}}
		s.Run.Status = StatusCompleted
	}); err != nil {
		t.Fatal(err)
	}
	s, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Plan.Tasks[0].Status != "failed" || s.Workers[0].CleanupStatus != "completed" || hasString(s.Plan.Frontier, "next") {
		t.Fatalf("scope mismatch lost independent cleanup or passed: %+v", s.Plan)
	}
}
