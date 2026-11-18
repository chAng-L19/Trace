package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

type yieldProvider struct{ childTurns int }

func (p *yieldProvider) Complete(_ context.Context, r ModelRequest) (ModelResponse, error) {
	p.childTurns++
	if p.childTurns == 1 {
		args, _ := json.Marshal(map[string]string{"command": "printf yield-proof"})
		return ModelResponse{ToolCalls: []ToolCall{{ID: "once", Name: "exec", Arguments: string(args)}}}, nil
	}
	if p.childTurns == 2 {
		var proof ToolResult
		for _, m := range r.Messages {
			if m.Role == "tool" && m.ToolCallID == "once" {
				json.Unmarshal([]byte(m.Content), &proof)
			}
		}
		checkpoint, _ := json.Marshal(WorkerHandoff{Report: &u7Text, Subject: "fixture", Instance: "fixture:80", Progress: "action completed", Findings: []string{"verified finding"}, NegativeResults: []string{"one bounded negative"}, NextAction: "continue from yield-proof without re-executing once", EvidenceIDs: []string{proof.EvidenceID}})
		args, _ := json.Marshal(map[string]string{"reason": "visit timebox reached", "checkpoint": string(checkpoint)})
		return ModelResponse{ToolCalls: []ToolCall{{ID: "yield", Name: "worker_yield", Arguments: string(args)}}}, nil
	}
	seen := false
	var proof ToolResult
	for _, m := range r.Messages {
		if strings.Contains(m.Content, "\"continuation\"") && strings.Contains(m.Content, "\"verified finding\"") && strings.Contains(m.Content, "source_verified") {
			seen = true
		}
		if m.Role == "tool" && m.ToolCallID == "once" {
			json.Unmarshal([]byte(m.Content), &proof)
		}
	}
	if !seen {
		return ModelResponse{}, fmt.Errorf("resume lost the yield checkpoint")
	}
	result := Result{Status: "success", Answers: []Answer{{Value: "yield-proof"}}}
	if proof.EvidenceID != "" {
		result.EvidenceIDs = []string{proof.EvidenceID}
		result.Answers[0].EvidenceIDs = []string{proof.EvidenceID}
	}
	body, _ := json.Marshal(map[string]any{"trace_result": result})
	return ModelResponse{Content: fixtureReport(string(body))}, nil
}

func TestWorkerYieldFreesSlotWithoutCompletingTaskAndResumesSameRun(t *testing.T) {
	p := &yieldProvider{}
	a, store := newTestAgent(t, p)
	a.SetRequireVerifiedCompletion(true)
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "visit", Goal: "inspect challenge"}, {ID: "next", Goal: "dependency", Dependencies: []string{"visit"}}}); err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), id, "visit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	parent, _ := store.Load(id)
	child, _ := store.Load(w.RunID)
	if parent.Plan.Tasks[0].Status != "paused" || hasString(parent.Plan.Frontier, "next") || occupiedWorkerSlots(&parent) != 0 || child.Run.Status != StatusPaused || !strings.HasPrefix(child.Run.PauseReason, "worker_yield:") || parent.Workers[0].CleanupStatus != "completed" {
		t.Fatalf("yield settled as terminal: plan=%+v child=%+v workers=%+v", parent.Plan, child.Run, parent.Workers)
	}
	if _, err = a.ResumeWorker(context.Background(), id, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	parent, _ = store.Load(id)
	child, _ = store.Load(w.RunID)
	if child.Run.Status != StatusCompleted || parent.Plan.Tasks[0].Status != "completed" || !hasString(parent.Plan.Frontier, "next") || len(parent.Workers) != 1 || p.childTurns != 3 || len(child.Actions) != 2 || child.Actions[0].CallID != "once" || child.Result.Status != "success" {
		t.Fatalf("visit did not resume existing ledger: plan=%+v child=%+v calls=%d", parent.Plan, child.Run, p.childTurns)
	}
	if child.WorkerCheckpoint == nil || child.WorkerCheckpoint.Instance != "fixture:80" || len(child.WorkerCheckpoint.EvidenceIDs) != 1 {
		t.Fatalf("typed checkpoint not persisted: %+v", child.WorkerCheckpoint)
	}
}

type handoffReceiverProvider struct {
	requests chan ModelRequest
	release  chan struct{}
}

