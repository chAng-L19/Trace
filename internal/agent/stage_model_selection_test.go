package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStageModelSelectionPolicyAndPreclaim(t *testing.T) {
	a, store := newTestAgent(t, &OpenAIProvider{BaseURL: "https://fixture.invalid", Model: "base", ReasoningEfforts: []string{"low"}})
	root, err := a.Create("research")
	if err != nil {
		t.Fatal(err)
	}
	task := PlanTask{ID: "work", Goal: "review", ModelSelection: &ModelSelection{Provider: "default", Model: "base"}}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{task}); err == nil {
		t.Fatal("disabled route accepted")
	}
	roles := RoleModels{StageRoutes: []ModelRoute{{Provider: "default", Model: "base"}}}
	if err = a.ConfigureModels(nil, roles); err != nil {
		t.Fatal(err)
	}
	for _, selection := range []ModelSelection{{Provider: "default", Model: "other"}, {Model: "base"}, {Provider: "default", Model: "base", ReasoningEffort: "high"}, {Provider: "default", Model: "base", ImageInput: true}, {Provider: "default", Model: "base", ProviderID: "forged"}} {
		invalid := task
		invalid.ModelSelection = &selection
		if _, err = a.SetPlan(root.Run.ID, []PlanTask{invalid}); err == nil {
			t.Fatalf("invalid selection accepted: %+v", selection)
		}
	}
	imageTask := task
	imageTask.RequiredImageInput = true
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{imageTask}); err == nil {
		t.Fatal("image requirement accepted")
	}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{task}); err != nil {
		t.Fatal(err)
	}
	if err = a.ConfigureModels(nil, RoleModels{}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ClaimTask(root.Run.ID, "work", "worker", 30); err == nil {
		t.Fatal("revoked route claimed")
	}
	s, err := store.Load(root.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.Plan.Tasks[0].Started || s.Plan.Tasks[0].Status != "pending" || len(s.Workers) != 0 {
		t.Fatal("invalid route changed ownership")
	}
	if err = a.ConfigureModels(nil, roles); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ClaimTask(root.Run.ID, "work", "worker", 30); err != nil {
		t.Fatal(err)
	}
	changed := task
	changed.ModelSelection = &ModelSelection{Provider: "default", Model: "base", ReasoningEffort: "low"}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{changed}); err == nil {
		t.Fatal("started route changed")
	}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{task}); err != nil {
		t.Fatal(err)
	}
}

func TestStageModelSelectionObservedImageAndSettingsCopy(t *testing.T) {
	a, _ := newTestAgent(t, &OpenAIProvider{BaseURL: "https://fixture.invalid", Model: "text"})
	routes := []ModelRoute{{Provider: "default", Model: "text"}}
	if err := a.ConfigureModels(nil, RoleModels{StageRoutes: routes}); err != nil {
		t.Fatal(err)
	}
	routes[0].Model = "mutated"
	task := PlanTask{ID: "visual", Goal: "inspect attachment", InputAttachmentIDs: []string{"image"}, ModelSelection: &ModelSelection{Provider: "default", Model: "text"}}
	parent := Snapshot{Messages: []Message{{Role: "user", Attachments: []InputAttachment{{ID: "image", MIMEType: "image/png"}}}}}
	if err := a.preflightStageModel(&parent, task); err == nil {
		t.Fatal("observed image downgraded")
	}
	task.InputAttachmentIDs = nil
	if err := a.preflightStageModel(&parent, task); err != nil {
		t.Fatalf("caller mutated settings: %v", err)
	}
	for _, invalid := range [][]ModelRoute{{{Provider: "default", Model: "text"}, {Provider: "default", Model: "text"}}, {{Provider: "missing", Model: "text"}}, {{Provider: "", Model: "text"}}} {
		if err := a.ConfigureModels(nil, RoleModels{StageRoutes: invalid}); err == nil {
			t.Fatal("invalid settings accepted")
		}
	}
	if err := a.preflightStageModel(&parent, task); err != nil {
		t.Fatalf("failed config changed live policy: %v", err)
	}
}

