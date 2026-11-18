package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWorkerViewsBoundedAndHistoryReachable(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	snapshot, err := store.Update(id, func(s *Snapshot) error {
		for i := 0; i < 85; i++ {
			key := fmt.Sprint(i)
			s.Workers = append(s.Workers, AgentWorker{ID: key, Status: "completed", CleanupStatus: "completed"})
			s.WorkerMessages = append(s.WorkerMessages, WorkerMessage{ID: key, Kind: "help", From: "worker", To: "parent", Content: strings.Repeat("研究", 2000)})
		}
		s.Workers[0].Status = "running"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(snapshot)
	view := compactWorkerState(snapshot)
	if view["worker_total"] != 85 || view["pending_message_total"] != 85 || view["open_help_total"] != 85 || len(view["workers"].([]AgentWorker)) != workerViewLimit || len(view["messages"].([]WorkerMessage)) != workerViewLimit {
		t.Fatalf("unbounded projection: %+v", view)
	}
	if view["workers"].([]AgentWorker)[0].ID != "0" {
		t.Fatal("live worker omitted behind history")
	}
	for _, m := range view["messages"].([]WorkerMessage) {
		if len(m.Content) > 4096 {
			t.Fatal("unbounded body")
		}
	}
	seen := 0
	for offset := 0; offset < 85; {
		page, err := a.workerHistory(id, WorkerHistoryQuery{Kind: "messages", Offset: offset, Limit: 17, OpenHelp: true})
		if err != nil {
			t.Fatal(err)
		}
		rows := page.Items.([]WorkerMessage)
		if page.Total != 85 || len(rows) != 17 || len(rows[0].Content) != len(snapshot.WorkerMessages[0].Content) {
			t.Fatal("history lost body or unresolved help")
		}
		seen += len(rows)
		offset = page.NextOffset
	}
	if seen != 85 {
		t.Fatal("history incomplete")
	}
	page, err := a.workerHistory(id, WorkerHistoryQuery{Kind: "workers", Offset: int(^uint(0) >> 1), Limit: 50})
	if err != nil || len(page.Items.([]AgentWorker)) != 0 || page.NextOffset != 85 {
		t.Fatalf("large offset: %+v %v", page, err)
	}
	for _, query := range []WorkerHistoryQuery{{Kind: "messages", Offset: -1}, {Kind: "messages", Limit: 51}, {Kind: "workers", OpenHelp: true}, {Kind: "unknown"}} {
		if _, err := a.workerHistory(id, query); err == nil {
			t.Fatalf("accepted invalid query: %+v", query)
		}
	}
	after, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(after)
	if string(before) != string(raw) {
		t.Fatal("read-only projection changed canonical ledger")
	}
}

func TestWorkerMessageToolSingleReceiptAndFailure(t *testing.T) {
	a, parent, child := schedulingInbox(t, true, 0, 0)
	if _, err := a.store.Update(parent, func(s *Snapshot) error {
		for i := 0; i < 100; i++ {
			s.WorkerMessages = append(s.WorkerMessages, WorkerMessage{ID: fmt.Sprint(i), Content: strings.Repeat("old body", 1000), Delivered: true})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	call := ToolCall{Name: "worker_message", Arguments: `{"to":"parent","content":"fresh","operation_id":"single-receipt"}`}
	var receipt WorkerMessage
	for i := 0; i < 2; i++ {
		value, err := a.builtin(context.Background(), child, call)
		if err != nil {
			t.Fatal(err)
		}
		result := value.(map[string]any)
		current := result["receipt"].(WorkerMessage)
		if result["created"] != (i == 0) || result["processed"] != false || current.Content != "fresh" {
			t.Fatalf("wrong receipt: %+v", result)
		}
		if i > 0 && !reflect.DeepEqual(receipt, current) {
			t.Fatal("retry receipt changed")
		}
		receipt = current
		raw, _ := json.Marshal(value)
		if len(raw) > 1500 || strings.Contains(string(raw), "old body") {
			t.Fatalf("history echoed in receipt: %d", len(raw))
		}
	}
	call.Arguments = `{"to":"missing","content":"invalid"}`
	value, err := a.builtin(context.Background(), child, call)
	if err == nil || value != nil {
		t.Fatalf("error returned false accepted receipt: %+v %v", value, err)
	}
}

func TestWorkerHistoryParentRequiresRegisteredIdentity(t *testing.T) {
	a, parent, child := schedulingInbox(t, true, 0, 0)
	if _, err := a.SendWorkerMessageRelated(parent, "worker", "parent", "shared question", "help", ""); err != nil {
		t.Fatal(err)
	}
	page, err := a.workerHistory(child, WorkerHistoryQuery{Kind: "messages", Parent: true, OpenHelp: true})
	if err != nil || page.Total != 1 || page.Items.([]WorkerMessage)[0].Content != "shared question" {
		t.Fatalf("shared inbox unavailable: %+v %v", page, err)
	}
	if _, err = a.workerHistory(parent, WorkerHistoryQuery{Kind: "messages", Parent: true}); err == nil {
		t.Fatal("root accepted missing parent")
	}
	if _, err = a.store.Update(parent, func(s *Snapshot) error { s.Workers[0].TaskID = "other"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = a.workerHistory(child, WorkerHistoryQuery{Kind: "messages", Parent: true}); err == nil {
		t.Fatal("unregistered parent ledger disclosed")
	}
}

func TestCompactWorkerActivityKeepsHelpTotal(t *testing.T) {
	a, parent, _ := schedulingInbox(t, true, 0, 0)
	s, err := a.store.Update(parent, func(s *Snapshot) error {
		for i := 0; i < 45; i++ {
			s.WorkerMessages = append(s.WorkerMessages, WorkerMessage{ID: fmt.Sprintf("help-%d", i), From: "worker", To: "parent", Kind: "help"})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rows := a.compactWorkerActivity(s)["worker_activity"].([]WorkerActivity)
	if len(rows) != 1 || rows[0].OpenHelpTotal != 45 || len(rows[0].OpenHelpIDs) != 20 {
		t.Fatalf("activity help view: %+v", rows)
	}
	full := a.store.WorkerActivities(s)
	if len(full[0].OpenHelpIDs) != 45 {
		t.Fatal("compact view mutated full activity")
	}
}

func TestCompactPlanBoundsHistoryAndKeepsActiveDependency(t *testing.T) {
	p := PlanState{Capacity: 1, Frontier: []string{"next"}}
	for i := 0; i < 100; i++ {
		p.Tasks = append(p.Tasks, PlanTask{ID: fmt.Sprint(i), Status: "failed"})
	}
	p.Tasks[0].Status = "verified"
	p.Tasks[0].EvidenceRunID = "source"
	p.Tasks = append(p.Tasks, PlanTask{ID: "next", Status: "pending", Dependencies: []string{"0"}}, PlanTask{ID: "slow", Status: "running", LeaseUntil: time.Now().Add(time.Hour)})
	before, _ := json.Marshal(p)
	view := compactPlan(p)
	rows := view["tasks"].([]PlanTask)
	if len(rows) != 23 || view["total"] != 102 || view["history_tool"] != "worker_history" {
		t.Fatalf("wrong bounded view: %d %+v", len(rows), view)
	}
	if rows[0].ID != "0" || rows[0].EvidenceRunID != "source" || rows[len(rows)-1].ID != "slow" || !rows[len(rows)-1].LeaseUntil.IsZero() {
		t.Fatal("dependency/active identity lost")
	}
	after, _ := json.Marshal(p)
	if string(before) != string(after) {
		t.Fatal("projection mutated plan")
	}
}
