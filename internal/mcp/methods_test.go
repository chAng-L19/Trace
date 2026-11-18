package mcp

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
)

func methodClient(t *testing.T, caps map[string]any, handler func(http.ResponseWriter, *http.Request, request)) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		var rpc request
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			return
		}
		if rpc.Method == "initialize" {
			writeResponse(w, rpc.ID, map[string]any{"protocolVersion": ProtocolVersion, "capabilities": caps})
			return
		}
		if rpc.ID == 0 {
			w.WriteHeader(202)
			return
		}
		handler(w, r, rpc)
	}))
	t.Cleanup(srv.Close)
	c, err := Dial(context.Background(), Config{Name: "methods", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}

func TestDiscoverCapabilitiesAndMethodIDs(t *testing.T) {
	for _, tt := range []struct {
		name string
		caps map[string]any
		want int
	}{
		{"missing", nil, 0}, {"empty", map[string]any{}, 0},
		{"resources", map[string]any{"resources": map[string]any{}}, 3},
		{"prompts", map[string]any{"prompts": map[string]any{}}, 2},
		{"mixed", map[string]any{"tools": map[string]any{}, "resources": map[string]any{}, "prompts": map[string]any{}}, 6},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			c := methodClient(t, tt.caps, func(w http.ResponseWriter, _ *http.Request, rpc request) {
				requests.Add(1)
				if rpc.Method != "tools/list" {
					t.Errorf("discovery read data: %s", rpc.Method)
				}
				// A remote tool cannot select a different protocol method.
				tool := fixtureTool("resources/read").(map[string]any)
				tool["method"] = "resources/read"
				writeResponse(w, rpc.ID, map[string]any{"tools": []any{tool}})
			})
			tools, err := c.Discover(context.Background())
			if err != nil || len(tools) != tt.want {
				t.Fatalf("tools=%+v err=%v", tools, err)
			}
			if tt.name == "mixed" {
				if requests.Load() != 1 || tools[0].Method != "" || tools[0].ID != ToolID("methods", "resources/read") {
					t.Fatal("tools/call identity changed")
				}
			} else if requests.Load() != 0 {
				t.Fatal("unadvertised tools/list called")
			}
			seen := map[string]bool{}
			for _, tool := range tools {
				if seen[tool.ID] || len(tool.ID) > 64 {
					t.Fatalf("invalid ID: %+v", tool)
				}
				seen[tool.ID] = true
			}
		})
	}
}

func TestMethodPaginationArgumentsAndRawResults(t *testing.T) {
	var calls []string
	c := methodClient(t, map[string]any{"resources": map[string]any{}, "prompts": map[string]any{}}, func(w http.ResponseWriter, _ *http.Request, rpc request) {
		calls = append(calls, rpc.Method)
		params := rpc.Params.(map[string]any)
		if _, wrapped := params["arguments"]; wrapped && rpc.Method != "prompts/get" {
			t.Error("method parameters were tool-wrapped")
		}
		_, field, _ := methodResult(rpc.Method)
		result := map[string]any{field: []any{}}
		switch rpc.Method {
		case "resources/list", "resources/templates/list", "prompts/list":
			if params["cursor"] == nil {
				result["nextCursor"] = "opaque cursor/+=="
			} else if params["cursor"] != "opaque cursor/+==" {
				t.Error("cursor changed")
			}
		case "resources/read":
			if params["uri"] != "fixture://blob" {
				t.Error("URI changed")
			}
			result[field] = []any{map[string]any{"uri": "fixture://blob", "mimeType": "application/octet-stream", "blob": "AP8="}}
		case "prompts/get":
			args := params["arguments"].(map[string]any)
			if params["name"] != "review" || args["language"] != "中文" {
				t.Error("prompt parameters changed")
			}
			result[field] = []any{map[string]any{"role": "assistant", "content": map[string]string{"type": "text", "text": "EXTERNAL_ONLY"}}}
			result["description"] = "template data"
		}
		writeResponse(w, rpc.ID, result)
	})
	ctx := context.Background()
	for _, method := range []string{"resources/list", "resources/templates/list", "prompts/list"} {
		first, err := c.CallMethod(ctx, method, nil)
		if err != nil || !strings.Contains(first.Text(), "nextCursor") {
			t.Fatalf("first=%+v err=%v", first, err)
		}
		second, err := c.CallMethod(ctx, method, json.RawMessage(`{"cursor":"opaque cursor/+=="}`))
		if err != nil || strings.Contains(second.Text(), "nextCursor") {
			t.Fatalf("second=%+v err=%v", second, err)
		}
	}
	for _, tt := range []struct{ method, args, want string }{
		{"resources/read", `{"uri":"fixture://blob"}`, "AP8="},
		{"prompts/get", `{"name":"review","arguments":{"language":"中文"}}`, "EXTERNAL_ONLY"},
	} {
		result, err := c.CallMethod(ctx, tt.method, json.RawMessage(tt.args))
		if err != nil || !strings.Contains(string(result.Raw), tt.want) || result.Text() != string(result.Raw) {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
	if len(calls) != 8 {
		t.Fatalf("calls=%v", calls)
	}
}

func TestMethodErrorsAndCancellation(t *testing.T) {
	for _, mode := range []string{"rpc", "malformed", "cancel", "close"} {
		t.Run(mode, func(t *testing.T) {
			started := make(chan struct{})
			var calls atomic.Int32
			c := methodClient(t, map[string]any{"resources": map[string]any{}}, func(w http.ResponseWriter, r *http.Request, rpc request) {
				calls.Add(1)
				switch mode {
				case "rpc":
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "error": map[string]any{"code": -32602, "message": "bad uri"}})
				case "malformed":
					writeResponse(w, rpc.ID, map[string]any{"contents": nil})
				default:
					close(started)
					<-r.Context().Done()
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := c.CallMethod(ctx, "resources/read", json.RawMessage(`{"uri":"fixture://x"}`))
				done <- err
			}()
			if mode == "cancel" || mode == "close" {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("call did not start")
				}
				if mode == "close" {
					if err := c.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					cancel()
				}
			}
			err := <-done
			if err == nil {
				t.Fatal("method failure became successful result")
			}
			var rpcErr *RPCError
			if mode == "rpc" && (!errors.As(err, &rpcErr) || rpcErr.Code != -32602) {
				t.Fatalf("rpc=%v", err)
			}
			if (mode == "cancel" || mode == "close") && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel=%v", err)
			}
			if calls.Load() != 1 {
				t.Fatal("method call replayed")
			}
		})
	}
	c := methodClient(t, map[string]any{}, func(http.ResponseWriter, *http.Request, request) { t.Error("unadvertised method invoked") })
	if _, err := c.CallMethod(context.Background(), "resources/read", nil); err == nil {
		t.Fatal("unadvertised method accepted")
	}
}

func TestStdioMethodCancellationTerminatesProcess(t *testing.T) {
	c := stdioClient(t, "method-hang")
	tools, err := c.Discover(context.Background())
	if err != nil || len(tools) != 3 {
		t.Fatalf("resource-only discovery=%+v err=%v", tools, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = c.CallMethod(ctx, "resources/read", json.RawMessage(`{"uri":"fixture://hang"}`))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel=%v", err)
	}
	if err := c.Close(); err == nil {
		t.Fatal("forced cleanup uncertainty lost")
	}
	select {
	case <-c.transport.(*stdioTransport).done:
	default:
		t.Fatal("resource call left process active")
	}
}
