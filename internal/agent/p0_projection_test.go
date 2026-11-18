package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"trace/internal/localtool"
)

func TestP0InventoryDiscoverySelectionAndActualRequest(t *testing.T) {
	manifests, _ := localtool.Presets(context.Background())
	var inventory localtool.Manifest
	for _, manifest := range manifests {
		if manifest.Name == "capability-inventory" {
			inventory = manifest
		}
	}
	if inventory.Name == "" {
		t.Fatal("server acceptance requires Python capability-inventory preset")
	}
	id := inventory.StableID()
	selection, _ := json.Marshal(map[string]any{"ids": []string{id}})
	p := &scriptedProvider{responses: []ModelResponse{
		{ToolCalls: []ToolCall{{ID: "multi", Name: "tool_search", Arguments: `{"query":"capability-inventory p0-no-such-term"}`}}},
		{ToolCalls: []ToolCall{{ID: "exact", Name: "tool_search", Arguments: `{"query":"capability-inventory"}`}}},
		{ToolCalls: []ToolCall{{ID: "select", Name: "tool_select", Arguments: string(selection)}}},
		{ToolCalls: []ToolCall{{ID: "inventory", Name: id, Arguments: `{}`}}},
		{Content: "complete"},
	}}
	a, _ := newTestAgent(t, p)
	if err := a.AddLocalTools(manifests); err != nil {
		t.Fatal(err)
	}
	run := createTestRun(t, a)
	if _, err := a.SelectTools(run, []string{}, true); err != nil {
		t.Fatal(err)
	}
	s, err := a.Run(context.Background(), run)
	if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 4 || len(s.Requests) != 5 {
		t.Fatalf("inventory flow: err=%v status=%s actions=%d requests=%d", err, s.Run.Status, len(s.Actions), len(s.Requests))
	}
	for i, want := range []int{0, 1} {
		var search ToolSearchResult
		if err := json.Unmarshal([]byte(s.Actions[i].Result.Stdout), &search); err != nil {
			t.Fatal(err)
		}
		if search.Total != want || want == 1 && (len(search.Tools) != 1 || search.Tools[0].ID != id) {
			t.Fatalf("search %d: %+v", i, search)
		}
	}
	for i, request := range s.Requests {
		found := false
		for _, def := range request.Tools {
			if def.Function.Name == id {
				found = true
				if def.Function.Parameters == nil {
					t.Fatal("selected inventory missing schema")
				}
			}
		}
		if found != (i >= 3) {
			t.Fatalf("schema projection at request %d: found=%v", i, found)
		}
	}
	action := s.Actions[3]
	if action.Name != id || action.Adapter != "local" || action.LocalName != inventory.Name || action.Result.ExitCode != 0 || action.Result.WorkerPID == 0 {
		t.Fatalf("inventory execution: %+v", action)
	}
	output, ok := verifiedOutput(&s, action.Result.EvidenceID)
	if !ok || !strings.Contains(output, "python_version") {
		t.Fatal("inventory source execution not verified")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(action.Result.Stdout), &body); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"tools", "modules", "browser", "alternatives"} {
		if len(body[field]) == 0 {
			t.Fatalf("inventory omitted %s", field)
		}
	}
}

func TestP0WorkerStageProjectionInvalidParentClearsMetadata(t *testing.T) {
	a, store, id := contextFixture(t, "source-proof")
	parent, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	work, err := planTask(&parent, "work")
	if err != nil {
		t.Fatal(err)
	}
	work.EstimatedSeconds, work.ExpectedProgress = 60, "recover access code; final output still required"
	if _, err := a.SetPlan(id, parent.Plan.Tasks); err != nil {
		t.Fatal(err)
	}
	child := contextChild(t, a, store, id)
	first := contextFromRequest(t, child.Requests[0])
	if first.EstimatedSeconds != 60 || first.ExpectedProgress != work.ExpectedProgress || child.Result.Status == "success" || len(child.Evidence) != 0 {
		t.Fatalf("stage metadata promoted or omitted: %+v", first)
	}
	manifestHash := workerContextHash(child.WorkerContext)
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Deleted = true; return nil }); err != nil {
		t.Fatal(err)
	}
	projection := a.workerContextProjection(&child)
	invalid := contextFromRequest(t, ModelRequest{Messages: []Message{{Role: "user", Content: projection}}})
	if invalid.EstimatedSeconds != 0 || invalid.ExpectedProgress != "" || len(invalid.Issues) == 0 {
		t.Fatalf("invalid parent retained stage metadata: %+v", invalid)
	}
	if workerContextHash(child.WorkerContext) != manifestHash || contextFromRequest(t, child.Requests[0]).EstimatedSeconds != 60 {
		t.Fatal("projection changed pinned manifest or archived request")
	}
}
