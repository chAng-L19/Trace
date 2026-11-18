package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func deliveryByID(t *testing.T, store *Store, run, id string) MessageDelivery {
	t.Helper()
	page, err := store.MessageDeliveries(run, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range page.Items {
		if row.MessageID == id {
			return row
		}
	}
	t.Fatalf("missing receipt %s: %+v", id, page)
	return MessageDelivery{}
}

func TestU13UserDeliveryArchiveIsNotProviderReceipt(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, snap := u7Store(t, backend)
			a, err := New(store, successProvider{}, "")
			if err != nil {
				t.Fatal(err)
			}
			_, q, _, err := a.QueueMessageOperation(snap.Run.ID, "steer", "user-op", "same content")
			if err != nil {
				t.Fatal(err)
			}
			row := deliveryByID(t, store, snap.Run.ID, q.ID)
			if row.HistoryStatus != "pending" || row.RequestStatus != "not_archived" {
				t.Fatalf("pending: %+v", row)
			}
			_, err = store.Update(snap.Run.ID, func(s *Snapshot) error { consumeMailbox(s, "steer"); return nil })
			if err != nil {
				t.Fatal(err)
			}
			row = deliveryByID(t, store, snap.Run.ID, q.ID)
			if row.HistoryStatus != "included" || row.RequestStatus != "not_archived" {
				t.Fatalf("history before request: %+v", row)
			}
			_, err = store.Update(snap.Run.ID, func(s *Snapshot) error {
				// Archive only: no response, send attempt or external effect exists.
				s.Requests = append(s.Requests, ModelRequest{ID: "summary", Purpose: "context_summary", Messages: append([]Message(nil), s.Messages...), CreatedAt: time.Now().UTC()})
				s.Requests = append(s.Requests, ModelRequest{ID: "normal", Messages: append([]Message(nil), s.Messages...), CreatedAt: time.Now().UTC()})
				s.Requests = append(s.Requests, ModelRequest{ID: "same-bytes-wrong-identity", Messages: []Message{{ID: "another", Role: "user", Content: q.Content}}})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			row = deliveryByID(t, store, snap.Run.ID, q.ID)
			if row.RequestStatus != "archived" || row.RequestCount != 2 || row.ArchivedRequests[0].Purpose != "context_summary" || row.ArchivedRequests[1].Purpose != "" {
				t.Fatalf("archive semantics: %+v", row)
			}
			raw, _ := json.Marshal(row)
			for _, term := range []string{"provider_received", "understood", "executed", "same content"} {
				if strings.Contains(string(raw), term) {
					t.Fatalf("overclaim/body leaked %s", raw)
				}
			}
			_, q2, _, err := a.QueueMessageOperation(snap.Run.ID, "steer", "user-op-2", q.Content)
			if err != nil {
				t.Fatal(err)
			}
			if got := deliveryByID(t, store, snap.Run.ID, q2.ID); got.RequestCount != 0 || got.HistoryStatus != "pending" {
				t.Fatal("same-content operation conflated")
			}
			before, _ := store.loadRawLocked(snap.Run.ID)
			page, err := store.MessageDeliveries(snap.Run.ID, "", 1)
			if err != nil || page.Total != 2 || page.NextBefore == "" {
				t.Fatalf("page: %+v %v", page, err)
			}
			last, err := store.MessageDeliveries(snap.Run.ID, page.NextBefore, 1)
			if err != nil || len(last.Items) != 1 || last.Items[0].MessageID == page.Items[0].MessageID || last.NextBefore != "" {
				t.Fatalf("cursor: %+v %v", last, err)
			}
			after, _ := store.loadRawLocked(snap.Run.ID)
			if valueHash(before) != valueHash(after) {
				t.Fatal("read projection mutated store")
			}
		})
	}
}

