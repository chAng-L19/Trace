package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"trace/internal/agent"
	"unicode/utf8"
)

func TestWorkspaceMessagePagesBoundBodiesAndKeepFullSources(t *testing.T) {
	app, a := fixture(t, nil)
	snap, err := a.Create("paged history")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("完整正文𠮷", 1200)
	snap, err = a.Store().Update(snap.Run.ID, func(s *agent.Snapshot) error {
		for i := 0; i < 205; i++ {
			s.Messages = append(s.Messages, agent.Message{ID: fmt.Sprintf("m%d", i), Role: "assistant", RequestID: fmt.Sprintf("r%d", i), Content: body, Thinking: body})
		}
		s.Actions = []agent.Action{{RequestID: "r1", CallID: "shared", Result: &agent.ToolResult{Stdout: body}}, {RequestID: "r2", CallID: "shared", Result: &agent.ToolResult{Stdout: "second"}}}
		s.Messages[1].ToolCalls = []agent.ToolCall{{ID: "shared", Arguments: body}}
		s.Messages[2].ToolCalls = []agent.ToolCall{{ID: "shared", Arguments: "{}"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	base := "/api/runs/" + snap.Run.ID
	var tail struct {
		Messages []workspaceMessage `json:"messages"`
		Page     agent.MessagePage  `json:"messages_page"`
	}
	raw := request(t, app.Handler(), "GET", base+"?view=workspace", nil, 200)
	if err = json.Unmarshal(raw.Body.Bytes(), &tail); err != nil {
		t.Fatal(err)
	}
	if len(tail.Messages) != 40 || tail.Page.End != 205 || raw.Body.Len() > 800000 {
		t.Fatalf("unbounded page %d / %d", len(tail.Messages), raw.Body.Len())
	}
	for _, message := range tail.Messages {
		if !message.Preview || len(message.Content) > 4096 || !utf8.ValidString(message.Content) {
			t.Fatal("invalid bounded UTF8 preview")
		}
	}
	page := request(t, app.Handler(), "GET", base+"/messages?before="+tail.Page.Oldest, nil, 200)
	if err = json.Unmarshal(page.Body.Bytes(), &tail); err != nil {
		t.Fatal(err)
	}
	if tail.Page.End != 165 {
		t.Fatalf("wrong before page: %+v", tail.Page)
	}
	request(t, app.Handler(), "GET", base+"/messages?around=m2", nil, 200)
	request(t, app.Handler(), "GET", base+"/messages?before=m2&after=m3", nil, 400)
	request(t, app.Handler(), "GET", base+"/messages?around=missing", nil, 404)
	request(t, app.Handler(), "GET", base+"/messages?request=r2&call=shared", nil, 200)
	full := request(t, app.Handler(), "GET", base+"/messages/m1", nil, 200)
	var message agent.Message
	_ = json.Unmarshal(full.Body.Bytes(), &message)
	if message.Content != body || message.Thinking != body {
		t.Fatal("source message truncated")
	}
	full = request(t, app.Handler(), "GET", base+"/actions/shared?request=r1", nil, 200)
	var action agent.Action
	_ = json.Unmarshal(full.Body.Bytes(), &action)
	if action.Result.Stdout != body {
		t.Fatal("source action truncated")
	}
	request(t, app.Handler(), "GET", base+"/actions/shared", nil, 400)
	_, err = a.Store().Update(snap.Run.ID, func(s *agent.Snapshot) error {
		s.Messages = append(s.Messages, agent.Message{ID: "manual-call", Role: "assistant", RequestID: "manual", ToolCalls: []agent.ToolCall{{ID: "manual", Name: "manual", Arguments: body}}}, agent.Message{ID: "manual-reply", Role: "tool", ToolCallID: "manual", Content: body})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	full = request(t, app.Handler(), "GET", base+"/actions/manual?request=manual", nil, 200)
	_ = json.Unmarshal(full.Body.Bytes(), &action)
	if action.Arguments != body || action.Result.Stdout != body {
		t.Fatal("manual source was replaced by preview")
	}
}
