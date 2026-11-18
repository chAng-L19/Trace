package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestActivityRetainsOldOpenHelpAndSettledReportIdentity(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store *Store
			var err error
			if backend == "sqlite" {
				store = sqliteTestStore(t)
			} else {
				store, err = NewStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
			}
			parent, err := store.Create("parent", "prompt", "hash")
			if err != nil {
				t.Fatal(err)
			}
			parent, err = store.Update(parent.Run.ID, func(s *Snapshot) error {
				s.Workers = []AgentWorker{{ID: "live", TaskID: "live-task", RunID: "missing-live", Status: "paused", CleanupStatus: "completed"}, {ID: "done", TaskID: "done-task", RunID: "missing-done", Visit: 2, Status: "completed", CleanupStatus: "completed"}}
				s.ChildReports = []ChildReport{{ParentRunID: s.Run.ID, WorkerID: "done", TaskID: "done-task", SourceRunID: "missing-done", Visit: 2, SourceStatus: "available", Current: true}}
				s.WorkerMessages = []WorkerMessage{{ID: "old-open", From: "live", To: "parent", Kind: "help", Content: "still blocked", Delivered: true}, {ID: "pending", From: "parent", To: "live", Kind: "reply", ReplyTo: "old-open", Content: "pending reply"}, {ID: "old-closed", From: "live", To: "parent", Kind: "help", Delivered: true}, {ID: "resolution", From: "parent", To: "live", Kind: "resolved", ReplyTo: "old-closed", Delivered: true}}
				for i := 0; i < 45; i++ {
					s.WorkerMessages = append(s.WorkerMessages, WorkerMessage{ID: fmt.Sprint("progress-", i), From: "live", To: "parent", Kind: "report", Content: "progress", Delivered: true})
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			projection, err := store.ActivityWorkspace(parent.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(projection.Snapshot.WorkerMessages) != 40 || len(projection.Snapshot.Workers) != 1 {
				t.Fatal("body window not bounded")
			}
			if len(projection.HelpRequests) != 1 || projection.HelpRequests[0].Message.ID != "old-open" || projection.HelpRequests[0].Message.Content != "still blocked" || projection.Totals["open_help"] != 1 {
				t.Fatalf("old help lost: %+v", projection.HelpRequests)
			}
			report := projection.Snapshot.ChildReports[0]
			if !report.Current || report.SourceStatus != "not_checked" {
				t.Fatalf("display filtering revoked registered report: %+v", report)
			}
			rows := store.ActivityWorkerActivities(projection)
			if len(rows) != 1 || rows[0].PendingMessages != 1 || rows[0].OpenHelpTotal != 1 || !reflect.DeepEqual(rows[0].OpenHelpIDs, []string{"old-open"}) {
				t.Fatalf("full inbox counts lost: %+v", rows)
			}
		})
	}
}

func TestActivityHelpWindowKeepsTrueTotal(t *testing.T) {
	snap := Snapshot{Run: Run{ID: "root"}, Workers: []AgentWorker{{ID: "w", Status: "paused", CleanupStatus: "completed"}}}
	for i := 0; i < 25; i++ {
		snap.WorkerMessages = append(snap.WorkerMessages, WorkerMessage{ID: fmt.Sprint(i), From: "w", To: "parent", Kind: "help", Content: strings.Repeat("x", 5000), Delivered: true})
	}
	raw, _ := json.Marshal(snap)
	v := &storedView{}
	if err := v.retainSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	p, err := buildActivity(v.fields)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.HelpRequests) != 20 || p.Totals["open_help"] != 25 || p.WorkerInbox["w"].OpenHelpTotal != 25 || len(p.WorkerInbox["w"].OpenHelpIDs) != 20 {
		t.Fatal("bounded help replaced true total")
	}
	for _, h := range p.HelpRequests {
		if len(h.Message.Content) > len(excerpt(strings.Repeat("x", 5000), 2048)) {
			t.Fatal("help body unbounded")
		}
	}
}

func TestWorkerActivityHeadersPreserveFullHistoryTimesAndCalls(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	child := Snapshot{Run: Run{ID: "child", Status: StatusRunning}}
	for i := 0; i < 90; i++ {
		id := fmt.Sprint(i)
		child.Requests = append(child.Requests, ModelRequest{ID: id, Messages: []Message{{Content: strings.Repeat("private history", 100)}}})
		child.Responses = append(child.Responses, ResponseRecord{RequestID: id, CreatedAt: now.Add(-time.Duration(i) * time.Second), Response: ModelResponse{Content: strings.Repeat("body", 1000)}})
		child.Actions = append(child.Actions, Action{CallID: id, RequestID: id, Name: "exec", Status: "completed", FinishedAt: now.Add(-time.Duration(i) * time.Second), Result: &ToolResult{Stdout: strings.Repeat("stream", 1000)}})
	}
	child.Requests = append(child.Requests, ModelRequest{ID: "inflight"})
	child.Actions = append(child.Actions, Action{CallID: "active", RequestID: "inflight", Name: "exec", Status: "started", StartedAt: now, TimeoutSeconds: 300})
	raw, _ := json.Marshal(child)
	v := &storedView{}
	if err := v.retainSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	run, got, err := projectWorkerActivityFields(v.fields)
	if err != nil {
		t.Fatal(err)
	}
	want := projectWorkerActivity(child)
	if run.ID != child.Run.ID || !reflect.DeepEqual(got, want) || !got.LastModelResponse.Equal(now) || !got.LastToolResult.Equal(now) {
		t.Fatalf("header census changed lifecycle: got %+v want %+v", got, want)
	}
	// Irrelevant bodies are deliberately not decoded into product payload types.
	fields := map[string]json.RawMessage{"run": json.RawMessage(`{"id":"child","status":"running"}`), "requests": json.RawMessage(`[{"id":"r","messages":42,"tools":42}]`), "responses": json.RawMessage(`[{"request_id":"r","response":42}]`), "actions": json.RawMessage(`[{"call_id":"c","status":"uncertain","result":{"stdout":42,"worker_cleanup_error":"pending"}}]`)}
	_, row, err := projectWorkerActivityFields(fields)
	if err != nil || row.CallTotal != 1 || row.Calls[0].CleanupError != "pending" {
		t.Fatalf("activity decoded irrelevant historical body: %+v %v", row, err)
	}
}
