package agent

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

	"trace/internal/mcp"
)

func TestWorkerMCPInitializeCleanupControlsRetry(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cleanupCode int
		pause       bool
	}{{"cleaned", http.StatusNoContent, false}, {"cleanup_failed", http.StatusServiceUnavailable, false}, {"paused_cleanup_failed", http.StatusServiceUnavailable, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var initializations, childDeletes, parentDeletes atomic.Int32
			entered := make(chan struct{})
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					if r.Header.Get("Mcp-Session-Id") == "parent" {
						parentDeletes.Add(1)
						w.WriteHeader(http.StatusNoContent)
					} else {
						childDeletes.Add(1)
						w.WriteHeader(tc.cleanupCode)
					}
					return
				}
				var rpc integrationRPC
				if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
					t.Error(err)
					return
				}
				if rpc.ID == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				var result any
				switch rpc.Method {
				case "initialize":
					if initializations.Add(1) > 1 {
						w.Header().Set("Mcp-Session-Id", "child")
						if tc.pause {
							// Allocate the session before initialization is interrupted.
							w.WriteHeader(http.StatusOK)
							w.(http.Flusher).Flush()
							close(entered)
							<-r.Context().Done()
							return
						}
						json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "error": map[string]any{"code": -32001, "message": "child initialize failed"}})
						return
					}
					w.Header().Set("Mcp-Session-Id", "parent")
					result = map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}}
				case "tools/list":
					result = map[string]any{"tools": []any{map[string]any{"name": "check", "inputSchema": map[string]any{"type": "object"}}}}
				case "tools/call":
					if r.Header.Get("Mcp-Session-Id") != "parent" {
						t.Error("call lost the original parent session")
					}
					result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "parent intact"}}}
				}
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
			}))
			defer remote.Close()
			a, store := newTestAgent(t, &scriptedProvider{})
			defer a.CloseMCP()
			if _, err := a.AddMCP(context.Background(), mcp.Config{Name: "fixture", URL: remote.URL}); err != nil {
				t.Fatal(err)
			}
			id := createTestRun(t, a)
			if _, err := a.SetPlan(id, []PlanTask{{ID: "retry", Goal: "inspect target"}}); err != nil {
				t.Fatal(err)
			}
			if tc.pause {
				if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			worker, err := a.DispatchTask(context.Background(), id, "retry")
			if err != nil {
				t.Fatal(err)
			}
			if tc.pause {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("child initialize did not allocate its session")
				}
				if _, err := a.Pause(id); err != nil {
					t.Fatal(err)
				}
			}
			parent, err := waitAllTestWorkers(a, context.Background(), id, 10)
			if err != nil {
				t.Fatal(err)
			}
			child, err := store.Load(worker.RunID)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus := StatusFailed
			if tc.pause {
				wantStatus = StatusPaused
			}
			if len(child.Actions) != 0 || child.Run.Status != wantStatus || (!tc.pause && !strings.Contains(child.Run.Error, "child initialize failed")) || childDeletes.Load() != 1 || parentDeletes.Load() != 0 || initializations.Load() != 2 {
				t.Fatalf("invalid failed initialization fixture: child=%+v deletes=%d/%d initializes=%d", child.Run, childDeletes.Load(), parentDeletes.Load(), initializations.Load())
			}
			if len(parent.Workers) != 1 {
				t.Fatalf("worker missing: %+v", parent.Workers)
			}
			if tc.cleanupCode == http.StatusServiceUnavailable {
				if parent.Workers[0].CleanupStatus != "uncertain" || parent.Plan.Tasks[0].Status != "uncertain" {
					t.Fatalf("failed Dial cleanup advertised complete: worker=%+v task=%+v", parent.Workers[0], parent.Plan.Tasks[0])
				}
				if _, err := a.RetryTask(id, "retry"); err == nil {
					t.Fatal("retry accepted a leaked child session")
				}
				var connection *mcp.ConnectionError
				cleanupErr := a.StopWorkers(context.Background())
				if !errors.As(cleanupErr, &connection) || len(connection.Status.Cleanup) != 1 || connection.Status.Stage != "initialize" {
					t.Fatalf("typed cleanup observation or primary failure lost: %v", cleanupErr)
				}
				if tc.pause && (!errors.Is(cleanupErr, context.Canceled) || parent.Workers[0].Error == "") {
					t.Fatalf("pause discarded initialization cancellation: worker=%+v err=%v", parent.Workers[0], cleanupErr)
				}
			} else {
				if parent.Workers[0].CleanupStatus != "completed" || parent.Plan.Tasks[0].Status != "failed" {
					t.Fatalf("successful cleanup was marked uncertain: %+v", parent.Workers[0])
				}
				if _, err := a.RetryTask(id, "retry"); err != nil {
					t.Fatalf("settled zero-action attempt could not retry: %v", err)
				}
			}
			result, err := a.Registry().Call(context.Background(), mcp.ToolID("fixture", "check"), json.RawMessage(`{}`))
			if err != nil || result.Text() != "parent intact" {
				t.Fatalf("child cleanup changed parent connection: %+v %v", result, err)
			}
		})
	}
}