func (p *handoffReceiverProvider) Complete(ctx context.Context, request ModelRequest) (ModelResponse, error) {
	select {
	case p.requests <- request:
	case <-ctx.Done():
		return ModelResponse{}, ctx.Err()
	}
	select {
	case <-p.release:
		return ModelResponse{Content: "handoff context inspected"}, nil
	case <-ctx.Done():
		return ModelResponse{}, ctx.Err()
	}
}

func TestExplicitTaskHandoffCarriesCheckpointButNotEvidence(t *testing.T) {
	p := &yieldProvider{}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "source", Goal: "inspect source"}, {ID: "followup", Goal: "continue source", HandoffTaskIDs: []string{"missing"}}}); err == nil || !strings.Contains(err.Error(), "invalid handoff source") {
		t.Fatalf("unknown handoff source accepted: %v", err)
	}
	if _, err := a.SetPlan(id, []PlanTask{{ID: "source", Goal: "inspect source"}, {ID: "followup", Goal: "continue source", HandoffTaskIDs: []string{"source"}}}); err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), id, "source")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	parent, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	followup, _ := planTask(&parent, "followup")
	ctx, err := a.newWorkerContext(&parent, *followup)
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.Handoffs) != 1 || ctx.Handoffs[0].SourceRunID != w.RunID || ctx.Handoffs[0].Status != "available" || ctx.Handoffs[0].Checkpoint == nil || len(ctx.Handoffs[0].Evidence) != 1 || ctx.Handoffs[0].Evidence[0].Status != "source_verified" {
		t.Fatalf("explicit handoff missing or unverified: %+v", ctx.Handoffs)
	}
	child, err := store.Load(w.RunID)
	verifiedSourceEvidence := false
	sourceEvidenceID := ""
	for _, evidence := range child.Evidence {
		if evidence.CallID == "once" && evidence.Verification == "verified" {
			verifiedSourceEvidence = true
			sourceEvidenceID = evidence.ID
		}
	}
	if err != nil || !verifiedSourceEvidence {
		t.Fatalf("handoff altered source execution ledger: %v %+v", err, child.Evidence)
	}
	receiverProvider := &handoffReceiverProvider{requests: make(chan ModelRequest, 1), release: make(chan struct{})}
	a.provider = receiverProvider
	limit := 1
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
	next, err := a.DispatchTask(context.Background(), id, "followup")
	if err != nil {
		t.Fatalf("dispatch with a saved handoff checkpoint: %v", err)
	}
	defer a.Cancel(next.RunID)
	var request ModelRequest
	select {
	case request = <-receiverProvider.requests:
	case <-time.After(5 * time.Second):
		t.Fatal("receiver did not issue a real model request")
	}
	actual := contextFromRequest(t, request)
	if actual.RunID != next.RunID || actual.WorkerID != next.ID || actual.ParentRunID != id || actual.TaskID != "followup" || len(actual.Handoffs) != 1 {
		t.Fatalf("actual receiver request lost task identity: %+v", actual)
	}
	handoff := actual.Handoffs[0]
	if handoff.SourceRunID != w.RunID || handoff.SourceTaskID != "source" || handoff.WorkerID != w.ID || handoff.Checkpoint == nil || handoff.Checkpoint.Instance != "fixture:80" || len(handoff.Checkpoint.NegativeResults) != 1 || len(handoff.Evidence) != 1 || handoff.Evidence[0].SourceRunID != w.RunID || handoff.Evidence[0].ID != sourceEvidenceID || handoff.Evidence[0].ContentHash != ctx.Handoffs[0].Evidence[0].ContentHash || handoff.Evidence[0].Status != "source_verified" {
		t.Fatalf("actual request lost source-qualified handoff: %+v", handoff)
	}
	close(receiverProvider.release)
	if _, err = waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	receiver, err := store.Load(next.RunID)
	if err != nil || receiver.WorkerContext == nil || len(receiver.WorkerContext.Handoffs) != 1 || receiver.WorkerContext.Handoffs[0].Status != "available" {
		t.Fatalf("receiver did not persist the verified handoff projection: %v %+v", err, receiver.WorkerContext)
	}
	if sourceEvidenceID == "" || len(receiver.WorkerContext.Handoffs[0].Evidence) != 1 || receiver.WorkerContext.Handoffs[0].Evidence[0].ID != sourceEvidenceID {
		t.Fatal("receiver handoff did not retain source-qualified evidence reference")
	}
	if len(receiver.Actions) != 0 || len(receiver.Evidence) != 0 || receiver.Result.Status == "success" || len(receiver.Requests) != 1 || workerContextHash(receiver.Requests[0]) != workerContextHash(request) {
		t.Fatalf("handoff promoted source execution or rewrote delivered request: %+v", receiver.Result)
	}
}

