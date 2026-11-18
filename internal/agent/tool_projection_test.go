package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"trace/internal/mcp"
)

func addLargeDirectory(a *Agent) {
	for i := 0; i < 60; i++ {
		d := ToolDefinition{Type: "function"}
		d.Function.Name = fmt.Sprintf("large-%02d", i)
		d.Function.Description = "bulk unrelated capability " + strings.Repeat("schema detail ", 400)
		d.Function.Parameters = map[string]any{"type": "object"}
		a.registry.tools[d.Function.Name] = RegisteredTool{Definition: d, Adapter: "mcp", ServerName: "bulk", RemoteName: d.Function.Name}
	}
}

func TestToolProjectionSearchSelectHTTPAndResume(t *testing.T) {
	server, calls, _ := integrationServer(t, "success")
	toolID := mcp.ToolID("fixture", "echo")
	args, _ := json.Marshal(map[string]any{"ids": []string{toolID}})
	p := &scriptedProvider{responses: []ModelResponse{
		{ToolCalls: []ToolCall{{ID: "find", Name: "tool_search", Arguments: `{"query":"fixture echo"}`}}},
		{ToolCalls: []ToolCall{{ID: "select", Name: "tool_select", Arguments: string(args)}}},
		{ToolCalls: []ToolCall{{ID: "execute", Name: toolID, Arguments: `{"message":"selected"}`}}},
		{Content: "complete"},
	}}
	a, store := newTestAgent(t, p)
	addIntegrationMCP(t, a, mcp.Config{Name: "fixture", URL: server.URL})
	addLargeDirectory(a)
	all := len(a.registry.Definitions())
	if EstimateContextTokens(nil, a.registry.Definitions()) <= 250000 {
		t.Fatal("fixture directory too small")
	}
	id := createTestRun(t, a)
	two := 2
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &two})
	s, err := a.Run(context.Background(), id)
	if err != nil || s.Run.Status != StatusPaused || s.Run.ToolSelection == nil || len(s.Run.ToolSelection.IDs) != 1 || s.Run.ToolSelection.IDs[0] != toolID {
		t.Fatalf("selection not persisted: %v %+v", err, s.Run)
	}
	if len(s.Requests[0].Tools) != 1+len(builtinDefinitions()) || len(a.registry.Definitions()) != all {
		t.Fatal("full registry mutated or large schemas leaked")
	}
	if !strings.Contains(s.Actions[0].Result.Stdout, toolID) || calls.Load() != 0 {
		t.Fatal("search did not discover stable ID or executed remote prematurely")
	}
	restarted, _ := New(store, p, "")
	addIntegrationMCP(t, restarted, mcp.Config{Name: "fixture", URL: server.URL})
	addLargeDirectory(restarted)
	twenty := 20
	restarted.ConfigureBudget(BudgetOverride{MaxModelRequests: &twenty})
	s, err = restarted.Run(context.Background(), id)
	if err != nil || s.Run.Status != StatusCompleted || calls.Load() != 1 {
		t.Fatalf("selected MCP call did not execute once: %v %+v", err, s.Run)
	}
	if len(s.Requests[2].Tools) != 2+len(builtinDefinitions()) {
		t.Fatal("selected schema not restored after restart")
	}
	for _, req := range s.Requests {
		if req.ContextTokens > 250000-16384 {
			t.Fatal("unbounded projected request")
		}
	}
	actual := s.Actions[2]
	if actual.Name != toolID || actual.RemoteName != "echo" || actual.ServerName != "fixture" {
		t.Fatal("tool provenance lost")
	}
	branch, err := restarted.Branch(id, s.Messages[len(s.Messages)-1].ID)
	if err != nil || branch.Run.ToolSelection == nil || branch.Run.ToolSelection.IDs[0] != toolID {
		t.Fatal("branch lost tool selection")
	}
}

func TestToolProjectionSelectionErrorsAreAtomic(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	addLargeDirectory(a)
	id := createTestRun(t, a)
	if _, err := a.SelectTools(id, []string{"large-00"}, true); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Load(id)
	for _, ids := range [][]string{{"does-not-exist"}, {"exec"}} {
		if _, err := a.SelectTools(id, ids, true); err == nil {
			t.Fatal("invalid selection accepted")
		}
	}
	var all []string
	for i := 0; i < 60; i++ {
		all = append(all, fmt.Sprintf("large-%02d", i))
	}
	if _, err := a.SelectTools(id, all, true); err == nil {
		t.Fatal("over-budget selection accepted")
	}
	after, _ := store.Load(id)
	b, _ := json.Marshal(before)
	c, _ := json.Marshal(after)
	if string(b) != string(c) {
		t.Fatal("failed selection changed snapshot")
	}
	page, err := a.SearchTools("bulk", 55, 10)
	if err != nil || page.Total != 60 || len(page.Tools) != 5 || page.NextOffset != 60 {
		t.Fatal("search pagination dropped capabilities")
	}
	s, err := a.SelectTools(id, []string{}, true)
	if err != nil || len(s.Run.ToolSelection.IDs) != 0 {
		t.Fatal("selection clear failed")
	}
	defs, _, err := a.modelTools(&s, "")
	if err != nil || len(defs) != 1+len(builtinDefinitions()) {
		t.Fatal("clear did not preserve discovery tools")
	}
}

func TestToolProjectionMissingSelectedRegistryPauses(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	addLargeDirectory(a)
	id := createTestRun(t, a)
	if _, err := a.SelectTools(id, []string{"large-00"}, true); err != nil {
		t.Fatal(err)
	}
	restarted, _ := New(store, successProvider{}, "")
	s, err := restarted.Run(context.Background(), id)
	if err != nil || s.Run.Status != StatusPaused || len(s.Requests) != 0 || !strings.Contains(s.Run.PauseReason, "no longer registered") {
		t.Fatal("missing selected identity silently dropped")
	}
}
