package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func writeResponse(w http.ResponseWriter, id int64, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func initializeResult() any {
	return map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fixture", "version": "1"}}
}

func fixtureTool(name string) any {
	return map[string]any{"name": name, "description": "fixture " + name, "inputSchema": map[string]any{"type": "object"}}
}

func testHTTPClient(t *testing.T, handler func(http.ResponseWriter, *http.Request, request)) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var rpc request
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if rpc.Method == "initialize" {
			writeResponse(w, rpc.ID, initializeResult())
			return
		}
		if rpc.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		handler(w, r, rpc)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Name: "test", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c, srv
}

func TestHTTPHandshakePaginationAndResult(t *testing.T) {
	var initialized, deleted atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Error("missing configured header")
		}
		if r.Header.Get("MCP-Protocol-Version") != ProtocolVersion {
			t.Error("missing protocol header")
		}
		if r.Method == http.MethodDelete {
			if r.Header.Get("Mcp-Session-Id") != "fixture-session" {
				t.Error("missing cleanup session")
			}
			deleted.Store(true)
			w.WriteHeader(204)
			return
		}
		var rpc request
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			return
		}
		if rpc.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "fixture-session")
			writeResponse(w, rpc.ID, initializeResult())
			return
		}
		if r.Header.Get("Mcp-Session-Id") != "fixture-session" {
			t.Error("missing request session")
		}
		if rpc.Method == "notifications/initialized" {
			initialized.Store(true)
			w.WriteHeader(202)
			return
		}
		if !initialized.Load() {
			t.Error("tool request before initialized")
		}
		switch rpc.Method {
		case "tools/list":
			params := rpc.Params.(map[string]any)
			if params["cursor"] == nil {
				writeResponse(w, rpc.ID, map[string]any{"tools": []any{fixtureTool("alpha")}, "nextCursor": "page2"})
			} else {
				if params["cursor"] != "page2" {
					t.Error("wrong cursor")
				}
				writeResponse(w, rpc.ID, map[string]any{"tools": []any{fixtureTool("beta")}})
			}
		case "tools/call":
			writeResponse(w, rpc.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": "first"}, map[string]any{"type": "image", "data": "eA==", "mimeType": "image/png"}, map[string]any{"type": "text", "text": "last"}}, "isError": true, "structuredContent": map[string]any{"number": 3}})
		}
	}))
	defer srv.Close()
	c, err := Dial(context.Background(), Config{Name: "test", URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer fixture-key"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tools, err := c.Tools(context.Background())
	if err != nil || len(tools) != 2 || tools[1].ID != ToolID("test", "beta") {
		t.Fatalf("tools=%+v err=%v", tools, err)
	}
	result, err := c.Call(context.Background(), "beta", json.RawMessage(`{"value":123}`))
	if err != nil || !result.IsError || result.Text() != "first\nlast" || len(result.Content) != 3 || !strings.Contains(string(result.Raw), "image/png") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err := c.Close(); err != nil || !deleted.Load() {
		t.Fatalf("cleanup=%v deleted=%v", err, deleted.Load())
	}
}

func TestHTTPRejectsWrongResponseIDAndRepeatedCursor(t *testing.T) {
	for _, mode := range []string{"wrong-id", "cursor-loop", "missing-content"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := testHTTPClient(t, func(w http.ResponseWriter, r *http.Request, rpc request) {
				switch mode {
				case "wrong-id":
					writeResponse(w, rpc.ID+1, map[string]any{"tools": []any{}})
				case "cursor-loop":
					writeResponse(w, rpc.ID, map[string]any{"tools": []any{}, "nextCursor": "same"})
				case "missing-content":
					writeResponse(w, rpc.ID, map[string]any{})
				}
			})
			var err error
			if mode == "missing-content" {
				_, err = c.Call(context.Background(), "x", nil)
			} else {
				_, err = c.Tools(context.Background())
			}
			if err == nil {
				t.Fatal("accepted malformed protocol response")
			}
		})
	}
}

