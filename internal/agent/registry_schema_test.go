package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"trace/internal/localtool"
	"trace/internal/mcp"
)

const piIntegerToolSchema = `{"properties":{"ticket":{"enum":[9007199254740993],"type":"integer"}},"required":["ticket"],"type":"object"}`

func TestToolRegistryParametersPreserveLargeIntegers(t *testing.T) {
	for _, adapter := range []string{"mcp", "local"} {
		t.Run(adapter, func(t *testing.T) {
			a, _ := newTestAgent(t, MockProvider{})
			id, _ := registerPiIntegerTool(t, a, adapter, json.RawMessage(piIntegerToolSchema))
			tool, ok := a.registry.Lookup(id)
			if !ok {
				t.Fatal("registered tool missing")
			}
			raw, err := json.Marshal(tool.Definition.Function.Parameters)
			if err != nil || string(raw) != piIntegerToolSchema {
				t.Fatalf("registration changed schema: %s, err=%v", raw, err)
			}
		})
	}
}

func TestToolDefinitionParametersJSONRoundTrip(t *testing.T) {
	for _, schema := range []string{
		`{"enum":[9007199254740993,1.00000000000000000001,1e400,1e-400],"properties":{"n":{"maximum":9007199254740995,"minimum":9007199254740993,"multipleOf":0.00000000000000000001}}}`,
		"true", "false", "null",
	} {
		t.Run(schema, func(t *testing.T) {
			var definition ToolDefinition
			raw := []byte(`{"type":"function","function":{"name":"fixture","description":"exact schema","parameters":` + schema + `}}`)
			if err := json.Unmarshal(raw, &definition); err != nil {
				t.Fatal(err)
			}
			if definition.Type != "function" || definition.Function.Name != "fixture" || definition.Function.Description != "exact schema" {
				t.Fatalf("tool identity changed: %+v", definition)
			}
			encoded, err := json.Marshal(definition.Function.Parameters)
			if err != nil || string(encoded) != schema {
				t.Fatalf("archive changed parameters: %s, want %s, err=%v", encoded, schema, err)
			}
		})
	}
}

func TestToolParametersRejectTrailingJSON(t *testing.T) {
	for _, schema := range []string{"{}{}", "{}]", "{} true"} {
		t.Run(schema, func(t *testing.T) {
			registry := NewToolRegistry()
			tool := mcp.Tool{ID: "trailing-schema", Name: "fixture", InputSchema: json.RawMessage(schema)}
			if err := registry.AddMCP("fixture", &mcp.Client{}, []mcp.Tool{tool}); err == nil {
				t.Fatalf("MCP registration accepted trailing JSON: %s", schema)
			}
			if _, exists := registry.Lookup(tool.ID); exists {
				t.Fatal("invalid schema was registered")
			}
		})
	}
}

func registerPiIntegerTool(t *testing.T, a *Agent, adapter string, schema json.RawMessage) (string, <-chan string) {
	t.Helper()
	if adapter == "local" {
		manifest := localManifest(t, "echo")
		manifest.InputSchema = schema
		if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
			t.Fatal(err)
		}
		return manifest.StableID(), nil
	}
	received := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var rpc integrationRPC
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			http.Error(w, "invalid RPC", http.StatusBadRequest)
			return
		}
		if rpc.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch rpc.Method {
		case "initialize":
			result = integrationReply(rpc, false)
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "echo", "inputSchema": schema}}}
		case "tools/call":
			if rpc.Params.Name != "echo" {
				t.Errorf("unexpected tool name: %q", rpc.Params.Name)
			}
			select {
			case received <- string(rpc.Params.Arguments):
			default:
				t.Error("unexpected repeated tool calls")
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(rpc.Params.Arguments)}}}
		default:
			t.Errorf("unexpected RPC method: %s", rpc.Method)
			http.Error(w, "unexpected method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result}); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	tools, err := a.AddMCP(piMigrationContext(t), mcp.Config{Name: "integer-schema", URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.CloseMCP(); err != nil {
			t.Error(err)
		}
	})
	if len(tools) != 1 {
		t.Fatalf("discovered %d tools, want 1", len(tools))
	}
	return tools[0].ID, received
}
