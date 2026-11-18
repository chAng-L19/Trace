package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func dispositionFixFixture(t *testing.T, backend string) (*Agent, *Store, string, AgentWorker) {
	t.Helper()
	a, store := treeCapacityAgent(t, backend, MockProvider{})
	parent := createTestRun(t, a)
	if _, err := a.SetPlan(parent, []PlanTask{{ID: "task", Goal: "inspect original effect"}}); err != nil {
		t.Fatal(err)
	}
	child, err := a.Create("inspect original effect")
	if err != nil {
		t.Fatal(err)
	}
	visit := uint64(1)
	w := AgentWorker{ID: "worker", TaskID: "task", RunID: child.Run.ID, Status: "failed", Visit: visit, SettledVisit: &visit, CleanupStatus: "completed"}
	if _, err = store.Update(w.RunID, func(s *Snapshot) error {
		s.Run.ParentRunID, s.Run.ParentTaskID, s.Run.Status = parent, w.TaskID, StatusFailed
		s.Run.Error = "original business failure"
		s.WorkerSettlement = &WorkerSettlement{ParentRunID: parent, TaskID: w.TaskID, WorkerID: w.ID, RunID: w.RunID, Visit: visit, CleanupStatus: "completed", ObservedAt: time.Now().UTC()}
		s.Requests = []ModelRequest{{ID: "request"}}
		s.Messages = append(s.Messages, Message{Role: "assistant", RequestID: "request", ToolCalls: []ToolCall{{ID: "call", Name: "exec", Arguments: `{"command":"PRIVATE_ARGUMENT"}`}}}, Message{Role: "tool", ToolCallID: "call", Content: "PRIVATE_OUTPUT"})
		s.Actions = []Action{{RequestID: "request", CallID: "call", Name: "exec", Status: "completed", Arguments: "PRIVATE_ARGUMENT", FinishedAt: time.Now().UTC(), Result: &ToolResult{Stdout: "PRIVATE_OUTPUT", ExitCode: 1}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(parent, func(s *Snapshot) error {
		s.Plan.Tasks[0].Status, s.Plan.Tasks[0].WorkerID, s.Plan.Tasks[0].ChildRunID = "failed", w.ID, w.RunID
		s.Workers = []AgentWorker{w}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return a, store, parent, w
}

func TestToolDispositionStructuredBlockersNoWrites(t *testing.T) {
	cases := []struct {
		name, code string
		parent     func(*Snapshot)
		child      func(*Snapshot)
	}{
		{name: "active_lock", code: "child_active"},
		{name: "task_running", code: "task_running", parent: func(s *Snapshot) { s.Plan.Tasks[0].Status = "running" }},
		{name: "worker_running", code: "worker_unsettled", parent: func(s *Snapshot) { s.Workers[0].Status = "running" }},
		{name: "cleanup_unknown", code: "worker_unsettled", parent: func(s *Snapshot) { s.Workers[0].CleanupStatus = "unknown" }},
		{name: "old_visit", code: "child_settlement_uncommitted", parent: func(s *Snapshot) { s.Workers[0].Visit++ }},
		{name: "old_receipt_identity", code: "child_settlement_uncommitted", child: func(s *Snapshot) { s.WorkerSettlement.WorkerID = "other-worker" }},
		{name: "child_identity", code: "child_identity_mismatch", child: func(s *Snapshot) { s.Run.ParentTaskID = "other-task" }},
		{name: "unregistered", code: "child_identity_mismatch", parent: func(s *Snapshot) { s.Workers[0].ID = "other-worker" }},
		{name: "waiting_tool", code: "child_execution_open", child: func(s *Snapshot) {
			s.Run.Status = StatusWaitingTool
			s.Messages = s.Messages[:len(s.Messages)-1]
			s.Actions[0].Status = "uncertain"
		}},
		{name: "uncertain", code: "child_action_uncertain", child: func(s *Snapshot) { s.Actions[0].Status = "uncertain" }},
		{name: "started", code: "child_action_uncertain", child: func(s *Snapshot) { s.Actions[0].Status = "started" }},
		{name: "tool_cleanup", code: "descendants_unsettled", child: func(s *Snapshot) { s.Actions[0].ResourceCleanupError = "cleanup unknown" }},
		{name: "cleanup_requirement", code: "child_cleanup_unresolved", child: func(s *Snapshot) { s.Requirements = []VerificationRequirement{{ID: "cleanup", Category: "cleanup"}} }},
		{name: "manual_uncertain", code: "manual_outcome_uncertain", parent: func(s *Snapshot) {
			s.Plan.Tasks[0].Status, s.Plan.Tasks[0].ChildRunID = "uncertain", ""
			s.Workers = nil
		}},
	}
	for _, backend := range []string{"json", "sqlite"} {
		for _, tc := range cases {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				a, store, parentID, w := dispositionFixFixture(t, backend)
				for _, update := range []struct {
					id string
					fn func(*Snapshot)
				}{{parentID, tc.parent}, {w.RunID, tc.child}} {
					if update.fn != nil {
						if _, err := store.Update(update.id, func(s *Snapshot) error { update.fn(s); return nil }); err != nil {
							t.Fatal(err)
						}
					}
				}
				if tc.name == "active_lock" {
					release, err := store.AcquireRunLock(w.RunID)
					if err != nil {
						t.Fatal(err)
					}
					defer release()
				}
				beforeParent, err := store.Load(parentID)
				if err != nil {
					t.Fatal(err)
				}
				beforeChild, err := store.Load(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				writes := 0
				store.beforePersist = func(string) { writes++ }
				_, err = a.DisposeTask(parentID, w.TaskID, "abandoned", "original attempt inspected", "")
				store.beforePersist = nil
				var blocked *TaskDispositionError
				if !errors.As(err, &blocked) || blocked.Code != tc.code || blocked.RunID != parentID || blocked.TaskID != w.TaskID || blocked.NextAction == "" {
					t.Fatalf("missing structured blocker: %+v %v", blocked, err)
				}
				if tc.name == "active_lock" && !errors.Is(err, ErrRunLockBusy) {
					t.Fatal("active lock cause lost")
				}
				if tc.name == "old_visit" && (blocked.Visit != 2 || blocked.SettledVisit == nil || *blocked.SettledVisit != 1 || blocked.SettlementVisit == nil || *blocked.SettlementVisit != 1) {
					t.Fatalf("visit mismatch concealed: %+v", blocked)
				}
				if tc.name == "waiting_tool" && (blocked.ChildStatus != StatusWaitingTool || blocked.RequestID != "request" || len(blocked.Calls) != 1 || blocked.Calls[0].CallID != "call" || blocked.Calls[0].Status != "uncertain") {
					t.Fatalf("pending call identity missing: %+v", blocked)
				}
				if tc.name == "child_identity" && (blocked.ChildIdentityMatches || blocked.RequestID != "" || len(blocked.Calls) != 0) {
					t.Fatalf("unrelated child details exposed: %+v", blocked)
				}
				data, marshalErr := json.Marshal(blocked)
				var payload map[string]any
				if marshalErr != nil || json.Unmarshal(data, &payload) != nil || payload["code"] != tc.code || payload["next_action"] == nil || payload["visit"] == nil || payload["cleanup_status"] == nil {
					t.Fatalf("invalid JSON error: %s %v", data, marshalErr)
				}
				if strings.Contains(string(data), "PRIVATE_") || !strings.Contains(err.Error(), string(data)) {
					t.Fatal("diagnostic exposed tool data or lost JSON rendering")
				}
				afterParent, parentErr := store.Load(parentID)
				afterChild, childErr := store.Load(w.RunID)
				if writes != 0 || parentErr != nil || childErr != nil || valueHash(beforeParent) != valueHash(afterParent) || valueHash(beforeChild) != valueHash(afterChild) {
					t.Fatalf("rejection wrote state: writes=%d parent=%v child=%v", writes, parentErr, childErr)
				}
			})
		}
	}
}

func TestToolDispositionBoundedDiagnosticsPreserveCause(t *testing.T) {
	parent := Snapshot{Run: Run{ID: "parent"}}
	child := Snapshot{Run: Run{ID: "child", ParentRunID: "parent", ParentTaskID: "task", Status: StatusWaitingTool}, Requests: []ModelRequest{{ID: "current"}}}
	nested := &ToolDiagnostic{Code: "nested", Blockers: &TaskDispositionError{Reason: strings.Repeat("PRIVATE_OUTPUT", 1000)}}
	for i := 0; i < 25; i++ {
		child.Actions = append(child.Actions, Action{RequestID: "old", CallID: fmt.Sprintf("old-%d", i), Name: strings.Repeat("界", 200), Status: "uncertain", Result: &ToolResult{WorkerCleanupError: "PRIVATE_OUTPUT", Diagnostic: nested}})
	}
	child.Messages = []Message{{ID: "batch", Role: "assistant", RequestID: "current", ToolCalls: []ToolCall{{ID: "not-started", Name: "exec", Arguments: "PRIVATE_ARGUMENT"}}}}
	cause := &os.PathError{Op: "read", Path: "fixture", Err: os.ErrPermission}
	blocked := newTaskDispositionError(&parent, PlanTask{ID: "task", ChildRunID: "child"}, &child, nil, "child_unavailable", cause)
	var pathError *os.PathError
	if !errors.As(blocked, &pathError) || pathError != cause || !errors.Is(blocked, os.ErrPermission) {
		t.Fatal("original typed cause lost")
	}
	data, err := json.Marshal(blocked)
	if err != nil || len(blocked.Calls) != 20 || blocked.CallsTotal != 26 || len(data) > 16000 || strings.Contains(string(data), "PRIVATE_") {
		t.Fatalf("unbounded or secret diagnostic: calls=%d total=%d bytes=%d err=%v", len(blocked.Calls), blocked.CallsTotal, len(data), err)
	}
	for _, call := range blocked.Calls {
		if call.RequestID != "old" || len(call.Name) > 256 || !utf8.ValidString(call.Name) || call.Diagnostic != nil {
			t.Fatalf("old identity rebased or invalid text: %+v", call)
		}
	}
	if child.Actions[0].Result.Diagnostic != nested {
		t.Fatal("diagnostic projection mutated original call")
	}
	child.Actions = nil
	blocked = newTaskDispositionError(&parent, PlanTask{ID: "task", ChildRunID: "child"}, &child, nil, "child_execution_open", cause)
	if len(blocked.Calls) != 1 || blocked.Calls[0].RequestID != "current" || blocked.Calls[0].CallID != "not-started" || blocked.Calls[0].Status != "pending" {
		t.Fatalf("not-started batch omitted: %+v", blocked)
	}
}

func TestToolDispositionSettledAttemptAndIdempotence(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, parentID, w := dispositionFixFixture(t, backend)
			childBefore, err := store.Load(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			var first PlanTask
			for i := 0; i < 2; i++ {
				s, err := a.DisposeTask(parentID, w.TaskID, "abandoned", "original attempt inspected", "")
				if err != nil || s.Plan.Tasks[0].Status != "abandoned" {
					t.Fatalf("settled disposition: %+v %v", s.Plan, err)
				}
				if i == 0 {
					first = s.Plan.Tasks[0]
				} else if valueHash(first) != valueHash(s.Plan.Tasks[0]) {
					t.Fatal("idempotent task disposition changed")
				}
				count := 0
				for _, event := range s.Events {
					if event.Type == "task_disposed" {
						count++
					}
				}
				if count != 1 {
					t.Fatalf("duplicate disposition events: %d", count)
				}
			}
			childAfter, err := store.Load(w.RunID)
			if err != nil || valueHash(childBefore) != valueHash(childAfter) {
				t.Fatalf("child history changed: %v", err)
			}
			_, err = a.DisposeTask(parentID, w.TaskID, "blocked", "different reason", "")
			var blocked *TaskDispositionError
			if !errors.As(err, &blocked) || blocked.Code != "disposition_immutable" {
				t.Fatalf("disposition became mutable: %v", err)
			}
		})
	}
}
