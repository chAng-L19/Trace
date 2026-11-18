package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestWorkerHelpLifecycleAndLegacyCompatibility(t *testing.T) {
	a, parent, _ := schedulingInbox(t, true, 0, 0)
	snap, err := a.SendWorkerMessage(parent, "parent", "worker", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	legacy := snap.WorkerMessages[len(snap.WorkerMessages)-1]
	data, _ := json.Marshal(legacy)
	if strings.Contains(string(data), "kind") || strings.Contains(string(data), "reply_to") || workerMessageContent(legacy) != "Worker message from parent (data):\nlegacy" {
		t.Fatalf("legacy changed: %s", data)
	}
	snap, err = a.SendWorkerMessageRelated(parent, "worker", "parent", "need decision", "help", "")
	if err != nil {
		t.Fatal(err)
	}
	help := snap.WorkerMessages[len(snap.WorkerMessages)-1]
	if err := a.DeliverWorkerMessages(parent); err != nil {
		t.Fatal(err)
	}
	snap, err = a.SendWorkerMessageRelated(parent, "parent", "worker", "try next", "reply", help.ID)
	if err != nil {
		t.Fatal(err)
	}
	h := WorkerHelps(snap.WorkerMessages)[0]
	if !h.Message.Delivered || len(h.Replies) != 1 || h.ResolutionID != "" {
		t.Fatalf("delivery/reply resolved help: %+v", h)
	}
	snap, err = a.SendWorkerMessageRelated(parent, "worker", "parent", "confirmed", "resolved", help.ID)
	if err != nil {
		t.Fatal(err)
	}
	h = WorkerHelps(snap.WorkerMessages)[0]
	resolution := snap.WorkerMessages[len(snap.WorkerMessages)-1]
	if h.ResolutionID != resolution.ID || resolution.Delivered {
		t.Fatalf("resolution incorrectly requires delivery: %+v", h)
	}
	if snap.Workers[0].Status != "completed" {
		t.Fatal("message altered worker status")
	}
}

func TestWorkerHelpRejectsInvalidRelationsAtomically(t *testing.T) {
	a, parent, _ := schedulingInbox(t, true, 0, 0)
	snap, err := a.SendWorkerMessageRelated(parent, "worker", "parent", "help", "help", "")
	if err != nil {
		t.Fatal(err)
	}
	help := snap.WorkerMessages[len(snap.WorkerMessages)-1].ID
	if _, err := a.store.Update(parent, func(s *Snapshot) error {
		s.Workers = append(s.Workers, AgentWorker{ID: "sibling", RunID: "another"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	other, err := a.Create("other run help")
	if err != nil {
		t.Fatal(err)
	}
	other, err = a.SendWorkerMessageRelated(other.Run.ID, "parent", "parent", "other help", "help", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ kind, reply, from, to string }{
		{"invalid", "", "parent", "worker"}, {"help", help, "parent", "worker"},
		{"report", help, "parent", "worker"}, {"", help, "parent", "worker"},
		{"reply", "", "parent", "worker"}, {"resolved", "missing", "parent", "worker"},
		{"reply", help, "sibling", "worker"}, {"reply", other.WorkerMessages[0].ID, "parent", "worker"},
	} {
		before, _ := a.store.Observe(parent)
		if _, err := a.SendWorkerMessageRelated(parent, tc.from, tc.to, "invalid", tc.kind, tc.reply); err == nil {
			t.Fatalf("accepted %+v", tc)
		}
		after, _ := a.store.Observe(parent)
		if len(after.WorkerMessages) != len(before.WorkerMessages) || after.Run.EventSeq != before.Run.EventSeq {
			t.Fatalf("rejected relation persisted %+v", tc)
		}
	}
}

func TestWorkerRelatedDeliveryRetriesCommittedReceiver(t *testing.T) {
	a, parent, child := schedulingInbox(t, true, 0, 0)
	snap, err := a.SendWorkerMessageRelated(parent, "parent", "worker", "help payload", "help", "")
	if err != nil {
		t.Fatal(err)
	}
	m := snap.WorkerMessages[len(snap.WorkerMessages)-1]
	if _, err := a.store.Update(child, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "user", Content: workerMessageContent(m)})
		addEvent(s, "worker_message:"+m.ID, "receiver already committed", nil)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := a.DeliverWorkerMessages(child); err != nil {
			t.Fatal(err)
		}
	}
	got, err := a.store.Load(child)
	if err != nil {
		t.Fatal(err)
	}
	markers := 0
	for _, event := range got.Events {
		if event.Type == "worker_message:"+m.ID {
			markers++
		}
	}
	if len(got.Messages) != 1 || markers != 1 || !strings.Contains(got.Messages[0].Content, "kind=help") || !strings.Contains(got.Messages[0].Content, m.ID) {
		t.Fatalf("typed replay duplicated/lost metadata: %+v", got.Messages)
	}
	root, _ := a.store.Observe(parent)
	if !root.WorkerMessages[len(root.WorkerMessages)-1].Delivered {
		t.Fatal("sender acknowledgement missing")
	}
}

func TestWorkerResolvedNoticeDoesNotResumeOrComplete(t *testing.T) {
	a, parent, child := schedulingInbox(t, true, 0, 0)
	if _, err := a.store.Update(parent, func(s *Snapshot) error {
		s.Workers[0].Status = "paused"
		s.Plan.Tasks = []PlanTask{{ID: "task", Status: "running", WorkerID: "worker", ChildRunID: child}}
		s.WorkerMessages = []WorkerMessage{{ID: "help", From: "worker", To: "parent", Kind: "help", Content: "blocked", Delivered: true}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.Update(child, func(s *Snapshot) error {
		s.Run.Status, s.Run.PauseReason = StatusPaused, "waiting_input:decision"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SendWorkerMessageRelated(parent, "parent", "worker", "closed", "resolved", "help"); err != nil {
		t.Fatal(err)
	}
	before, _ := a.store.Load(child)
	if pending, err := a.pendingWorkerInput(&before); err != nil || pending {
		t.Fatalf("resolution counts as input: %v %v", pending, err)
	}
	if err := a.resumePausedWorkers(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ResumeWorker(context.Background(), parent, "worker"); err == nil || !strings.Contains(err.Error(), "waiting for input") {
		t.Fatalf("resolution bypassed explicit input gate: %v", err)
	}
	after, _ := a.store.Load(child)
	root, _ := a.store.Observe(parent)
	if after.Run.Status != StatusPaused || after.Run.EventSeq != before.Run.EventSeq || root.Workers[0].Status != "paused" || root.Plan.Tasks[0].Status != "running" {
		t.Fatal("resolution changed execution/task state")
	}
	if _, err := a.SendWorkerMessageRelated(parent, "parent", "worker", "actual input", "reply", "help"); err != nil {
		t.Fatal(err)
	}
	if pending, err := a.pendingWorkerInput(&after); err != nil || !pending {
		t.Fatalf("reply stopped being input: %v %v", pending, err)
	}
}
