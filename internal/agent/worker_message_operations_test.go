package agent

import (
	"fmt"
	"sync"
	"testing"
)

func TestWorkerMessageOperationConcurrentRetryRestartAndConflict(t *testing.T) {
	a, parent, child := schedulingInbox(t, true, 0, 0)
	otherStore, err := NewStore(a.store.root)
	if err != nil {
		t.Fatal(err)
	}
	otherAgent, err := New(otherStore, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	const operation = "lost-ack"
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < cap(errs); i++ {
		sender := a
		if i%2 != 0 {
			sender = otherAgent
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, m, _, err := sender.SendWorkerMessageOperation(parent, "parent", "worker", "payload", "help", "", operation)
			if err == nil && (m.ID == "" || m.OperationID != operation || m.RequestHash == "") {
				err = fmt.Errorf("missing receipt: %+v", m)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	before, err := a.store.Load(parent)
	if err != nil || len(before.WorkerMessages) != 2 {
		t.Fatalf("duplicate send: %v %+v", err, before.WorkerMessages)
	}
	m := before.WorkerMessages[1]
	// A committed receiver with a lost sender ACK must remain a single input.
	if _, err = a.store.Update(child, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "user", Content: workerMessageContent(m)})
		addEvent(s, "worker_message:"+m.ID, "receiver committed before ACK", nil)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	restartedStore, err := NewStore(a.store.root)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(restartedStore, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, replay, created, err := restarted.SendWorkerMessageOperation(parent, "parent", "worker", "payload", "help", "", operation)
	if err != nil || created || replay.ID != m.ID {
		t.Fatalf("restart retry: %+v %v %v", replay, created, err)
	}
	for i := 0; i < 2; i++ {
		if err = restarted.DeliverWorkerMessages(child); err != nil {
			t.Fatal(err)
		}
	}
	receipt, err := restartedStore.WorkerMessageOperation(parent, operation)
	if err != nil || receipt.ID != m.ID || !receipt.Delivered {
		t.Fatalf("receipt lost: %+v %v", receipt, err)
	}
	received, err := restartedStore.Load(child)
	if err != nil || len(received.Messages) != 1 {
		t.Fatalf("duplicate delivery: %+v %v", received.Messages, err)
	}
	before, _ = restartedStore.Load(parent)
	for _, change := range []struct{ from, to, content, kind, reply string }{
		{"worker", "parent", "payload", "help", ""}, {"parent", "parent", "payload", "help", ""},
		{"parent", "worker", "changed", "help", ""}, {"parent", "worker", "payload", "report", ""},
		{"parent", "worker", "payload", "help", "changed"},
	} {
		if _, _, _, err = restarted.SendWorkerMessageOperation(parent, change.from, change.to, change.content, change.kind, change.reply, operation); err == nil {
			t.Fatalf("accepted conflicting operation: %+v", change)
		}
	}
	after, _ := restartedStore.Load(parent)
	if after.Run.EventSeq != before.Run.EventSeq || len(after.WorkerMessages) != 2 {
		t.Fatal("conflict changed inbox")
	}
	other, err := restarted.Create("independent scope")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, created, err = restarted.SendWorkerMessageOperation(other.Run.ID, "parent", "parent", "other", "report", "", operation); err != nil || !created {
		t.Fatalf("scope collision: %v", err)
	}
	if _, err = restartedStore.WorkerMessageOperation(parent, "missing"); err == nil {
		t.Fatal("missing receipt found")
	}
	for _, invalid := range []string{"a/b", "a\\b", "bad\nID"} {
		if _, _, _, err = restarted.SendWorkerMessageOperation(parent, "parent", "worker", "payload", "help", "", invalid); err == nil {
			t.Fatal("invalid operation accepted")
		}
	}
}
