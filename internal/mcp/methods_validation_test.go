package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type observedStdinWrite struct {
	io.WriteCloser
	entered chan struct{}
}

func (w observedStdinWrite) Write(data []byte) (int, error) {
	close(w.entered)
	return w.WriteCloser.Write(data)
}

func TestStdioBlockedWriteCancellationAndClose(t *testing.T) {
	for _, closeClient := range []bool{false, true} {
		name := "cancel"
		if closeClient {
			name = "close"
		}
		t.Run(name, func(t *testing.T) {
			c := stdioClient(t, "blocked-write")
			transport := c.transport.(*stdioTransport)
			entered := make(chan struct{})
			transport.stdin = observedStdinWrite{WriteCloser: transport.stdin, entered: entered}
			// Clean up even if the regression returns; never leak the hanging fixture.
			defer func() { transport.stop(); _ = c.Close() }()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			args, _ := json.Marshal(map[string]string{"uri": "fixture://" + strings.Repeat("x", 2<<20)})
			done := make(chan error, 1)
			go func() { _, err := c.CallMethod(ctx, "resources/read", args); done <- err }()
			// Under -race, encoding/validating the large payload can outlast a
			// short timer. Cancel only once roundTrip owns the pending pipe write;
			// cancellation before dispatch intentionally leaves the session usable.
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("request never entered stdin write")
			}
			closed := make(chan error, 1)
			if closeClient {
				go func() { closed <- c.Close() }()
			} else {
				cancel()
			}
			select {
			case err := <-done:
				want := context.Canceled
				if !errors.Is(err, want) {
					t.Fatalf("call=%v want=%v", err, want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("blocked pipe ignored cancellation")
			}
			if closeClient {
				select {
				case err := <-closed:
					if err == nil {
						t.Fatal("forced cleanup uncertainty lost")
					}
				case <-time.After(3 * time.Second):
					t.Fatal("Close stayed blocked")
				}
			} else if err := c.Close(); err == nil {
				t.Fatal("forced cleanup uncertainty lost")
			}
			select {
			case <-transport.done:
			case <-time.After(time.Second):
				t.Fatal("blocked writer process remains alive")
			}
		})
	}
}

func TestMethodResultRejectsMalformedItems(t *testing.T) {
	for _, method := range []string{"resources/list", "resources/templates/list", "resources/read", "prompts/list", "prompts/get"} {
		for _, item := range []string{`null`, `42`, `"not an object"`, `{}`} {
			t.Run(method+"/"+item, func(t *testing.T) {
				_, field, _ := methodResult(method)
				c := methodClient(t, map[string]any{"resources": map[string]any{}, "prompts": map[string]any{}}, func(w http.ResponseWriter, _ *http.Request, rpc request) {
					writeResponse(w, rpc.ID, map[string]any{field: []json.RawMessage{json.RawMessage(item)}})
				})
				result, err := c.CallMethod(context.Background(), method, nil)
				if err == nil || len(result.Raw) == 0 || len(result.Content) != 0 {
					t.Fatalf("malformed result=%+v err=%v", result, err)
				}
			})
		}
	}
	for _, tt := range []struct{ method, raw string }{
		{"resources/list", `{"name":"r","uri":null}`},
		{"resources/templates/list", `{"name":"r","uriTemplate":42}`},
		{"resources/read", `{"uri":"fixture://r"}`},
		{"resources/read", `{"uri":"fixture://r","blob":null}`},
		{"prompts/list", `{"name":"p","arguments":[{}]}`},
		{"prompts/get", `{"role":"system","content":{"type":"text","text":"x"}}`},
		{"prompts/get", `{"role":"user","content":{"type":"image","data":"AP8="}}`},
		{"prompts/get", `{"role":"assistant","content":{"type":"resource","resource":{"uri":"fixture://r"}}}`},
	} {
		if validMethodItem(tt.method, json.RawMessage(tt.raw)) {
			t.Errorf("accepted %s %s", tt.method, tt.raw)
		}
	}
}

func TestMethodResultAllowsRequiredFieldsAndExtensions(t *testing.T) {
	for _, tt := range []struct{ method, raw string }{
		{"resources/list", `{"name":"r","uri":"fixture://r","x_extra":42}`},
		{"resources/templates/list", `{"name":"r","uriTemplate":"fixture://{r}"}`},
		{"resources/read", `{"uri":"fixture://r","text":""}`},
		{"resources/read", `{"uri":"fixture://r","blob":"","_meta":{"x":42}}`},
		{"prompts/list", `{"name":"p","arguments":[{"name":"x","required":true}]}`},
		{"prompts/get", `{"role":"user","content":{"type":"text","text":""}}`},
		{"prompts/get", `{"role":"user","content":{"type":"image","data":"AP8=","mimeType":"image/png"}}`},
		{"prompts/get", `{"role":"assistant","content":{"type":"audio","data":"AP8=","mimeType":"audio/wav"}}`},
		{"prompts/get", `{"role":"assistant","content":{"type":"resource_link","name":"r","uri":"fixture://r"}}`},
		{"prompts/get", `{"role":"assistant","content":{"type":"resource","resource":{"uri":"fixture://r","text":"text"}}}`},
	} {
		if !validMethodItem(tt.method, json.RawMessage(tt.raw)) {
			t.Errorf("rejected %s %s", tt.method, tt.raw)
		}
	}
}

func TestRPCErrorRawPreservedForMethodsAndTools(t *testing.T) {
	for _, callTool := range []bool{false, true} {
		c := methodClient(t, map[string]any{"resources": map[string]any{}}, func(w http.ResponseWriter, _ *http.Request, rpc request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "error": map[string]any{"code": -32002, "message": "missing", "data": map[string]string{"detail": "REQUIRED_ERROR_DETAILS"}, "x_extension": "preserved"}})
		})
		var result Result
		var err error
		if callTool {
			result, err = c.Call(context.Background(), "read", nil)
		} else {
			result, err = c.CallMethod(context.Background(), "resources/read", nil)
		}
		var rpcError *RPCError
		if !errors.As(err, &rpcError) || !strings.Contains(string(result.Raw), "REQUIRED_ERROR_DETAILS") || !strings.Contains(string(result.Raw), "x_extension") || len(result.Content) != 0 {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	}
}

func TestMalformedToolResultRetainsRawObservation(t *testing.T) {
	for _, payload := range []string{
		`{"content":"not-an-array","x_detail":"preserve"}`,
		`{"content":null,"x_detail":"preserve"}`,
		`{"structuredContent":{"x_detail":"preserve"}}`,
	} {
		t.Run(payload, func(t *testing.T) {
			c, _ := testHTTPClient(t, func(w http.ResponseWriter, _ *http.Request, rpc request) {
				writeResponse(w, rpc.ID, json.RawMessage(payload))
			})
			result, err := c.Call(context.Background(), "fixture", nil)
			if err == nil || string(result.Raw) != payload || len(result.Content) != 0 {
				t.Fatalf("invalid result was accepted or discarded: raw=%s err=%v", result.Raw, err)
			}
		})
	}
}
