package agent

import (
	"context"
	"slices"
	"testing"

	"trace/internal/mcp"
)

func TestMCPDisabledSelectionSaveResumeAndReenable(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			if backend == "sqlite" {
				if err := MigrateStoreToSQLite(root); err != nil {
					t.Fatal(err)
				}
			}
			open := func(p Provider) *Agent {
				t.Helper()
				store, err := NewStore(root)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { store.Close() })
				a, err := New(store, p, "")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := a.CloseMCP(); err != nil {
						t.Error(err)
					}
				})
				return a
			}
			cfg := bridgeConfig(t, "http")
			// The fixture advertises an ordinary tool and a method with the same name.
			ids := []string{mcp.ToolID(cfg.Name, "resources/read"), mcp.MethodID(cfg.Name, "resources/read")}
			a := open(manualProvider(ToolCall{ID: "find", Name: "tool_search", Arguments: `{}`}))
			if _, err := a.AddMCP(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			s, err := a.CreateConfigured("projection", RunConfiguration{ToolSchema: ToolSchemaConfig{Mode: "narrow", IDs: ids[:1]}})
			if err != nil {
				t.Fatal(err)
			}
			if s.Run.ToolSelection.MCPServers[ids[0]] != cfg.Name {
				t.Fatal("narrow selection lost registered owner")
			}
			id := s.Run.ID
			if _, err = a.SelectTools(id, ids[1:], false); err != nil {
				t.Fatal(err)
			}
			one := 1
			a.ConfigureBudget(BudgetOverride{MaxModelRequests: &one})
			s, err = a.Run(ctx, id)
			if err != nil || s.Run.Status != StatusPaused || len(s.Requests) != 1 {
				t.Fatalf("pause: %s, %v", s.Run.Status, err)
			}
			if err = a.CloseMCP(); err != nil {
				t.Fatal(err)
			}
			if err = a.store.Close(); err != nil {
				t.Fatal(err)
			}

			off := open(successProvider{})
			if _, err = off.SelectTools(id, ids, true); err == nil {
				t.Fatal("missing service treated as disabled")
			}
			// No URL/command: a disabled configuration must never attempt discovery.
			if _, err = off.AddMCP(ctx, mcp.Config{Name: cfg.Name, Disabled: true}); err != nil {
				t.Fatal(err)
			}
			if found, err := off.SearchTools("", 0, 10); err != nil || found.Total != 0 {
				t.Fatalf("disabled directory: %+v %v", found, err)
			}
			saved, err := off.store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			_, _, inherited, err := off.executionConfiguration(RunConfiguration{inheritedRun: &saved.Run})
			if err != nil {
				t.Fatal(err)
			}
			clone, clients, err := off.registry.Clone(ctx)
			if err != nil || len(clients) != 0 {
				t.Fatalf("disabled clone opened sessions: %d, %v", len(clients), err)
			}
			for _, selected := range ids {
				if !clone.disabledSelection(inherited, selected) {
					t.Fatalf("cloned registry lost inherited owner for %s", selected)
				}
			}
			// Even a saved, correctly shaped ID is not proof of registration.
			unknown := mcp.ToolID(cfg.Name, "never-advertised")
			saved.Run.ToolSelection.IDs = append(saved.Run.ToolSelection.IDs, unknown)
			if _, _, err := off.modelTools(&saved, ""); err == nil {
				t.Fatal("accepted an unknown saved ID without ownership")
			}
			fresh := createTestRun(t, off)
			for _, bad := range []struct{ run, tool string }{
				{fresh, ids[0]},
				{id, mcp.MethodID(cfg.Name, "prompts/get")},
				{id, unknown},
				{id, "unknown"},
			} {
				if _, err := off.SelectTools(bad.run, []string{bad.tool}, true); err == nil {
					t.Fatalf("accepted new/unregistered selection: %+v", bad)
				}
			}
			if _, err = off.SelectTools(id, ids, true); err != nil {
				t.Fatal(err)
			}
			two := 2
			off.ConfigureBudget(BudgetOverride{MaxModelRequests: &two})
			s, err = off.Run(ctx, id)
			if err != nil || s.Run.Status != StatusCompleted || len(s.Requests) != 2 || !slices.Equal(s.Run.ToolSelection.IDs, ids) {
				t.Fatalf("resume: %+v %v", s.Run, err)
			}
			for _, tool := range s.Requests[1].Tools {
				if slices.Contains(ids, tool.Function.Name) {
					t.Fatalf("disabled schema projected: %s", tool.Function.Name)
				}
			}
			on := open(successProvider{})
			if _, err = on.AddMCP(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			s, err = on.store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			defs, _, err := on.modelTools(&s, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, selected := range ids {
				if !slices.ContainsFunc(defs, func(d ToolDefinition) bool { return d.Function.Name == selected }) {
					t.Fatalf("re-enabled schema missing: %s", selected)
				}
			}
			if _, err = off.SelectTools(id, nil, true); err != nil {
				t.Fatal(err)
			}
			if _, err = off.SelectTools(id, ids, true); err == nil {
				t.Fatal("reselected cleared disabled IDs")
			}
		})
	}
}
