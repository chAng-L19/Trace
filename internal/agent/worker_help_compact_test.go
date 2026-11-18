package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCompactWorkerHelpBoundsHistoryContentAndReplies(t *testing.T) {
	var s Snapshot
	for i := 0; i < 140; i++ {
		id := fmt.Sprintf("help-%d", i)
		s.WorkerMessages = append(s.WorkerMessages, WorkerMessage{ID: id, From: "worker", To: "parent", Kind: "help", Content: strings.Repeat("研究上下文", 1000), Delivered: true})
		for j := 0; j < 30; j++ {
			s.WorkerMessages = append(s.WorkerMessages, WorkerMessage{ID: fmt.Sprintf("reply-%d-%d", i, j), From: "parent", To: "worker", Kind: "reply", ReplyTo: id, Content: "large-history-reply", Delivered: true})
		}
		if i < 80 {
			s.WorkerMessages[len(s.WorkerMessages)-31].Content = strings.Repeat("closed-history-content", 1000)
			s.WorkerMessages = append(s.WorkerMessages, WorkerMessage{ID: "resolved-" + id, From: "parent", To: "worker", Kind: "resolved", ReplyTo: id, Delivered: true})
		}
	}
	legacy := WorkerMessage{ID: "legacy-pending", From: "parent", To: "worker", Content: strings.Repeat("legacy-full-content", 100)}
	s.WorkerMessages = append(s.WorkerMessages, legacy)
	before, _ := json.Marshal(s)
	state := compactWorkerState(s)
	rows := state["help_requests"].([]compactWorkerHelp)
	if state["open_help_total"] != 60 || state["resolved_help_total"] != 80 || len(rows) != 20 {
		t.Fatalf("unbounded or wrong counts: %d %+v %+v", len(rows), state["open_help_total"], state["resolved_help_total"])
	}
	for i, row := range rows {
		if row.ID != fmt.Sprintf("help-%d", 139-i) || row.From != "worker" || row.To != "parent" || row.ReplyCount != 30 || !row.Delivered || !row.ContentTruncated || utf8.RuneCountInString(row.Content) != 320 || !utf8.ValidString(row.Content) {
			t.Fatalf("incorrect compact row %d: %+v", i, row)
		}
	}
	shown := legacy
	shown.Content = excerpt(shown.Content, 1024)
	if !reflect.DeepEqual(state["messages"], []WorkerMessage{shown}) {
		t.Fatal("pending message identity or excerpt changed")
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "closed-history-content") || strings.Contains(string(data), "reply-139-") || strings.Contains(string(data), "resolved-help-") || len(data) > 30000 {
		t.Fatalf("historical body or reply identities leaked into compact output: %d bytes", len(data))
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		t.Fatal("tool projection mutated canonical inbox")
	}
	full := WorkerHelps(s.WorkerMessages)
	if len(full) != 140 || len(full[0].Replies) != 30 || full[0].ResolutionID == "" || len(full[139].Message.Content) <= len(rows[0].Content) {
		t.Fatal("full workspace/detail lifecycle changed")
	}
}

func TestCompactWorkerHelpKeepsResolutionAndDeliveryIndependent(t *testing.T) {
	messages := []WorkerMessage{
		{ID: "open", From: "worker", To: "parent", Kind: "help", Content: strings.Repeat("界", 320), Delivered: true},
		{ID: "reply", From: "parent", To: "worker", Kind: "reply", ReplyTo: "open", Delivered: true},
		{ID: "wrong-participant", From: "parent", To: "sibling", Kind: "resolved", ReplyTo: "open"},
		{ID: "closed", From: "worker", To: "parent", Kind: "help", Content: "closed but not delivered"},
		{ID: "resolved", From: "parent", To: "worker", Kind: "resolved", ReplyTo: "closed"},
	}
	rows, open, resolved := compactWorkerHelps(messages)
	if open != 1 || resolved != 1 || len(rows) != 1 || rows[0].ID != "open" || rows[0].ReplyCount != 1 || !rows[0].Delivered || rows[0].ContentTruncated || rows[0].Content != messages[0].Content {
		t.Fatalf("lifecycle or boundary changed: %+v %d %d", rows, open, resolved)
	}
	rows, open, resolved = compactWorkerHelps(nil)
	if rows == nil || len(rows) != 0 || open != 0 || resolved != 0 {
		t.Fatal("empty projection must be an empty worklist")
	}
}
