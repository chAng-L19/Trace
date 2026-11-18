package agent

import (
	"fmt"
	"os"
)

// Display cursors refer to immutable message identities, never mutable offsets.
// Adapted from Muteki useConversation.ts before/around paging (c60c8fee).
type MessagePageQuery struct {
	Before, After, Around, Request, Call string
	Limit                                int
}

type MessagePage struct {
	Total     int    `json:"total"`
	Start     int    `json:"start"`
	End       int    `json:"end"`
	Limit     int    `json:"limit"`
	Oldest    string `json:"oldest"`
	Newest    string `json:"newest"`
	HasBefore bool   `json:"has_more_before"`
	HasAfter  bool   `json:"has_more_after"`
}

func DisplayMessageID(run string, message Message, index int) string {
	if message.ID != "" {
		return message.ID
	}
	return fmt.Sprintf("%s-m%06d", run, index+1)
}

// PageMessages selects a bounded window without modifying the execution ledger.
// Call IDs can repeat across requests: ambiguous naked call cursors are errors.
func PageMessages(snap Snapshot, query MessagePageQuery) ([]Message, MessagePage, error) {
	limit := query.Limit
	if limit <= 0 {
		limit = 40
	}
	if limit > 80 {
		limit = 80
	}
	n := len(snap.Messages)
	start, end := max(0, n-limit), n
	target := query.Around
	if query.Before != "" {
		target = query.Before
	}
	if query.After != "" {
		target = query.After
	}
	index := -1
	for i, m := range snap.Messages {
		match := target != "" && DisplayMessageID(snap.Run.ID, m, i) == target
		if target == "" && (query.Request != "" || query.Call != "") {
			match = query.Request == "" || m.RequestID == query.Request
			if query.Call != "" {
				found := m.ToolCallID == query.Call
				for _, call := range m.ToolCalls {
					found = found || call.ID == query.Call
				}
				match = match && found
			}
		}
		if !match {
			continue
		}
		if index >= 0 && query.Call != "" && query.Request == "" && snap.Messages[index].RequestID != m.RequestID {
			return nil, MessagePage{}, fmt.Errorf("%w: call ID is reused; request is required", os.ErrInvalid)
		}
		if index < 0 {
			index = i
		}
	}
	if target != "" || query.Request != "" || query.Call != "" {
		if index < 0 {
			return nil, MessagePage{}, os.ErrNotExist
		}
		switch {
		case query.Before != "":
			end = index
			start = max(0, end-limit)
		case query.After != "":
			start = index + 1
			end = min(n, start+limit)
		default:
			start = max(0, index-limit/2)
			end = min(n, start+limit)
			start = max(0, end-limit)
		}
	}
	page := MessagePage{Total: n, Start: start, End: end, Limit: limit, HasBefore: start > 0, HasAfter: end < n}
	messages := append([]Message{}, snap.Messages[start:end]...)
	for i := range messages {
		messages[i].ID = DisplayMessageID(snap.Run.ID, messages[i], start+i)
	}
	if len(messages) > 0 {
		page.Oldest = messages[0].ID
		page.Newest = messages[len(messages)-1].ID
	}
	return messages, page, nil
}
