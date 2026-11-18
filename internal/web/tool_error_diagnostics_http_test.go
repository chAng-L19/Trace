package web

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"trace/internal/agent"
)

func TestTaskDisposeHTTPDiagnosticPreservesErrorAndLedgers(t *testing.T) {
	s, a := fixture(t, nil)
	parent, err := a.Create("disposition diagnostic parent")
	if err != nil {
		t.Fatal(err)
	}
	child, err := a.Create("disposition diagnostic child")
	if err != nil {
		t.Fatal(err)
	}
	const taskID, workerID, requestID, callID = "inspect", "worker", "request", "call"
	visit := uint64(1)
	_, err = a.Store().Update(parent.Run.ID, func(v *agent.Snapshot) error {
		v.Run.Status = agent.StatusPaused
		v.Plan.Tasks = []agent.PlanTask{{ID: taskID, Goal: "inspect original effects", Status: "failed", WorkerID: workerID, ChildRunID: child.Run.ID}}
		v.Workers = []agent.AgentWorker{{ID: workerID, TaskID: taskID, RunID: child.Run.ID, Status: "failed", Visit: visit, SettledVisit: &visit, CleanupStatus: "completed"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Store().Update(child.Run.ID, func(v *agent.Snapshot) error {
		v.Run.ParentRunID, v.Run.ParentTaskID, v.Run.Status = parent.Run.ID, taskID, agent.StatusWaitingTool
		v.WorkerSettlement = &agent.WorkerSettlement{ParentRunID: parent.Run.ID, TaskID: taskID, WorkerID: workerID, RunID: child.Run.ID, Visit: visit, CleanupStatus: "completed", ObservedAt: time.Now().UTC()}
		v.Requests = []agent.ModelRequest{{ID: requestID}}
		v.Messages = []agent.Message{{Role: "assistant", RequestID: requestID, ToolCalls: []agent.ToolCall{{ID: callID, Name: "exec", Arguments: `{"command":"PRIVATE_ARGUMENT"}`}}}}
		v.Actions = []agent.Action{{RequestID: requestID, CallID: callID, Name: "exec", Adapter: "exec", Status: "uncertain", Result: &agent.ToolResult{Error: "original unresolved error", Stdout: "PRIVATE_OUTPUT", ExitCode: 1, OutcomeUncertain: true}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := make(map[string]agent.Snapshot)
	for _, id := range []string{parent.Run.ID, child.Run.ID} {
		before[id], err = a.Store().Load(id)
		if err != nil {
			t.Fatal(err)
		}
	}

	w := request(t, s.Handler(), http.MethodPost, "/api/runs/"+parent.Run.ID+"/tasks/"+taskID+"/dispose", map[string]string{"status": "abandoned", "reason": "inspect unresolved effects before disposition"}, http.StatusConflict)
	var response struct {
		Error      string `json:"error"`
		Diagnostic struct {
			Code       string                      `json:"code"`
			Summary    string                      `json:"summary"`
			NextAction string                      `json:"next_action"`
			Blockers   *agent.TaskDispositionError `json:"blockers"`
		} `json:"diagnostic"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	d, blocked := response.Diagnostic, response.Diagnostic.Blockers
	if d.Code != "task_disposition_blocked" || blocked == nil {
		t.Fatalf("missing structured disposition diagnostic: %s", w.Body.String())
	}
	if blocked.Code != "child_execution_open" || blocked.Reason != "child execution remains open" || response.Error != blocked.Error() {
		t.Fatalf("original error or blocker cause changed: %s", w.Body.String())
	}
	if d.Summary != blocked.Reason || d.NextAction == "" || d.NextAction != blocked.NextAction {
		t.Fatalf("missing actionable diagnostic: %s", w.Body.String())
	}
	if blocked.RunID != parent.Run.ID || blocked.TaskID != taskID || blocked.WorkerID != workerID || blocked.ChildRunID != child.Run.ID || blocked.ChildStatus != agent.StatusWaitingTool || !blocked.ChildIdentityMatches {
		t.Fatalf("blocker identities missing: %+v", blocked)
	}
	if blocked.Visit != visit || blocked.SettledVisit == nil || *blocked.SettledVisit != visit || blocked.SettlementVisit == nil || *blocked.SettlementVisit != visit || blocked.CleanupStatus != "completed" || blocked.SettlementCleanupStatus != "completed" {
		t.Fatalf("typed visit or cleanup fields missing: %+v", blocked)
	}
	if blocked.RequestID != requestID || blocked.CallsTotal != 1 || len(blocked.Calls) != 1 || blocked.Calls[0].CallID != callID || blocked.Calls[0].RequestID != requestID || blocked.Calls[0].Status != "uncertain" {
		t.Fatalf("unresolved call identity missing: %+v", blocked)
	}
	if strings.Contains(w.Body.String(), "PRIVATE_") {
		t.Fatal("diagnostic exposed private tool arguments or output")
	}
	for id, original := range before {
		after, err := a.Store().Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(original, after) {
			t.Fatalf("rejected disposition changed parent/child ledger %s", id)
		}
	}
}
