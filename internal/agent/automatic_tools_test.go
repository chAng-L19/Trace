package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestAutomaticTimeoutReturnsOriginalOutputWithoutReplay(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "once")
	args, _ := json.Marshal(map[string]any{"command": "printf x >> " + marker + "; printf partial; sleep 30", "timeout_seconds": 1})
	p := manualProvider(ToolCall{ID: "timed", Name: "exec", Arguments: string(args)})
	a, _ := newTestAgent(t, p)
	s, err := a.Run(piMigrationContext(t), createTestRun(t, a))
	if err != nil || s.Run.Status != StatusCompleted || len(p.requests) != 2 || len(s.Actions) != 1 || s.Actions[0].Status != "uncertain" || s.Actions[0].Result == nil || !s.Actions[0].Result.OutcomeUncertain {
		t.Fatalf("timeout did not return to pi: %+v %v", s.Run, err)
	}
	raw, err := os.ReadFile(marker)
	if err != nil || string(raw) != "x" || !strings.Contains(lastConversationMessage(p.requests[1]).Content, "partial") || s.Result.Status == "success" {
		t.Fatal("timeout replayed, hid output or accepted an unknown result", err)
	}
}

func TestAutomaticRecoveryCorruptReadyResultContinuesWithoutConsumingArtifact(t *testing.T) {
	p := &scriptedProvider{responses: []ModelResponse{{Content: "keep unknown and continue"}}}
	a, store := newTestAgent(t, p)
	call := ToolCall{ID: "corrupt", Name: "read", Arguments: `{"path":"must-not-read"}`}
	id := seedPiBatch(t, a, store, []ToolCall{call}, a.registry.Definitions())
	before, err := store.Update(id, func(s *Snapshot) error {
		s.Actions = []Action{{RequestID: currentRequestID(s), CallID: call.ID, Name: call.Name, Arguments: call.Arguments, Adapter: "builtin", Status: "result_ready", Result: &ToolResult{Stdout: "untrusted retained preview", Artifacts: []ArtifactRef{{Hash: strings.Repeat("a", 64), Kind: "stdout", Bytes: 50}}}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ = store.Load(id)
	s, err := a.Run(piMigrationContext(t), id)
	if err != nil || s.Run.Status != StatusCompleted || len(p.requests) != 1 || !reflect.DeepEqual(before.Actions, s.Actions) || len(s.Evidence) != 1 || s.Evidence[0].Type != "runtime_observation" || s.Evidence[0].Verification != "unverified" || !strings.Contains(s.Evidence[0].Content, "artifact verification failed") || strings.Contains(s.Evidence[0].Content, "untrusted retained preview") {
		t.Fatalf("corrupt result crossed execution boundary: %+v %v", s, err)
	}
}

func TestAutomaticAttachmentFailureDoesNotResendArchivedRequest(t *testing.T) {
	p := &scriptedProvider{responses: []ModelResponse{{Content: "never call"}}}
	a, store := newTestAgent(t, p)
	input, err := store.PutInputAttachment("input.txt", strings.NewReader("original"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.Create("inspect", input)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(s.Run.ID, func(s *Snapshot) error { s.Requests = []ModelRequest{{ID: "archived"}}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(store.root, "artifacts", input.Hash)); err != nil {
		t.Fatal(err)
	}
	s, err = a.Run(context.Background(), s.Run.ID)
	if err == nil || s.Run.Status != StatusFailed || len(s.Requests) != 1 || len(p.requests) != 0 || s.Run.PauseReason != "" {
		t.Fatalf("bad attachment resent an archived request: %+v %v", s.Run, err)
	}
}

func TestAutomaticParallelValidationKeepsResultOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte("observed once"), 0600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"path": path})
	bad := strings.TrimSuffix(string(args), "}") + `,"ticket":9007199254740993}`
	p := manualProvider(ToolCall{ID: "first", Name: "read", Arguments: string(args)}, ToolCall{ID: "second", Name: "read", Arguments: bad})
	a, _ := newTestAgent(t, p)
	s, err := a.Run(piMigrationContext(t), createTestRun(t, a))
	if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 1 || s.Actions[0].CallID != "first" || len(p.requests) != 2 {
		t.Fatalf("parallel validation failed: %+v %v", s.Run, err)
	}
	var order []string
	for _, m := range s.Messages {
		if m.Role == "tool" {
			order = append(order, m.ToolCallID)
		}
	}
	if !reflect.DeepEqual(order, []string{"first", "second"}) {
		t.Fatalf("tool results reordered: %v", order)
	}
}
