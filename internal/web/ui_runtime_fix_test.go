package web

import (
	"reflect"
	"testing"
	"time"

	"trace/internal/agent"
)

func TestToolObservationHTTPNeverStartsRun(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("manual observation fixture")
	if err != nil {
		t.Fatal(err)
	}
	id, req := snap.Run.ID, snap.Run.ID+"-0001"
	_, err = a.Store().Update(id, func(v *agent.Snapshot) error {
		v.Run.Status = agent.StatusPaused
		v.Run.RequestSeq = 1
		v.Requests = []agent.ModelRequest{{ID: req, CreatedAt: time.Now().UTC()}}
		v.Messages = []agent.Message{{Role: "assistant", RequestID: req, ToolCalls: []agent.ToolCall{{ID: "call", Name: "exec", Arguments: `{"command":"must-not-run"}`}}}}
		v.Actions = []agent.Action{{RequestID: req, CallID: "call", Name: "exec", Adapter: "exec", Status: "uncertain", Result: &agent.ToolResult{ExitCode: 1, Error: "pids limit", OutcomeUncertain: true}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/runs/" + id + "/tool-observations"
	s.active[id] = true
	request(t, s.Handler(), "POST", path, map[string]string{"request_id": req, "call_id": "call", "content": "observed"}, 409)
	delete(s.active, id)
	request(t, s.Handler(), "POST", path, map[string]string{"request_id": "stale-request", "call_id": "call", "content": "observed"}, 409)
	request(t, s.Handler(), "POST", path, map[string]string{"request_id": req, "call_id": "call", "content": "Observed failed process; external effects unconfirmed"}, 200)
	got, err := a.Store().Load(id)
	if err != nil || got.Run.Status != agent.StatusPaused || len(got.Requests) != 1 || got.Actions[0].Status != "resolved_manually" || len(got.Evidence) != 1 || got.Evidence[0].Type != "manual_observation" || got.Evidence[0].Verification != "unverified" {
		t.Fatalf("unexpected observation: %+v %v", got, err)
	}
}

func TestToolObservationHTTPRejectsHistoricalPendingCall(t *testing.T) {
	s, a := fixture(t, nil)
	v, err := a.Create("historical observation fixture")
	if err != nil {
		t.Fatal(err)
	}
	id := v.Run.ID
	before, err := a.Store().Update(id, func(v *agent.Snapshot) error {
		v.Run.Status = agent.StatusPaused
		v.Requests = []agent.ModelRequest{{ID: "old-request"}, {ID: "current-request"}}
		v.Messages = []agent.Message{{Role: "assistant", RequestID: "old-request", ToolCalls: []agent.ToolCall{{ID: "old-call", Name: "exec"}}}, {Role: "assistant", RequestID: "current-request", ToolCalls: []agent.ToolCall{{ID: "current-call", Name: "exec"}}}}
		v.Actions = []agent.Action{{RequestID: "old-request", CallID: "old-call", Adapter: "exec", Status: "uncertain", Result: &agent.ToolResult{OutcomeUncertain: true}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	request(t, s.Handler(), "POST", "/api/runs/"+id+"/tool-observations", map[string]string{"request_id": "current-request", "call_id": "old-call", "content": "foreign pending call"}, 409)
	after, err := a.Store().Load(id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("rejected observation mutated state", err)
	}
}
