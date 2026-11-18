package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type d2InputProvider struct {
	d2EvidenceProvider
	entered, release chan struct{}
}

func (p *d2InputProvider) Complete(ctx context.Context, request ModelRequest) (ModelResponse, error) {
	hasRead := false
	for _, m := range request.Messages {
		hasRead = hasRead || m.Role == "tool" && m.ToolCallID == "d2-read-1"
	}
	if !hasRead {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return ModelResponse{}, ctx.Err()
		}
	} else {
		raw, _ := json.Marshal(request.Messages)
		for _, required := range []string{"supplement for active task", "return misroute to parent", "continue original scope", "kind=resolved"} {
			if !strings.Contains(string(raw), required) {
				return ModelResponse{}, fmt.Errorf("next request lost %q", required)
			}
		}
		if strings.Contains(string(raw), "sibling-only-input") {
			return ModelResponse{}, fmt.Errorf("sibling input broadcast to active child")
		}
	}
	return p.d2EvidenceProvider.Complete(ctx, request)
}

func TestD2RunningInputHandbackAndNoBroadcast(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proof.txt")
	if err := os.WriteFile(path, []byte("d2-proof"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &d2InputProvider{d2EvidenceProvider: d2EvidenceProvider{path: path}, entered: make(chan struct{}), release: make(chan struct{})}
	a, store := newTestAgent(t, p)
	t.Cleanup(func() { a.StopWorkers(context.Background()); a.CloseMCP() })
	a.SetRequireVerifiedCompletion(true)
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "active", Goal: "read assigned proof"}}); err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), id, "active")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never entered")
	}
	sibling, err := a.Create("sibling unrelated scope")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(sibling.Run.ID, func(s *Snapshot) error { s.Run.ParentRunID, s.Run.ParentTaskID = id, "sibling-task"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(id, func(s *Snapshot) error {
		s.Workers = append(s.Workers, AgentWorker{ID: "sibling", TaskID: "sibling-task", RunID: sibling.Run.ID, Status: "completed", CleanupStatus: "completed"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	send := func(run, to, kind, reply, content string) WorkerMessage {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"to": to, "kind": kind, "reply_to": reply, "content": content})
		value, err := a.builtin(context.Background(), run, ToolCall{Name: "worker_message", Arguments: string(raw)})
		if err != nil {
			t.Fatal(err)
		}
		result, ok := value.(map[string]any)
		if !ok || result["status"] != "accepted" || result["processed"] != false || result["created"] != true {
			t.Fatalf("invalid durable message receipt: %+v", value)
		}
		receipt, ok := result["receipt"].(WorkerMessage)
		if !ok || receipt.ID == "" || receipt.To != to || receipt.Kind != kind || receipt.ReplyTo != reply || receipt.Content != content {
			t.Fatalf("message receipt changed routing or content: %+v", result)
		}
		return receipt
	}
	send(id, w.ID, "report", "", "supplement for active task")
	send(id, "sibling", "report", "", "sibling-only-input")
	// Explicit handback is a correlated question, not keyword-based broadcast.
	help := send(w.RunID, "parent", "help", "", "return misroute to parent: this additional request belongs to sibling")
	if err = a.DeliverWorkerMessages(id); err != nil {
		t.Fatal(err)
	}
	parent, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range parent.Messages {
		found = found || strings.Contains(m.Content, help.ID)
	}
	if !found {
		t.Fatal("help not delivered while worker provider was active")
	}
	send(id, w.ID, "reply", help.ID, "continue original scope; return misroute to parent acknowledged")
	send(id, w.ID, "resolved", help.ID, "routing decision recorded")
	close(p.release)
	if _, err = waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
		t.Fatal(err)
	}
	child, err := store.Load(w.RunID)
	if err != nil || child.Result.Status != "success" || len(child.Actions) != 1 || len(child.Requests) != 2 {
		t.Fatalf("child result=%+v actions=%d requests=%d err=%v", child.Result, len(child.Actions), len(child.Requests), err)
	}
	parent, _ = store.Load(id)
	helps := WorkerHelps(parent.WorkerMessages)
	if len(helps) != 1 || helps[0].ResolutionID == "" || len(helps[0].Replies) != 1 {
		t.Fatal("correlated help not resolved")
	}
	for _, m := range parent.WorkerMessages {
		if m.To == "sibling" && m.Delivered {
			t.Fatal("undelivered sibling message falsely acknowledged")
		}
	}
	if err = a.DeliverWorkerMessages(sibling.Run.ID); err != nil {
		t.Fatal(err)
	}
	sibling, _ = store.Load(sibling.Run.ID)
	if len(sibling.Messages) != 1 || !strings.Contains(sibling.Messages[0].Content, "sibling-only-input") {
		t.Fatalf("sibling route: %+v", sibling.Messages)
	}
	t.Log("fixture only: supplemental input reached next model boundary, one local read, two model requests; help delivered before worker release; no sibling broadcast")
}
