package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"trace/internal/mcp"
)

type integrationRPC struct {
	ID     int64  `json:"id"`
	Method string `json:"method"`
	Params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Cursor    string          `json:"cursor"`
		Meta      struct {
			ProgressToken int64 `json:"progressToken"`
		} `json:"_meta"`
	} `json:"params"`
}

func integrationReply(rpc integrationRPC, isError bool) any {
	switch rpc.Method {
	case "initialize":
		return map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}}
	case "tools/list":
		name := "echo"
		if rpc.Params.Cursor != "" {
			name = "second"
		}
		result := map[string]any{"tools": []any{map[string]any{"name": name, "description": "integration fixture", "inputSchema": map[string]any{"type": "object"}}}}
		if rpc.Params.Cursor == "" {
			result["nextCursor"] = "page2"
		}
		return result
	default:
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": "INTEGRATED " + string(rpc.Params.Arguments)}, map[string]any{"type": "image", "data": "eA==", "mimeType": "image/png"}}, "structuredContent": map[string]any{"value": 9}, "isError": isError}
	}
}

func integrationServer(t *testing.T, mode string) (*httptest.Server, *atomic.Int32, <-chan struct{}) {
	t.Helper()
	var calls atomic.Int32
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		var rpc integrationRPC
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			return
		}
		if rpc.ID == 0 {
			w.WriteHeader(202)
			return
		}
		if rpc.Method == "tools/call" {
			calls.Add(1)
			if rpc.Params.Name != "echo" {
				t.Errorf("remote name = %q", rpc.Params.Name)
			}
			started <- struct{}{}
			if mode == "hang" {
				<-r.Context().Done()
				return
			}
			if mode == "wrong-id" {
				rpc.ID++
			}
			if mode == "progress" {
				w.Header().Set("Content-Type", "text/event-stream")
				progress, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progressToken": rpc.Params.Meta.ProgressToken, "progress": 1, "total": 2, "message": "working"}})
				_, _ = w.Write(append(append([]byte("data: "), progress...), []byte("\n\n")...))
				response, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": integrationReply(rpc, false)})
				_, _ = w.Write(append(append([]byte("data: "), response...), []byte("\n\n")...))
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": integrationReply(rpc, mode == "error")})
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, started
}

func addIntegrationMCP(t *testing.T, a *Agent, config mcp.Config) {
	t.Helper()
	tools, err := a.AddMCP(context.Background(), config)
	if err != nil || len(tools) != 2 {
		t.Fatalf("discovery: %v %+v", err, tools)
	}
	t.Cleanup(func() {
		if err := a.CloseMCP(); err != nil {
			t.Error(err)
		}
	})
}

func assertMCPPersistence(t *testing.T, a *Agent, s *Store, id string, isError bool) {
	t.Helper()
	snap, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusCompleted || len(snap.Actions) != 1 || len(snap.Evidence) != 1 {
		t.Fatalf("persistence: %+v", snap)
	}
	action := snap.Actions[0]
	if action.Name != mcp.ToolID("fixture", "echo") || action.RemoteName != "echo" || action.ServerName != "fixture" {
		t.Fatalf("identity: %+v", action)
	}
	if action.Result.MCPIsError != isError || !strings.Contains(string(action.Result.MCPResult), "structuredContent") || !strings.Contains(string(action.Result.MCPResult), "image/png") {
		t.Fatalf("raw result lost: %+v", action.Result)
	}
	ledger, err := s.EvidenceLedger(id)
	if err != nil {
		t.Fatal(err)
	}
	want := "verified"
	if isError {
		want = "unverified"
	}
	if ledger[0].Verification != want || ledger[0].RequestID != action.RequestID || ledger[0].CallID != action.CallID {
		t.Fatalf("ledger: %+v", ledger)
	}
	if len(snap.Requests) != 2 || len(snap.Requests[0].Tools) != 3+len(builtinDefinitions()) {
		t.Fatal("model registry or tool continuation missing")
	}
	last := lastConversationMessage(snap.Requests[1])
	if last.Role != "tool" || last.ToolCallID != action.CallID || last.Content != snap.Evidence[0].Content {
		t.Fatal("tool continuation missing")
	}
	if snap.Result.Status != "incomplete" {
		t.Fatal("MCP success asserted domain success")
	}
}

func TestMCPIntegrationHTTPPersistenceAndError(t *testing.T) {
	for _, mode := range []string{"success", "error", "progress"} {
		t.Run(mode, func(t *testing.T) {
			srv, calls, _ := integrationServer(t, mode)
			a, s := newTestAgent(t, manualProvider(ToolCall{ID: "mcp-call", Name: mcp.ToolID("fixture", "echo"), Arguments: `{"message":"hello"}`}))
			addIntegrationMCP(t, a, mcp.Config{Name: "fixture", URL: srv.URL})
			id := createTestRun(t, a)
			if _, err := a.Run(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			assertMCPPersistence(t, a, s, id, mode == "error")
			if mode == "progress" {
				snap, _ := s.Load(id)
				found := false
				for _, e := range snap.Events {
					if e.Type == "mcp_progress" && e.Summary == "working" {
						found = true
					}
				}
				if !found {
					t.Fatal("MCP progress missing from durable Agent event sequence")
				}
			}
			if calls.Load() != 1 {
				t.Fatal("call count")
			}
		})
	}
}

func TestMCPIntegrationUnknownAndMalformedDoNotMutate(t *testing.T) {
	srv, calls, _ := integrationServer(t, "success")
	toolID := mcp.ToolID("fixture", "echo")
	a, s := newTestAgent(t, manualProvider(ToolCall{ID: "pending", Name: toolID, Arguments: `{}`}))
	id := createTestRun(t, a)
	waitTestRun(t, a, id) // no config: keep pending rather than guessing a tool
	addIntegrationMCP(t, a, mcp.Config{Name: "fixture", URL: srv.URL})
	before, _ := s.Load(id)
	beforeJSON, _ := json.Marshal(before)
	for _, item := range []struct{ call, tool string }{{"pending", "unknown"}, {"missing", toolID}} {
		if _, err := a.ExecuteMCP(context.Background(), id, item.call, item.tool); err == nil {
			t.Fatal("accepted invalid identity")
		}
		after, _ := s.Load(id)
		afterJSON, _ := json.Marshal(after)
		if !bytes.Equal(beforeJSON, afterJSON) {
			t.Fatal("invalid identity mutated state")
		}
	}
	if _, err := a.ExecuteMCP(context.Background(), id, "pending", toolID); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("explicit call missing")
	}
	for _, args := range []string{`null`, `[]`, `bad`} {
		b, store := newTestAgent(t, manualProvider(ToolCall{ID: "bad", Name: toolID, Arguments: args}))
		run := createTestRun(t, b)
		waitTestRun(t, b, run)
		addIntegrationMCP(t, b, mcp.Config{Name: "fixture", URL: srv.URL})
		before, _ := store.Load(run)
		x, _ := json.Marshal(before)
		if _, err := b.ExecuteMCP(context.Background(), run, "bad", toolID); err == nil {
			t.Fatal("accepted invalid JSON object")
		}
		after, _ := store.Load(run)
		y, _ := json.Marshal(after)
		if !bytes.Equal(x, y) {
			t.Fatal("invalid JSON mutated run")
		}
	}
}

func TestMCPIntegrationUncertainNeverReplays(t *testing.T) {
	for _, mode := range []string{"hang", "wrong-id"} {
		t.Run(mode, func(t *testing.T) {
			srv, calls, started := integrationServer(t, mode)
			provider := manualProvider(ToolCall{ID: "one", Name: mcp.ToolID("fixture", "echo"), Arguments: `{}`})
			a, s := newTestAgent(t, provider)
			config := mcp.Config{Name: "fixture", URL: srv.URL}
			if mode == "hang" {
				// Time out the remote call without cancelling the agent's own
				// context, so the uncertain observation can reach its next turn.
				config.HTTPClient = &http.Client{Timeout: time.Second}
			}
			addIntegrationMCP(t, a, config)
			id := createTestRun(t, a)
			done := make(chan error, 1)
			go func() { _, err := a.Run(context.Background(), id); done <- err }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("call not started")
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("call not cancelled")
			}
			snap, err := s.Load(id)
			if err != nil || snap.Run.Status != StatusCompleted || len(pendingToolCalls(&snap)) != 0 || len(snap.Evidence) != 1 || len(snap.Actions) != 1 || snap.Actions[0].Status != "uncertain" || snap.Actions[0].Result == nil || !snap.Actions[0].Result.OutcomeUncertain || snap.Evidence[0].Verification != "unverified" || snap.Result.Status == "success" || len(provider.requests) != 2 {
				t.Fatalf("uncertain observation did not continue without promotion: status=%s actions=%d evidence=%d requests=%d err=%v", snap.Run.Status, len(snap.Actions), len(snap.Evidence), len(provider.requests), err)
			}
			before := valueHash([]any{snap.Actions, snap.Evidence})
			for i := 0; i < 2; i++ {
				if _, err := a.Run(context.Background(), id); err == nil {
					t.Fatal("completed run reopened an uncertain call")
				}
			}
			if _, err := a.SubmitTool(context.Background(), id, "one", "operator inspected uncertain outcome"); err == nil {
				t.Fatal("settled observation accepted a duplicate manual result")
			}
			final, err := s.Load(id)
			if err != nil || calls.Load() != 1 || len(provider.requests) != 2 || valueHash([]any{final.Actions, final.Evidence}) != before {
				t.Fatal("uncertain action replayed or its observation changed", err)
			}
		})
	}
}

