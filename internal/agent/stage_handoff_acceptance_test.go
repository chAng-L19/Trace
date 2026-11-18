package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Deterministic HTTP decisions; real pi, files, child lifecycle and Judge.
// Run with the server test suite, not as a live-model quality measurement.
func TestStageHandoffDifferentModelEndToEndAcceptance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.txt")
	const content = "source-version-one: verified report input"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Model    string
			Messages []struct {
				Role       string
				Content    json.RawMessage
				ToolCallID string `json:"tool_call_id"`
			}
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		results := map[string]ToolResult{}
		for _, m := range input.Messages {
			if m.Role == "tool" {
				var text string
				var result ToolResult
				if json.Unmarshal(m.Content, &text) == nil && json.Unmarshal([]byte(text), &result) == nil {
					results[m.ToolCallID] = result
				}
			}
		}
		for id, result := range results {
			if result.ExitCode != 0 || result.Error != "" {
				http.Error(w, fmt.Sprintf("fixture action %s failed: %s", id, result.Error), 400)
				return
			}
		}
		message := map[string]any{"role": "assistant", "content": fixtureReport("Declared evidence check completed.")}
		call := func(id, name string, args any) {
			raw, _ := json.Marshal(args)
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(raw)}}}}
		}
		if _, ok := results["read-source"]; !ok {
			call("read-source", "read", map[string]any{"path": path})
		} else if input.Model == "old" {
			checkpoint, _ := json.Marshal(WorkerHandoff{Subject: "report", Instance: "version-one", Progress: "source inspected", Findings: []string{content}, NextAction: "verify source under replacement stage", EvidenceIDs: []string{results["read-source"].EvidenceID}})
			call("yield", "worker_yield", map[string]any{"reason": "explicit route handoff", "checkpoint": string(checkpoint)})
		} else if _, ok := results["check"]; !ok {
			requirement := "stage-proof"
			if input.Model == "root" {
				requirement = "root-proof"
			}
			if input.Model == "down" {
				requirement = "down-proof"
			}
			call("check", "evidence_check", map[string]any{"requirement_id": requirement, "evidence_ids": []string{results["read-source"].EvidenceID}})
		}
		finish := "stop"
		if message["tool_calls"] != nil {
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finish}}})
	}))
	defer server.Close()
	a, store := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "root", ModelCapabilities: map[string]ModelCapabilities{"old": {}, "new": {}, "down": {}}})
	defer func() {
		if err := a.StopWorkers(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	a.SetRequireVerifiedCompletion(true)
	if err := a.ConfigureModels(nil, RoleModels{StageRoutes: []ModelRoute{{Provider: "default", Model: "old"}, {Provider: "default", Model: "new"}, {Provider: "default", Model: "down"}}}); err != nil {
		t.Fatal(err)
	}
	requirement := func(id string) VerificationRequirement {
		return VerificationRequirement{ID: id, Description: "exact source observation", Category: "goal", ToolID: "read", Predicate: EvidencePredicate{Kind: "json_equals", Pointer: "/content", Expected: content}}
	}
	rootRequirement := requirement("root-proof")
	root, err := a.CreateConfigured("Deliver verified report after a route handoff", RunConfiguration{Requirements: []VerificationRequirement{rootRequirement}, RootContract: &RootContract{Version: 1, RequirementIDs: []string{"root-proof"}}})
	if err != nil {
		t.Fatal(err)
	}
	old := PlanTask{ID: "old-stage", Goal: "verify report input", Stage: true, EvidenceScope: &EvidenceScope{ObjectID: "report-source", Version: "version-one"}, Requirements: []VerificationRequirement{requirement("stage-proof")}, ModelSelection: &ModelSelection{Provider: "default", Model: "old"}}
	downstream := PlanTask{ID: "downstream", Goal: "review completed input", Dependencies: []string{old.ID}, Requirements: []VerificationRequirement{requirement("down-proof")}, ModelSelection: &ModelSelection{Provider: "default", Model: "down"}}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{old, downstream}); err != nil {
		t.Fatal(err)
	}
	oldWorker, err := a.DispatchTask(context.Background(), root.Run.ID, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := a.WaitWorkers(context.Background(), root.Run.ID, 30)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Load(oldWorker.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if source.Run.Status != StatusPaused || source.WorkerCheckpoint == nil || len(source.WorkerCheckpoint.EvidenceIDs) != 1 || parent.Workers[0].CleanupStatus != "completed" || parent.Plan.Tasks[0].Status != "paused" {
		t.Fatalf("source did not yield a settled checkpoint: %+v", source.Run)
	}
	oldWorker = parent.Workers[0]
	next := old
	next.ID = "replacement"
	next.ModelSelection = &ModelSelection{Provider: "default", Model: "new"}
	next.HandoffTaskIDs = []string{old.ID}
	tasks := append(append([]PlanTask(nil), parent.Plan.Tasks...), next)
	if _, err = a.SetPlan(root.Run.ID, tasks); err != nil {
		t.Fatal(err)
	}
	parent, err = a.DisposeTask(root.Run.ID, old.ID, "superseded", "checkpoint preserved; change route", next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if taskDone(parent.Plan.Tasks[0]) || hasString(parent.Plan.Frontier, downstream.ID) {
		t.Fatal("retired stage satisfied old dependency")
	}
	for i := range parent.Plan.Tasks {
		if parent.Plan.Tasks[i].ID == downstream.ID {
			parent.Plan.Tasks[i].Dependencies = []string{next.ID}
		}
	}
	if _, err = a.SetPlan(root.Run.ID, parent.Plan.Tasks); err != nil {
		t.Fatal(err)
	}
	if err = a.DeliverWorkerMessages(root.Run.ID); err != nil {
		t.Fatal(err)
	}
	nextWorker, err := a.DispatchTask(context.Background(), root.Run.ID, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	parent, err = a.WaitWorkers(context.Background(), root.Run.ID, 30)
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Load(nextWorker.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Result.Status != "success" || child.Run.ModelSelection.Model != "new" || source.Run.ModelSelection.Model != "old" {
		t.Fatal("new stage did not execute selected distinct route")
	}
	if !sameTaskRequirements(child.Requirements, old.Requirements) || !sameEvidenceScope(executionEvidenceScope(&child), old.EvidenceScope) {
		t.Fatal("replacement weakened acceptance or changed object version")
	}
	if child.WorkerContext == nil || len(child.WorkerContext.Handoffs) != 1 || child.WorkerContext.Handoffs[0].SourceRunID != source.Run.ID || child.WorkerContext.Handoffs[0].CheckpointHash == "" {
		t.Fatal("source handoff was not pinned")
	}
	seenContext := false
	for _, request := range child.Requests {
		if request.ProviderModel != "new" {
			t.Fatal("request route drift")
		}
		for _, m := range request.Messages {
			if strings.Contains(m.Content, source.Run.ID) && strings.Contains(m.Content, "same_declared_scope_not_reobserved") && strings.Contains(m.Content, content) {
				seenContext = true
			}
		}
	}
	if !seenContext || len(child.Actions) != 2 || child.Actions[0].Name != "read" || child.Actions[1].Name != "evidence_check" {
		t.Fatal("replacement did not consume pinned context and execute its own proof")
	}
	if len(rootContractGaps(&parent)) == 0 {
		t.Fatal("child success bypassed root-local acceptance")
	}
	if taskDone(parent.Plan.Tasks[0]) || !hasString(parent.Plan.Frontier, downstream.ID) {
		t.Fatal("replacement failed to unlock explicitly rewired dependency")
	}
	unlock, err := store.AcquireRunLock(oldWorker.RunID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(root.Run.ID, func(s *Snapshot) error { return a.applyWorkerSettlementLocked(s, oldWorker) })
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	parent, err = store.Load(root.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := planTask(&parent, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Plan.Tasks[0].Status != "superseded" || replacement.Status != "verified" || replacement.ChildRunID != nextWorker.RunID {
		t.Fatal("late original receipt rewrote replacement")
	}
	if err = a.DeliverWorkerMessages(root.Run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DispatchTask(context.Background(), root.Run.ID, downstream.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = a.WaitWorkers(context.Background(), root.Run.ID, 30); err != nil {
		t.Fatal(err)
	}
	finished, err := a.Run(context.Background(), root.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Result.Status != "success" || len(rootContractGaps(&finished)) != 0 || !sameTaskRequirements(finished.Requirements, []VerificationRequirement{rootRequirement}) {
		t.Fatalf("root acceptance incomplete: %+v", finished.Result)
	}
	for _, task := range finished.Plan.Tasks {
		if !taskSatisfiedForRoot(&finished, task) {
			t.Fatal(fmt.Sprint("unsettled task ", task.ID))
		}
	}
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reopened.Load(root.Run.ID)
	if err != nil || saved.Result.Status != "success" || saved.Plan.Tasks[0].Status != "superseded" {
		t.Fatalf("restart lost handoff acceptance: %v", err)
	}
}
