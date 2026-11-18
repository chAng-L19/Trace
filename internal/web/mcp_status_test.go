package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"trace/internal/agent"
	"trace/internal/mcp"
)

func TestMCPServiceStatusProbeAndFailedRebuild(t *testing.T) {
	var calls atomic.Int32
	var fail atomic.Bool
	var callFail atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		calls.Add(1)
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.ID == 0 {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		result := any(map[string]any{})
		switch req.Method {
		case "initialize":
			if fail.Load() {
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32001, "message": "header-secret url-secret"}})
				return
			}
			result = map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}, "resources": map[string]any{}, "prompts": map[string]any{}, "secret-capability": map[string]any{"token": "header-secret"}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "check", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			if callFail.Load() {
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32002, "message": "header-secret"}})
				return
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}))
	defer upstream.Close()
	s, _ := fixture(t, nil)
	s.options.BuildAgent = func(ctx context.Context, c Config) (*agent.Agent, error) {
		a, err := agent.New(s.store, nil, "")
		if err != nil {
			return nil, err
		}
		for _, config := range c.MCP {
			if _, err = a.AddMCP(ctx, config); err != nil {
				a.CloseMCP()
				return nil, err
			}
		}
		return a, nil
	}
	c := s.config
	c.MCP = []mcp.Config{{Name: "one", URL: upstream.URL + "/?token=url-secret", Headers: map[string]string{"Authorization": "header-secret"}}, {Name: "off", URL: upstream.URL, Disabled: true}}
	request(t, s.Handler(), "PUT", "/api/config", c, 200)
	live := s.a
	before := calls.Load()
	w := request(t, s.Handler(), "GET", "/api/mcp/status", nil, 200)
	var rows []mcpServiceStatus
	if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Connection.State != "connected" || rows[0].Connection.InterfaceCount != 6 || len(rows[0].Connection.Capabilities) != 3 || rows[1].Connection.State != "disabled" {
		t.Fatalf("statuses: %s", w.Body.String())
	}
	if calls.Load() != before {
		t.Fatal("GET status performed remote I/O")
	}
	if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), upstream.URL) {
		t.Fatal("status leaked credentials or config")
	}
	request(t, s.Handler(), "POST", "/api/mcp/probe", map[string]string{"name": "off"}, 200)
	if calls.Load() != before {
		t.Fatal("disabled probe connected")
	}
	w = request(t, s.Handler(), "POST", "/api/mcp/probe", map[string]string{"name": "one"}, 200)
	var probe mcp.Status
	json.Unmarshal(w.Body.Bytes(), &probe)
	if probe.State != "connected" || probe.InterfaceCount != 6 || s.a != live {
		t.Fatalf("probe: %s", w.Body.String())
	}
	fail.Store(true)
	w = request(t, s.Handler(), "PUT", "/api/config", c, 409)
	if s.a != live || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("failed rebuild replaced Agent or leaked credentials: %s", w.Body.String())
	}
	w = request(t, s.Handler(), "GET", "/api/mcp/status", nil, 200)
	rows = nil
	json.Unmarshal(w.Body.Bytes(), &rows)
	if rows[0].Connection.State != "connected" || rows[0].LastAttempt == nil || rows[0].LastAttempt.Code != "rpc" || rows[0].LastAttempt.Stage != "initialize" {
		t.Fatalf("failure did not retain live connection separately: %s", w.Body.String())
	}
	callFail.Store(true)
	if _, err := live.Registry().Call(context.Background(), mcp.ToolID("one", "check"), json.RawMessage(`{}`)); err == nil {
		t.Fatal("runtime call should return RPC error")
	}
	w = request(t, s.Handler(), "GET", "/api/mcp/status", nil, 200)
	rows = nil
	json.Unmarshal(w.Body.Bytes(), &rows)
	if rows[0].Connection.State != "failed" || rows[0].Connection.Code != "rpc" || strings.Contains(w.Body.String(), "secret") {
		t.Fatalf("runtime status did not refresh: %s", w.Body.String())
	}
	callFail.Store(false)
	if _, err := live.Registry().Call(context.Background(), mcp.ToolID("one", "check"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	w = request(t, s.Handler(), "GET", "/api/mcp/status", nil, 200)
	rows = nil
	json.Unmarshal(w.Body.Bytes(), &rows)
	if rows[0].Connection.State != "connected" {
		t.Fatalf("successful observation did not restore status: %s", w.Body.String())
	}
	c.MCP[0].Disabled = true
	request(t, s.Handler(), "PUT", "/api/config", c, 200)
	w = request(t, s.Handler(), "GET", "/api/mcp/status", nil, 200)
	rows = nil
	json.Unmarshal(w.Body.Bytes(), &rows)
	if len(rows) != 2 || rows[0].Connection.State != "disabled" || rows[0].LastAttempt != nil {
		t.Fatalf("disabled status: %s", w.Body.String())
	}
}

func TestMCPProbeAndRebuildPreservePrimaryAndCleanupFailures(t *testing.T) {
	for _, stage := range []string{"initialize", "discovery"} {
		t.Run(stage, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusServiceUnavailable)
					w.Write([]byte("cleanup-secret"))
					return
				}
				var rpc struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
					t.Error(err)
					return
				}
				if rpc.ID == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Mcp-Session-Id", "session-secret")
				if stage == "initialize" || rpc.Method == "tools/list" {
					json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "error": map[string]any{"code": -32001, "message": "primary-secret"}})
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}}})
			}))
			defer upstream.Close()
			s, live := fixture(t, nil)
			s.config.MCP = []mcp.Config{{Name: "fixture", URL: upstream.URL + "?token=url-secret", Headers: map[string]string{"Authorization": "header-secret"}}}
			s.options.BuildAgent = func(ctx context.Context, c Config) (*agent.Agent, error) {
				a, err := agent.New(s.store, nil, "")
				if err != nil {
					return nil, err
				}
				_, err = a.AddMCP(ctx, c.MCP[0])
				return a, errors.Join(err, a.CloseMCP())
			}
			check := func(status mcp.Status) {
				t.Helper()
				if status.Stage != stage || status.Code != "rpc" || len(status.Cleanup) != 1 || status.Cleanup[0].Stage != "cleanup" || status.Cleanup[0].Code != "http" {
					t.Fatalf("combined failure lost: %+v", status)
				}
				data, _ := json.Marshal(status)
				if strings.Contains(string(data), "secret") || strings.Contains(string(data), upstream.URL) {
					t.Fatalf("status leaked connection data: %s", data)
				}
			}
			w := request(t, s.Handler(), "POST", "/api/mcp/probe", map[string]string{"name": "fixture"}, 200)
			var probe mcp.Status
			if err := json.Unmarshal(w.Body.Bytes(), &probe); err != nil {
				t.Fatal(err)
			}
			check(probe)
			next := s.config
			next.Provider.Model = "candidate"
			next.Context.WindowTokens++ // Exercise full rebuild; model-only saves retain MCP.
			w = request(t, s.Handler(), "PUT", "/api/config", next, 409)
			if s.a != live || s.config.Provider.Model == "candidate" || strings.Contains(w.Body.String(), "secret") || !strings.Contains(w.Body.String(), "清理失败") {
				t.Fatalf("failed rebuild lost retained Agent/config or sanitized cleanup: %s", w.Body.String())
			}
			w = request(t, s.Handler(), "GET", "/api/mcp/status", nil, 200)
			var rows []mcpServiceStatus
			if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].LastAttempt == nil {
				t.Fatalf("failed attempt not retained: %s", w.Body.String())
			}
			check(*rows[0].LastAttempt)
		})
	}
}

