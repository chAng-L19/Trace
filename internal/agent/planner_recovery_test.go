package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"trace/internal/localtool"
)

type retryProvider struct {
	calls       int
	afterAction bool
}

func (p *retryProvider) Complete(ctx context.Context, request ModelRequest) (ModelResponse, error) {
	p.calls++
	if (!p.afterAction && p.calls == 1) || (p.afterAction && lastConversationMessage(request).Role == "tool") {
		return ModelResponse{}, errors.New("transient provider failure")
	}
	return (resultProvider{}).Complete(ctx, request)
}

func TestPlannerRetryFailedZeroActionAttempt(t *testing.T) {
	a, store := newTestAgent(t, &retryProvider{})
	id := createTestRun(t, a)
	a.SetPlan(id, []PlanTask{{ID: "retry", Goal: "produce proof", SuccessContains: "flag{verified}"}})
	old, err := a.DispatchTask(context.Background(), id, "retry")
	if err != nil {
		t.Fatal(err)
	}
	failed, err := waitAllTestWorkers(a, context.Background(), id, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer a.CloseMCP()
	if failed.Plan.Tasks[0].Status != "failed" {
		t.Fatalf("failure fixture %+v", failed.Plan)
	}
	child, _ := store.Load(old.RunID)
	if len(child.Actions) != 0 {
		t.Fatal("fixture executed an action")
	}
	release, err := store.AcquireRunLock(old.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.RetryTask(id, "retry"); err == nil {
		t.Fatal("retry raced an active child resume")
	}
	release()
	value, err := a.builtin(context.Background(), id, ToolCall{Name: "task_retry", Arguments: `{"task_id":"retry"}`})
	if err != nil {
		t.Fatal(err)
	}
	view, ok := value.(map[string]any)
	if !ok || view["tasks"].([]PlanTask)[0].Status != "pending" || !hasString(view["frontier"].([]string), "retry") {
		t.Fatal("retry did not restore frontier")
	}
	if _, err = a.RetryTask(id, "retry"); err == nil {
		t.Fatal("retry applied twice")
	}
	child, _ = store.Load(old.RunID)
	if child.Run.Status != StatusCancelled {
		t.Fatal("old child not retired")
	}
	if _, err = a.Run(context.Background(), old.RunID); err == nil {
		t.Fatal("old child could still execute")
	}
	next, err := a.DispatchTask(context.Background(), id, "retry")
	if err != nil {
		t.Fatal(err)
	}
	done, err := waitAllTestWorkers(a, context.Background(), id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if next.RunID == old.RunID || len(done.Workers) != 2 || done.Workers[0].Status != "superseded" || done.Plan.Tasks[0].Status != "verified" {
		t.Fatalf("new attempt not verified: %+v %+v", done.Plan, done.Workers)
	}
	child, _ = store.Load(next.RunID)
	if len(child.Actions) != 1 || child.Actions[0].Result.WorkerPID == 0 {
		t.Fatal("retried child did not execute exactly one real action")
	}
}

func TestPlannerRetryRejectsExecutedOrUnsettledAttempt(t *testing.T) {
	for _, scenario := range []string{"executed", "cleanup_unknown", "pending_intent"} {
		t.Run(scenario, func(t *testing.T) {
			a, store := newTestAgent(t, &retryProvider{afterAction: scenario == "executed"})
			id := createTestRun(t, a)
			a.SetPlan(id, []PlanTask{{ID: "retry", Goal: "proof", SuccessContains: "flag{verified}"}})
			w, err := a.DispatchTask(context.Background(), id, "retry")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := waitAllTestWorkers(a, context.Background(), id, 10); err != nil {
				t.Fatal(err)
			}
			defer a.CloseMCP()
			if scenario == "cleanup_unknown" {
				store.Update(id, func(s *Snapshot) error { s.Workers[0].CleanupStatus = "unknown"; return nil })
			}
			if scenario == "pending_intent" {
				store.Update(w.RunID, func(s *Snapshot) error {
					s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "intent", Name: "exec", Arguments: `{"command":"echo effect"}`}}})
					return nil
				})
			}
			if _, err = a.RetryTask(id, "retry"); err == nil {
				t.Fatal("unsettled/effected task replay allowed")
			}
			parent, _ := store.Load(id)
			child, _ := store.Load(w.RunID)
			if len(parent.Workers) != 1 || parent.Plan.Tasks[0].Status != "failed" || child.Run.Status != StatusFailed {
				t.Fatal("rejected retry mutated attempt")
			}
			if scenario == "executed" && (len(child.Actions) != 1 || child.Actions[0].Result.WorkerPID == 0) {
				t.Fatal("missing real executed action")
			}
		})
	}
}

