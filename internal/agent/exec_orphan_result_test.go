package agent

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestExecPIDFailureReturnsWithoutPausing(t *testing.T) {
	t.Setenv("TRACE_EXEC_PIDS", "16")
	a, store := newTestAgent(t, MockProvider{})
	id := seedPiBatch(t, a, store, []ToolCall{{ID: "call", Name: "exec", Arguments: `{}`}}, []ToolDefinition{execToolDefinition()})
	s, err := a.executeCommand(context.Background(), id, "call", []string{"python3", "-c", `import concurrent.futures,threading
gate=threading.Event()
with concurrent.futures.ThreadPoolExecutor(max_workers=200) as pool:
    try:
        for _ in range(200): pool.submit(gate.wait)
    finally: gate.set()
`})
	if err != nil {
		t.Fatal(err)
	}
	r := s.Actions[len(s.Actions)-1].Result
	if s.Run.Status == StatusPaused || r == nil || r.ExitCode != 1 || r.WorkerStatus != "failed" || r.OutcomeUncertain || r.WorkerOutcomeUncertain || r.WorkerResourceLimit == "" || r.Diagnostic == nil || r.Diagnostic.Code != "exec_resource_limit" {
		t.Fatalf("known PID failure incorrectly paused or lost diagnostics: %+v %+v", s.Run, r)
	}
}

func TestExecOrphanResultRecovery(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			id := createTestRun(t, a)
			_, err := store.Update(id, func(s *Snapshot) error {
				s.Run.Status = StatusPaused
				s.Run.PauseReason = "operator_pause"
				s.Actions = []Action{
					{RequestID: "legacy", CallID: "legacy", Name: "exec", Adapter: "exec", Status: "started", StartedAt: time.Now().Add(-time.Hour)},
					{RequestID: "r", CallID: "ready", Adapter: "exec", Status: "result_ready", Result: &ToolResult{Stdout: "saved"}},
					{RequestID: "r", CallID: "manual", Adapter: "exec", Status: "resolved_manually"},
					{RequestID: "r", CallID: "returned", Adapter: "exec", Status: "started", Result: &ToolResult{ExitCode: 1}},
					{RequestID: "r", CallID: "local", Adapter: "local", Status: "started"},
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if err = a.RecoverExecResources(); err != nil {
				t.Fatal(err)
			}
			after, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			want := before.Actions[0]
			want.Status = "uncertain"
			if !reflect.DeepEqual(want, after.Actions[0]) || !reflect.DeepEqual(before.Actions[1:], after.Actions[1:]) || !reflect.DeepEqual(before.Evidence, after.Evidence) || after.Run.Status != StatusPaused || after.Run.PauseReason != "operator_pause" || len(after.Events) != len(before.Events)+1 {
				t.Fatalf("recovery changed outcome, cleanup, time or unrelated actions: %+v", after.Actions)
			}
			event := after.Events[len(after.Events)-1]
			if event.Type != "action_result_missing" || event.CreatedAt.IsZero() || toolCleanupError(after) == nil {
				t.Fatalf("missing observation or fake cleanup: %+v", event)
			}
			writes := 0
			store.beforePersist = func(string) { writes++ }
			err = a.RecoverExecResources()
			store.beforePersist = nil
			if err != nil || writes != 0 {
				t.Fatalf("recovery not idempotent: writes=%d err=%v", writes, err)
			}
		})
	}
}

func TestExecOrphanResultBusyLockNoWrites(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			id := createTestRun(t, a)
			_, err := store.Update(id, func(s *Snapshot) error {
				s.Actions = []Action{{Name: "exec", Adapter: "exec", Status: "started"}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			release, err := store.AcquireRunLock(id)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if err = a.RecoverExecResources(); err != nil {
				t.Fatal(err)
			}
			after, err := store.Load(id)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("active execution modified", err)
			}
		})
	}
}
