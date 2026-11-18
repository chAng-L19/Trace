package agent

import (
	"fmt"
	"os"
	"testing"
)

func TestMessagePagesStableCursorsAndAmbiguousCalls(t *testing.T) {
	snap := Snapshot{Run: Run{ID: "run"}}
	for i := 0; i < 205; i++ {
		snap.Messages = append(snap.Messages, Message{ID: fmt.Sprintf("m%d", i), Role: "assistant", RequestID: fmt.Sprintf("r%d", i), Content: "text"})
	}
	messages, page, err := PageMessages(snap, MessagePageQuery{})
	if err != nil || len(messages) != 40 || page.Start != 165 || page.Oldest != "m165" || !page.HasBefore || page.HasAfter {
		t.Fatalf("tail: %+v %v", page, err)
	}
	older, before, err := PageMessages(snap, MessagePageQuery{Before: page.Oldest})
	if err != nil || len(older) != 40 || before.End != 165 || older[39].ID != "m164" {
		t.Fatalf("before: %+v %v", before, err)
	}
	newer, after, err := PageMessages(snap, MessagePageQuery{After: before.Newest})
	if err != nil || newer[0].ID != messages[0].ID || after.End != page.End {
		t.Fatalf("after: %+v %v", after, err)
	}
	_, around, err := PageMessages(snap, MessagePageQuery{Around: "m80"})
	if err != nil || around.Start > 80 || around.End <= 80 {
		t.Fatalf("around: %+v %v", around, err)
	}
	snap.Messages = append(snap.Messages, Message{ID: "new"})
	_, stable, err := PageMessages(snap, MessagePageQuery{Before: page.Oldest})
	if err != nil || stable.Start != before.Start || stable.End != before.End {
		t.Fatal("append shifted stable cursor")
	}
	if _, _, err = PageMessages(snap, MessagePageQuery{Around: "missing"}); !os.IsNotExist(err) {
		t.Fatal("missing cursor accepted")
	}
	snap.Messages[0].ToolCalls = []ToolCall{{ID: "reused"}}
	snap.Messages[1].ToolCalls = []ToolCall{{ID: "reused"}}
	if _, _, err = PageMessages(snap, MessagePageQuery{Call: "reused"}); err == nil {
		t.Fatal("ambiguous call accepted")
	}
	if _, _, err = PageMessages(snap, MessagePageQuery{Call: "reused", Request: "r1"}); err != nil {
		t.Fatal(err)
	}
	snap.Messages[0].ID = ""
	if rows, _, err := PageMessages(snap, MessagePageQuery{Around: "run-m000001"}); err != nil || rows[0].ID != "run-m000001" {
		t.Fatal("legacy cursor lost", err)
	}
}
