package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"trace/internal/worker"
)

func TestToolDiagnosticLegacyEvidenceBytes(t *testing.T) {
	const raw = `{"worker_resource_limit":"pids.events:max 1","stdout":"partial","stderr":"thread error","exit_code":1,"error":"exit status 1","worker_status":"failed","outcome_uncertain":true}`
	var result ToolResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	result.executionEndedAt = time.Now()
	got, err := json.Marshal(result)
	if err != nil || string(got) != raw {
		t.Fatalf("legacy bytes changed: %s %v", got, err)
	}
	result.Diagnostic = &ToolDiagnostic{Code: "task_disposition_blocked", Summary: "blocked", NextAction: "inspect", Blockers: &TaskDispositionError{Code: "child_execution_open", RunID: "parent", TaskID: "task", NextAction: "inspect child", Calls: []WorkerCallActivity{{CallID: "call", Status: "uncertain"}}}}
	first, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded ToolResult
	if err = json.Unmarshal(first, &reloaded); err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(reloaded)
	if err != nil || string(first) != string(second) {
		t.Fatalf("new blocker bytes changed: %s != %s: %v", first, second, err)
	}
}

func TestToolDiagnosticResourceGuidanceUsesActualLimits(t *testing.T) {
	for _, tc := range []struct{ pids, want int64 }{{64, 16}, {12, 5}, {2, 1}, {1, 1}} {
		limits := &worker.ResourceLimits{Mode: worker.ResourceModeCgroup, PIDs: tc.pids, MemoryBytes: 123456, OutputBytes: 4096}
		d := execResultDiagnostic(ToolResult{WorkerResourceLimit: "pids.events:max 1"}, limits)
		if d.PIDsLimit != tc.pids || d.RecommendedConcurrency != tc.want || d.MemoryLimitBytes != 123456 || d.OutputLimitBytes != 4096 {
			t.Fatalf("%+v", d)
		}
		if !strings.Contains(execResourceEnvironment(limits)[0], "CONCURRENCY=") {
			t.Fatal("missing child environment")
		}
	}
	d := execResultDiagnostic(ToolResult{WorkerResourceLimit: "output_bytes"}, &worker.ResourceLimits{Mode: worker.ResourceModeProcessGroup, PIDs: 64, MemoryBytes: 123456, OutputBytes: 4096})
	if d.PIDsLimit != 0 || d.MemoryLimitBytes != 0 || d.ResourceMode != worker.ResourceModeProcessGroup {
		t.Fatalf("claimed hard isolation: %+v", d)
	}
	if execResultDiagnostic(ToolResult{ExitCode: 1}, &worker.ResourceLimits{}) != nil || scriptResultDiagnostic(ToolResult{ExitCode: 2, WorkerStatus: worker.StatusFailed, Stderr: "usage: command"}) != nil {
		t.Fatal("exit code inferred cause")
	}
	limits := &worker.ResourceLimits{Mode: worker.ResourceModeCgroup, MemoryBytes: 123456, OutputBytes: 4096, PIDs: 64}
	for _, tc := range []struct{ event, want, absent string }{{"memory.events:oom 1", "OOM", "线程达到"}, {"output_bytes", "4096", "OOM"}, {"pids.events:max 1", "64", "OOM"}} {
		d := execResultDiagnostic(ToolResult{WorkerResourceLimit: tc.event}, limits)
		if !strings.Contains(d.Summary, tc.want) || strings.Contains(d.Summary, tc.absent) {
			t.Fatalf("%+v", d)
		}
	}
}

func TestToolDiagnosticParentPauseCurrentEvent(t *testing.T) {
	s := Snapshot{Run: Run{ID: "parent", Status: StatusPaused, PauseReason: "operator_pause"}, Events: []Event{{Sequence: 1, Type: "run_paused"}, {Sequence: 2, Type: "run_started"}}}
	if _, ok := parentPauseEventData(s)["parent_event_sequence"]; ok {
		t.Fatal("old pause from prior execution reused")
	}
	s.Events = append(s.Events, Event{Sequence: 3, Type: "action_uncertain"})
	s.Run.PauseReason = "tool outcome uncertain; inspect side effects and submit its result manually"
	if parentPauseEventData(s)["parent_event_sequence"] != 3 {
		t.Fatal("missing current pause trigger")
	}
	s.Events = append(s.Events, Event{Sequence: 4, Type: "budget_paused"})
	if _, ok := parentPauseEventData(s)["parent_event_sequence"]; ok {
		t.Fatal("newer different pause reason ignored")
	}
	s.Run.Status = StatusRunning
	if _, ok := parentPauseEventData(s)["parent_event_sequence"]; ok {
		t.Fatal("running parent claimed paused event")
	}
}