func TestAgentMCPStdioFixture(t *testing.T) {
	if os.Getenv("TRACE_AGENT_MCP_FIXTURE") == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var rpc integrationRPC
		if json.Unmarshal(scanner.Bytes(), &rpc) != nil {
			os.Exit(2)
		}
		if rpc.ID == 0 {
			continue
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": integrationReply(rpc, false)})
	}
	os.Exit(0)
}

func TestMCPIntegrationStdioPersistence(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	a, s := newTestAgent(t, manualProvider(ToolCall{ID: "stdio", Name: mcp.ToolID("fixture", "echo"), Arguments: `{"source":"stdio"}`}))
	addIntegrationMCP(t, a, mcp.Config{Name: "fixture", Command: binary, Args: []string{"-test.run=^TestAgentMCPStdioFixture$"}, Env: map[string]string{"TRACE_AGENT_MCP_FIXTURE": "1"}, Dir: t.TempDir()})
	id := createTestRun(t, a)
	if _, err := a.Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	assertMCPPersistence(t, a, s, id, false)
}

func TestRegistryOrderAndDuplicateAtomicity(t *testing.T) {
	srv, _, _ := integrationServer(t, "success")
	a, _ := newTestAgent(t, MockProvider{})
	addIntegrationMCP(t, a, mcp.Config{Name: "fixture", URL: srv.URL})
	before, _ := json.Marshal(a.registry.Definitions())
	for i := 0; i < 20; i++ {
		after, _ := json.Marshal(a.registry.Definitions())
		if !bytes.Equal(before, after) {
			t.Fatal("unstable registry ordering")
		}
	}
	if _, err := a.AddMCP(context.Background(), mcp.Config{Name: "fixture", URL: srv.URL}); err == nil {
		t.Fatal("duplicate accepted")
	}
	after, _ := json.Marshal(a.registry.Definitions())
	if !bytes.Equal(before, after) {
		t.Fatal("duplicate altered registry")
	}
}

