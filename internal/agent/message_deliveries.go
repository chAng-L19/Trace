package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

type MessageParticipant struct {
	RunID    string `json:"run_id"`
	WorkerID string `json:"worker_id"`
	TaskID   string `json:"task_id,omitempty"`
	Visit    uint64 `json:"visit"`
}

func messageParticipant(s *Snapshot, workerID string) *MessageParticipant {
	if workerID == "parent" {
		return &MessageParticipant{RunID: s.Run.ID, WorkerID: workerID}
	}
	for _, w := range s.Workers {
		if w.ID == workerID {
			return &MessageParticipant{RunID: w.RunID, WorkerID: w.ID, TaskID: w.TaskID, Visit: w.Visit}
		}
	}
	return nil
}

func workerHistoryMessageID(inbox, id string) string {
	return "worker-input-" + valueHash([]string{inbox, id})
}

func sameMessageParticipant(a, b *MessageParticipant) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

type workerDeliveryMarker struct {
	InboxRunID       string              `json:"inbox_run_id"`
	MessageID        string              `json:"message_id"`
	HistoryMessageID string              `json:"history_message_id"`
	RepresentativeID string              `json:"representative_id"`
	Coalesced        bool                `json:"coalesced"`
	Receiver         *MessageParticipant `json:"receiver,omitempty"`
}

type ArchivedMessageRequest struct {
	RequestID string    `json:"request_id"`
	Purpose   string    `json:"purpose"`
	CreatedAt time.Time `json:"created_at"`
}

// These are ledger observations, never read/understood/executed receipts.
type MessageDelivery struct {
	Key               string                   `json:"key"`
	InboxRunID        string                   `json:"inbox_run_id"`
	Source            string                   `json:"source"`
	MessageID         string                   `json:"message_id"`
	OperationID       string                   `json:"operation_id,omitempty"`
	Kind              string                   `json:"kind"`
	From              string                   `json:"from"`
	To                string                   `json:"to"`
	Sender            *MessageParticipant      `json:"sender,omitempty"`
	Recipient         *MessageParticipant      `json:"recipient,omitempty"`
	ReceivedBy        *MessageParticipant      `json:"received_by,omitempty"`
	IdentityStatus    string                   `json:"identity_status"`
	CreatedAt         time.Time                `json:"created_at"`
	InboxStatus       string                   `json:"inbox_status"`
	HistoryStatus     string                   `json:"history_status"`
	HistoryMessageID  string                   `json:"history_message_id,omitempty"`
	HistoryAt         time.Time                `json:"history_at,omitempty"`
	RepresentativeID  string                   `json:"representative_id,omitempty"`
	RequestStatus     string                   `json:"request_status"`
	ArchivedRequests  []ArchivedMessageRequest `json:"archived_requests"`
	RequestCount      int                      `json:"request_count"`
	RequestsTruncated bool                     `json:"requests_truncated,omitempty"`
}

type MessageDeliveryPage struct {
	Items      []MessageDelivery `json:"items"`
	Total      int               `json:"total"`
	NextBefore string            `json:"next_before,omitempty"`
}