func TestToolDiagnosticControlExecutionWindow(t *testing.T) {
	start := time.Now().UTC()
	end := start.Add(time.Second)
	action := Action{StartedAt: start, FinishedAt: end.Add(time.Hour), Result: &ToolResult{WorkerStatus: worker.StatusCancelled, executionEndedAt: end}}
	s := Snapshot{Run: Run{ID: "child"}, Events: []Event{{Sequence: 9, Type: "run_paused", CreatedAt: start.Add(time.Millisecond), Data: map[string]any{"parent_run_id": "parent", "parent_event_sequence": 7}}}}
	for _, seq := range []any{7, float64(7)} {
		s.Events[0].Data.(map[string]any)["parent_event_sequence"] = seq
		d := controlResultDiagnostic(&s, action)
		if d == nil || d.ControlRunID != "child" || d.ControlEventSequence != 9 || d.TriggerRunID != "parent" || d.TriggerEventSequence != 7 {
			t.Fatalf("%+v", d)
		}
	}
	for _, at := range []time.Time{start.Add(-time.Nanosecond), end.Add(time.Nanosecond), action.FinishedAt} {
		s.Events[0].CreatedAt = at
		if controlResultDiagnostic(&s, action) != nil {
			t.Fatal("event outside real execution window attributed")
		}
	}
	s.Events[0].CreatedAt = start
	action.Result.executionEndedAt = time.Time{}
	if controlResultDiagnostic(&s, action) != nil {
		t.Fatal("legacy archived finish used as process boundary")
	}
	action.Result.executionEndedAt = end
	action.Result.WorkerStatus = worker.StatusFailed
	if controlResultDiagnostic(&s, action) != nil {
		t.Fatal("ordinary failure attributed")
	}
	action.Result.WorkerStatus = worker.StatusCancelled
	s.Events = nil
	if controlResultDiagnostic(&s, action) != nil {
		t.Fatal("pause inferred from current state")
	}
}

func TestToolDiagnosticExecEnvironmentAndScriptFeedback(t *testing.T) {
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("server Python fixture")
	}
	for _, tc := range []struct{ name, code, diagnostic string }{
		{"bounded", "import os,concurrent.futures\nn=int(os.environ['TRACE_EXEC_RECOMMENDED_CONCURRENCY'])\nassert 1<=n<=16\nwith concurrent.futures.ThreadPoolExecutor(max_workers=n) as p: assert list(p.map(lambda i:i*i,range(32)))==[i*i for i in range(32)]\nprint('bounded-ok')", ""},
		{"percent-failure", "print('/x%3Fv?s=%s' % 'value')", "script_type_error"},
		{"percent-fixed", "value='value'\nassert '/x%3Fv?s='+value == f'/x%3Fv?s={value}'\nprint('format-ok')", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, store := newTestAgent(t, MockProvider{})
			id := seedPiBatch(t, a, store, []ToolCall{{ID: "call", Name: "exec", Arguments: `{}`}}, []ToolDefinition{execToolDefinition()})
			s, err := a.executeCommand(context.Background(), id, "call", []string{"/usr/bin/python3", "-c", tc.code})
			if err != nil {
				t.Fatal(err)
			}
			result := s.Actions[len(s.Actions)-1].Result
			if tc.diagnostic != "" {
				if result.Diagnostic == nil || result.Diagnostic.Code != tc.diagnostic || result.ExitCode != 1 || !strings.Contains(result.Stderr, "must be real number, not str") {
					t.Fatalf("%+v", result)
				}
			} else if result.ExitCode != 0 || result.Error != "" || result.Diagnostic != nil {
				t.Fatalf("%+v", result)
			}
		})
	}
}

func TestToolDiagnosticActivityProjectionReadOnly(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			s, err := a.Create("diagnostic projection")
			if err != nil {
				t.Fatal(err)
			}
			d := &ToolDiagnostic{Code: "control_paused", Summary: "saved summary", NextAction: "inspect"}
			_, err = store.Update(s.Run.ID, func(s *Snapshot) error {
				s.Actions = []Action{{CallID: "call", Name: "exec", Status: "uncertain", Result: &ToolResult{Diagnostic: d}}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			row, err := store.workerActivityContext(context.Background(), s.Run.ID)
			if err != nil || len(row.row.Calls) != 1 || !reflect.DeepEqual(row.row.Calls[0].Diagnostic, d) {
				t.Fatalf("%+v %v", row, err)
			}
		})
	}
}

func TestToolDiagnosticBuiltinDispositionFeedback(t *testing.T) {
	a, store, parent, w := dispositionFixFixture(t, "json")
	call := ToolCall{ID: "dispose", Name: "task_dispose"}
	// Read the actual fixture task ID rather than substituting an identity.
	s, err := store.Load(parent)
	if err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]string{"task_id": s.Plan.Tasks[0].ID, "status": "abandoned", "reason": "stop attempt"})
	call.Arguments = string(args)
	_, err = store.Update(w.RunID, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Actions[0].Status = "uncertain"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(parent, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Messages = append(s.Messages, Message{ID: "dispose-batch", Role: "assistant", ToolCalls: []ToolCall{call}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(store.root, w.RunID+".json")
	before, err := os.ReadFile(childPath)
	if err != nil {
		t.Fatal(err)
	}
	s, err = a.executeBuiltin(context.Background(), parent, call)
	if err != nil {
		t.Fatal(err)
	}
	r := s.Actions[len(s.Actions)-1].Result
	if r.Diagnostic == nil || r.Diagnostic.Code != "task_disposition_blocked" || r.Error == "" || r.ExitCode != 1 {
		t.Fatalf("%+v", r)
	}
	after, _ := os.ReadFile(childPath)
	if string(before) != string(after) {
		t.Fatal("blocked child mutated")
	}
}
