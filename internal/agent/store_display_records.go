package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"time"
)

const displayIndexVersion = 1

// Derived addressing metadata belongs to the original Store. Execution, detail
// and evidence reads never consult it. JSON spans address the pinned source
// generation; SQLite rows hold only the existing bounded display previews.
type displayRecordSpan struct {
	Start, End int64
	Hash       [32]byte
}

type displayFileGeneration struct {
	Identity       string
	Size, Modified int64
}

type displayRecordIndex struct {
	Version          int
	Generation       displayFileGeneration
	Fields           map[string]json.RawMessage
	Spans            map[string][]displayRecordSpan
	State            [32]byte
	Capacity         json.RawMessage
	Canonical        bool
	EventsValidated  bool
	RunEnd, EventEnd int64
}

type displayIndexBuilder struct {
	index          *displayRecordIndex
	write          func(string, int, []byte) error
	clear          func(string) error
	capacity       Snapshot
	capacityBudget int
	seen           map[string]bool
}

func displayContentHash(content, thinking string, calls []ToolCall) string {
	if len(calls) == 0 {
		calls = nil
	}
	raw, _ := json.Marshal(struct {
		Content, Thinking string
		Calls             []ToolCall
	}{content, thinking, calls})
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func (b *displayIndexBuilder) reset(kind string) error {
	if kind != "messages" && kind != "responses" && kind != "actions" {
		return nil
	}
	delete(b.index.Spans, kind)
	if b.clear != nil {
		return b.clear(kind)
	}
	return nil
}

func (b *displayIndexBuilder) record(kind string, ordinal int, raw []byte, span displayRecordSpan) ([]byte, error) {
	var header, preview any
	switch kind {
	case "messages":
		var m Message
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, err
		}
		var h struct {
			activityMessage
			Attachments []InputAttachment `json:"attachments,omitempty"`
		}
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, err
		}
		h.ContentHash = displayContentHash(m.Content, m.Thinking, m.ToolCalls)
		p := ActivityProjection{Snapshot: Snapshot{Messages: []Message{m}}}
		trimActivityDisplay(&p)
		h.Preview = p.MessagePreviews[m.ID]
		header, preview = h, p.Snapshot.Messages[0]
	case "responses":
		var response ResponseRecord
		if err := json.Unmarshal(raw, &response); err != nil {
			return nil, err
		}
		header = struct {
			RequestID   string    `json:"request_id"`
			CreatedAt   time.Time `json:"created_at"`
			Error       string    `json:"error,omitempty"`
			ContentHash string    `json:"display_content_hash"`
		}{response.RequestID, response.CreatedAt, response.Error, displayContentHash(response.Response.Content, response.Response.Thinking, response.Response.ToolCalls)}
		p := ActivityProjection{Snapshot: Snapshot{Responses: []ResponseRecord{response}}}
		trimActivityDisplay(&p)
		preview = p.Snapshot.Responses[0]
	case "actions":
		var action Action
		if err := json.Unmarshal(raw, &action); err != nil {
			return nil, err
		}
		var h workerActionHeader
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, err
		}
		header = h
		p := ActivityProjection{Snapshot: Snapshot{Messages: []Message{{RequestID: action.RequestID}}, Actions: []Action{action}}}
		trimActivityDisplay(&p)
		preview = p.Snapshot.Actions[0]
	default:
		return nil, fmt.Errorf("unsupported display record %s", kind)
	}
	if b.write != nil {
		body, err := json.Marshal(preview)
		if err != nil {
			return nil, err
		}
		if len(body) > activityProjectionMaxBytes {
			return nil, errActivityTooLarge
		}
		if err = b.write(kind, ordinal, body); err != nil {
			return nil, err
		}
	} else {
		span.Hash = sha256.Sum256(raw)
		b.index.Spans[kind] = append(b.index.Spans[kind], span)
	}
	return json.Marshal(header)
}

func buildDisplayRecordIndex(source interface {
	io.Reader
	io.ReaderAt
}, write func(string, int, []byte) error, clear func(string) error) (*displayRecordIndex, error) {
	index := &displayRecordIndex{Version: displayIndexVersion, Spans: map[string][]displayRecordSpan{}, Canonical: true, EventsValidated: true}
	b := &displayIndexBuilder{index: index, write: write, clear: clear, capacityBudget: activityProjectionMaxBytes, seen: map[string]bool{}}
	v, _, err := readActivityFieldsIndexed(source, "index", b)
	if err != nil {
		return nil, err
	}
	index.Fields = v.fields
	index.Capacity, err = json.Marshal(b.capacity)
	if err != nil {
		return nil, err
	}
	return index, nil
}

func (b *displayIndexBuilder) capacityField(name string, raw []byte) error {
	switch name {
	case "run", "plan", "workers", "worker_context", "worker_settlement":
		wrapped := append([]byte(fmt.Sprintf("{%q:", name)), raw...)
		wrapped = append(wrapped, '}')
		return readCapacityProjection(context.Background(), bytes.NewReader(wrapped), &b.capacity, &b.capacityBudget)
	}
	return nil
}

func (b *displayIndexBuilder) capacityAction(ordinal int, raw []byte) error {
	b.capacityBudget -= 2 * int(reflect.TypeOf(Action{}).Size())
	if b.capacityBudget < 0 {
		return errActivityTooLarge
	}
	if ordinal >= cap(b.capacity.Actions) {
		b.capacity.Actions = append(b.capacity.Actions, Action{})
	} else if ordinal >= len(b.capacity.Actions) {
		b.capacity.Actions = b.capacity.Actions[:ordinal+1]
	}
	return readCapacityProjection(context.Background(), bytes.NewReader(raw), &b.capacity.Actions[ordinal], &b.capacityBudget)
}

// Each access is bounded before allocation; a corrupted index can neither seek
// outside its committed file nor reinterpret unrelated bytes as a selected row.
func (index *displayRecordIndex) records(read func(string, int) ([]byte, error)) *activityRecords {
	loaded := 0
	get := func(kind string, ordinal int) (snapshotJSON, error) {
		body, err := read(kind, ordinal)
		if err != nil {
			return nil, err
		}
		loaded += len(body)
		if loaded > activityProjectionMaxBytes {
			return nil, fmt.Errorf("selected display records bytes=%d: %w", loaded, errActivityTooLarge)
		}
		return body, nil
	}
	return &activityRecords{
		indexed:  true,
		message:  func(i int) (snapshotJSON, error) { return get("messages", i) },
		response: func(i int) (snapshotJSON, error) { return get("responses", i) },
		action:   func(i int) (snapshotJSON, error) { return get("actions", i) },
	}
}

func (index *displayRecordIndex) view(mode string) *storedView {
	fields := index.Fields
	if mode == "planner" {
		fields = map[string]json.RawMessage{}
		for _, name := range []string{"run", "plan", "workers", "mailbox", "root_contract", "messages", "worker_messages", "child_reports"} {
			if raw, ok := index.Fields[name]; ok {
				fields[name] = raw
			}
		}
	}
	return &storedView{fields: fields, workspaceBase: &workspaceBase{}}
}
