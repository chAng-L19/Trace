package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"trace/internal/mcp"
)

// The parent and initial child connect normally. Later session initialization
// fails, representing an unavailable integration during manual reconciliation.
func round2UnavailableCloneFixture(t *testing.T) (*Agent, *Store, string, AgentWorker, *atomic.Int32) {
	t.Helper()
	var initializations atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
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
		if rpc.Method == "initialize" && initializations.Add(1) > 2 {
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "error": map[string]any{"code": -32001, "message": "integration unavailable during reconciliation"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": integrationReply(rpc, false)})
	}))
	t.Cleanup(remote.Close)
	a, store, id, worker := settlementFixture(t)
	a.provider = manualProvider(
		ToolCall{ID: "one", Name: "external", Arguments: `{}`},
		ToolCall{ID: "two", Name: "external", Arguments: `{}`},
	)
	t.Cleanup(func() { a.CloseMCP() })
	if _, err := a.AddMCP(context.Background(), mcp.Config{Name: "round2-unavailable", URL: remote.URL}); err != nil {
		t.Fatal(err)
	}
	// Preserve a legacy pending batch without expecting today's unknown-tool
	// dispatch to wait. The real child lifecycle still clones and closes MCP.
	unlock, err := store.AcquireRunLock(worker.RunID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.runChildOperation(context.Background(), id, worker, func(child *Agent, _ context.Context, runID string) (Snapshot, error) {
		waitTestRun(t, child, runID)
		return store.Load(runID)
	})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	parent, err := waitAllTestWorkers(a, context.Background(), id, 5)
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Load(worker.RunID)
	if err != nil || initializations.Load() != 2 || child.Run.Status != StatusWaitingTool || len(pendingToolCalls(&child)) != 2 || parent.Workers[0].CleanupStatus != "completed" {
		t.Fatalf("pending clone fixture: child=%+v workers=%+v init=%d err=%v", child.Run, parent.Workers, initializations.Load(), err)
	}
	return a, store, id, worker, &initializations
}

func TestRuntimeRound2ExtraPartialManualResultSurvivesMCPOutage(t *testing.T) {
	a, store, _, worker, initializations := round2UnavailableCloneFixture(t)
	_, submitErr := a.SubmitTool(context.Background(), worker.RunID, "one", "operator inspected first result")
	child, err := store.Load(worker.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if submitErr != nil || child.Run.Status != StatusWaitingTool || len(pendingToolCalls(&child)) != 1 || len(child.Evidence) != 1 || initializations.Load() != 2 {
		t.Fatalf("partial manual result depended on reconnect: status=%s pending=%d evidence=%d init=%d error=%v", child.Run.Status, len(pendingToolCalls(&child)), len(child.Evidence), initializations.Load(), submitErr)
	}
	if child.Evidence[0].Type != "manual_observation" || child.Evidence[0].Verification == "verified" || len(child.Actions) != 0 || len(child.Requests) != 1 {
		t.Fatal("manual reconciliation executed work or promoted supplied data")
	}
}

func TestRuntimeRound2ExtraInvalidChildResolutionDoesNotReconnect(t *testing.T) {
	for _, operation := range []string{"manual", "exec", "registered"} {
		t.Run(operation, func(t *testing.T) {
			a, store, id, worker, initializations := round2UnavailableCloneFixture(t)
			before, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			switch operation {
			case "manual":
				_, err = a.SubmitTool(context.Background(), worker.RunID, "unknown", "not a pending result")
			case "exec":
				_, err = a.ExecuteTool(context.Background(), worker.RunID, "unknown", []string{"must-not-execute"})
			case "registered":
				_, err = a.ExecuteRegistered(context.Background(), worker.RunID, "one", "read")
			}
			if err == nil {
				t.Fatal("invalid child resolution was accepted")
			}
			child, loadErr := store.Load(worker.RunID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			parent, loadErr := store.Load(id)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if child.Run.Status != StatusWaitingTool || len(pendingToolCalls(&child)) != 2 || len(child.Evidence) != 0 || len(child.Actions) != 0 || initializations.Load() != 2 || parent.Run.EventSeq != before.Run.EventSeq {
				t.Fatalf("invalid request started lifecycle: child=%s pending=%d init=%d parent_events=%d->%d error=%v", child.Run.Status, len(pendingToolCalls(&child)), initializations.Load(), before.Run.EventSeq, parent.Run.EventSeq, err)
			}
		})
	}
}
