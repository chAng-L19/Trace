package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"
	"trace/internal/mcp"
)

func TestToolSchemaModesPersistAndMeasure(t *testing.T) {
	for _, mode := range []string{"auto", "deferred", "narrow"} {
		t.Run(mode, func(t *testing.T) {
			p := &scriptedProvider{responses: []ModelResponse{{Content: "done"}}}
			a, store := newTestAgent(t, p)
			d := ToolDefinition{Type: "function"}
			d.Function.Name = "fixture-tool"
			d.Function.Description = "explicit fixture"
			d.Function.Parameters = map[string]any{"type": "object"}
			a.registry.tools[d.Function.Name] = RegisteredTool{Definition: d, Adapter: "local"}
			cfg := ToolSchemaConfig{Mode: mode}
			if mode == "narrow" {
				cfg.IDs = []string{d.Function.Name}
			}
			s, err := a.CreateConfigured("mode", RunConfiguration{ToolSchema: cfg, Exec: &ExecConfig{TimeoutSeconds: 7}})
			if err != nil {
				t.Fatal(err)
			}
			s, err = a.Run(context.Background(), s.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := store.Load(s.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Run.ToolSchema.Mode != mode || len(saved.Requests) != 1 {
				t.Fatalf("mode lost %+v", saved.Run)
			}
			req := saved.Requests[0]
			metrics := req.ToolMetrics
			if metrics == nil || metrics.Mode != mode || metrics.SchemaBytes <= 0 || metrics.SchemaCount != len(req.Tools) || metrics.EstimatedSchemaTokens != EstimateContextTokens(nil, req.Tools) {
				t.Fatalf("metrics: %+v", metrics)
			}
			found := false
			for _, tool := range req.Tools {
				found = found || tool.Function.Name == d.Function.Name
			}
			if found != (mode != "deferred") {
				t.Fatalf("schema delivery %v in %s", found, mode)
			}
			branch, err := a.Branch(s.Run.ID, s.Messages[len(s.Messages)-1].ID)
			if err != nil || branch.Run.ToolSchema.Mode != mode || branch.Run.Exec == nil || branch.Run.Exec.TimeoutSeconds != 7 {
				t.Fatalf("branch config lost: %+v %v", branch.Run, err)
			}
		})
	}
}

func TestExplicitDeferredDiscoveryActualRequestsAndRestart(t *testing.T) {
	server, calls, _ := integrationServer(t, "success")
	toolID := mcp.ToolID("fixture", "echo")
	args, _ := json.Marshal(map[string]any{"ids": []string{toolID}})
	p := &scriptedProvider{responses: []ModelResponse{
		{ToolCalls: []ToolCall{{ID: "search", Name: "tool_search", Arguments: `{"query":"fixture echo"}`}}},
		{ToolCalls: []ToolCall{{ID: "select", Name: "tool_select", Arguments: string(args)}}},
		{ToolCalls: []ToolCall{{ID: "execute", Name: toolID, Arguments: `{"message":"selected"}`}}},
		{Content: "done"},
	}}
	a, store := newTestAgent(t, p)
	addIntegrationMCP(t, a, mcp.Config{Name: "fixture", URL: server.URL})
	s, err := a.CreateConfigured("explicit discovery", RunConfiguration{ToolSchema: ToolSchemaConfig{Mode: "deferred"}})
	if err != nil {
		t.Fatal(err)
	}
	two := 2
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &two})
	s, err = a.Run(context.Background(), s.Run.ID)
	if err != nil || s.Run.Status != StatusPaused || calls.Load() != 0 {
		t.Fatalf("discovery checkpoint: %+v %v", s.Run, err)
	}
	restarted, err := New(store, p, "")
	if err != nil {
		t.Fatal(err)
	}
	addIntegrationMCP(t, restarted, mcp.Config{Name: "fixture", URL: server.URL})
	twenty := 20
	restarted.ConfigureBudget(BudgetOverride{MaxModelRequests: &twenty})
	s, err = restarted.Run(context.Background(), s.Run.ID)
	if err != nil || s.Run.Status != StatusCompleted || calls.Load() != 1 || len(p.requests) != 4 {
		t.Fatalf("actual execution: %+v %v", s.Run, err)
	}
	for i, req := range p.requests {
		found := false
		for _, d := range req.Tools {
			found = found || d.Function.Name == toolID
		}
		if found != (i >= 2) {
			t.Fatalf("actual request %d schemas: %+v", i, req.Tools)
		}
		m := s.Requests[i].ToolMetrics
		wantSearch, wantSelect := 0, 0
		if i >= 1 {
			wantSearch = 1
		}
		if i >= 2 {
			wantSelect = 1
		}
		if m == nil || m.SearchCalls != wantSearch || m.SelectCalls != wantSelect || m.Mode != "deferred" {
			t.Fatalf("request %d metrics: %+v", i, m)
		}
	}
}

