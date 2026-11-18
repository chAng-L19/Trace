package agent

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func TestSnapshotStreamRequestStringsPreserveJSONAndOwnership(t *testing.T) {
	body := strings.Repeat("正文𠮷\n<>&", 256)
	thinking := strings.Repeat("reasoning\n", 256)
	original := Snapshot{Requests: []ModelRequest{
		{ID: "first", Messages: []Message{{Content: body, Thinking: thinking, ToolCalls: []ToolCall{{ID: "call", Arguments: body}}}}},
		{ID: "second", Messages: []Message{{Content: body, Thinking: thinking, ToolCalls: []ToolCall{{ID: "call", Arguments: body}}}}},
		{ID: "different", Messages: []Message{{Content: strings.Repeat("z", len(body)), Thinking: thinking}}},
	}}
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var got Snapshot
	if err = decodeSnapshot(bytes.NewReader(raw), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, original) {
		t.Fatal("request string reuse changed typed history")
	}
	var encoded bytes.Buffer
	if err = encodeSnapshot(&encoded, got); err != nil || !bytes.Equal(encoded.Bytes(), raw) {
		t.Fatal("request string reuse changed exact encoded history", err)
	}
	if valueHash(got.Requests) != valueHash(original.Requests) {
		t.Fatal("request source digest changed")
	}
	first, second := &got.Requests[0].Messages[0], &got.Requests[1].Messages[0]
	if unsafe.StringData(first.Content) != unsafe.StringData(second.Content) || unsafe.StringData(first.Thinking) != unsafe.StringData(second.Thinking) {
		t.Fatal("identical request strings still occupy separate backing storage")
	}
	if first.Content == got.Requests[2].Messages[0].Content {
		t.Fatal("same-length distinct strings were merged")
	}
	var later Snapshot
	if err = decodeSnapshot(bytes.NewReader(raw), &later); err != nil {
		t.Fatal(err)
	}
	if unsafe.StringData(first.Content) == unsafe.StringData(later.Requests[0].Messages[0].Content) {
		t.Fatal("request string pool escaped one decode")
	}
	first.Content, first.Thinking, first.ToolCalls[0].Arguments = "changed", "changed", "changed"
	if second.Content != body || second.Thinking != thinking || second.ToolCalls[0].Arguments != body || later.Requests[0].Messages[0].Content != body {
		t.Fatal("request string reuse shared mutable records or tool calls")
	}
}

func TestSnapshotStreamRequestStringsLegacyJSON(t *testing.T) {
	text, err := json.Marshal(strings.Repeat("中文𠮷\n<>&", 256))
	if err != nil {
		t.Fatal(err)
	}
	first := `"requests":[{"id":"first","messages":[{"content":` + string(text) + `,"thinking":` + string(text) + `,"tool_calls":[{"id":"call","arguments":` + string(text) + `}]}]}]`
	cases := []string{
		`{` + first + `,"REQUESTS":[{"messages":[{"id":"later"}]}]}`,
		`{` + first + `,"requests":[{"messages":[{"content":null,"thinking":null}]}]}`,
		`{` + first + `,"requests":[{"messages":null}]}`,
		`{` + first + `,"requests":null}`,
		`{` + first + `,"requests":[]}`,
		`{"REQUESTS":[{"MESSAGES":[{"CONTENT":` + string(text) + `,"THINKING":` + string(text) + `}]}]}`,
		`{` + first + `,"requests":[{"messages":[{"content":42}]}]}`,
		`{` + first + `,"requests":[{"messages":[{"thinking":{}}]}]}`,
		`{` + first + `,"requests":[{"messages":[{"tool_calls":[{"arguments":42}]}]}]}`,
		`{` + first + `} {}`,
	}
	for i, raw := range cases {
		var got, want Snapshot
		wantErr := json.Unmarshal([]byte(raw), &want)
		err := decodeSnapshot(strings.NewReader(raw), &got)
		if (err == nil) != (wantErr == nil) {
			t.Fatalf("case %d: changed JSON acceptance: got=%v want=%v", i, err, wantErr)
		}
		if err != nil {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("case %d: changed duplicate-key, null or case folding semantics", i)
		}
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var streamed bytes.Buffer
		if err = encodeSnapshot(&streamed, got); err != nil || !bytes.Equal(streamed.Bytes(), encoded) {
			t.Fatalf("case %d: changed exact encoding: %v", i, err)
		}
	}
}

func TestSnapshotStreamRequestStringsBoundedRetention(t *testing.T) {
	var raw bytes.Buffer
	raw.WriteString(`{"requests":[{"messages":[`)
	encoder := json.NewEncoder(&raw)
	appendMessage := func(text string) {
		if err := encoder.Encode(Message{Content: text}); err != nil {
			t.Fatal(err)
		}
		raw.WriteByte(',')
	}
	// Fill the unique-payload allowance without relying on repeated entries
	// being charged again. The next message must still reuse the first entry.
	for i := 0; i < 16; i++ {
		appendMessage(strings.Repeat(string(rune('a'+i)), 1<<20))
	}
	appendMessage(strings.Repeat("a", 1<<20))
	appendMessage(strings.Repeat("new", 1024))
	appendMessage(strings.Repeat("new", 1024))
	raw.Truncate(raw.Len() - 1)
	raw.WriteString(`]}]}`)
	var got Snapshot
	if err := decodeSnapshot(bytes.NewReader(raw.Bytes()), &got); err != nil {
		t.Fatal(err)
	}
	messages := got.Requests[0].Messages
	if len(messages) != 19 || messages[0].Content != messages[16].Content || messages[17].Content != messages[18].Content {
		t.Fatal("bounded reuse changed request history")
	}
	if unsafe.StringData(messages[0].Content) != unsafe.StringData(messages[16].Content) {
		t.Fatal("full pool stopped reusing an existing string")
	}
	if unsafe.StringData(messages[17].Content) == unsafe.StringData(messages[18].Content) {
		t.Fatal("pool retained new strings beyond its payload allowance")
	}
}