func TestExplicitHandoffWaitsUntilSourceCheckpointExists(t *testing.T) {
	a, _ := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "a", Goal: "a", HandoffTaskIDs: []string{"b"}}, {ID: "b", Goal: "b", Dependencies: []string{"a"}}}); err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("mixed dependency/handoff cycle accepted: %v", err)
	}
	if _, err := a.SetPlan(id, []PlanTask{{ID: "source", Goal: "inspect source"}, {ID: "followup", Goal: "continue source", HandoffTaskIDs: []string{"source"}}}); err != nil {
		t.Fatal(err)
	}
	workers, err := a.DispatchFrontier(context.Background(), id)
	if err != nil || len(workers) != 1 || workers[0].TaskID != "source" {
		t.Fatalf("handoff receiver dispatched before source checkpoint: workers=%+v err=%v", workers, err)
	}
	if _, err = waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	parent, err := a.store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	followup, _ := planTask(&parent, "followup")
	if _, err = a.ClaimTask(id, followup.ID, "direct-claim", 30); err == nil || !strings.Contains(err.Error(), "handoff checkpoints") {
		t.Fatalf("direct task_claim bypassed missing handoff: %v", err)
	}
	if _, err = a.DispatchTask(context.Background(), id, followup.ID); err == nil || !strings.Contains(err.Error(), "handoff checkpoints") {
		t.Fatalf("direct dispatch accepted a missing source checkpoint: %v", err)
	}
}

func TestWorkerYieldRejectsRootAndBlankCheckpoints(t *testing.T) {
	a, _ := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	for _, args := range [][2]string{{"reason", "checkpoint"}, {" ", "checkpoint"}, {"reason", " "}} {
		if _, err := a.YieldWorker(id, args[0], args[1]); err == nil {
			t.Fatal("invalid yield accepted")
		}
	}
	seen := map[string]bool{}
	for _, d := range builtinDefinitions() {
		if seen[d.Function.Name] {
			t.Fatalf("duplicate builtin %s", d.Function.Name)
		}
		seen[d.Function.Name] = true
	}
	if !seen["worker_yield"] {
		t.Fatal("yield tool not registered")
	}
}

func TestWorkerYieldRejectsMalformedStructuredCheckpoint(t *testing.T) {
	a, _ := newTestAgent(t, successProvider{})
	parent := createTestRun(t, a)
	if _, err := a.SetPlan(parent, []PlanTask{{ID: "visit", Goal: "parser fixture"}}); err != nil {
		t.Fatal(err)
	}
	id := registerBudgetChild(t, a, parent, "visit", "parser-worker")
	for _, tc := range []struct{ raw, want string }{
		{`{"evidence_ids":["missing"]}`, "structured checkpoint requires next_action"},
		{`{"next_action":"continue","extra":"ignored"}`, `unknown field "extra"`},
		{`{"next_action":"continue"} {}`, "checkpoint must contain one JSON object"},
		{`{"next_action":"continue","evidence_ids":["missing"]}`, "not verified in this worker ledger"},
	} {
		call, _ := json.Marshal(map[string]string{"reason": "yield", "checkpoint": tc.raw})
		_, err := a.builtin(context.Background(), id, ToolCall{ID: "yield", Name: "worker_yield", Arguments: string(call)})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("structured checkpoint %s: want %q, got %v", tc.raw, tc.want, err)
		}
	}
	call, _ := json.Marshal(map[string]string{"reason": "yield", "checkpoint": `{"next_action":"continue"}`})
	if _, err := a.builtin(context.Background(), id, ToolCall{ID: "yield-valid", Name: "worker_yield", Arguments: string(call)}); err != nil {
		t.Fatalf("valid structured checkpoint rejected: %v", err)
	}
}
