package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trace/internal/mcp"
)

// These are real pi-host integration checks; install the pinned runtime before
// running them on the designated server. They do not replace pi with a Go loop.
func piMigrationContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestPiMigrationSchemaRejectsInvalidExecWithoutAction(t *testing.T) {
	for _, args := range []string{`{}`, `{"command":`} {
		t.Run(args, func(t *testing.T) {
			p := manualProvider(ToolCall{ID: "invalid-exec", Name: "exec", Arguments: args})
			a, _ := newTestAgent(t, p)
			snap, err := a.Run(piMigrationContext(t), createTestRun(t, a))
			if err != nil || snap.Run.Status != StatusCompleted {
				t.Fatalf("pi validation did not continue normally: status=%s err=%v", snap.Run.Status, err)
			}
			if len(snap.Actions) != 0 || len(snap.Requests) != 2 || len(pendingToolCalls(&snap)) != 0 {
				t.Fatalf("invalid exec crossed the action boundary: actions=%d requests=%d pending=%d", len(snap.Actions), len(snap.Requests), len(pendingToolCalls(&snap)))
			}
			if len(snap.Evidence) != 1 || snap.Evidence[0].Type != "manual_observation" || snap.Evidence[0].Verification == "verified" {
				t.Fatalf("validation was represented as execution evidence: %+v", snap.Evidence)
			}
			var result ToolResult
			if err := json.Unmarshal([]byte(snap.Evidence[0].Content), &result); err != nil || result.ExitCode != -1 || result.Error == "" {
				t.Fatalf("validation lost the existing ToolResult contract: %+v %v", result, err)
			}
			validationEvents := 0
			for _, event := range snap.Events {
				if event.Type != "tool_result" {
					continue
				}
				raw, _ := json.Marshal(event.Data)
				var details map[string]string
				if err := json.Unmarshal(raw, &details); err == nil && details["source"] == "pi_validation" {
					validationEvents++
				}
			}
			if validationEvents != 1 {
				t.Fatalf("expected one durable pi validation result, got %d", validationEvents)
			}
		})
	}
}

func TestPiMigrationReusedCallIDUnknownReturnsFreshObservation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observation.txt")
	if err := os.WriteFile(path, []byte("old-result"), 0o600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"path": path})
	p := &scriptedProvider{responses: []ModelResponse{
		{ToolCalls: []ToolCall{{ID: "reused", Name: "read", Arguments: string(args)}}},
		{ToolCalls: []ToolCall{{ID: "reused", Name: "manual_external", Arguments: `{}`}}},
		{Content: "completed after unavailable tool observation"},
	}}
	a, _ := newTestAgent(t, p)
	id := createTestRun(t, a)
	ctx := piMigrationContext(t)
	snap, err := a.Run(ctx, id)
	pending := pendingToolCalls(&snap)
	if err != nil || snap.Run.Status != StatusCompleted || len(p.requests) != 3 || len(snap.Actions) != 1 || len(pending) != 0 {
		t.Fatalf("reused ID consumed an old result: status=%s requests=%d actions=%d pending=%+v err=%v", snap.Run.Status, len(p.requests), len(snap.Actions), pending, err)
	}
	var toolMessages []string
	for _, message := range snap.Messages {
		if message.Role == "tool" && message.ToolCallID == "reused" {
			toolMessages = append(toolMessages, message.Content)
		}
	}
	if len(toolMessages) != 2 || len(snap.Evidence) != 2 || !strings.Contains(toolMessages[0], "old-result") || strings.Contains(toolMessages[1], "old-result") || !strings.Contains(toolMessages[1], "was not executed") {
		t.Fatalf("reused ID mixed tool observations: messages=%+v evidence=%d", toolMessages, len(snap.Evidence))
	}
	if snap.Actions[0].RequestID != p.requests[0].ID || snap.Actions[0].Name != "read" || snap.Actions[0].Status != "completed" || snap.Actions[0].Result == nil || !strings.Contains(snap.Actions[0].Result.Stdout, "old-result") {
		t.Fatal("unknown tool rewrote the earlier batch action")
	}
	if snap.Evidence[1].Type != "runtime_observation" || snap.Evidence[1].Verification != "unverified" || snap.Evidence[1].RequestID != p.requests[1].ID || snap.Evidence[0].RequestID == snap.Evidence[1].RequestID {
		t.Fatal("unavailable tool observation lost exact request ownership")
	}
	var projected []string
	for _, message := range p.requests[2].Messages {
		if message.Role == "tool" && message.ToolCallID == "reused" {
			projected = append(projected, message.Content)
		}
	}
	if len(projected) != 2 || projected[0] != toolMessages[0] || projected[1] != toolMessages[1] {
		t.Fatal("model did not receive both batch-specific observations in order")
	}
}

