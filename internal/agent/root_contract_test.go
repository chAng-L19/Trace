package agent

import "testing"

func TestRootContractPinsAcceptanceAndFiniteScope(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	r := taskRequirement("deliverable")
	if _, err := a.SetVerificationRequirements(id, []VerificationRequirement{r}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.UpsertAsset(id, Asset{ID: "document", Kind: "file", Value: "report.md"}); err != nil {
		t.Fatal(err)
	}
	c := RootContract{Version: 1, RequirementIDs: []string{r.ID}, AssetIDs: []string{"document"}}
	s, err := a.SetRootContract(id, c)
	if err != nil {
		t.Fatal(err)
	}
	if s.RootContract.RequirementHashes[r.ID] != valueHash(r) {
		t.Fatal("requirement not pinned")
	}
	if _, err = a.SetRootContract(id, c); err == nil {
		t.Fatal("root contract replaced")
	}
	if _, err = a.SetVerificationRequirements(id, nil); err == nil {
		t.Fatal("root acceptance removed")
	}
	s, err = store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	s.Checks = []VerificationCheck{{RequirementID: r.ID, Status: "pass"}}
	if gaps := rootContractGaps(&s); len(gaps) != 0 {
		t.Fatal(gaps)
	}
	s.Assets[0].Scope = "excluded"
	if len(rootContractGaps(&s)) == 0 {
		t.Fatal("excluded asset bypassed root scope")
	}
	s.Assets[0].Scope = "included"
	s.Requirements[0].Predicate.Value = "weaker"
	if len(rootContractGaps(&s)) == 0 {
		t.Fatal("changed requirement bypassed root contract")
	}
}

func TestExplicitStagesDoNotBecomeSuccessfulDependencies(t *testing.T) {
	task := PlanTask{ID: "investigate", Goal: "investigate", Stage: true, Status: "abandoned"}
	s := Snapshot{Plan: PlanState{Tasks: []PlanTask{task}}}
	if planDispositionFailure(&s) == "" {
		t.Fatal("legacy failure semantics changed")
	}
	r := taskRequirement("report")
	s.Requirements = []VerificationRequirement{r}
	c := pinRootContract(&s, RootContract{Version: 1, RequirementIDs: []string{r.ID}})
	s.RootContract = &c
	if planDispositionFailure(&s) != "" || !taskSatisfiedForRoot(&s, task) {
		t.Fatal("settled stage stopped root")
	}
	if taskDone(task) {
		t.Fatal("abandoned stage satisfied dependency")
	}
	if len(rootContractGaps(&s)) == 0 {
		t.Fatal("stage disposal satisfied root acceptance")
	}
	s.Workers = []AgentWorker{{TaskID: task.ID, Status: "paused", CleanupStatus: "pending"}}
	if settledStage(&s, task) {
		t.Fatal("stage with pending cleanup retired")
	}
}

func TestEvidenceScopeAllowsSameVersionAcrossVisits(t *testing.T) {
	scope := &EvidenceScope{ObjectID: "source/report", Version: "sha256:one"}
	s := Snapshot{WorkerContext: &WorkerContext{EvidenceScope: scope}}
	ev := Evidence{Scope: executionEvidenceScope(&s)}
	if !evidenceApplicable(&s, ev) {
		t.Fatal("same contract rejected")
	}
	scope.Version = "sha256:two"
	if evidenceApplicable(&s, ev) {
		t.Fatal("old version accepted")
	}
	scope.Version = "sha256:one"
	scope.ObjectID = "other"
	if evidenceApplicable(&s, ev) {
		t.Fatal("other object accepted")
	}
	if !evidenceApplicable(&Snapshot{}, Evidence{}) {
		t.Fatal("legacy evidence changed")
	}
	if evidenceApplicable(&s, Evidence{}) {
		t.Fatal("unscoped evidence accepted for scoped task")
	}
}

func TestRootStagesRequireOptInAndFreezeIdentity(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	// Legacy runs remain opt-in; newly created roots already freeze v2 intent.
	if _, err := store.Update(id, func(s *Snapshot) error { s.RootContract = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	task := PlanTask{ID: "stage", Goal: "read report", Stage: true, EvidenceScope: &EvidenceScope{ObjectID: "report", Version: "1"}}
	if _, err := a.SetPlan(id, []PlanTask{task}); err == nil {
		t.Fatal("stage without root contract")
	}
	r := taskRequirement("report")
	if _, err := a.SetVerificationRequirements(id, []VerificationRequirement{r}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetRootContract(id, RootContract{Version: 1, RequirementIDs: []string{r.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlan(id, []PlanTask{task}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(id, task.ID, "manual", 30); err != nil {
		t.Fatal(err)
	}
	task.EvidenceScope.Version = "2"
	if _, err := a.SetPlan(id, []PlanTask{task}); err == nil {
		t.Fatal("started scope changed")
	}
}

func TestReplacementPreservesBusinessNotModelSelection(t *testing.T) {
	original := PlanTask{ID: "a", Goal: "produce report", Stage: true, Requirements: []VerificationRequirement{taskRequirement("proof")}, EvidenceScope: &EvidenceScope{ObjectID: "report", Version: "1"}}
	replacement := original
	replacement.ID = "b"
	replacement.ModelSelection = &ModelSelection{}
	if !taskContractCovers(replacement, original) {
		t.Fatal("model choice incorrectly part of business coverage")
	}
	replacement.Requirements = nil
	if taskContractCovers(replacement, original) {
		t.Fatal("weakened acceptance accepted")
	}
	replacement = original
	original.RequiredImageInput = true
	if taskContractCovers(replacement, original) {
		t.Fatal("image capability weakened")
	}
}
