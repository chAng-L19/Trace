package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// This fixture is a real external process, not a transport stub.
func TestMCPStdioFixture(t *testing.T) {
	mode := os.Getenv("TRACE_MCP_TEST_HELPER")
	if mode == "" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), MaxMessageBytes+1)
	initialized := false
	for scanner.Scan() {
		var rpc request
		if json.Unmarshal(scanner.Bytes(), &rpc) != nil {
			os.Exit(3)
		}
		if rpc.Method == "notifications/initialized" {
			initialized = true
			if mode == "blocked-write" {
				time.Sleep(time.Hour)
				os.Exit(0)
			}
			continue
		}
		result := any(nil)
		switch rpc.Method {
		case "initialize":
			result = initializeResult()
			if mode == "method-hang" || mode == "blocked-write" {
				result = map[string]any{"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"resources": map[string]any{}}}
			}
		case "resources/read":
			if !initialized {
				os.Exit(6)
			}
			if mode == "method-hang" {
				time.Sleep(time.Hour)
				os.Exit(7)
			}
		case "tools/list":
			if !initialized {
				os.Exit(4)
			}
			params := rpc.Params.(map[string]any)
			if params["cursor"] == nil {
				result = map[string]any{"tools": []any{fixtureTool("one")}, "nextCursor": "second"}
			} else {
				if params["cursor"] != "second" {
					os.Exit(5)
				}
				result = map[string]any{"tools": []any{fixtureTool("two")}}
			}
		case "tools/call":
			if !initialized {
				os.Exit(6)
			}
			switch mode {
			case "hang":
				time.Sleep(time.Hour)
				os.Exit(7)
			case "exit":
				os.Exit(23)
			case "oversize":
				fmt.Fprintln(os.Stdout, strings.Repeat("x", MaxMessageBytes+10))
				time.Sleep(time.Hour)
			case "wrong-id":
				rpc.ID++
			case "progress":
				params := rpc.Params.(map[string]any)
				meta := params["_meta"].(map[string]any)
				_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": map[string]any{"progressToken": meta["progressToken"], "progress": 1, "total": 2, "message": "observed"}})
			}
			params := rpc.Params.(map[string]any)
			args, _ := json.Marshal(params["arguments"])
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(args)}}, "isError": mode == "tool-error"}
		}
		// Logs go only to stderr; asynchronous notifications may precede replies.
		fmt.Fprintln(os.Stderr, "fixture diagnostic")
		fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","method":"notifications/message","params":{"level":"info"}}`)
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}
	if mode == "exit-on-close" {
		os.Exit(19)
	}
	os.Exit(0)
}

func stdioClient(t *testing.T, mode string) *Client {
	t.Helper()
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Name: "stdio-fixture", Command: bin, Args: []string{"-test.run=^TestMCPStdioFixture$"}, Env: map[string]string{"TRACE_MCP_TEST_HELPER": mode}, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil && mode != "hang" && mode != "method-hang" && mode != "blocked-write" && mode != "exit" && mode != "wrong-id" && mode != "oversize" && mode != "exit-on-close" {
			t.Error(err)
		}
	})
	return c
}

func TestStdioPaginationResultAndCleanup(t *testing.T) {
	for _, mode := range []string{"normal", "tool-error"} {
		t.Run(mode, func(t *testing.T) {
			c := stdioClient(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			tools, err := c.Tools(ctx)
			if err != nil || len(tools) != 2 || tools[1].Name != "two" {
				t.Fatalf("tools=%+v err=%v", tools, err)
			}
			result, err := c.Call(ctx, "one", json.RawMessage(`{"hello":"世界"}`))
			if err != nil || result.Text() != `{"hello":"世界"}` || result.IsError != (mode == "tool-error") {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			transport := c.transport.(*stdioTransport)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-transport.done:
			default:
				t.Fatal("MCP child process still running")
			}
			if _, err := c.Tools(ctx); err == nil {
				t.Fatal("closed client accepted request")
			}
		})
	}
}

func TestStdioWrongIDProcessExitAndBoundedFrames(t *testing.T) {
	for _, mode := range []string{"wrong-id", "exit", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			c := stdioClient(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := c.Call(ctx, "test", nil)
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected immediate protocol/process error, got %v", err)
			}
			if mode == "wrong-id" && !strings.Contains(err.Error(), "ID mismatch") {
				t.Fatal(err)
			}
		})
	}
}

func TestStdioCancellationTerminatesProcess(t *testing.T) {
	c := stdioClient(t, "hang")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := c.Call(ctx, "hang", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel=%v", err)
	}
	if err := c.Close(); err == nil {
		t.Fatal("forced cancellation must retain cleanup uncertainty")
	}
	transport := c.transport.(*stdioTransport)
	select {
	case <-transport.done:
	default:
		t.Fatal("cancel left process active")
	}
}
