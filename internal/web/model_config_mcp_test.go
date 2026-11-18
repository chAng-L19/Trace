package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"trace/internal/agent"
	"trace/internal/mcp"
)

func TestModelConfigurationDoesNotReconnectOrCloseMCP(t *testing.T) {
	var calls, closed atomic.Int32
	var rejectInitialize atomic.Bool
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			closed.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		calls.Add(1)
		var rpc struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if rpc.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if rpc.Method == "initialize" && rejectInitialize.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		result := any(map[string]any{})
		switch rpc.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "model-config-fixture")
			result = map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	t.Cleanup(remote.Close)
	s, original := modelConfigFixture(t)
	s.config.MCP = []mcp.Config{{Name: "retained", URL: remote.URL}}
	if _, err := original.AddMCP(context.Background(), s.config.MCP[0]); err != nil {
		t.Fatal(err)
	}
	builds := 0
	s.options.BuildAgent = func(ctx context.Context, c Config) (*agent.Agent, error) {
		builds++
		a, err := agent.New(s.store, nil, "")
		if err != nil {
			return nil, err
		}
		for _, config := range c.MCP {
			if _, err = a.AddMCP(ctx, config); err != nil {
				_ = a.CloseMCP()
				return nil, err
			}
		}
		return a, nil
	}
	rejectInitialize.Store(true)
	before := calls.Load()
	next := redactConfig(s.config)
	next.Provider.APIKey = "rotated-default-fixture"
	request(t, s.Handler(), "PUT", "/api/config", next, http.StatusOK)
	if builds != 0 || calls.Load() != before || closed.Load() != 0 || s.a.Registry() != original.Registry() {
		t.Fatal("key-only save reconnected or closed existing MCP")
	}
	next = redactConfig(s.config)
	next.ActiveProvider = "b"
	request(t, s.Handler(), "PUT", "/api/config", next, http.StatusOK)
	if builds != 0 || calls.Load() != before || closed.Load() != 0 {
		t.Fatal("profile switch reconnected or closed existing MCP")
	}
	published := s.a
	next = redactConfig(s.config)
	next.Context.WindowTokens++
	request(t, s.Handler(), "PUT", "/api/config", next, http.StatusConflict)
	if builds != 1 || calls.Load() <= before || s.a != published || closed.Load() != 0 {
		t.Fatal("non-model update lost full rebuild failure boundary")
	}
}
