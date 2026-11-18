package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func schedulingInbox(t testing.TB, child bool, history, count int) (*Agent, string, string) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(store, successProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := a.Create("isolated scheduling inbox")
	if err != nil {
		t.Fatal(err)
	}
	receiver, to := parent.Run.ID, "parent"
	if child {
		c, err := a.Create("isolated child inbox")
		if err != nil {
			t.Fatal(err)
		}
		receiver, to = c.Run.ID, "worker"
		if _, err := store.Update(receiver, func(s *Snapshot) error { s.Run.ParentRunID, s.Run.ParentTaskID = parent.Run.ID, "task"; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	_, err = store.Update(parent.Run.ID, func(s *Snapshot) error {
		if child {
			s.Workers = []AgentWorker{{ID: to, TaskID: "task", RunID: receiver, Status: "completed", CleanupStatus: "completed"}}
		}
		for i := 0; i < count; i++ {
			s.WorkerMessages = append(s.WorkerMessages, WorkerMessage{ID: fmt.Sprintf("m%d", i), From: "parent", To: to, Content: fmt.Sprint(i), CreatedAt: time.Now().UTC()})
		}
		s.WorkerMessages = append(s.WorkerMessages, WorkerMessage{ID: "other", From: "parent", To: "other", Content: "not this receiver"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if history > 0 {
		events := make([]Event, history)
		for i := range events {
			events[i] = Event{Type: "model_stream", Data: strings.Repeat("x", 700)}
		}
		if err := store.AppendEvents(parent.Run.ID, events); err != nil {
			t.Fatal(err)
		}
	}
	return a, parent.Run.ID, receiver
}

func TestWorkerMessageBatchOrderRetryAndConcurrentDelivery(t *testing.T) {
	for _, child := range []bool{false, true} {
		t.Run(fmt.Sprint(child), func(t *testing.T) {
			a, parent, receiver := schedulingInbox(t, child, 0, 8)
			// Simulate a crash after receiver commit but before sender acknowledgement.
			if _, err := a.store.Update(receiver, func(s *Snapshot) error {
				s.Messages = append(s.Messages, Message{Role: "user", Content: "Worker message from parent (data):\n0"})
				addEvent(s, "worker_message:m0", "already committed", nil)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if err := a.DeliverWorkerMessages(receiver); err != nil {
						t.Error(err)
					}
				}()
			}
			wg.Wait()
			got, err := a.store.Load(receiver)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Messages) != 8 {
				t.Fatalf("duplicate/missing messages: %d", len(got.Messages))
			}
			markers := map[string]int{}
			for _, event := range got.Events {
				markers[event.Type]++
			}
			for i, message := range got.Messages {
				if !strings.HasSuffix(message.Content, fmt.Sprintf("\n%d", i)) || markers[fmt.Sprintf("worker_message:m%d", i)] != 1 {
					t.Fatalf("message order/marker %d: %+v", i, message)
				}
			}
			root, err := a.store.Load(parent)
			if err != nil {
				t.Fatal(err)
			}
			for i, message := range root.WorkerMessages {
				if message.Delivered != (i < 8) {
					t.Fatalf("wrong recipient ack: %+v", message)
				}
			}
			path := filepath.Join(a.store.root, receiver+".json")
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := a.DeliverWorkerMessages(receiver); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(path)
			if err != nil || !sameGeneration(before, after) {
				t.Fatalf("empty delivery rewrote receiver: %v", err)
			}
		})
	}
}

func TestWorkerMessageFailedReceiverDoesNotAcknowledge(t *testing.T) {
	a, parent, receiver := schedulingInbox(t, true, 0, 8)
	if err := os.Chmod(a.store.root, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(a.store.root, 0700)
	if err := a.DeliverWorkerMessages(receiver); err == nil {
		t.Fatal("receiver persistence failure was hidden")
	}
	if err := os.Chmod(a.store.root, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := a.store.Load(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range root.WorkerMessages {
		if message.Delivered {
			t.Fatalf("failed delivery acknowledged: %+v", message)
		}
	}
	if err := a.DeliverWorkerMessages(receiver); err != nil {
		t.Fatal(err)
	}
	got, err := a.store.Load(receiver)
	if err != nil || len(got.Messages) != 8 {
		t.Fatalf("retry lost messages: %d %v", len(got.Messages), err)
	}
}

func BenchmarkSchedulingInbox(b *testing.B) {
	for _, scenario := range []struct {
		name  string
		child bool
		count int
	}{{"idle", false, 0}, {"parent8", false, 8}, {"child8", true, 8}} {
		b.Run(scenario.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				a, _, receiver := schedulingInbox(b, scenario.child, 5000, scenario.count)
				b.StartTimer()
				if err := a.DeliverWorkerMessages(receiver); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSchedulingTurn(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		a, _, receiver := schedulingInbox(b, false, 5000, 0)
		b.StartTimer()
		if _, err := a.Run(context.Background(), receiver); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPlannerFactBatchRetainsFailedRequestCursor(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "task", Goal: "work"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error {
		for i := 0; i < 25; i++ {
			addEvent(s, "worker_finished", fmt.Sprintf("fact-%02d", i), nil)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	text, through, err := a.plannerContextAt(id)
	if err != nil || !strings.Contains(text, "fact-19") || strings.Contains(text, "fact-20") {
		t.Fatalf("first fact batch: %s %v", text, err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Requests = append(s.Requests, ModelRequest{ID: "failed", PlannerThrough: through})
		s.Responses = append(s.Responses, ResponseRecord{RequestID: "failed", Error: "provider error"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	text, retryThrough, err := a.plannerContextAt(id)
	if err != nil || retryThrough != through || !strings.Contains(text, "fact-00") {
		t.Fatalf("failed response skipped facts: %s %d %v", text, retryThrough, err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Responses[0].Error = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	text, _, err = a.plannerContextAt(id)
	if err != nil || strings.Contains(text, "fact-19") || !strings.Contains(text, "fact-24") {
		t.Fatalf("next fact batch: %s %v", text, err)
	}
}

func TestPlannerRequestArchivesExactFactBatch(t *testing.T) {
	p := &modelRetryProvider{errors: []error{fmt.Errorf("provider stopped")}}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "task", Goal: "work"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error {
		for i := 0; i < 25; i++ {
			addEvent(s, "worker_finished", fmt.Sprintf("request-fact-%02d", i), nil)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, through, err := a.plannerContextAt(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), id); err == nil {
		t.Fatal("provider failure was hidden")
	}
	if len(p.requests) != 1 {
		t.Fatalf("unexpected requests: %d", len(p.requests))
	}
	request := p.requests[0]
	var projected strings.Builder
	for _, message := range request.Messages {
		projected.WriteString(message.Content)
	}
	if request.PlannerThrough != through || !strings.Contains(projected.String(), "request-fact-19") || strings.Contains(projected.String(), "request-fact-20") {
		t.Fatalf("request cursor/body disagree: %d %d %s", request.PlannerThrough, through, projected.String())
	}
	saved, err := store.Load(id)
	if err != nil || len(saved.Requests) != 1 || saved.Requests[0].PlannerThrough != request.PlannerThrough || len(saved.Responses) != 1 || saved.Responses[0].Error == "" {
		t.Fatalf("request/failed response not archived: %v", err)
	}
	text, retryThrough, err := a.plannerContextAt(id)
	if err != nil || retryThrough != through || !strings.Contains(text, "request-fact-00") {
		t.Fatalf("failed request consumed facts: %d %s %v", retryThrough, text, err)
	}
}

func TestWorkerMessageConcurrentProducerAndDelivery(t *testing.T) {
	a, parent, receiver := schedulingInbox(t, true, 0, 0)
	done := make(chan struct{})
	produced := make(chan []string, 1)
	go func() {
		defer close(done)
		var ids []string
		for i := 0; i < 20; i++ {
			s, err := a.SendWorkerMessage(parent, "parent", "worker", fmt.Sprintf("producer-%02d", i))
			if err != nil {
				t.Error(err)
				return
			}
			ids = append(ids, s.WorkerMessages[len(s.WorkerMessages)-1].ID)
		}
		produced <- ids
	}()
	for {
		if err := a.DeliverWorkerMessages(receiver); err != nil {
			t.Error(err)
			<-done
			return
		}
		select {
		case <-done:
			goto drain
		default:
		}
	}
drain:
	if err := a.DeliverWorkerMessages(receiver); err != nil {
		t.Fatal(err)
	}
	select {
	case ids := <-produced:
		s, err := a.store.Load(receiver)
		if err != nil || len(s.Messages) != len(ids) {
			t.Fatalf("concurrent messages lost: %d %v", len(s.Messages), err)
		}
		for i, message := range s.Messages {
			if !strings.HasSuffix(message.Content, fmt.Sprintf("producer-%02d", i)) {
				t.Fatalf("concurrent order %d: %s", i, message.Content)
			}
		}
		root, err := a.store.Load(parent)
		if err != nil {
			t.Fatal(err)
		}
		for i, m := range root.WorkerMessages {
			if m.Delivered != (i > 0) {
				t.Fatalf("wrong concurrent ack: %+v", m)
			}
		}
	default:
		t.Fatal("producer did not finish")
	}
}

func TestWorkerMessageFailedSenderAcknowledgementRetriesCommittedReceiver(t *testing.T) {
	a, parent, receiver := schedulingInbox(t, true, 0, 8)
	path := filepath.Join(a.store.root, parent+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	// Observe reads routing metadata; a corrupt unrelated field must still
	// fail the sender's full ACK transaction after the receiver commits.
	fields["requests"] = json.RawMessage(`42`)
	broken, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, broken, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.DeliverWorkerMessages(receiver); err == nil {
		t.Fatal("sender transaction failure was hidden")
	}
	got, err := a.store.Load(receiver)
	if err != nil || len(got.Messages) != 8 {
		t.Fatalf("receiver not committed before failed ACK: %d %v", len(got.Messages), err)
	}
	root, err := a.store.Observe(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range root.WorkerMessages {
		if m.Delivered {
			t.Fatalf("sender failure acknowledged: %+v", m)
		}
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.DeliverWorkerMessages(receiver); err != nil {
		t.Fatal(err)
	}
	got, err = a.store.Load(receiver)
	if err != nil || len(got.Messages) != 8 {
		t.Fatalf("ACK retry duplicated messages: %d %v", len(got.Messages), err)
	}
	saved, err := a.store.Load(parent)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range saved.WorkerMessages {
		if m.Delivered != (i < 8) {
			t.Fatalf("ACK retry lost acknowledgement: %+v", m)
		}
	}
}
