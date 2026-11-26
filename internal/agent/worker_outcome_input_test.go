package agent

import "testing"

func TestOutcomeHandoffRequiresNewWorkerHistory(t *testing.T) {
	for _, test := range []struct {
		name                                         string
		coalesced, missingHistory, ackOnly, repeated bool
		wantPending                                  bool
	}{
		{name: "new-history"},
		{name: "coalesced", coalesced: true, wantPending: true},
		{name: "marker-without-history", missingHistory: true, wantPending: true},
		{name: "sender-ack", ackOnly: true, wantPending: true},
		{name: "retried-receiver-marker", repeated: true, wantPending: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := Snapshot{Run: Run{ID: "child", ParentRunID: "parent", ParentTaskID: "work"}, WorkerContext: &WorkerContext{RunID: "child", ParentRunID: "parent", TaskID: "work"}}
			marker := workerDeliveryMarker{InboxRunID: "parent", MessageID: "input", RepresentativeID: "input", HistoryMessageID: workerHistoryMessageID("parent", "input"), Receiver: &MessageParticipant{RunID: "child", WorkerID: "worker", Visit: 1}}
			if !test.missingHistory {
				s.Messages = append(s.Messages, Message{ID: marker.HistoryMessageID, Role: "user", Content: "new research input"})
			}
			if test.repeated {
				addEvent(&s, "worker_message:input", "first receiver commit", marker)
			}
			addEvent(&s, "completion_deferred", "outcome handoff", map[string]any{"reasons": []string{workerOutcomeHandoffReason}})
			marker.Coalesced = test.coalesced
			if test.ackOnly {
				addEvent(&s, "worker_message_delivered", "sender acknowledgement", marker)
			} else {
				addEvent(&s, "worker_message:input", "receiver commit", marker)
			}
			if got := outcomeHandoffPending(&s); got != test.wantPending {
				t.Fatalf("handoff pending: want %t, got %t", test.wantPending, got)
			}
		})
	}
}

func TestOutcomeHandoffWorkerDeliveryRetryAndCrossVisit(t *testing.T) {
	a, parent, receiver := schedulingInbox(t, true, 0, 0)
	defer a.store.Close()
	defer a.CloseMCP()
	arm := func() {
		t.Helper()
		if _, err := a.store.Update(receiver, func(s *Snapshot) error {
			s.WorkerContext = &WorkerContext{RunID: receiver, ParentRunID: parent, TaskID: "task", WorkerID: "worker"}
			addEvent(s, "completion_deferred", "outcome handoff", map[string]any{"reasons": []string{workerOutcomeHandoffReason}})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	pending := func() bool {
		t.Helper()
		s, err := a.store.Load(receiver)
		if err != nil {
			t.Fatal(err)
		}
		return outcomeHandoffPending(&s)
	}
	arm()
	_, receipt, _, err := a.SendWorkerMessageOperation(parent, "parent", "worker", "new input", "report", "", "first-input")
	if err != nil {
		t.Fatal(err)
	}
	if !pending() {
		t.Fatal("enqueue acknowledgement unlocked outcome-only")
	}
	if err := a.DeliverWorkerMessages(receiver); err != nil {
		t.Fatal(err)
	}
	if pending() {
		t.Fatal("receiver history did not unlock business tools")
	}
	arm()
	if _, err := a.store.Update(parent, func(s *Snapshot) error {
		for i := range s.WorkerMessages {
			if s.WorkerMessages[i].ID == receipt.ID {
				s.WorkerMessages[i].Delivered = false
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.DeliverWorkerMessages(receiver); err != nil {
		t.Fatal(err)
	}
	if !pending() {
		t.Fatal("sender ACK retry unlocked outcome-only")
	}
	if _, _, _, err := a.SendWorkerMessageOperation(parent, "parent", "worker", "input for continuing worker", "report", "", "cross-visit-input"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.Update(parent, func(s *Snapshot) error { s.Workers[0].Visit++; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := a.DeliverWorkerMessages(receiver); err != nil {
		t.Fatal(err)
	}
	if pending() {
		t.Fatal("ordinary message from prior visit was dropped")
	}
}
