package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestP2TransportRealRequestPrefixAndCanonicalEquivalence(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Messages = []Message{{Role: "user", Content: strings.Repeat("retained objective ", 500)}, {Role: "assistant", Content: strings.Repeat("earlier explanation ", 500)}, {Role: "user", Content: "current instruction"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := a.preparePiRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var cache piContextTransport
	full, err := cache.prepare(first, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(id, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: "new continuation"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.preparePiRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if first.Messages[0].CreatedAt.IsZero() || second.Messages[0].CreatedAt.IsZero() {
		t.Fatal("request archive lost system creation time")
	}
	delta, err := cache.prepare(second, nil, full["revision"].(int))
	if err != nil {
		t.Fatal(err)
	}
	prefix := delta["message_prefix"].(int)
	firstHistory, observations := splitContextObservations(id, first.Messages)
	// Request-only observations follow the reusable history and change each
	// turn; they must be replaced, not retained as a stale prefix.
	if len(observations) == 0 || prefix != len(firstHistory) {
		t.Fatalf("real request did not retain exactly its stable prefix: prefix=%d history=%d total=%d", prefix, len(firstHistory), len(first.Messages))
	}
	fullBytes, _ := json.Marshal(full)
	deltaBytes, _ := json.Marshal(delta)
	if len(deltaBytes)*4 >= len(fullBytes) {
		t.Fatalf("no material RPC reduction: full=%d delta=%d", len(fullBytes), len(deltaBytes))
	}
	rebuilt := append(append([]json.RawMessage(nil), full["messages"].([]json.RawMessage)[:prefix]...), delta["messages"].([]json.RawMessage)...)
	if len(rebuilt) != len(second.Messages) {
		t.Fatal("canonical reconstruction changed message count")
	}
	for i, raw := range rebuilt {
		var actual Message
		if err := json.Unmarshal(raw, &actual); err != nil {
			t.Fatal(err)
		}
		expected := second.Messages[i]
		if expected.Role == "system" {
			expected.CreatedAt = time.Time{}
		}
		if !reflect.DeepEqual(actual, expected) {
			t.Fatalf("canonical reconstruction differs at %d", i)
		}
	}
	third := second
	third.Messages = []Message{{Role: "system", Content: "compacted and changed system"}, {Role: "user", Content: "late input"}}
	third.Tools = nil
	reset, err := cache.prepare(third, nil, delta["revision"].(int))
	if err != nil {
		t.Fatal(err)
	}
	if reset["message_prefix"].(int) > 1 {
		t.Fatal("compaction retained old history")
	}
	if _, ok := reset["message_head"]; !ok {
		t.Fatal("changed system header omitted")
	}
	if _, ok := reset["tools"]; !ok {
		t.Fatal("tool schema deletion omitted")
	}
}
