package web

import (
	"encoding/json"
	"testing"

	"trace/internal/agent"
)

type workerWorkspaceFixture struct {
	agent.Snapshot
	WorkerActivity []agent.WorkerActivity `json:"worker_activity"`
	HelpRequests   []agent.WorkerHelp     `json:"help_requests"`
}

func TestWorkerMessageAPIAndWorkspaceHelpProjection(t *testing.T) {
	s, a := fixture(t, nil)
	parent, err := a.Create("worker help API")
	if err != nil {
		t.Fatal(err)
	}
	child, err := a.Create("paused child")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Update(child.Run.ID, func(c *agent.Snapshot) error {
		c.Run.ParentRunID, c.Run.ParentTaskID = parent.Run.ID, "task"
		c.Run.Status, c.Run.PauseReason = agent.StatusPaused, "waiting_input:decision"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.Update(parent.Run.ID, func(p *agent.Snapshot) error {
		p.Workers = []agent.AgentWorker{{ID: "worker", TaskID: "task", RunID: child.Run.ID, Status: "paused", CleanupStatus: "completed"}}
		p.Plan.Tasks = []agent.PlanTask{{ID: "task", Status: "running", WorkerID: "worker", ChildRunID: child.Run.ID}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	path := "/api/runs/" + parent.Run.ID + "/workers/worker/message"
	post := func(body any) agent.WorkerMessage {
		t.Helper()
		w := request(t, h, "POST", path, body, 200)
		var snap agent.Snapshot
		if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
			t.Fatal(err)
		}
		return snap.WorkerMessages[len(snap.WorkerMessages)-1]
	}
	legacy := post(map[string]string{"content": "legacy payload"})
	if legacy.Kind != "" || legacy.ReplyTo != "" {
		t.Fatalf("legacy changed: %+v", legacy)
	}
	// Simulate a lost POST response: the same send returns its original receipt.
	body := map[string]string{"operation_id": "worker-op", "content": "legacy payload"}
	first := post(body)
	retry := post(body)
	if retry.ID != first.ID {
		t.Fatal("POST retry duplicated worker input")
	}
	request(t, h, "POST", path, map[string]string{"operation_id": "worker-op", "content": "conflict"}, 409)
	w := request(t, h, "GET", "/api/runs/"+parent.Run.ID+"/worker-message-operations/worker-op", nil, 200)
	var receipt agent.WorkerMessage
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || receipt.ID != first.ID {
		t.Fatalf("receipt query: %s", w.Body.String())
	}
	request(t, h, "GET", "/api/runs/"+parent.Run.ID+"/worker-message-operations/missing", nil, 404)
	help := post(map[string]string{"content": "decision needed", "kind": "help"})
	reply := post(map[string]string{"content": "decision", "kind": "reply", "reply_to": help.ID})
	view := func() workerWorkspaceFixture {
		t.Helper()
		w := request(t, h, "GET", "/api/runs/"+parent.Run.ID+"?view=workspace", nil, 200)
		var got workerWorkspaceFixture
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	got := view()
	if len(got.HelpRequests) != 1 || len(got.HelpRequests[0].Replies) != 1 || got.HelpRequests[0].Replies[0] != reply.ID || got.HelpRequests[0].ResolutionID != "" {
		t.Fatalf("reply projection: %+v", got.HelpRequests)
	}
	resolved := post(map[string]string{"content": "decision closed", "kind": "resolved", "reply_to": help.ID})
	request(t, h, "POST", path, map[string]string{"content": "bad", "kind": "reply", "reply_to": "missing"}, 409)
	got = view()
	if got.HelpRequests[0].ResolutionID != resolved.ID || got.HelpRequests[0].Message.Delivered || len(got.WorkerMessages) != 5 {
		t.Fatalf("resolution/delivery conflated: %+v", got.HelpRequests)
	}
	if len(got.WorkerActivity) != 1 || !got.WorkerActivity[0].Available || got.WorkerActivity[0].Phase != "paused" || got.WorkerActivity[0].PendingMessages != 5 || len(got.WorkerActivity[0].OpenHelpIDs) != 0 {
		t.Fatalf("activity output: %+v", got.WorkerActivity)
	}
	childAfter, err := s.store.Load(child.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if childAfter.Run.Status != agent.StatusPaused || got.Plan.Tasks[0].Status != "running" || got.Workers[0].Status != "paused" {
		t.Fatal("message API executed or completed task")
	}
}