func TestPlannerRevokesInvalidChildEvidence(t *testing.T) {
	for _, mutation := range []string{"action", "missing_child", "missing_artifact"} {
		t.Run(mutation, func(t *testing.T) {
			a, store := newTestAgent(t, resultProvider{})
			predicate := "flag{verified}"
			if mutation == "missing_artifact" {
				binary, _ := os.Executable()
				manifest := localtool.Manifest{Name: "large-recovery", Command: binary, Args: []string{"-test.run=^TestPlannerLargeProcessFixture$"}, Env: map[string]string{"TRACE_LARGE_FIXTURE": "1"}}
				if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
					t.Fatal(err)
				}
				a.provider = manualProvider(ToolCall{ID: "large", Name: manifest.StableID(), Arguments: `{}`})
				predicate = "proof-tail"
			}
			id := createTestRun(t, a)
			_, err := a.SetPlan(id, []PlanTask{{ID: "source", Goal: "proof", SuccessContains: predicate}, {ID: "next", Goal: "dependent", Dependencies: []string{"source"}}})
			if err != nil {
				t.Fatal(err)
			}
			w, err := a.DispatchTask(context.Background(), id, "source")
			if err != nil {
				t.Fatal(err)
			}
			before, err := waitAllTestWorkers(a, context.Background(), id, 10)
			if err != nil {
				t.Fatal(err)
			}
			defer a.CloseMCP()
			if before.Plan.Tasks[0].Status != "verified" {
				t.Fatalf("baseline %+v", before.Plan)
			}
			child, err := store.Load(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			for _, coverage := range []string{"observed", "verified", "negative"} {
				_, err = a.UpsertAsset(id, Asset{ID: coverage, Kind: "file", Value: coverage, Coverage: coverage, Predicate: predicate, SourceRunID: w.RunID, EvidenceIDs: child.Result.EvidenceIDs})
				if err != nil {
					t.Fatal(err)
				}
			}
			switch mutation {
			case "action":
				_, err = store.Update(w.RunID, func(s *Snapshot) error { s.Actions[0].Result.ExitCode = 7; return nil })
			case "missing_child":
				err = os.Remove(filepath.Join(store.root, w.RunID+".json"))
			case "missing_artifact":
				err = os.Remove(filepath.Join(store.root, "artifacts", child.Actions[0].Result.Artifacts[0].Hash))
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			coverage := Coverage(after)
			if after.Plan.Tasks[0].Status != "uncertain" || coverage.Tested != 0 || coverage.ByStatus["uncertain"] != 3 || len(after.Plan.Frontier) != 0 {
				t.Fatalf("stale derived proof: plan=%+v coverage=%+v", after.Plan, coverage)
			}
			if _, err = a.ClaimTask(id, "next", "test", 30); err == nil {
				t.Fatal("invalid dependency accepted")
			}
			if _, err = a.DispatchTask(context.Background(), id, "next"); err == nil {
				t.Fatal("invalid dependency dispatched")
			}
			reloaded, _ := NewStore(store.root)
			again, err := reloaded.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if again.Plan.Tasks[0].Status != "uncertain" || again.Run.EventSeq != after.Run.EventSeq {
				t.Fatal("invalidation not durable/idempotent")
			}
		})
	}
}

func TestPlannerRevocationCascadesAndPersistsOnRejectedClaim(t *testing.T) {
	a, store := newTestAgent(t, resultProvider{})
	id := createTestRun(t, a)
	_, err := a.SetPlan(id, []PlanTask{{ID: "leaf", Goal: "leaf", Dependencies: []string{"middle"}}, {ID: "middle", Goal: "middle", Dependencies: []string{"source"}}, {ID: "source", Goal: "source", SuccessContains: "flag{verified}"}})
	if err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), id, "source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitAllTestWorkers(a, context.Background(), id, 10); err != nil {
		t.Fatal(err)
	}
	defer a.CloseMCP()
	if _, err = a.ClaimTask(id, "middle", "manual", 30); err != nil {
		t.Fatal(err)
	}
	if _, err = a.CompleteTask(id, "middle", "manual", "completed", nil); err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(w.RunID, func(s *Snapshot) error { s.Evidence[0].ContentHash = "changed"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	// No explicit parent read precedes claim: invalidation must commit even
	// though the requested claim returns an error.
	if _, err = a.ClaimTask(id, "leaf", "manual", 30); err == nil {
		t.Fatal("claim trusted invalid ancestor")
	}
	parent, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Plan.Tasks[1].Status != "uncertain" || parent.Plan.Tasks[2].Status != "uncertain" || len(parent.Plan.Frontier) != 0 {
		t.Fatalf("revocation failed to cascade: %+v", parent.Plan)
	}
	raw, err := store.loadRawLocked(id)
	if err != nil {
		t.Fatal(err)
	}
	if raw.Plan.Tasks[1].Status != "uncertain" {
		t.Fatal("failed claim rolled back necessary invalidation")
	}
}
