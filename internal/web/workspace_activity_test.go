package web

import (
	"encoding/json"
	"fmt"
	"testing"
	"trace/internal/agent"
)

func TestActivityWorkspaceHTTPUsesBoundedWindowAndRealTotals(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("activity")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Store().Update(snap.Run.ID, func(v *agent.Snapshot) error {
		v.Messages = nil
		for i := 0; i < 100; i++ {
			id := fmt.Sprint("r", i)
			v.Messages = append(v.Messages, agent.Message{Role: "assistant", RequestID: id, Content: id})
			v.Requests = append(v.Requests, agent.ModelRequest{ID: id})
			v.Responses = append(v.Responses, agent.ResponseRecord{RequestID: id})
			v.Actions = append(v.Actions, agent.Action{RequestID: id, CallID: "same", Status: "completed", Result: &agent.ToolResult{}})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	w := request(t, s.Handler(), "GET", "/api/runs/"+snap.Run.ID+"?view=workspace&activity=true", nil, 200)
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["result"]; ok {
		t.Fatal("display asserted unvalidated result")
	}
	if _, ok := raw["coverage"]; ok {
		t.Fatal("display asserted source coverage")
	}
	var got struct {
		Messages []agent.Message   `json:"messages"`
		Actions  []agent.Action    `json:"actions"`
		Page     agent.MessagePage `json:"messages_page"`
		Totals   map[string]int    `json:"totals"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 40 || len(got.Actions) != 40 || got.Page.Total != 100 || got.Page.Start != 60 || !got.Page.HasBefore || got.Totals["actions"] != 100 || got.Totals["artifacts"] != 0 {
		t.Fatalf("bad activity page: %+v", got)
	}
	full := request(t, s.Handler(), "GET", "/api/runs/"+snap.Run.ID+"?view=workspace", nil, 200)
	if err = json.Unmarshal(full.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["coverage"]; !ok {
		t.Fatal("full workspace lost validation")
	}
}
