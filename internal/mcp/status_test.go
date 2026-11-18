package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConnectionStatusProtocolFailuresAreRedacted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		reply  string
		code   string
	}{
		{"auth", 401, `secret`, "http"},
		{"rpc", 200, `{"jsonrpc":"2.0","id":1,"error":{"code":-32600,"message":"secret"}}`, "rpc"},
		{"wrong-id", 200, `{"jsonrpc":"2.0","id":2,"result":{"secret":"secret"}}`, "protocol"},
		{"malformed", 200, `secret`, "protocol"},
		{"version", 200, `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"secret","capabilities":{}}}`, "protocol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.reply))
			}))
			defer server.Close()
			_, err := Dial(context.Background(), Config{Name: "fixture", URL: server.URL + "/?key=secret"})
			if err == nil {
				t.Fatal("expected protocol failure")
			}
			var status Status
			if connection, ok := err.(*ConnectionError); ok {
				status = connection.Status
			} else {
				status = Failure("connect", err)
			}
			data, _ := json.Marshal(status)
			if status.Code != tc.code || status.State != "failed" || strings.Contains(string(data), "secret") {
				t.Fatalf("unexpected public status: %s", data)
			}
		})
	}
}

func TestInitializeAndCleanupFailuresRemainSeparate(t *testing.T) {
	for _, cleanupCode := range []int{http.StatusNoContent, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(cleanupCode), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(cleanupCode)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Mcp-Session-Id", "secret-session")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"secret"}}`)
			}))
			defer server.Close()
			_, err := Dial(context.Background(), Config{Name: "fixture", URL: server.URL + "?token=secret"})
			var connection *ConnectionError
			var rpc *RPCError
			if !errors.As(err, &connection) || !errors.As(err, &rpc) || rpc.Code != -32001 {
				t.Fatalf("primary initialization error lost: %v", err)
			}
			status := connection.Status
			if status.Stage != "initialize" || status.Code != "rpc" {
				t.Fatalf("primary status replaced: %+v", status)
			}
			if cleanupCode == http.StatusNoContent {
				if len(status.Cleanup) != 0 {
					t.Fatalf("successful cleanup reported failed: %+v", status)
				}
			} else {
				var response *HTTPError
				if len(status.Cleanup) != 1 || status.Cleanup[0].Stage != "cleanup" || status.Cleanup[0].Code != "http" || !errors.As(err, &response) || response.StatusCode != cleanupCode {
					t.Fatalf("cleanup failure lost: status=%+v err=%v", status, err)
				}
			}
			data, _ := json.Marshal(status)
			if strings.Contains(string(data), "secret") || strings.Contains(string(data), server.URL) {
				t.Fatalf("public status leaked connection data: %s", data)
			}
		})
	}
}

func TestConnectionStatusCleanupFailureIsNotClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var rpc request
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			t.Error(err)
			return
		}
		if rpc.ID == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "fixture")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-06-18","capabilities":{}}}`, rpc.ID)
	}))
	defer server.Close()
	c, err := Dial(context.Background(), Config{Name: "fixture", URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err == nil {
		t.Fatal("cleanup failure was hidden")
	}
	status := c.Status()
	if status.State != "failed" || status.Stage != "cleanup" || status.Code != "http" {
		t.Fatalf("failed cleanup advertised closed: %+v", status)
	}
}