func TestMCPIntegrationBudgetBeforeRemoteCall(t *testing.T) {
	srv, calls, started := integrationServer(t, "success")
	toolID := mcp.ToolID("fixture", "echo")
	a, _ := newTestAgent(t, manualProvider(ToolCall{ID: "first", Name: toolID, Arguments: `{}`}, ToolCall{ID: "second", Name: toolID, Arguments: `{}`}))
	addIntegrationMCP(t, a, mcp.Config{Name: "fixture", URL: srv.URL})
	limit := 1
	a.ConfigureBudget(BudgetOverride{MaxActions: &limit})
	id := createTestRun(t, a)
	snap, err := a.Run(context.Background(), id)
	if err != nil || snap.Run.Status != StatusPaused || snap.Run.PauseReason != "tool action budget exhausted" || len(snap.Actions) != 1 || calls.Load() != 1 {
		t.Fatalf("budget: status=%s reason=%s calls=%d err=%v", snap.Run.Status, snap.Run.PauseReason, calls.Load(), err)
	}
	<-started
	limit = 2
	snap, err = a.Run(context.Background(), id)
	if err != nil || snap.Run.Status != StatusCompleted || len(snap.Actions) != 2 || calls.Load() != 2 {
		t.Fatalf("resume budget: %+v %v", snap.Run, err)
	}
}

func TestMCPIntegrationRegistrationCleanupFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(500)
			return
		}
		var rpc integrationRPC
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		if rpc.ID == 0 {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Mcp-Session-Id", "cleanup-test")
		w.Header().Set("Content-Type", "application/json")
		result := integrationReply(rpc, false)
		if rpc.Method == "tools/list" {
			result = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	defer srv.Close()
	a, _ := newTestAgent(t, MockProvider{})
	_, err := a.AddMCP(context.Background(), mcp.Config{Name: "fixture", URL: srv.URL})
	if err == nil || !strings.Contains(err.Error(), "omitted tools") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("cleanup error lost: %v", err)
	}
	if len(a.registry.Definitions()) != 1+len(builtinDefinitions()) {
		t.Fatal("partial registration persisted")
	}
}