func TestHTTPContextCancellationAndClose(t *testing.T) {
	for _, closeClient := range []bool{false, true} {
		t.Run(fmt.Sprint(closeClient), func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			c, _ := testHTTPClient(t, func(w http.ResponseWriter, r *http.Request, rpc request) {
				close(started)
				<-r.Context().Done()
				close(stopped)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := c.Call(ctx, "hang", nil); done <- err }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not start")
			}
			if closeClient {
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("got %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("cancel did not return")
			}
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("HTTP request stayed alive")
			}
		})
	}
}

func TestHTTPSSEMultilineAndNotifications(t *testing.T) {
	c, _ := testHTTPClient(t, func(w http.ResponseWriter, r *http.Request, rpc request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		fmt.Fprint(w, ": keepalive\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
		fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\n")
		fmt.Fprintf(w, "data: \"id\":%d,\n", rpc.ID)
		fmt.Fprint(w, "data: \"result\":{\"content\":[{\"type\":\"text\",\"text\":\" spaced \\n value \"}]}}\n\n")
	})
	result, err := c.Call(context.Background(), "echo", nil)
	if err != nil || result.Text() != " spaced \n value " {
		t.Fatalf("text=%q err=%v", result.Text(), err)
	}
	// Splitting a JSON string across data fields inserts a newline and is invalid.
	bad := "data: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":\"a\n" + "data: b\"}\n\n"
	if _, err := readSSE(strings.NewReader(bad), 1); err == nil {
		t.Fatal("SSE data lines were concatenated without newline")
	}
}

func TestHTTPReportsCleanupFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(500)
			return
		}
		var rpc request
		_ = json.NewDecoder(r.Body).Decode(&rpc)
		if rpc.ID == 0 {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Mcp-Session-Id", "session")
		writeResponse(w, rpc.ID, initializeResult())
	}))
	defer srv.Close()
	c, err := Dial(context.Background(), Config{Name: "test", URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("cleanup=%v", err)
	}
}

func TestToolIDStableUnambiguousAndModelCompatible(t *testing.T) {
	a, b := ToolID("a__b", "c"), ToolID("a", "b__c")
	if a == b || a != ToolID("a__b", "c") || len(a) > 64 {
		t.Fatalf("ids=%q,%q", a, b)
	}
}

func TestRPCErrorAndResponseEnvelope(t *testing.T) {
	_, err := decodeResponse([]byte(`{"jsonrpc":"2.0","id":4,"error":{"code":-32602,"message":"bad arguments","data":{"field":"x"}}}`), 4)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -32602 || string(rpcErr.Data) != `{"field":"x"}` {
		t.Fatalf("RPC error=%v", err)
	}
	for _, frame := range []string{
		`{"jsonrpc":"2.0","id":"4","result":{}}`,
		`{"jsonrpc":"2.0","id":4}`,
		`{"jsonrpc":"2.0","id":4,"result":{},"error":{"code":1,"message":"x"}}`,
		`{"jsonrpc":"1.0","id":4,"result":{}}`,
		`{"jsonrpc":"2.0","id":4,"method":"sampling/createMessage"}`,
	} {
		if _, err := decodeResponse([]byte(frame), 4); err == nil {
			t.Errorf("accepted invalid frame: %s", frame)
		}
	}
}

func TestHTTPBoundedFrames(t *testing.T) {
	c, _ := testHTTPClient(t, func(w http.ResponseWriter, r *http.Request, rpc request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, strings.Repeat("x", MaxMessageBytes+2))
	})
	if _, err := c.Call(context.Background(), "large", nil); err == nil || !strings.Contains(err.Error(), "frame limit") {
		t.Fatalf("size error=%v", err)
	}
	oversizeSSE := "data: " + strings.Repeat("x", MaxMessageBytes/2) + "\n" + "data: " + strings.Repeat("y", MaxMessageBytes/2) + "\n\n"
	if _, err := readSSE(strings.NewReader(oversizeSSE), 1); err == nil || !strings.Contains(err.Error(), "frame limit") {
		t.Fatalf("SSE size error=%v", err)
	}
}

func TestQueuedCallHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	c, _ := testHTTPClient(t, func(w http.ResponseWriter, r *http.Request, rpc request) {
		close(started)
		<-r.Context().Done()
	})
	done := make(chan error, 1)
	go func() { _, err := c.Call(context.Background(), "hang", nil); done <- err }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request not started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Call(ctx, "queued", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued cancellation=%v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active request not cancelled")
	}
}
