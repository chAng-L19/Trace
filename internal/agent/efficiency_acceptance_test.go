package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"trace/internal/mcp"
)

func TestToolSchemaCostComparisonActualRequests(t *testing.T) {
	totals := map[string]int{}
	for _, mode := range []string{"auto", "deferred", "narrow"} {
		t.Run(mode, func(t *testing.T) {
			server, calls, _ := integrationServer(t, "success")
			toolID := mcp.ToolID("fixture", "echo")
			selected, _ := json.Marshal(map[string]any{"ids": []string{toolID}})
			responses := []ModelResponse{}
			if mode == "deferred" {
				responses = append(responses, ModelResponse{ToolCalls: []ToolCall{{ID: "search", Name: "tool_search", Arguments: `{"query":"fixture echo"}`}}}, ModelResponse{ToolCalls: []ToolCall{{ID: "select", Name: "tool_select", Arguments: string(selected)}}})
			}
			responses = append(responses, ModelResponse{ToolCalls: []ToolCall{{ID: "execute", Name: toolID, Arguments: `{"message":"same observed operation"}`}}}, ModelResponse{Content: "done"})
			p := &scriptedProvider{responses: responses}
			a, _ := newTestAgent(t, p)
			addIntegrationMCP(t, a, mcp.Config{Name: "fixture", URL: server.URL})
			// Deliberately declared large external directory, not guessed relevance.
			for i := 0; i < 24; i++ {
				d := ToolDefinition{Type: "function"}
				d.Function.Name = fmt.Sprintf("unselected-%02d", i)
				d.Function.Description = strings.Repeat("large explicit external schema ", 30)
				d.Function.Parameters = map[string]any{"type": "object"}
				a.registry.tools[d.Function.Name] = RegisteredTool{Definition: d, Adapter: "local"}
			}
			config := RunConfiguration{ToolSchema: ToolSchemaConfig{Mode: mode}}
			if mode == "narrow" {
				config.ToolSchema.IDs = []string{toolID}
			}
			s, err := a.CreateConfigured("same one-tool observation", config)
			if err != nil {
				t.Fatal(err)
			}
			s, err = a.Run(context.Background(), s.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			expected := 2
			if mode == "deferred" {
				expected = 4
			}
			if s.Run.Status != StatusCompleted || calls.Load() != 1 || len(s.Requests) != expected {
				t.Fatalf("changed operation or duplicate requests: %+v calls=%d", s.Run, calls.Load())
			}
			bytes := 0
			for _, request := range s.Requests {
				if request.ToolMetrics == nil {
					t.Fatal("metrics missing")
				}
				bytes += request.ToolMetrics.SchemaBytes
			}
			totals[mode] = bytes
			t.Logf("synthetic-directory mode=%s requests=%d actual_remote_calls=%d total_schema_bytes=%d; not tokenizer, provider cache or real latency", mode, len(s.Requests), calls.Load(), bytes)
		})
	}
	if totals["narrow"] >= totals["auto"] {
		t.Fatalf("explicit subset did not reduce actual schema bytes: %+v", totals)
	}
}