func TestPiMigrationInputRequestContinuesRemainingBatch(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "independent-work.txt")
	args, _ := json.Marshal(map[string]string{"command": `echo independent-work > "` + marker + `"`})
	p := manualProvider(
		ToolCall{ID: "input", Name: "request_input", Arguments: `{"reason":"supply operator-answer"}`},
		ToolCall{ID: "later-exec", Name: "exec", Arguments: string(args)},
	)
	a, _ := newTestAgent(t, p)
	id := createTestRun(t, a)
	ctx := piMigrationContext(t)
	snap, err := a.Run(ctx, id)
	if err != nil || snap.Run.Status != StatusCompleted || len(snap.Actions) != 2 || len(pendingToolCalls(&snap)) != 0 || len(p.requests) != 2 {
		t.Fatalf("missing input blocked independent work: status=%s actions=%d pending=%d err=%v", snap.Run.Status, len(snap.Actions), len(pendingToolCalls(&snap)), err)
	}
	if data, err := os.ReadFile(marker); err != nil || !strings.Contains(string(data), "independent-work") {
		t.Fatalf("independent batch work did not execute: %v", err)
	}
	counts := map[string]int{}
	for _, message := range p.requests[1].Messages {
		if message.Role == "tool" {
			counts[message.ToolCallID]++
			if message.ToolCallID == "input" && !strings.Contains(message.Content, "input_unavailable") {
				t.Fatal("missing input observation was lost")
			}
		}
		if message.Role == "user" && message.Content == "operator-answer" {
			t.Fatal("autonomous continuation invented an operator answer")
		}
	}
	if counts["input"] != 1 || counts["later-exec"] != 1 {
		t.Fatalf("continued batch lost pairing or replayed results: %+v", counts)
	}
}

func TestPiMigrationToolSelectionRefreshesNextRequest(t *testing.T) {
	server, calls, _ := integrationServer(t, "success")
	toolID := mcp.ToolID("fixture", "echo")
	args, _ := json.Marshal(map[string]any{"ids": []string{toolID}, "replace": true})
	p := &scriptedProvider{responses: []ModelResponse{
		{ToolCalls: []ToolCall{{ID: "select", Name: "tool_select", Arguments: string(args)}}},
		{ToolCalls: []ToolCall{{ID: "selected-call", Name: toolID, Arguments: `{"message":"selected"}`}}},
		{ToolCalls: []ToolCall{{ID: "clear", Name: "tool_select", Arguments: `{"ids":[],"replace":true}`}}},
		{Content: "complete"},
	}}
	a, _ := newTestAgent(t, p)
	addIntegrationMCP(t, a, mcp.Config{Name: "fixture", URL: server.URL})
	addLargeDirectory(a)
	snap, err := a.Run(piMigrationContext(t), createTestRun(t, a))
	if err != nil || snap.Run.Status != StatusCompleted || len(p.requests) != 4 || calls.Load() != 1 || len(snap.Actions) != 3 {
		t.Fatalf("dynamic selection failed: status=%s requests=%d remote=%d actions=%d err=%v", snap.Run.Status, len(p.requests), calls.Load(), len(snap.Actions), err)
	}
	for index, want := range []bool{false, true, true, false} {
		found := false
		for _, definition := range p.requests[index].Tools {
			found = found || definition.Function.Name == toolID
		}
		if found != want {
			t.Fatalf("request %d tool declaration stale: found=%v want=%v", index, found, want)
		}
	}
	if snap.Run.ToolSelection == nil || len(snap.Run.ToolSelection.IDs) != 0 || snap.Actions[1].RemoteName != "echo" {
		t.Fatalf("selection or remote provenance was lost: selection=%+v action=%+v", snap.Run.ToolSelection, snap.Actions[1])
	}
}

func TestPiMigrationMissingRuntimeFailsWithoutFallback(t *testing.T) {
	t.Setenv("TRACE_PI_RUNTIME", t.TempDir())
	p := &scriptedProvider{responses: []ModelResponse{{Content: "fallback must not run"}}}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	snap, err := a.Run(piMigrationContext(t), id)
	if err == nil || !strings.Contains(err.Error(), "pi runtime") || snap.Run.Status != StatusFailed {
		t.Fatalf("missing runtime was not explicit: status=%s err=%v", snap.Run.Status, err)
	}
	if len(p.requests) != 0 || len(snap.Requests) != 0 || len(snap.Actions) != 0 {
		t.Fatalf("missing framework fell back to Go execution: calls=%d requests=%d actions=%d", len(p.requests), len(snap.Requests), len(snap.Actions))
	}
	saved, err := store.Load(id)
	if err != nil || saved.Run.Status != StatusFailed || !strings.Contains(saved.Run.Error, "pi runtime") {
		t.Fatalf("runtime failure not persisted: status=%s error=%s load=%v", saved.Run.Status, saved.Run.Error, err)
	}
}
