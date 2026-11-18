package web

import (
	"encoding/json"
	"testing"

	"trace/internal/agent"
)

func TestWorkspaceProjectionPreservesRequestIdentityAndRawRequestOnDemand(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("workspace fixture")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	_, err = a.Store().Update(id, func(v *agent.Snapshot) error {
		v.Requests = []agent.ModelRequest{{ID: "req", Messages: []agent.Message{{Role: "user", Content: "large history"}}}}
		v.Responses = []agent.ResponseRecord{{RequestID: "req", Response: agent.ModelResponse{Content: "answer", Thinking: "thought"}}}
		v.Messages = []agent.Message{{ID: "assistant", Role: "assistant", Content: "answer", Thinking: "thought"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Store().AppendEvents(id, []agent.Event{{Type: "model_stream", Data: map[string]any{"request_id": "req", "delta": map[string]string{"kind": "text", "text": "answer"}}}, {Type: "run_completed"}}); err != nil {
		t.Fatal(err)
	}
	w := request(t, s.Handler(), "GET", "/api/runs/"+id+"?view=workspace", nil, 200)
	var got struct {
		Messages []workspaceMessage   `json:"messages"`
		Requests []agent.ModelRequest `json:"requests"`
		Events   []agent.Event        `json:"events"`
	}
	if err = json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Messages[0].RequestID != "req" || got.Messages[0].Content != "answer" || len(got.Requests[0].Messages) != 0 {
		t.Fatalf("invalid projection: %+v", got)
	}
	for _, e := range got.Events {
		if e.Type == "model_stream" {
			t.Fatal("archived stream duplicated")
		}
	}
	full := request(t, s.Handler(), "GET", "/api/runs/"+id+"/requests/req", nil, 200)
	var req agent.ModelRequest
	if err = json.Unmarshal(full.Body.Bytes(), &req); err != nil || len(req.Messages) != 1 {
		t.Fatalf("raw history missing: %v", err)
	}
	request(t, s.Handler(), "GET", "/api/runs/"+id+"/requests/unknown", nil, 404)
}

func TestEventHistoryFiltersBeforePaginating(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("history fixture")
	if err != nil {
		t.Fatal(err)
	}
	var batch []agent.Event
	for i := 0; i < 6; i++ {
		batch = append(batch, agent.Event{Type: "model_stream"}, agent.Event{Type: "run_paused", Summary: "matching"})
	}
	if err = a.Store().AppendEvents(snap.Run.ID, batch); err != nil {
		t.Fatal(err)
	}
	path := "/api/runs/" + snap.Run.ID + "/events/history?summary=true&limit=2&query=matching"
	var first struct {
		Events []agent.Event `json:"events"`
		Next   int           `json:"next_before"`
		More   bool          `json:"has_more"`
	}
	if err = json.Unmarshal(request(t, s.Handler(), "GET", path, nil, 200).Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 2 || !first.More || first.Next != first.Events[1].Sequence {
		t.Fatalf("page: %+v", first)
	}
	var second struct {
		Events []agent.Event `json:"events"`
	}
	if err = json.Unmarshal(request(t, s.Handler(), "GET", path+"&before="+jsonNumber(first.Next), nil, 200).Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if len(second.Events) != 2 || second.Events[0].Sequence >= first.Next {
		t.Fatalf("overlap: %+v", second)
	}
}

func jsonNumber(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestWorkspaceDoesNotConfuseSummaryReviewOrReusedCallIDs(t *testing.T) {
	call := agent.ToolCall{ID: "shared", Name: "exec", Arguments: "{}"}
	snap := agent.Snapshot{
		Requests:    []agent.ModelRequest{{ID: "summary"}, {ID: "review", Purpose: "evidence_review"}, {ID: "ordinary"}},
		Compactions: []agent.Compaction{{RequestID: "summary"}},
		Responses: []agent.ResponseRecord{
			{RequestID: "summary", Response: agent.ModelResponse{Content: "same"}},
			{RequestID: "review", Response: agent.ModelResponse{Content: "same"}},
			{RequestID: "ordinary", Response: agent.ModelResponse{Content: "same"}},
		},
		Messages: []agent.Message{{Role: "assistant", Content: "same"}},
		Actions:  []agent.Action{{RequestID: "old", CallID: call.ID, Result: &agent.ToolResult{}}, {RequestID: "new", CallID: call.ID}},
		Events: []agent.Event{
			{Sequence: 1, Type: "action_started", Data: map[string]string{"request_id": "old", "call_id": call.ID}},
			{Sequence: 2, Type: "tool_output", Data: map[string]string{"call_id": call.ID, "text": "OLD_OUTPUT"}},
			{Sequence: 3, Type: "action_started", Data: map[string]string{"request_id": "new", "call_id": call.ID}},
			{Sequence: 4, Type: "tool_output", Data: map[string]string{"call_id": call.ID, "text": "NEW_OUTPUT"}},
		},
	}
	for i := 5; i < 210; i++ {
		snap.Events = append(snap.Events, agent.Event{Sequence: i, Type: "worker_message"})
	}
	raw, err := json.Marshal(workspaceSnapshot(snap))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages  []agent.Message        `json:"messages"`
		Responses []agent.ResponseRecord `json:"responses"`
		Requests  []agent.ModelRequest   `json:"requests"`
		Events    []agent.Event          `json:"events"`
	}
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Messages[0].RequestID != "ordinary" || got.Requests[0].Purpose != "context_summary" {
		t.Fatalf("wrong ownership: %+v", got.Messages)
	}
	if got.Responses[0].Response.Content != "same" || got.Responses[1].Response.Content != "same" || got.Responses[2].Response.Content != "" {
		t.Fatal("internal responses lost or ordinary response duplicated")
	}
	outputs := 0
	for _, e := range got.Events {
		if e.Type == "tool_output" {
			outputs++
			b, _ := json.Marshal(e.Data)
			var d map[string]string
			_ = json.Unmarshal(b, &d)
			if d["request_id"] != "new" || d["text"] != "NEW_OUTPUT" {
				t.Fatalf("old output crossed request: %s", b)
			}
		}
	}
	if outputs != 1 {
		t.Fatalf("pending outputs=%d", outputs)
	}
	// Two identical historical ordinary responses have no unique owner; keep the
	// raw response bodies instead of inventing identity or removing either body.
	snap.Requests = append(snap.Requests, agent.ModelRequest{ID: "ambiguous"})
	snap.Responses = append(snap.Responses, agent.ResponseRecord{RequestID: "ambiguous", Response: agent.ModelResponse{Content: "same"}})
	raw, _ = json.Marshal(workspaceSnapshot(snap))
	got.Messages = nil
	_ = json.Unmarshal(raw, &got)
	if got.Messages[0].RequestID != "" || got.Responses[2].Response.Content != "same" {
		t.Fatal("ambiguous legacy response was guessed")
	}
	snap.Messages[0].RequestID = "ordinary"
	raw, _ = json.Marshal(workspaceSnapshot(snap))
	got.Messages = nil
	_ = json.Unmarshal(raw, &got)
	if got.Messages[0].RequestID != "ordinary" {
		t.Fatal("persisted identity was discarded")
	}
}
