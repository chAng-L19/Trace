package agent

import (
	"context"
	"encoding/json"
	"testing"
)

func TestWorkerMessageToolParentSiblingAndNestedRouting(t *testing.T) {
	a, parent, child := schedulingInbox(t, true, 0, 0)
	add := func(owner, id, task string) string {
		t.Helper()
		snap, err := a.Create("registered tool child")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.store.Update(snap.Run.ID, func(s *Snapshot) error { s.Run.ParentRunID, s.Run.ParentTaskID = owner, task; return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := a.store.Update(owner, func(s *Snapshot) error {
			s.Workers = append(s.Workers, AgentWorker{ID: id, RunID: snap.Run.ID, TaskID: task, Status: "paused", CleanupStatus: "completed"})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return snap.Run.ID
	}
	sibling, nested := add(parent, "sibling", "sibling-task"), add(child, "nested", "nested-task")
	send := func(run, to, kind, reply string) WorkerMessage {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"to": to, "kind": kind, "reply_to": reply, "content": "correlated input"})
		value, err := a.builtin(context.Background(), run, ToolCall{Name: "worker_message", Arguments: string(raw)})
		if err != nil {
			t.Fatal(err)
		}
		result := value.(map[string]any)
		if result["status"] != "accepted" || result["processed"] != false || result["created"] != true {
			t.Fatalf("invalid send receipt: %+v", result)
		}
		return result["receipt"].(WorkerMessage)
	}
	for _, tc := range []struct{ run, to, from, root, replyRun, replyTo string }{
		{parent, "worker", "parent", parent, child, "parent"},
		{child, "parent", "worker", parent, parent, "worker"},
		{child, "sibling", "worker", parent, sibling, "worker"},
		{child, "nested", "parent", child, nested, "parent"},
		{nested, "parent", "nested", child, child, "nested"},
	} {
		help := send(tc.run, tc.to, "help", "")
		if help.From != tc.from || help.To != tc.to {
			t.Fatalf("wrong identity: %+v", help)
		}
		reply := send(tc.replyRun, tc.replyTo, "reply", help.ID)
		root, _ := a.store.Observe(tc.root)
		if root.WorkerMessages[len(root.WorkerMessages)-1].ID != reply.ID || reply.ReplyTo != help.ID {
			t.Fatal("relation routed to wrong ledger")
		}
	}
}

func TestWorkerMessageToolOperationRetries(t *testing.T) {
	a, parent, child := schedulingInbox(t, true, 0, 0)
	call := ToolCall{Name: "worker_message", Arguments: `{"to":"parent","content":"same logical send","kind":"report","operation_id":"tool-op"}`}
	for i := 0; i < 2; i++ {
		if _, err := a.builtin(context.Background(), child, call); err != nil {
			t.Fatal(err)
		}
	}
	m, err := a.store.WorkerMessageOperation(parent, "tool-op")
	if err != nil || m.From != "worker" || m.To != "parent" {
		t.Fatalf("tool receipt: %+v %v", m, err)
	}
	snap, err := a.store.Load(parent)
	if err != nil || len(snap.WorkerMessages) != 2 {
		t.Fatalf("tool retry duplicated input: %+v %v", snap.WorkerMessages, err)
	}
}