// MessageDeliveries reads only selected inboxes and receivers from the original
// Store. Request archives prove construction/persistence, not transport receipt.
func (s *Store) MessageDeliveries(runID, before string, limit int) (MessageDeliveryPage, error) {
	if limit == 0 {
		limit = 40
	}
	if limit < 1 || limit > 100 {
		return MessageDeliveryPage{}, fmt.Errorf("limit must be 1..100")
	}
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return MessageDeliveryPage{}, err
	}
	defer unlock()
	owner, err := s.loadRawLocked(runID)
	if err != nil {
		return MessageDeliveryPage{}, err
	}
	if owner.Run.Deleted {
		return MessageDeliveryPage{}, os.ErrNotExist
	}
	cache := map[string]*Snapshot{runID: &owner}
	load := func(id string) *Snapshot {
		if v, ok := cache[id]; ok {
			return v
		}
		v, err := s.loadRawLocked(id)
		if err != nil || v.Run.Deleted {
			cache[id] = nil
			return nil
		}
		cache[id] = &v
		return &v
	}
	type candidate struct {
		row    MessageDelivery
		queued *QueuedMessage
		worker *WorkerMessage
		inbox  *Snapshot
	}
	rows := []candidate{}
	for i := range owner.Mailbox {
		q := &owner.Mailbox[i]
		rows = append(rows, candidate{row: MessageDelivery{Key: runID + "/user/" + q.ID, InboxRunID: runID, Source: "user", MessageID: q.ID, OperationID: q.OperationID, Kind: q.Kind, From: "user", To: "run", Recipient: &MessageParticipant{RunID: runID, WorkerID: "run"}, IdentityStatus: "pinned", CreatedAt: q.CreatedAt}, queued: q, inbox: &owner})
	}
	appendWorkers := func(inbox *Snapshot, receiverOnly bool) {
		for i := range inbox.WorkerMessages {
			m := &inbox.WorkerMessages[i]
			recipient := m.ToIdentity
			identity := "pinned"
			if recipient == nil {
				recipient = messageParticipant(inbox, m.To)
				identity = "unknown"
			}
			if receiverOnly && (recipient == nil || recipient.RunID != runID) {
				continue
			}
			rows = append(rows, candidate{row: MessageDelivery{Key: inbox.Run.ID + "/worker/" + m.ID, InboxRunID: inbox.Run.ID, Source: "worker", MessageID: m.ID, OperationID: m.OperationID, Kind: m.Kind, From: m.From, To: m.To, Sender: m.FromIdentity, Recipient: recipient, IdentityStatus: identity, CreatedAt: m.CreatedAt}, worker: m, inbox: inbox})
		}
	}
	appendWorkers(&owner, false)
	if owner.Run.ParentRunID != "" && owner.Run.ParentTaskID != "" {
		if parent := load(owner.Run.ParentRunID); parent != nil {
			appendWorkers(parent, true)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].row.CreatedAt.Equal(rows[j].row.CreatedAt) {
			return rows[i].row.CreatedAt.After(rows[j].row.CreatedAt)
		}
		return rows[i].row.Key > rows[j].row.Key
	})
	start := 0
	if before != "" {
		start = -1
		for i := range rows {
			if rows[i].row.Key == before {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return MessageDeliveryPage{}, fmt.Errorf("unknown before cursor")
		}
	}
	page := MessageDeliveryPage{Items: []MessageDelivery{}, Total: len(rows)}
	end := min(len(rows), start+limit)
	for _, c := range rows[start:end] {
		r := c.row
		r.InboxStatus, r.HistoryStatus, r.RequestStatus = "persisted", "pending", "not_archived"
		r.ArchivedRequests = []ArchivedMessageRequest{}
		var receiver *Snapshot
		if r.Recipient != nil {
			receiver = load(r.Recipient.RunID)
		}
		expected := ""
		if q := c.queued; q != nil {
			if !q.CancelledAt.IsZero() {
				r.InboxStatus, r.HistoryStatus = "cancelled", "not_applicable"
			}
			if !q.ConsumedAt.IsZero() {
				r.HistoryStatus, r.HistoryAt, r.HistoryMessageID = "included", q.ConsumedAt, q.MessageID
				expected = q.Content
			}
		} else if m := c.worker; m != nil {
			if m.Delivered {
				r.HistoryStatus = "unknown"
			}
			if receiver != nil {
				for _, e := range receiver.Events {
					if e.Type != "worker_message:"+m.ID {
						continue
					}
					r.HistoryStatus = "unknown"
					raw, _ := json.Marshal(e.Data)
					var marker workerDeliveryMarker
					if json.Unmarshal(raw, &marker) != nil || marker.InboxRunID != r.InboxRunID || marker.MessageID != m.ID || marker.HistoryMessageID == "" {
						break
					}
					if marker.HistoryMessageID != workerHistoryMessageID(r.InboxRunID, marker.RepresentativeID) || marker.Receiver == nil || marker.Receiver.RunID != receiver.Run.ID || marker.Receiver.WorkerID != m.To {
						break
					}
					for _, rep := range c.inbox.WorkerMessages {
						if rep.ID != marker.RepresentativeID || rep.From != m.From || rep.To != m.To || rep.Kind != m.Kind || rep.Content != m.Content || !sameMessageParticipant(rep.FromIdentity, m.FromIdentity) || !sameMessageParticipant(rep.ToIdentity, m.ToIdentity) {
							continue
						}
						if marker.Coalesced && m.Kind != "report" || !marker.Coalesced && rep.ID != m.ID {
							break
						}
						r.HistoryStatus, r.HistoryAt, r.HistoryMessageID, r.ReceivedBy = "included", e.CreatedAt, marker.HistoryMessageID, marker.Receiver
						if marker.Coalesced {
							r.HistoryStatus, r.RepresentativeID = "coalesced", rep.ID
						}
						expected = workerMessageContent(rep)
						break
					}
					break
				}
			}
		}
		if receiver == nil {
			r.HistoryStatus, r.RequestStatus = "unknown", "unknown"
		}
		if r.HistoryStatus == "unknown" {
			r.RequestStatus = "unknown"
		}
		if r.HistoryStatus == "included" && r.HistoryMessageID == "" {
			r.HistoryStatus, r.RequestStatus = "unknown", "unknown"
		}
		if receiver != nil && r.HistoryMessageID != "" {
			messageFound := false
			for _, msg := range receiver.Messages {
				if msg.ID == r.HistoryMessageID && msg.Role == "user" && msg.Content == expected {
					messageFound = true
					break
				}
			}
			if !messageFound {
				r.HistoryStatus, r.RequestStatus = "unknown", "unknown"
			} else {
				seen := map[string]bool{}
				for _, req := range receiver.Requests {
					for _, msg := range req.Messages {
						if req.ID == "" || seen[req.ID] || msg.ID != r.HistoryMessageID || msg.Role != "user" || msg.Content != expected {
							continue
						}
						seen[req.ID] = true
						r.RequestCount++
						if len(r.ArchivedRequests) < 8 {
							r.ArchivedRequests = append(r.ArchivedRequests, ArchivedMessageRequest{RequestID: req.ID, Purpose: req.Purpose, CreatedAt: req.CreatedAt})
						}
					}
				}
				if r.RequestCount > 0 {
					r.RequestStatus = "archived"
				}
				r.RequestsTruncated = r.RequestCount > len(r.ArchivedRequests)
			}
		}
		if r.HistoryStatus == "unknown" {
			r.HistoryMessageID, r.RepresentativeID, r.ReceivedBy = "", "", nil
		}
		page.Items = append(page.Items, r)
	}
	if end < len(rows) && end > start {
		page.NextBefore = rows[end-1].row.Key
	}
	return page, nil
}