func TestChildInheritsCurrentExecutionConfigNotStartupIDs(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "bounded", true: "legacy"}[legacy], func(t *testing.T) {
			p := &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
			a, store := newTestAgent(t, p)
			for _, name := range []string{"startup", "current"} {
				d := ToolDefinition{Type: "function"}
				d.Function.Name = name
				d.Function.Parameters = map[string]any{"type": "object"}
				a.registry.tools[name] = RegisteredTool{Definition: d, Adapter: "local"}
			}
			parent, err := a.CreateConfigured("inherit", RunConfiguration{Exec: &ExecConfig{TimeoutSeconds: 7}, ToolSchema: ToolSchemaConfig{Mode: "narrow", IDs: []string{"startup"}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = a.SelectTools(parent.Run.ID, []string{"current"}, true); err != nil {
				t.Fatal(err)
			}
			delete(a.registry.tools, "startup")
			if legacy {
				_, err = store.Update(parent.Run.ID, func(s *Snapshot) error { s.Run.Exec = nil; return nil })
				if err != nil {
					t.Fatal(err)
				}
			}
			parent, err = store.Update(parent.Run.ID, func(s *Snapshot) error {
				s.Messages = append(s.Messages, Message{ID: "branch-input", Role: "user", Content: "inherit current configuration"})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			branch, err := a.Branch(parent.Run.ID, "branch-input")
			if err != nil || (branch.Run.Exec == nil) != legacy || branch.Run.ToolSelection == nil || branch.Run.ToolSelection.IDs[0] != "current" {
				t.Fatalf("branch lost current config: %+v %v", branch.Run, err)
			}
			if _, err = a.SetPlan(parent.Run.ID, []PlanTask{{ID: "child", Goal: "inherit current selection"}}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w, err := a.DispatchTask(ctx, parent.Run.ID, "child")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); a.StopWorkers(context.Background()); a.CloseMCP() }()
			select {
			case <-p.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("child did not enter provider")
			}
			child, err := store.Load(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if (child.Run.Exec == nil) != legacy || (!legacy && child.Run.Exec.TimeoutSeconds != 7) {
				t.Fatalf("exec inheritance %+v", child.Run.Exec)
			}
			if child.Run.ToolSchema.Mode != "narrow" || child.Run.ToolSelection == nil || len(child.Run.ToolSelection.IDs) != 1 || child.Run.ToolSelection.IDs[0] != "current" {
				t.Fatalf("schema inheritance %+v", child.Run)
			}
			if len(child.Requests) != 1 {
				t.Fatalf("child request missing: %d", len(child.Requests))
			}
			found := false
			for _, d := range child.Requests[0].Tools {
				found = found || d.Function.Name == "current"
			}
			if !found {
				t.Fatal("inherited schema absent from actual child request")
			}
			cancel()
		})
	}
}

func TestInheritedConfigurationCopiesAndRejectsMissingSelection(t *testing.T) {
	a, _ := newTestAgent(t, successProvider{})
	source := Run{Exec: &ExecConfig{TimeoutSeconds: 7}, ToolSchema: ToolSchemaConfig{Mode: "deferred"}, ToolSelection: &ToolSelection{IDs: []string{}}}
	child, err := a.CreateConfigured("inherited", RunConfiguration{inheritedRun: &source})
	if err != nil {
		t.Fatal(err)
	}
	source.Exec.TimeoutSeconds = 9
	if child.Run.Exec.TimeoutSeconds != 7 {
		t.Fatal("inherited exec aliases caller memory")
	}
	source.ToolSelection = nil
	if _, err = a.CreateConfigured("missing selection", RunConfiguration{inheritedRun: &source}); err == nil {
		t.Fatal("missing inherited selection accepted")
	}
	source.ToolSelection = &ToolSelection{IDs: []string{"removed-tool"}}
	if _, err = a.CreateConfigured("removed current tool", RunConfiguration{inheritedRun: &source}); err == nil {
		t.Fatal("removed current selection silently broadened")
	}
}

func TestToolSchemaRejectsInvalidAndMissingSavedSelection(t *testing.T) {
	a, _ := newTestAgent(t, successProvider{})
	for _, c := range []ToolSchemaConfig{{Mode: "unknown"}, {Mode: "narrow"}, {Mode: "narrow", IDs: []string{"exec"}}, {Mode: "narrow", IDs: []string{"missing"}}, {Mode: "auto", IDs: []string{"x"}}, {Mode: "deferred", IDs: []string{"x"}}} {
		if _, err := a.CreateConfigured("reject", RunConfiguration{ToolSchema: c}); err == nil {
			t.Fatalf("accepted %+v", c)
		}
	}
	s := Snapshot{Run: Run{Context: DefaultContextConfig(), ToolSchema: ToolSchemaConfig{Mode: "deferred"}}}
	if _, _, err := a.modelTools(&s, ""); err == nil {
		t.Fatal("missing saved selection silently broadened")
	}
	s.Actions = []Action{{Name: "tool_search"}, {Name: "tool_select"}, {Name: "tool_search"}, {Name: "exec"}}
	m := measureToolRequest(nil, &s)
	if m.SearchCalls != 2 || m.SelectCalls != 1 {
		t.Fatalf("attempt census %+v", m)
	}
}
