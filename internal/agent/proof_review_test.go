package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func reviewProofFixture(t *testing.T) (*Agent, *Store, string, Snapshot) {
	t.Helper()
	a, store := newTestAgent(t, manualProvider(ToolCall{ID: "origin", Name: "external", Arguments: `{}`}))
	id := createTestRun(t, a)
	waitTestRun(t, a, id)
	s, err := a.executeCommand(context.Background(), id, "origin", []string{"sh", "-c", "printf 'observed-marker'"})
	if err != nil {
		t.Fatal(err)
	}
	return a, store, id, s
}

func reviewProofFinding(s Snapshot) Finding {
	return Finding{ID: "review-fixture", Title: "observed-marker", Severity: "low", RootCause: "fixture root cause", Impact: "fixture impact", Remediation: "fixture remediation", ReproductionSteps: []string{"Run the original probe again"}, EvidenceIDs: []string{s.Evidence[0].ID}, Predicate: EvidencePredicate{Kind: "contains", Value: "observed-marker"}}
}

func TestProofReviewRejectsSelfReferentialRetest(t *testing.T) {
	a, _, id, s := reviewProofFixture(t)
	if _, err := a.SubmitFinding(id, reviewProofFinding(s)); err != nil {
		t.Fatal(err)
	}
	// A real builtin action echoes finding metadata, but performs no retest.
	// Archive the exact batch, then execute only its metadata observation so
	// the assertion isolates retest provenance from later model continuations.
	a.provider = manualProvider(ToolCall{ID: "status", Name: "verification_status", Arguments: `{}`}, ToolCall{ID: "hold", Name: "external", Arguments: `{}`})
	waitTestRun(t, a, id)
	if _, err := a.executePendingTool(context.Background(), id, ToolCall{ID: "status", Name: "verification_status", Arguments: `{}`}); err != nil {
		t.Fatal(err)
	}
	s, err := a.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var echoID string
	for _, e := range s.Evidence {
		if e.CallID == "status" {
			echoID = e.ID
		}
	}
	if echoID == "" {
		t.Fatal("metadata echo execution missing")
	}
	_, err = a.RetestFinding(id, "review-fixture", []string{echoID})
	if err == nil {
		t.Fatal("finding metadata echo was accepted as independent execution retest")
	}
}

