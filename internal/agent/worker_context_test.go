package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func contextFixture(t *testing.T, body string) (*Agent, *Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(FileRequest{Path: path, Limit: 64000})
	a, store := newTestAgent(t, manualProvider(ToolCall{ID: "source-read", Name: "read", Arguments: string(args)}))
	id := createTestRun(t, a)
	s, err := a.Run(context.Background(), id)
	if err != nil || len(s.Evidence) != 1 || s.Evidence[0].Verification != "verified" {
		t.Fatalf("source fixture: %v %+v", err, s.Evidence)
	}
	if _, err = a.QueueMessage(id, "follow_up", "unrelated-parent-history"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.UpsertAsset(id, Asset{ID: "chosen", Kind: "file", Value: "chosen-asset-value", Coverage: "observed", EvidenceIDs: []string{s.Evidence[0].ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.UpsertAsset(id, Asset{ID: "other", Kind: "file", Value: "unrelated-asset-value"}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPlan(id, []PlanTask{{ID: "dependency", Goal: "chosen-dependency-goal"}, {ID: "work", Goal: "child-task-goal", AssetIDs: []string{"chosen", "missing", "chosen"}, Dependencies: []string{"dependency"}, SuccessContains: "source-proof"}, {ID: "unrelated-task", Goal: "unrelated-task-goal"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ClaimTask(id, "dependency", "fixture", 30); err != nil {
		t.Fatal(err)
	}
	if _, err = a.CompleteTask(id, "dependency", "fixture", "completed", []string{s.Evidence[0].ID}); err != nil {
		t.Fatal(err)
	}
	a.provider = successProvider{}
	return a, store, id
}

func contextChild(t *testing.T, a *Agent, store *Store, id string) Snapshot {
	t.Helper()
	// These providers only inspect inherited context and produce no execution
	// evidence. Bound this negative fixture explicitly instead of declaring its
	// unmet child predicate completed on model stop.
	limit := 1
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
	w, err := a.DispatchTask(context.Background(), id, "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	s, err := store.Load(w.RunID)
	if err != nil || len(s.Requests) != 1 {
		t.Fatalf("child request: %v %+v", err, s.Run)
	}
	resumeLimit := 3 // one summary request plus one resumed model response
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &resumeLimit})
	return s
}

func contextFromRequest(t *testing.T, request ModelRequest) WorkerContext {
	t.Helper()
	count := 0
	var c WorkerContext
	for _, m := range request.Messages {
		if !strings.HasPrefix(m.Content, "Worker task context (") {
			continue
		}
		count++
		if m.Role != "user" {
			t.Fatalf("source context escalated to %s", m.Role)
		}
		_, raw, _ := strings.Cut(m.Content, "\n")
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatal(err)
		}
	}
	if count != 1 {
		t.Fatalf("context count=%d", count)
	}
	return c
}

func TestWorkerContextActualRequestScopeResumeAndNoEvidencePromotion(t *testing.T) {
	a, store, id := contextFixture(t, "source-proof")
	child := contextChild(t, a, store, id)
	first := child.Requests[0]
	c := contextFromRequest(t, first)
	assertTail := func(request ModelRequest) {
		t.Helper()
		lastHistory, source := 0, -1
		for i, m := range request.Messages {
			if m.ID == child.Run.ID+"-worker-context" {
				source = i
			}
			if m.Role == "assistant" || m.Role == "tool" || m.Content == child.Run.Goal {
				lastHistory = i
			}
		}
		if source <= lastHistory {
			t.Fatal("worker source precedes reusable history")
		}
	}
	assertTail(first)
	if c.ParentRunID != id || c.TaskID != "work" || c.Goal != "child-task-goal" || len(c.Assets) != 2 || len(c.Dependencies) != 1 || len(c.Evidence) != 1 {
		t.Fatalf("manifest %+v", c)
	}
	if c.Assets[1].Status != "missing" || c.Evidence[0].SourceRunID != id || c.Evidence[0].Status != "source_verified" {
		t.Fatalf("sources %+v", c)
	}
	raw, _ := json.Marshal(first.Messages)
	for _, excluded := range []string{"unrelated-parent-history", "unrelated-asset-value", "unrelated-task-goal"} {
		if strings.Contains(string(raw), excluded) {
			t.Fatalf("unrelated input copied: %s", excluded)
		}
	}
	if strings.Contains(first.Messages[0].Content, "chosen-asset-value") || len(child.Evidence) != 0 || len(child.Actions) != 0 || child.Result.Status == "success" {
		t.Fatal("source data became instructions or child execution")
	}
	if child.WorkerContext == nil || len(child.Messages) != 3 || child.Messages[2].Role != "user" || !strings.HasPrefix(child.Messages[2].Content, "Runtime completion audit:") {
		t.Fatal("manifest not persisted independently of message history")
	}
	// Corrupt only the source content, preserving the historical request.
	if _, err := store.Update(id, func(s *Snapshot) error { s.Evidence[0].Content += "corrupt"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.QueueMessage(child.Run.ID, "follow_up", "resume child"); err != nil {
		t.Fatal(err)
	}
	resumed, err := a.Run(context.Background(), child.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	next := contextFromRequest(t, resumed.Requests[len(resumed.Requests)-1])
	assertTail(resumed.Requests[len(resumed.Requests)-1])
	if next.Evidence[0].Status != "invalid" || next.Evidence[0].Output != "" {
		t.Fatalf("corrupt source retained: %+v", next.Evidence)
	}
	if workerContextHash(first) != workerContextHash(resumed.Requests[0]) || workerContextHash(child.WorkerContext) != workerContextHash(resumed.WorkerContext) {
		t.Fatal("resume rewrote original manifest/request")
	}
	for _, m := range resumed.Messages {
		if strings.HasPrefix(m.Content, "Worker task context (") {
			t.Fatal("projection duplicated into durable history")
		}
	}
}

func TestWorkerContextMissingChangedAndCASProvenance(t *testing.T) {
	for _, mode := range []string{"artifact_changed", "artifact_missing", "evidence_missing", "evidence_changed", "parent_missing", "asset_changed"} {
		t.Run(mode, func(t *testing.T) {
			a, store, id := contextFixture(t, strings.Repeat("source-proof\n", 2000))
			child := contextChild(t, a, store, id)
			ref := child.WorkerContext.Evidence[0]
			if len(ref.Artifacts) == 0 || len(ref.Output) > 1100 {
				t.Fatalf("CAS context %v", ref)
			}
			switch mode {
			case "artifact_changed":
				if err := os.WriteFile(filepath.Join(store.root, "artifacts", ref.Artifacts[0].Hash), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "artifact_missing":
				if err := os.Remove(filepath.Join(store.root, "artifacts", ref.Artifacts[0].Hash)); err != nil {
					t.Fatal(err)
				}
			case "parent_missing":
				if err := os.Remove(filepath.Join(store.root, id+".json")); err != nil {
					t.Fatal(err)
				}
			default:
				_, err := store.Update(id, func(s *Snapshot) error {
					switch mode {
					case "evidence_missing":
						s.Evidence = nil
					case "evidence_changed":
						s.Evidence[0].ContentHash = "changed"
					case "asset_changed":
						s.Assets[0].Value = "different"
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			text := a.workerContextProjection(&child)
			c := contextFromRequest(t, ModelRequest{Messages: []Message{{Role: "user", Content: text}}})
			if mode == "asset_changed" {
				if c.Assets[0].Status != "changed" || c.Assets[0].Asset != nil {
					t.Fatalf("asset change %v", c.Assets)
				}
			} else if c.Evidence[0].Status == "source_verified" || c.Evidence[0].Output != "" || len(c.Evidence[0].Artifacts) != 0 {
				t.Fatalf("invalid source still delivered: %+v", c)
			}
		})
	}
}

func TestWorkerContextNestedChildAndOldRun(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "nested-parent"}}); err != nil {
		t.Fatal(err)
	}
	child := contextChild(t, a, store, id)
	if _, err := a.QueueMessage(child.Run.ID, "follow_up", "nested child"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SetPlan(child.Run.ID, []PlanTask{{ID: "work", Goal: "nested-leaf"}}); err != nil {
		t.Fatal(err)
	}
	nested := contextChild(t, a, store, child.Run.ID)
	if contextFromRequest(t, nested.Requests[0]).ParentRunID != child.Run.ID {
		t.Fatal("nested provenance lost")
	}
	if _, err := store.Load(id); err != nil {
		t.Fatalf("parent/child provenance recursion: %v", err)
	}
	if _, err := store.Update(nested.Run.ID, func(s *Snapshot) error { s.WorkerContext = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.QueueMessage(nested.Run.ID, "follow_up", "legacy resume"); err != nil {
		t.Fatal(err)
	}
	legacy, err := a.Run(context.Background(), nested.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range legacy.Requests[len(legacy.Requests)-1].Messages {
		if strings.HasPrefix(m.Content, "Worker task context (") {
			t.Fatal("old child context invented")
		}
	}
}

func TestWorkerContextCompactionKeepsExactManifestOrPauses(t *testing.T) {
	for _, mode := range []string{"projection", "semantic"} {
		t.Run(mode, func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id, _ := longHistory(t, a, store)
			s, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			s.Run.Context.CompactionMode = mode
			s.workerContextMessage = "exact-task-context-source"
			if mode == "semantic" {
				end := len(s.Messages) - 1
				ids := make([]string, end)
				for i := range ids {
					ids[i] = s.Messages[i].ID
				}
				s.Compactions = []Compaction{{ID: "summary", Status: "completed", Method: "llm_semantic", SourceMessageIDs: ids, SourceHash: messageHash(s.Messages[:end]), Summary: "short old history"}}
			}
			messages, _, _, err := projectContext(&s, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, m := range messages {
				if m.Content == s.workerContextMessage && m.Role == "user" {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("compaction context count=%d", count)
			}
			s.workerContextMessage = strings.Repeat("large-manifest", 700)
			if _, _, _, err = projectContext(&s, nil, ""); err == nil {
				t.Fatal("oversized manifest silently omitted")
			}
		})
	}
}

func TestWorkerContextRunQualifiedEvidenceIDs(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	other, err := a.Create("dependency")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(other.Run.ID, func(s *Snapshot) error { s.Run.ParentRunID = id; s.Run.ParentTaskID = "dep"; return nil }); err != nil {
		t.Fatal(err)
	}
	parent, _ := store.Load(id)
	parent.Workers = []AgentWorker{{ID: "w", TaskID: "dep", RunID: other.Run.ID}}
	parent.Assets = []Asset{{ID: "one", SourceRunID: id, EvidenceIDs: []string{"same-id"}}, {ID: "two", SourceRunID: other.Run.ID, EvidenceIDs: []string{"same-id"}}}
	c, err := a.newWorkerContext(&parent, PlanTask{ID: "next", AssetIDs: []string{"one", "two", "one"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Evidence) != 2 || c.Evidence[0].SourceRunID == c.Evidence[1].SourceRunID {
		t.Fatalf("source ID collision %+v", c.Evidence)
	}
	for _, ref := range c.Evidence {
		if ref.Status != "missing" {
			t.Fatalf("missing source promoted: %+v", ref)
		}
	}
	// A registered source with altered ancestry is rejected rather than trusted.
	if _, err = store.Update(other.Run.ID, func(s *Snapshot) error { s.Run.ParentTaskID = "different"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = a.workerContextSource(&parent, other.Run.ID); err == nil {
		t.Fatal("changed ancestry accepted")
	}
}

func TestWorkerContextToolSelectionBudgetIncludesManifest(t *testing.T) {
	a, store, id := contextFixture(t, "source-proof")
	child := contextChild(t, a, store, id)
	addLargeDirectory(a)
	defs, err := a.selectedDefinitions(&ToolSelection{IDs: []string{"large-00"}})
	if err != nil {
		t.Fatal(err)
	}
	system := Message{Role: "system", Content: child.Prompt + a.skillCatalog() + deferredToolHint}
	goal := Message{Role: "user", Content: child.Run.Goal}
	bare := EstimateContextTokens([]Message{system, goal}, defs)
	child.workerContextMessage = a.workerContextProjection(&child)
	full := EstimateContextTokens(append(contextPrefix(&child, system), goal), defs)
	if bare >= full {
		t.Fatal("manifest fixture empty")
	}
	limit := bare + (full-bare)/2
	before, err := store.Update(child.Run.ID, func(s *Snapshot) error {
		s.Run.Context = ContextConfig{WindowTokens: limit + 500, OutputReserve: 500, CompactionMode: "projection"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SelectTools(child.Run.ID, []string{"large-00"}, true); err == nil {
		t.Fatal("accepted schemas that cannot fit with worker context")
	}
	after, err := store.Load(child.Run.ID)
	if err != nil || workerContextHash(before.Run.ToolSelection) != workerContextHash(after.Run.ToolSelection) {
		t.Fatal("failed selection changed prior schemas")
	}
	if _, err = a.SelectTools(child.Run.ID, nil, true); err != nil {
		t.Fatalf("clearing schemas failed: %v", err)
	}
}
