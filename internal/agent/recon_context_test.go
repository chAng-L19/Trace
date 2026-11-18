package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReconPreparedBeforeModelAndNoUnneededCollection(t *testing.T) {
	var searches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		searches.Add(1)
		t.Error("input-only task queried external source")
	}))
	defer server.Close()
	p := manualProvider()
	a, store := newTestAgent(t, p)
	if err := a.ConfigureSearchSources(map[string]SearchSourceConfig{"fofa": {Enabled: true, APIKey: "private-source-key", Email: "private-account-email", BaseURL: server.URL}}); err != nil {
		t.Fatal(err)
	}
	attachment, err := store.PutInputAttachment("input.txt", strings.NewReader("Known interface: /api/v2/items"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.CreateConfigured("Summarize the supplied text", RunConfiguration{CompletionCriteria: "concise answer"}, attachment)
	if err != nil || s.RootContract == nil || s.Recon == nil || s.Recon.RootContractHash != valueHash(s.RootContract) || len(s.Recon.Sources) != 1 {
		t.Fatalf("startup missing fixed root/inputs: %v %+v", err, s.Recon)
	}
	digest := s.Recon.Digest
	if _, err = a.Run(context.Background(), s.Run.ID); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) == 0 {
		t.Fatal("no actual model request")
	}
	text := ""
	for _, message := range p.requests[0].Messages {
		text += message.Content
	}
	if !strings.Contains(text, "Prepared inputs (source data)") || !strings.Contains(text, attachment.Hash) || strings.Contains(text, "private-source-key") || strings.Contains(text, "private-account-email") || searches.Load() != 0 {
		t.Fatal("actual first request lost known inputs or leaked credentials")
	}
	loaded, err := store.Load(s.Run.ID)
	if err != nil || loaded.Recon.Digest != digest {
		t.Fatal("unchanged inputs were recollected")
	}
}

func TestReconHandoffPinsSourcesAndInvalidatesChangedVersions(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	parent, err := a.UpsertAsset(id, Asset{ID: "interface", Kind: "api", Value: "https://user:secret@example.test/api/v2?token=private", Coverage: "discovered"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err = a.UpsertAsset(id, Asset{ID: "unrelated", Kind: "file", Value: "unrelated-private-context", Coverage: "discovered"})
	if err != nil {
		t.Fatal(err)
	}
	task := PlanTask{ID: "work", Goal: "use known interface", AssetIDs: []string{"interface"}, ExecutionRole: "worker", EvidenceScope: &EvidenceScope{ObjectID: "service", Version: "v2"}}
	manifest, err := a.newWorkerContext(&parent, task)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.KnownInputs == nil || manifest.KnownInputs.ObjectVersion.Version != "v2" || len(manifest.KnownInputs.Sources) != 1 || manifest.KnownInputs.Sources[0].Value != "https://example.test/api/v2" {
		t.Fatalf("minimal handoff: %+v", manifest.KnownInputs)
	}
	pinned := workerContextHash(manifest)
	parent, err = a.UpsertAsset(id, Asset{ID: "interface", Kind: "api", Value: "https://example.test/api/v3", Coverage: "discovered"})
	if err != nil {
		t.Fatal(err)
	}
	projection := a.reprojectKnownInputs(&parent, manifest.KnownInputs)
	if projection.Sources[0].Status != "changed" || projection.Sources[0].Value != "" || workerContextHash(manifest) != pinned {
		t.Fatal("changed source silently substituted or original manifest mutated")
	}
	if _, err = store.Update(id, func(s *Snapshot) error { a.prepareRecon(s); return nil }); err != nil {
		t.Fatal(err)
	}
	latest, _ := store.Load(id)
	if latest.Recon == nil || latest.Recon.Digest == parent.Recon.Digest {
		t.Fatal("source changes did not update root inventory")
	}
}

func TestReconRoleSelectionFrozenPrecedenceAndValidation(t *testing.T) {
	a, _ := newTestAgent(t, &OpenAIProvider{BaseURL: "https://models.invalid", Model: "base"})
	roles := RoleModels{Worker: &ModelSelection{Model: "worker"}, Recon: &ModelSelection{Model: "collector"}, StageRoutes: []ModelRoute{{Provider: "default", Model: "override"}}}
	if err := a.ConfigureModels(nil, roles); err != nil {
		t.Fatal(err)
	}
	root, err := a.Create("collect sources")
	if err != nil {
		t.Fatal(err)
	}
	task := PlanTask{ID: "recon", Goal: "index existing sources", ExecutionRole: "recon"}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{task}); err != nil {
		t.Fatal(err)
	}
	claimed, err := a.ClaimTask(root.Run.ID, "recon", "collector-worker", 30)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Plan.Tasks[0].ModelBinding.Model != "collector" {
		t.Fatal("recon role ignored")
	}
	changed := task
	changed.ExecutionRole = "worker"
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{changed}); err == nil {
		t.Fatal("started execution role changed")
	}
	if _, err = a.stageModelSelection(PlanTask{ID: "bad", ExecutionRole: "recon-ish"}); err == nil {
		t.Fatal("invalid role accepted")
	}
	explicit := task
	explicit.ModelSelection = &ModelSelection{Provider: "default", Model: "override"}
	selected, err := a.stageModelSelection(explicit)
	if err != nil || selected.Model != "override" {
		t.Fatal("allowed task override lost")
	}
	explicit.ModelBinding = selected
	if err = a.ConfigureModels(nil, RoleModels{Worker: &ModelSelection{Model: "new-worker"}, Recon: &ModelSelection{Model: "new-collector"}}); err != nil {
		t.Fatal(err)
	}
	restored, err := a.stageModelSelection(explicit)
	if err != nil || *restored != *selected {
		t.Fatal("saved binding lost to changed defaults/allowlist")
	}
	if err = a.ConfigureModels(nil, RoleModels{Worker: &ModelSelection{Model: "fallback"}}); err != nil {
		t.Fatal(err)
	}
	selected, err = a.stageModelSelection(task)
	if err != nil || selected.Model != "fallback" {
		t.Fatal("missing recon did not inherit worker")
	}
	if err = a.ConfigureModels(nil, RoleModels{}); err != nil {
		t.Fatal(err)
	}
	selected, err = a.stageModelSelection(task)
	if err != nil || selected.Model != "base" {
		t.Fatal("missing worker did not inherit provider")
	}
}

func TestReconRoleActuallyDispatchedToDedicatedModel(t *testing.T) {
	seen := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		json.NewDecoder(r.Body).Decode(&payload)
		model, _ := payload["model"].(string)
		seen <- model
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "input collection complete"}, "finish_reason": "stop"}}})
	}))
	defer server.Close()
	a, store := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "base"})
	if err := a.ConfigureModels(nil, RoleModels{Worker: &ModelSelection{Model: "worker"}, Recon: &ModelSelection{Model: "collector"}}); err != nil {
		t.Fatal(err)
	}
	root := createTestRun(t, a)
	if _, err := a.SetPlan(root, []PlanTask{{ID: "recon", Goal: "organize inputs", ExecutionRole: "recon"}}); err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), root, "recon")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.WaitWorkers(context.Background(), root, 10); err != nil {
		t.Fatal(err)
	}
	child, err := store.Load(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if <-seen != "collector" || child.Run.ModelSelection.Model != "collector" || child.WorkerContext.ExecutionRole != "recon" || child.WorkerContext.KnownInputs == nil {
		t.Fatal("dispatch did not preserve actual role/model/inputs")
	}
}
