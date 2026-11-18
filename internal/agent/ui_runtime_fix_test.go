package agent

import (
	"strings"
	"testing"
	"time"
)

func TestExecDefinitionDisclosesActualThreadLimits(t *testing.T) {
	t.Setenv("TRACE_EXEC_RESOURCE_MODE", "cgroup")
	t.Setenv("TRACE_EXEC_PIDS", "37")
	t.Setenv("TRACE_EXEC_MEMORY_BYTES", "123456")
	d := execToolDefinition().Function.Description
	for _, want := range []string{"threads=37", "memory=123456", "ThreadPoolExecutor", "concurrency at most 16"} {
		if !strings.Contains(d, want) {
			t.Fatal("missing resource guidance", want, d)
		}
	}
	t.Setenv("TRACE_EXEC_RESOURCE_MODE", "process-group")
	d = execToolDefinition().Function.Description
	if strings.Contains(d, "threads=37") || !strings.Contains(d, "does not enforce") {
		t.Fatal(d)
	}
}

func TestToolObservationPreservesPausedOriginalAndNeverExecutes(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := seedPiBatch(t, a, store, []ToolCall{{ID: "call", Name: "exec", Arguments: `{"command":"must-not-run"}`}}, []ToolDefinition{execToolDefinition()})
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusPaused
		s.Run.PauseReason = "tool outcome uncertain; inspect side effects and submit its result manually"
		s.Actions = []Action{{RequestID: id + "-0001", CallID: "call", Name: "exec", Adapter: "exec", Status: "uncertain", Result: &ToolResult{ExitCode: 1, Error: "thread limit", OutcomeUncertain: true, WorkerCleanupError: "retained cleanup error"}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.RecordToolObservation(id, "old-request", "call", "observed"); err == nil {
		t.Fatal("stale identity accepted")
	}
	if _, err = a.RecordToolObservation(id, id+"-0001", "call", " "); err == nil {
		t.Fatal("empty observation accepted")
	}
	s, err := a.RecordToolObservation(id, id+"-0001", "call", "Observed failure; external state remains unknown")
	if err != nil {
		t.Fatal(err)
	}
	if s.Run.Status != StatusPaused || len(s.Requests) != 1 || s.Actions[0].Status != "resolved_manually" || !s.Actions[0].Result.OutcomeUncertain || s.Actions[0].Result.Error != "thread limit" || toolCleanupError(s) == nil {
		t.Fatalf("changed original or started execution: %+v", s)
	}
	if len(s.Evidence) != 1 || s.Evidence[0].Type != "manual_observation" || s.Evidence[0].Verification != "unverified" {
		t.Fatal(s.Evidence)
	}
	if _, err = a.RecordToolObservation(id, id+"-0001", "call", "duplicate"); err == nil {
		t.Fatal("already resolved call accepted")
	}
}

func TestDeleteWaitingChildRequiresCurrentSettlement(t *testing.T) {
	for _, op := range []string{"hide", "purge"} {
		for _, fault := range []string{"none", "receipt", "visit", "cleanup", "active", "tool"} {
			t.Run(op+"/"+fault, func(t *testing.T) {
				a, store := newTestAgent(t, MockProvider{})
				root, child := createTestRun(t, a), createTestRun(t, a)
				visit := uint64(2)
				w := AgentWorker{ID: "worker", TaskID: "task", RunID: child, Status: "uncertain", CleanupStatus: "completed", Visit: visit, SettledVisit: &visit}
				_, err := store.Update(root, func(s *Snapshot) error { s.Run.Status = StatusPaused; s.Workers = []AgentWorker{w}; return nil })
				if err != nil {
					t.Fatal(err)
				}
				_, err = store.Update(child, func(s *Snapshot) error {
					s.Run.Status = StatusWaitingTool
					s.Run.ParentRunID = root
					s.Run.ParentTaskID = "task"
					s.WorkerSettlement = &WorkerSettlement{ParentRunID: root, TaskID: "task", WorkerID: "worker", RunID: child, Visit: 2, CleanupStatus: "completed", ObservedAt: time.Now().UTC()}
					s.Actions = []Action{{Adapter: "exec", Status: "uncertain", Result: &ToolResult{OutcomeUncertain: true}}}
					switch fault {
					case "receipt":
						s.WorkerSettlement = nil
					case "visit":
						s.WorkerSettlement.Visit = 1
					case "cleanup":
						s.WorkerSettlement.CleanupStatus = "uncertain"
					case "active":
						s.Run.Status = StatusRunning
					case "tool":
						s.Actions[0].Result.WorkerCleanupError = "not settled"
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if op == "hide" {
					err = store.Delete(root)
				} else {
					_, err = store.PurgeRun(root)
				}
				if (err == nil) != (fault == "none") {
					t.Fatal("deletion settlement guard", fault, err)
				}
			})
		}
	}
}