func TestU13WorkerCoalescingReceiverFirstAndLegacyUnknown(t *testing.T) {
	a, parent, child := schedulingInbox(t, true, 0, 0)
	var sent []WorkerMessage
	for _, op := range []string{"first", "second"} {
		_, m, _, err := a.SendWorkerMessageOperation(parent, "parent", "worker", "same report", "report", "", op)
		if err != nil {
			t.Fatal(err)
		}
		sent = append(sent, m)
	}
	if err := a.DeliverWorkerMessages(child); err != nil {
		t.Fatal(err)
	}
	first, second := deliveryByID(t, a.store, parent, sent[0].ID), deliveryByID(t, a.store, parent, sent[1].ID)
	if first.HistoryStatus != "included" || second.HistoryStatus != "coalesced" || second.RepresentativeID != sent[0].ID || second.HistoryMessageID != first.HistoryMessageID {
		t.Fatalf("coalescing: %+v %+v", first, second)
	}
	_, err := a.store.Update(child, func(s *Snapshot) error {
		s.Requests = append(s.Requests, ModelRequest{ID: "archived-only", Messages: append([]Message(nil), s.Messages...)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Lost sender ACK never duplicates history or loses receiver-side proof.
	_, err = a.store.Update(parent, func(s *Snapshot) error {
		for i := range s.WorkerMessages {
			s.WorkerMessages[i].Delivered = false
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := deliveryByID(t, a.store, child, sent[1].ID); got.RequestCount != 1 || got.HistoryStatus != "coalesced" {
		t.Fatalf("receiver-first: %+v", got)
	}
	if err := a.DeliverWorkerMessages(child); err != nil {
		t.Fatal(err)
	}
	receiver, _ := a.store.Load(child)
	if len(receiver.Messages) != 1 {
		t.Fatal("lost ACK duplicated projection")
	}
	_, legacy, _, err := a.SendWorkerMessageOperation(parent, "parent", "worker", "legacy input", "", "", "legacy")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.store.Update(child, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "user", Content: workerMessageContent(legacy)})
		addEvent(s, "worker_message:"+legacy.ID, "old marker", map[string]string{"message_id": legacy.ID})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DeliverWorkerMessages(child); err != nil {
		t.Fatal(err)
	}
	if got := deliveryByID(t, a.store, parent, legacy.ID); got.HistoryStatus != "unknown" || got.RequestStatus != "unknown" || got.HistoryMessageID != "" {
		t.Fatalf("legacy content guessed: %+v", got)
	}
}

func TestU13WorkerVisitAndNestedInboxRemainDistinct(t *testing.T) {
	a, parent, child := schedulingInbox(t, true, 0, 0)
	_, old, _, err := a.SendWorkerMessageOperation(parent, "parent", "worker", "same", "report", "", "old-visit")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.store.Update(parent, func(s *Snapshot) error { s.Workers[0].Visit = 2; return nil })
	if err != nil {
		t.Fatal(err)
	}
	_, next, _, err := a.SendWorkerMessageOperation(parent, "parent", "worker", "same", "report", "", "new-visit")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DeliverWorkerMessages(child); err != nil {
		t.Fatal(err)
	}
	for _, m := range []WorkerMessage{old, next} {
		got := deliveryByID(t, a.store, child, m.ID)
		if got.HistoryStatus != "included" || got.Recipient.Visit != m.ToIdentity.Visit || got.ReceivedBy.Visit != 2 {
			t.Fatalf("visit conflation: %+v", got)
		}
	}
	_, local, _, err := a.SendWorkerMessageOperation(child, "parent", "parent", "own subtree", "help", "", "nested")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.DeliverWorkerMessages(child); err != nil {
		t.Fatal(err)
	}
	if got := deliveryByID(t, a.store, child, local.ID); got.InboxRunID != child || got.HistoryStatus != "included" {
		t.Fatalf("own inbox missed: %+v", got)
	}
	if got := deliveryByID(t, a.store, child, old.ID); got.InboxRunID != parent {
		t.Fatal("parent inbox conflated")
	}
}