func TestStageModelSelectionRejectsEmptyBindingIdentity(t *testing.T) {
	for _, name := range []string{"default", "named"} {
		t.Run(name, func(t *testing.T) {
			p := &OpenAIProvider{BaseURL: "https://fixture.invalid/v1", Model: "fixture"}
			a, err := New(nil, p, "")
			if err != nil {
				t.Fatal(err)
			}
			if err = a.ConfigureModels(map[string]*OpenAIProvider{"named": p}, RoleModels{Worker: &ModelSelection{Provider: name, Model: "fixture"}}); err != nil {
				t.Fatal(err)
			}
			binding, err := a.stageModelSelection(PlanTask{ID: "stage"})
			if err != nil || binding == nil || binding.ProviderID == "" {
				t.Fatalf("valid stage route failed: %v", err)
			}
			task := PlanTask{ID: "stage", ModelBinding: binding}
			if saved, err := a.stageModelSelection(task); err != nil || saved == nil || *saved != *binding {
				t.Fatalf("valid stage binding changed: %v", err)
			}
			binding.ProviderID = ""
			if saved, err := a.stageModelSelection(task); err == nil || saved != nil {
				t.Fatal("empty stage binding identity accepted")
			}
			a.modelProviders[name].BaseURL = "https://user:url-secret@fixture.invalid/%zz"
			_, err = a.stageModelSelection(task)
			requireProviderURLRejected(t, err)
		})
	}
}

func TestStageModelSelectionHTTPAndRestart(t *testing.T) {
	seen := make(chan map[string]any, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		seen <- input
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "complete"}, "finish_reason": "stop"}}})
	}))
	defer server.Close()
	a, store := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "base"})
	p := &OpenAIProvider{BaseURL: server.URL + "/worker", Model: "small", ReasoningEfforts: []string{"low"}, ModelCapabilities: map[string]ModelCapabilities{"large": {ImageInput: true, ReasoningEfforts: []string{"high"}}}}
	roles := RoleModels{Worker: &ModelSelection{Provider: "worker", Model: "small", ReasoningEffort: "low"}, StageRoutes: []ModelRoute{{Provider: "worker", Model: "large"}}}
	if err := a.ConfigureModels(map[string]*OpenAIProvider{"worker": p}, roles); err != nil {
		t.Fatal(err)
	}
	root, err := a.Create("research")
	if err != nil {
		t.Fatal(err)
	}
	tasks := []PlanTask{{ID: "explicit", Goal: "visual review", RequiredImageInput: true, ModelSelection: &ModelSelection{Provider: "worker", Model: "large"}}, {ID: "default", Goal: "ordinary review"}}
	if _, err = a.SetPlan(root.Run.ID, tasks); err != nil {
		t.Fatal(err)
	}
	worker, err := a.DispatchTask(context.Background(), root.Run.ID, "explicit")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.WaitWorkers(context.Background(), root.Run.ID, 10); err != nil {
		t.Fatal(err)
	}
	got := <-seen
	if got["model"] != "large" || got["reasoning_effort"] != nil {
		t.Fatalf("route borrowed old effort: %+v", got)
	}
	if _, err = a.DispatchTask(context.Background(), root.Run.ID, "default"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.WaitWorkers(context.Background(), root.Run.ID, 10); err != nil {
		t.Fatal(err)
	}
	got = <-seen
	if got["model"] != "small" || got["reasoning_effort"] != "low" {
		t.Fatalf("default changed: %+v", got)
	}
	child, err := store.Load(worker.RunID)
	if err != nil {
		t.Fatal(err)
	}
	providerID, err := providerIdentity(p.BaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if len(child.Requests) != 1 || child.Requests[0].ProviderModel != "large" || child.Requests[0].ProviderID != providerID {
		t.Fatal("request route not archived")
	}
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(reopened, &OpenAIProvider{BaseURL: server.URL, Model: "changed"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.ConfigureModels(map[string]*OpenAIProvider{"worker": p}, RoleModels{}); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.QueueMessage(worker.RunID, "follow_up", "continue original route"); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.Run(context.Background(), worker.RunID); err != nil {
		t.Fatal(err)
	}
	got = <-seen
	if got["model"] != "large" || got["reasoning_effort"] != nil {
		t.Fatal(fmt.Sprint("restore changed route: ", got))
	}
}
