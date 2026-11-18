package agent

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Titles and archive status are display metadata, not execution state. Reference:
// Muteki run_bindings.py:90/101 (144df766, AGPL-3.0); pi session-manager.ts:1150
// (8a7b0c03, MIT). No goal, transcript, acceptance contract or evidence is rewritten.
func (s *Store) UpdateRunMetadata(id string, title *string, archived *bool) (Snapshot, error) {
	return s.Update(id, func(snap *Snapshot) error {
		if title != nil {
			snap.Run.Title = strings.TrimSpace(*title)
		}
		if archived != nil {
			snap.Run.Archived = *archived
		}
		addEvent(snap, "run_metadata_updated", "会话名称或归档状态已更新", map[string]any{"title": snap.Run.Title, "archived": snap.Run.Archived})
		return nil
	})
}

func (s *Store) MessageOperation(id, operationID string) (QueuedMessage, error) {
	snap, err := s.Load(id)
	if err != nil {
		return QueuedMessage{}, err
	}
	if snap.Run.Deleted {
		return QueuedMessage{}, os.ErrNotExist
	}
	for _, q := range snap.Mailbox {
		if operationID != "" && q.OperationID == operationID {
			return q, nil
		}
	}
	return QueuedMessage{}, os.ErrNotExist
}

// The same Store transaction serializes operator edits against consumeMailbox.
// Delivered input is immutable; cancellations retain the receipt for ACK lookup.
func (s *Store) EditQueuedMessage(id, messageID string, revision int, content *string, cancel bool) (Snapshot, error) {
	if !cancel && (content == nil || strings.TrimSpace(*content) == "") {
		return Snapshot{}, fmt.Errorf("message is required")
	}
	return s.Update(id, func(snap *Snapshot) error {
		for i := range snap.Mailbox {
			q := &snap.Mailbox[i]
			if q.ID != messageID {
				continue
			}
			if cancel && !q.CancelledAt.IsZero() {
				return nil
			}
			if !q.Pending() {
				return fmt.Errorf("message has already been delivered or cancelled")
			}
			if q.Revision != revision {
				return fmt.Errorf("message revision changed; refresh the queue")
			}
			q.Revision++
			if cancel {
				q.CancelledAt = time.Now().UTC()
				addEvent(snap, "message_cancelled", "待投递指令已取消", map[string]any{"id": q.ID, "revision": q.Revision})
			} else {
				q.Content = *content
				addEvent(snap, "message_edited", "待投递指令已修改", map[string]any{"id": q.ID, "revision": q.Revision})
			}
			return nil
		}
		return os.ErrNotExist
	})
}

// Ordering is within each delivery class: steer is consumed at a safe boundary;
// follow_up remains the next idle turn. Reordering never changes that contract.
func (s *Store) OrderQueuedMessages(id string, ids []string) (Snapshot, error) {
	return s.Update(id, func(snap *Snapshot) error {
		pending := map[string]QueuedMessage{}
		for _, q := range snap.Mailbox {
			if q.Pending() {
				pending[q.ID] = q
			}
		}
		if len(ids) != len(pending) {
			return fmt.Errorf("queue changed; include every pending message exactly once")
		}
		ordered := make([]QueuedMessage, 0, len(ids))
		for _, id := range ids {
			q, ok := pending[id]
			if !ok {
				return fmt.Errorf("queue changed or duplicate message ID")
			}
			ordered = append(ordered, q)
			delete(pending, id)
		}
		next := 0
		for i := range snap.Mailbox {
			if snap.Mailbox[i].Pending() {
				snap.Mailbox[i] = ordered[next]
				next++
			}
		}
		addEvent(snap, "mailbox_reordered", "待投递指令顺序已更新", ids)
		return nil
	})
}

type MessageSearchMatch struct {
	RunID     string `json:"run_id"`
	Title     string `json:"title"`
	Goal      string `json:"goal"`
	Archived  bool   `json:"archived"`
	MessageID string `json:"message_id"`
	RequestID string `json:"request_id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Role      string `json:"role"`
	Content   string `json:"content"`
}

type MessageSearchResult struct {
	Matches   []MessageSearchMatch `json:"matches"`
	Truncated bool                 `json:"truncated"`
}

// ponytail: scan existing snapshots for explicit body searches; introduce an
// index only after measured archive size makes this read path inadequate.
func (s *Store) SearchMessages(query string, includeArchived bool, limit int) (MessageSearchResult, error) {
	result := MessageSearchResult{Matches: []MessageSearchMatch{}}
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return result, fmt.Errorf("search query is required")
	}
	if limit < 1 || limit > 200 {
		return result, fmt.Errorf("limit must be 1..200")
	}
	runs, err := s.List()
	if err != nil {
		return result, err
	}
	for _, run := range runs {
		if run.Archived && !includeArchived {
			continue
		}
		snap, err := s.Load(run.ID)
		if err != nil {
			return result, err
		}
		if snap.Run.Deleted || snap.Run.Archived && !includeArchived {
			continue
		}
		assignMessageIDs(&snap)
		for _, m := range snap.Messages {
			lower := strings.ToLower(m.Content)
			index := strings.Index(lower, query)
			if index < 0 {
				continue
			}
			if len(result.Matches) == limit {
				result.Truncated = true
				return result, nil
			}
			// Match against the full body; only the result preview is bounded.
			body := []rune(m.Content)
			start := len([]rune(lower[:index])) - 80
			if start < 0 {
				start = 0
			}
			end := start + 400
			if end > len(body) {
				end = len(body)
			}
			result.Matches = append(result.Matches, MessageSearchMatch{RunID: run.ID, Title: snap.Run.Title, Goal: snap.Run.Goal, Archived: snap.Run.Archived, MessageID: m.ID, RequestID: m.RequestID, CallID: m.ToolCallID, Role: m.Role, Content: string(body[start:end])})
		}
	}
	return result, nil
}