func TestMCPProbeCancellationPreservesAgent(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Method != "initialize" {
			w.WriteHeader(202)
			return
		}
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)
	s, _ := fixture(t, nil)
	s.config.MCP = []mcp.Config{{Name: "cancel", URL: upstream.URL}}
	live := s.a
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/api/mcp/probe", strings.NewReader(`{"name":"cancel"}`)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.Handler().ServeHTTP(w, r); close(done) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not reach initialize")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("probe cancellation did not finish")
	}
	var result mcp.Status
	json.Unmarshal(w.Body.Bytes(), &result)
	if result.State != "cancelled" || s.a != live {
		t.Fatalf("cancel result: %s", w.Body.String())
	}
}

func TestMCPDiscardCandidateRetainsCleanupFailure(t *testing.T) {
	for _, reason := range []string{"cancelled", "store", "save"} {
		t.Run(reason, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusServiceUnavailable)
					w.Write([]byte("cleanup-secret"))
					return
				}
				var rpc struct {
					ID int `json:"id"`
				}
				if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
					t.Error(err)
					return
				}
				if rpc.ID == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Mcp-Session-Id", "session-secret")
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{}}})
			}))
			defer upstream.Close()
			s, live := fixture(t, nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.options.BuildAgent = func(_ context.Context, c Config) (*agent.Agent, error) {
				store := s.store
				if reason == "store" {
					var err error
					store, err = agent.NewStore(t.TempDir())
					if err != nil {
						return nil, err
					}
				}
				a, err := agent.New(store, nil, "")
				if err != nil {
					return nil, err
				}
				_, err = a.AddMCP(context.Background(), mcp.Config{Name: "candidate", URL: upstream.URL + "?token=url-secret"})
				if reason == "cancelled" {
					cancel()
				}
				return a, err
			}
			if reason == "save" {
				s.options.ConfigPath = ""
			}
			s.mu.Lock()
			_, err := s.applyConfig(ctx, s.config)
			s.mu.Unlock()
			primary := map[string]string{"cancelled": "context canceled", "store": "shared store", "save": "service config path is not configured"}[reason]
			if err == nil || !strings.Contains(err.Error(), primary) || !strings.Contains(err.Error(), "候选连接清理失败") || strings.Contains(err.Error(), "secret") || s.a != live {
				t.Fatalf("discard lost primary/cleanup failure or live Agent: %v", err)
			}
			if reason == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatal("discard lost original cancellation cause")
			}
		})
	}
}