func TestProofReviewRejectsEditedReviewOutputs(t *testing.T) {
	a, store, id, s := reviewProofFixture(t)
	if _, err := a.SubmitFinding(id, reviewProofFinding(s)); err != nil {
		t.Fatal(err)
	}
	a.provider = &scriptedProvider{responses: []ModelResponse{{Content: `{"status":"pass","reason":"fixture reviewed exact original output"}`, StopReason: "stop"}}}
	s, err := a.ReviewFinding(context.Background(), id, "review-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if s.Findings[0].Status != "reviewed" {
		t.Fatalf("baseline review not accepted: %+v", s.Findings[0])
	}
	s, err = store.Update(id, func(s *Snapshot) error {
		for i := range s.Requests {
			if s.Requests[i].Purpose != "evidence_review" {
				continue
			}
			var input map[string]any
			if err := json.Unmarshal([]byte(s.Requests[i].Messages[1].Content), &input); err != nil {
				return err
			}
			input["outputs"] = []string{"invented different observation"}
			raw, _ := json.Marshal(input)
			s.Requests[i].Messages[1].Content = string(raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Findings[0].Status == "reviewed" || s.Findings[0].Status == "reproduced" {
		t.Fatal("changed reviewer evidence payload did not revoke assessment")
	}
}

func TestProofReviewCleanupInvalidatedByLaterExecution(t *testing.T) {
	a, store := newTestAgent(t, manualProvider(ToolCall{ID: "probe", Name: "external", Arguments: `{"probe":"clean"}`}, ToolCall{ID: "mutate", Name: "external", Arguments: `{"operation":"dirty"}`}))
	id := createTestRun(t, a)
	r := VerificationRequirement{ID: "cleanup", Description: "environment is clean after all work", Category: "cleanup", ToolID: "external", ArgumentsHash: promptHash(`{"probe":"clean"}`), Predicate: EvidencePredicate{Kind: "equals", Value: "clean"}}
	if _, err := a.SetVerificationRequirements(id, []VerificationRequirement{r}); err != nil {
		t.Fatal(err)
	}
	waitTestRun(t, a, id)
	s, err := a.executeCommand(context.Background(), id, "probe", []string{"sh", "-c", "printf clean"})
	if err != nil {
		t.Fatal(err)
	}
	s, err = a.CheckEvidence(context.Background(), id, "cleanup", []string{s.Evidence[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if s.Result.CleanupStatus != "completed" {
		t.Fatal("baseline cleanup probe did not pass")
	}
	if _, err = a.executeCommand(context.Background(), id, "mutate", []string{"sh", "-c", "printf dirty"}); err != nil {
		t.Fatal(err)
	}
	s, err = store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Result.CleanupStatus == "completed" || latestCheck(&s, "cleanup").Status == "pass" {
		t.Fatal("old cleanup proof survived a later external action")
	}
}

func TestProofReviewBranchPreservesRequirements(t *testing.T) {
	a, _ := newTestAgent(t, manualProvider(ToolCall{ID: "hold", Name: "external", Arguments: `{}`}))
	id := createTestRun(t, a)
	r := VerificationRequirement{ID: "must-prove", Description: "fixed operator goal", Category: "goal", Predicate: EvidencePredicate{Kind: "equals", Value: "completed-proof"}}
	if _, err := a.SetVerificationRequirements(id, []VerificationRequirement{r}); err != nil {
		t.Fatal(err)
	}
	waitTestRun(t, a, id)
	s, err := a.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := a.Branch(id, s.Messages[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(branch.Requirements) != 1 || valueHash(branch.Requirements[0]) != valueHash(r) {
		t.Fatal("branch silently dropped fixed acceptance requirements for the same goal")
	}
}

func TestProofReviewRetestMustStartAfterFindingRevision(t *testing.T) {
	a, _, id, s := reviewProofFixture(t)
	f := reviewProofFinding(s)
	a.provider = manualProvider(ToolCall{ID: "already-started", Name: "external", Arguments: `{}`})
	waitTestRun(t, a, id)
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := a.executeAction(context.Background(), id, "already-started", "exec", []string{"sh", "-c", "printf 'observed-marker'"}, func() ToolResult { close(entered); <-release; return ToolResult{Stdout: "observed-marker"} })
		done <- err
	}()
	<-entered
	_, submitErr := a.SubmitFinding(id, f)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if submitErr != nil {
		t.Fatal(submitErr)
	}
	s, err := a.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var evidenceID string
	for _, e := range s.Evidence {
		if e.CallID == "already-started" {
			evidenceID = e.ID
		}
	}
	if _, err = a.RetestFinding(id, f.ID, []string{evidenceID}); err == nil {
		t.Fatal("execution started before finding revision was accepted as fresh retest")
	}
}

func TestProofReviewRespondsToPersistedPause(t *testing.T) {
	a, _, id, s := reviewProofFixture(t)
	if _, err := a.SubmitFinding(id, reviewProofFinding(s)); err != nil {
		t.Fatal(err)
	}
	p := &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a.provider = p
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := a.ReviewFinding(ctx, id, "review-fixture"); done <- err }()
	select {
	case <-p.entered:
	case <-time.After(time.Second):
		t.Fatal("review did not call provider")
	}
	if _, err := a.Pause(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		cancel()
		<-done
		t.Fatal("persisted pause did not cancel independent evidence review")
	}
	s, err := a.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Run.Status != StatusPaused || !s.Run.BudgetActiveSince.IsZero() {
		t.Fatalf("pause or settled clock lost: %+v", s.Run)
	}
}

func TestProofReviewHashCanonicalJSON(t *testing.T) {
	subject := struct {
		Z string
		A int64
	}{"value", 9007199254740993}
	decoded := map[string]any{"A": json.Number("9007199254740993"), "Z": "value"}
	if valueHash(subject) != valueHash(decoded) {
		t.Fatal("struct and decoded JSON have different hashes")
	}
	decoded["A"] = json.Number("9007199254740992")
	if valueHash(subject) == valueHash(decoded) {
		t.Fatal("large integer predicate lost precision")
	}
}

func TestProofReviewCompletedRunRestoredAndBudgetSettled(t *testing.T) {
	a, store, id, s := reviewProofFixture(t)
	if _, err := a.SubmitFinding(id, reviewProofFinding(s)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusCompleted; return nil }); err != nil {
		t.Fatal(err)
	}
	a.provider = &scriptedProvider{responses: []ModelResponse{{Content: `{"status":"pass","reason":"exact fixture reviewed"}`, StopReason: "stop"}}}
	s, err := a.ReviewFinding(context.Background(), id, "review-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if s.Run.Status != StatusCompleted || !s.Run.BudgetActiveSince.IsZero() || s.Findings[0].Status != "reviewed" {
		t.Fatalf("completed review lifecycle incorrect: %+v", s.Run)
	}
}

func TestProofReviewTimeBudgetCancelsOperatorRequest(t *testing.T) {
	a, store, id, s := reviewProofFixture(t)
	if _, err := a.SubmitFinding(id, reviewProofFinding(s)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Budget.MaxSeconds = 1; s.Run.ElapsedMillis = 950; return nil }); err != nil {
		t.Fatal(err)
	}
	a.provider = &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := a.ReviewFinding(ctx, id, "review-fixture"); err == nil {
		t.Fatal("timeboxed review unexpectedly succeeded")
	}
	s, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Run.Status != StatusPaused || s.Run.PauseReason != "time budget exhausted" || !s.Run.BudgetActiveSince.IsZero() || s.Run.ElapsedMillis < 990 {
		t.Fatalf("review bypassed cumulative time budget: %+v", s.Run)
	}
}

func TestProofReviewNestedActionKeepsOriginalRequest(t *testing.T) {
	a, store, id, s := reviewProofFixture(t)
	finding := reviewProofFinding(s)
	finding.Report = &u7Text
	if _, err := a.SubmitFinding(id, finding); err != nil {
		t.Fatal(err)
	}
	a.provider = &scriptedProvider{responses: []ModelResponse{
		{ToolCalls: []ToolCall{{ID: "review-tool", Name: "finding_review", Arguments: `{"finding_id":"review-fixture"}`}}, StopReason: "tool_calls"},
		{Content: `{"status":"pass","reason":"exact nested evidence reviewed"}`, StopReason: "stop"},
		{Content: "review done", StopReason: "stop"},
	}}
	// Bound this provenance fixture at the completed outer review turn; report
	// translation is covered separately and must not create hidden retries here.
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(len(s.Requests) + 3)})
	if _, err := a.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	s, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var action *Action
	for i := range s.Actions {
		if s.Actions[i].CallID == "review-tool" {
			action = &s.Actions[i]
		}
	}
	if action == nil {
		t.Fatal("nested review action missing")
	}
	for _, r := range s.Requests {
		if r.ID == action.RequestID && r.Purpose == "evidence_review" {
			t.Fatal("action rebound to nested reviewer request")
		}
	}
	for _, e := range s.Evidence {
		if e.CallID == "review-tool" && e.RequestID != action.RequestID {
			t.Fatal("nested review evidence detached from action")
		}
	}
	if s.Findings[0].Status != "reviewed" || !s.Run.BudgetActiveSince.IsZero() || s.Run.Status != StatusPaused || s.Run.PauseReason != "model request budget exhausted" {
		t.Fatal("nested review or outer budget did not finish")
	}
}

func TestProofReviewRequestInputContinuesWithoutDroppingRequirements(t *testing.T) {
	a, store := newTestAgent(t, &scriptedProvider{responses: []ModelResponse{
		{ToolCalls: []ToolCall{{ID: "missing", Name: "request_input", Arguments: `{"reason":"provide the target address"}`}}, StopReason: "tool_calls"},
		{Content: `{"trace_result":{"status":"incomplete","summary":"target address remains unavailable"}}`},
	}})
	two := 2
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &two})
	id := createTestRun(t, a)
	r := VerificationRequirement{ID: "proof", Description: "observed target response", Category: "goal", Predicate: EvidencePredicate{Kind: "equals", Value: "ready"}}
	if _, err := a.SetVerificationRequirements(id, []VerificationRequirement{r}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	s, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Run.Status != StatusPaused || s.Run.PauseReason != "model request budget exhausted" || len(s.Requests) != 2 || len(s.Requirements) != 1 || s.Result.Status == "success" {
		t.Fatalf("missing input stopped progress or dropped the evidence gate: %+v", s.Run)
	}
	if len(s.Actions) != 1 || s.Actions[0].Result == nil || s.Actions[0].Result.ExitCode != 0 {
		t.Fatal("missing-input observation was not archived")
	}
	if !strings.Contains(s.Actions[0].Result.Stdout, "input_unavailable") || len(unmetRequirements(&s)) != 1 {
		t.Fatal("missing input was replaced with success or fabricated evidence")
	}
}
