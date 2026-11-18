package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestStageModelSelectionBindingSurvivesRetryAndRejectsDrift(t *testing.T) {
	a, store := newTestAgent(t, &OpenAIProvider{BaseURL: "https://fixture.invalid", Model: "first", ReasoningEfforts: []string{"low"}})
	if err := a.ConfigureModels(nil, RoleModels{Worker: &ModelSelection{Provider: "default", Model: "first", ReasoningEffort: "low"}}); err != nil {
		t.Fatal(err)
	}
	root, err := a.Create("pin the effective worker default")
	if err != nil {
		t.Fatal(err)
	}
	task := PlanTask{ID: "work", Goal: "review"}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{task}); err != nil {
		t.Fatal(err)
	}
	claimed, err := a.ClaimTask(root.Run.ID, task.ID, "first-worker", 30)
	if err != nil {
		t.Fatal(err)
	}
	binding := claimed.Plan.Tasks[0].ModelBinding
	if binding == nil || binding.Model != "first" || binding.ReasoningEffort != "low" {
		t.Fatalf("default not frozen: %+v", binding)
	}
	forged := task
	forged.ModelBinding = &ModelSelection{Provider: "default", Model: "forged"}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{forged}); err == nil {
		t.Fatal("started binding overwritten")
	}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{task}); err != nil {
		t.Fatal(err)
	}
	if err = a.finishWorkerFailure(root.Run.ID, task.ID, "first-worker", fmt.Errorf("zero-action setup failure")); err != nil {
		t.Fatal(err)
	}
	if _, err = a.RetryTask(root.Run.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	restart, err := New(reopened, &OpenAIProvider{BaseURL: "https://fixture.invalid", Model: "new-default", ModelCapabilities: map[string]ModelCapabilities{"first": {ReasoningEfforts: []string{"low"}}}}, "")
	if err != nil {
		t.Fatal(err)
	}
	claimed, err = restart.ClaimTask(root.Run.ID, task.ID, "second-worker", 30)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Plan.Tasks[0].ModelBinding == nil || *claimed.Plan.Tasks[0].ModelBinding != *binding {
		t.Fatal("retry borrowed changed default")
	}
	if err = restart.finishWorkerFailure(root.Run.ID, task.ID, "second-worker", fmt.Errorf("zero-action setup failure")); err != nil {
		t.Fatal(err)
	}
	if _, err = restart.RetryTask(root.Run.ID, task.ID); err != nil {
		t.Fatal(err)
	}
	drift, err := New(reopened, &OpenAIProvider{BaseURL: "https://changed.invalid", Model: "first", ReasoningEfforts: []string{"low"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = drift.ClaimTask(root.Run.ID, task.ID, "third-worker", 30); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("retry endpoint drift accepted: %v", err)
	}
	after, err := reopened.Load(root.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Plan.Tasks[0].Status != "pending" || after.Plan.Tasks[0].WorkerID != "" {
		t.Fatal("drift consumed ownership")
	}
	fresh := PlanTask{ID: "forged", Goal: "fake binding", ModelBinding: binding}
	if _, err = restart.SetPlan(root.Run.ID, []PlanTask{task, fresh}); err == nil {
		t.Fatal("new task accepted a forged binding")
	}
}

func TestStageModelSelectionBindingDoesNotReplayUncertain(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	root, err := a.Create("uncertain remains uncertain")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{{ID: "work", Goal: "review"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(root.Run.ID, func(s *Snapshot) error {
		s.Plan.Tasks[0].Status = "uncertain"
		s.Plan.Tasks[0].Started = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.DispatchTask(context.Background(), root.Run.ID, "work"); err == nil {
		t.Fatal("uncertain dispatched")
	}
	if _, err = a.RetryTask(root.Run.ID, "work"); err == nil {
		t.Fatal("uncertain retried")
	}
}

func TestStageModelSelectionPolicyVisibleBeforePlanning(t *testing.T) {
	a, _ := newTestAgent(t, &OpenAIProvider{BaseURL: "https://private-endpoint.invalid", APIKey: "private-fixture-secret", Model: "base", ReasoningEfforts: []string{"low"}})
	roles := RoleModels{StageRoutes: []ModelRoute{{Provider: "default", Model: "base"}, {Provider: "default", Model: "other"}}}
	if err := a.ConfigureModels(nil, roles); err != nil {
		t.Fatal(err)
	}
	root, err := a.Create("direct task with no plan")
	if err != nil {
		t.Fatal(err)
	}
	req, err := a.preparePiRequest(context.Background(), root.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	policy := requestRuntimeObservations(root.Run.ID, req)
	if !strings.Contains(policy, "Stage model policy") || !strings.Contains(policy, "worker_default") || !strings.Contains(policy, "other") {
		t.Fatalf("routes not discoverable: %s", policy)
	}
	if strings.Contains(policy, "private-endpoint") || strings.Contains(policy, "private-fixture-secret") || strings.Contains(policy, "provider_id") {
		t.Fatal("model policy leaked endpoint or credentials")
	}
	_, body, ok := strings.Cut(policy, "Never replay uncertain effects.\n")
	if !ok {
		t.Fatal("policy JSON missing")
	}
	var decoded map[string]json.RawMessage
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	var routes []map[string]json.RawMessage
	if err := json.Unmarshal(decoded["stage_routes"], &routes); err != nil {
		t.Fatal(err)
	}
	if len(routes) != 2 || string(routes[0]["reasoning_efforts"]) != "[\"low\"]" || string(routes[1]["reasoning_efforts"]) != "null" {
		t.Fatalf("wrong per-route efforts: %s", body)
	}
}
