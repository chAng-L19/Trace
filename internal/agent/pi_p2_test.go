package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestP2PiParallelReadBatchOrderedAndRecoverable(t *testing.T) {
	dir := t.TempDir()
	calls := []ToolCall{}
	for _, name := range []string{"first", "second", "third", "fourth", "fifth"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]string{"path": path})
		calls = append(calls, ToolCall{ID: name, Name: "read", Arguments: string(args)})
	}
	a, _ := newTestAgent(t, manualProvider(calls...))
	id := createTestRun(t, a)
	snap, err := a.Run(piMigrationContext(t), id)
	if err != nil || snap.Run.Status != StatusCompleted || len(snap.Actions) != 5 || len(snap.Evidence) != 5 {
		t.Fatalf("status=%s actions=%d evidence=%d err=%v", snap.Run.Status, len(snap.Actions), len(snap.Evidence), err)
	}
	index := 0
	for _, m := range snap.Messages {
		if m.Role == "tool" {
			if m.ToolCallID != calls[index].ID {
				t.Fatalf("out of order: %s want %s", m.ToolCallID, calls[index].ID)
			}
			index++
		}
	}
	if index != 5 {
		t.Fatalf("missing tool messages: %d", index)
	}
	for _, action := range snap.Actions {
		if action.Status != "completed" || action.Result == nil {
			t.Fatalf("unsettled action: %+v", action)
		}
	}
	// Persist a completed execution before pi's result emission, then remove the
	// source. Recovery must settle the saved CAS/result, not repeat the read.
	b, _ := newTestAgent(t, &scriptedProvider{responses: []ModelResponse{{Content: "done"}}})
	bid := createTestRun(t, b)
	_, err = b.store.Update(bid, func(s *Snapshot) error {
		s.Requests = append(s.Requests, ModelRequest{ID: "original", Tools: b.registry.Definitions()})
		s.Messages = append(s.Messages, Message{Role: "assistant", RequestID: "original", ToolCalls: calls[:1]})
		s.Run.Status = StatusWaitingTool
		s.Actions = append(s.Actions, Action{RequestID: "original", CallID: calls[0].ID, Name: "read", Arguments: calls[0].Arguments, Status: "result_ready", Result: &ToolResult{Stdout: "saved-result", ExitCode: 0}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(dir, "first")); err != nil {
		t.Fatal(err)
	}
	snap, err = b.Run(piMigrationContext(t), bid)
	if err != nil || snap.Run.Status != StatusCompleted || len(snap.Actions) != 1 || len(snap.Evidence) != 1 || snap.Actions[0].Result.Stdout != "saved-result" {
		t.Fatalf("recovery repeated/lost action: %+v %v", snap.Run, err)
	}
}

func TestP2ReadyResultRejectsOutOfOrderAndStopped(t *testing.T) {
	s := Snapshot{Run: Run{Status: StatusWaitingTool}, Requests: []ModelRequest{{ID: "r"}}, Messages: []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "a"}, {ID: "b"}}}}, Actions: []Action{{RequestID: "r", CallID: "b", Status: "result_ready", Result: &ToolResult{ExitCode: 0}}}}
	if _, err := settleReadyPiResult(&s, "b"); err == nil {
		t.Fatal("out-of-order result accepted")
	}
	if len(s.Evidence) > 0 {
		t.Fatal("out-of-order result created evidence")
	}
}

func TestP2ReadyResultIdentityAndArtifactFailureRemainPending(t *testing.T) {
	s := Snapshot{Run: Run{ID: "r", Status: StatusWaitingTool}, Requests: []ModelRequest{{ID: "r-1"}}, Actions: []Action{{RequestID: "r-1", CallID: "c", Name: "read", Arguments: "{}", Status: "result_ready", Result: &ToolResult{ExitCode: 0}}}}
	if err := validateReadyPiAction(&s, ToolCall{ID: "c", Name: "read", Arguments: "{\"path\":\"changed\"}"}); err == nil {
		t.Fatal("changed arguments accepted")
	}
	s.artifactRoot = t.TempDir()
	s.Actions[0].Result.Artifacts = []ArtifactRef{{Hash: string(make([]byte, 64)), Kind: "stdout", Bytes: 1}}
	if err := validateReadyPiAction(&s, ToolCall{ID: "c", Name: "read", Arguments: "{}"}); err == nil {
		t.Fatal("missing artifact accepted")
	}
	if s.Actions[0].Status != "result_ready" || len(s.Evidence) > 0 {
		t.Fatal("invalid saved result was settled")
	}
}
