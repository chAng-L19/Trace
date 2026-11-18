package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"trace/internal/worker"
)

func TestExecTimeoutConfigurationAndLegacy(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	fresh, err := a.Create("timeout defaults")
	if err != nil || fresh.Run.Exec == nil || fresh.Run.Exec.TimeoutSeconds != 300 {
		t.Fatalf("default: %+v %v", fresh.Run.Exec, err)
	}
	saved, err := store.Load(fresh.Run.ID)
	if err != nil || saved.Run.Exec.TimeoutSeconds != 300 {
		t.Fatal("default not persisted", err)
	}
	for _, raw := range []string{`{"timeout_seconds":0}`, `{"timeout_seconds":-1}`, `{"timeout_seconds":86401}`, `{"timeout_seconds":1.5}`, `{"timeout_seconds":"3"}`} {
		if _, err := execTimeout(saved.Run, raw); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if n, err := execTimeout(saved.Run, `{"timeout_seconds":2}`); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	var legacy Run
	if err := json.Unmarshal([]byte(`{"id":"legacy"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if n, err := execTimeout(legacy, `{}`); err != nil || n != 0 {
		t.Fatal("legacy acquired implicit timeout", n, err)
	}
	if _, err := a.CreateConfigured("bad config", RunConfiguration{Exec: &ExecConfig{TimeoutSeconds: 0}}); err == nil {
		t.Fatal("zero config accepted")
	}
}

func TestExecParentDeadlineUncertainAndLegacyCompatibility(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "bounded", true: "legacy"}[legacy], func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id := createTestRun(t, a)
			call := ToolCall{ID: "parent-deadline", Name: "exec", Arguments: `{"command":"printf partial; sleep 30"}`}
			_, err := store.Update(id, func(s *Snapshot) error {
				if legacy {
					s.Run.Exec = nil
				}
				s.Run.Status = StatusWaitingTool
				s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			s, err := a.executePendingTool(ctx, id, call)
			if err != nil || len(s.Actions) != 1 || s.Actions[0].Result == nil {
				t.Fatalf("missing interrupted action: %+v %v", s.Actions, err)
			}
			result := s.Actions[0].Result
			if result.WorkerStatus != worker.StatusTimedOut || !result.Interrupted || !result.WorkerOutcomeUncertain {
				t.Fatalf("worker state lost: %+v", result)
			}
			if result.OutcomeUncertain == legacy {
				t.Fatalf("boundary compatibility legacy=%v: %+v", legacy, result)
			}
			if !legacy && (s.Actions[0].Status != "uncertain" || len(pendingToolCalls(&s)) != 0 || len(s.Evidence) != 1 || s.Evidence[0].Verification == "verified") {
				t.Fatal("parent deadline outcome was not returned unverified")
			}
			if legacy && len(pendingToolCalls(&s)) != 0 {
				t.Fatal("legacy interrupted-result behavior changed")
			}
			reopened, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := reopened.Load(id)
			if err != nil || saved.Actions[0].Result.OutcomeUncertain != result.OutcomeUncertain {
				t.Fatal("outcome not durable", err)
			}
		})
	}
}

func TestExecTimeoutRetainsPartialCASAndNeverReplays(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group fixture")
	}
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	call := ToolCall{ID: "bounded", Name: "exec", Arguments: `{"command":"printf partial; head -c 20000 /dev/zero; sleep 30","timeout_seconds":1}`}
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.executePendingTool(context.Background(), id, call)
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.Status != StatusCreated || len(result.Actions) != 1 || len(pendingToolCalls(&result)) != 0 {
		t.Fatalf("timeout was not returned to the model: %+v", result.Run)
	}
	action := result.Actions[0]
	if action.TimeoutSeconds != 1 || action.Status != "uncertain" || action.FinishedAt.IsZero() || action.Result == nil || action.Result.WorkerStatus != worker.StatusTimedOut || !action.Result.OutcomeUncertain || !action.Result.Interrupted {
		t.Fatalf("timeout state: %+v", action)
	}
	if len(action.Result.Artifacts) == 0 || !verifyArtifacts(store.root, action.Result) {
		t.Fatal("partial CAS lost")
	}
	// New execution is blocked even after an explicit resume to waiting_tool.
	_, err = store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusWaitingTool; return nil })
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	result, err = a.executeAction(context.Background(), id, call.ID, "exec", nil, func() ToolResult { ran = true; return ToolResult{} })
	if err == nil || ran {
		t.Fatalf("uncertain replay: ran=%v err=%v", ran, err)
	}
}

func TestExecTimeoutInvalidCallDoesNotStart(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	call := ToolCall{ID: "invalid", Name: "exec", Arguments: `{"command":"never-start","timeout_seconds":0}`}
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.executePendingTool(context.Background(), id, call)
	if err != nil || len(s.Actions) != 0 || !strings.Contains(s.Messages[len(s.Messages)-1].Content, "timeout_seconds") {
		t.Fatalf("invalid started: %+v %v", s.Actions, err)
	}
}

func TestManualCommandUsesRunBoundaryNotForeignArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	call := ToolCall{ID: "manual", Name: "external", Arguments: `{"command":123,"timeout_seconds":"foreign field"}`}
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.executeCommand(context.Background(), id, call.ID, []string{"/bin/sh", "-c", "printf manual"})
	if err != nil || len(s.Actions) != 1 || s.Actions[0].TimeoutSeconds != 300 || s.Actions[0].Result == nil || s.Actions[0].Result.Stdout != "manual" {
		t.Fatalf("foreign args interpreted as exec: %+v %v", s.Actions, err)
	}
	if s.Actions[0].RequestID == "" || s.Actions[0].ResourceOwner != worker.ResourceOwner(id, s.Actions[0].RequestID, call.ID) || len(s.Requests) != 0 || len(s.Responses) != 0 {
		t.Fatal("manual execution lacks durable ownership or fabricated model history")
	}
	if len(s.Evidence) != 1 || s.Evidence[0].Verification == "verified" {
		t.Fatal("message-derived identity fabricated independent provenance")
	}
}

func TestManualBatchExecutionIdentityIsDurableAndDistinct(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	snap, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "one", Name: "external", Arguments: `{}`}, {ID: "two", Name: "external", Arguments: `{}`}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	first := currentRequestID(&snap)
	if !strings.HasPrefix(first, "manual-batch:") {
		t.Fatal("manual batch has no owned identity")
	}
	for _, call := range []string{"one", "two"} {
		snap, err = a.executeAction(context.Background(), id, call, "exec", nil, func() ToolResult { return ToolResult{Stdout: "observed"} })
		if err != nil {
			t.Fatal(err)
		}
		action := snap.Actions[len(snap.Actions)-1]
		if action.RequestID != first || action.ResourceOwner != worker.ResourceOwner(id, first, call) {
			t.Fatal("same batch split ownership")
		}
	}
	reran := false
	if _, err = a.executeAction(context.Background(), id, "one", "exec", nil, func() ToolResult { reran = true; return ToolResult{} }); err == nil || reran {
		t.Fatal("completed manual call replayed")
	}
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snap, err = reopened.Load(id)
	if err != nil || currentRequestID(&snap) != first || len(snap.Requests) != 0 || len(snap.Responses) != 0 {
		t.Fatal("manual ownership not durable")
	}
	snap, err = store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "one", Name: "external", Arguments: `{}`}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	second := currentRequestID(&snap)
	if second == first || second == "" {
		t.Fatal("new manual batch reused old owner")
	}
	ran := false
	snap, err = a.executeAction(context.Background(), id, "one", "exec", nil, func() ToolResult { ran = true; return ToolResult{} })
	if err != nil || !ran || len(snap.Actions) != 3 || snap.Actions[2].RequestID != second {
		t.Fatal("new manual batch confused with old call")
	}
}

func TestManualBatchIdentityPreservesArchivedRequestsAndLegacyEvidence(t *testing.T) {
	s := Snapshot{Run: Run{ID: "run"}, Messages: []Message{{ID: "manual-message", Role: "assistant", ToolCalls: []ToolCall{{ID: "call", Name: "external"}}}}, Requests: []ModelRequest{{ID: "real-model-request"}, {ID: "review-only", Purpose: "evidence_review"}}}
	if currentRequestID(&s) != "real-model-request" {
		t.Fatal("manual fallback replaced actual archived model identity")
	}
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	snap, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "legacy", Name: "external", Arguments: `{}`}}})
		result := ToolResult{Stdout: "historical"}
		raw, _ := json.Marshal(result)
		s.Actions = []Action{{RequestID: "", CallID: "legacy", Name: "external", Status: "completed", Result: &result}}
		appendToolResult(s, "legacy", string(raw), "executed")
		s.Evidence[0].RequestID = ""
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeActions, _ := json.Marshal(snap.Actions)
	beforeEvidence, _ := json.Marshal(snap.Evidence)
	loaded, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	afterActions, _ := json.Marshal(loaded.Actions)
	afterEvidence, _ := json.Marshal(loaded.Evidence)
	if string(beforeActions) != string(afterActions) || string(beforeEvidence) != string(afterEvidence) {
		t.Fatal("derived manual identity rewrote historical source ledger")
	}
	if loaded.Evidence[0].Verification == "verified" {
		t.Fatal("manual batch identity fabricated model response proof")
	}
}

func TestManualBatchIdentityNeverReplaysLegacyEmptyRequest(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "old", Name: "external", Arguments: `{}`}}})
		s.Actions = []Action{{CallID: "old", Adapter: "exec", Status: "started"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	snap, err := a.executeAction(context.Background(), id, "old", "exec", nil, func() ToolResult { ran = true; return ToolResult{} })
	if err != nil || ran || len(snap.Actions) != 1 || snap.Actions[0].Status != "uncertain" || snap.Actions[0].RequestID != "" {
		t.Fatal("new identity replayed or rewrote legacy action")
	}
	if _, err = store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusWaitingTool; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = a.recordManualTool(id, "old", "operator inspected outcome"); err == nil {
		t.Fatal("already observed legacy outcome accepted twice")
	}
	snap, err = store.Load(id)
	if err != nil || snap.Actions[0].Status != "uncertain" || snap.Actions[0].Result != nil || !snap.Actions[0].FinishedAt.IsZero() || snap.Actions[0].ResourceCleanupStatus == "completed" || len(pendingToolCalls(&snap)) != 0 {
		t.Fatal("legacy observation lost cleanup boundary")
	}
}

func TestManualBatchIdentitySettlesLegacyReadyWithoutReplay(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	call := ToolCall{ID: "ready", Name: "external", Arguments: `{}`}
	snap, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}})
		s.Actions = []Action{{CallID: call.ID, Name: call.Name, Arguments: call.Arguments, Status: "result_ready", Result: &ToolResult{Stdout: "retained"}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readyPiResult(&snap, call.ID); err != nil || !ok {
		t.Fatal("legacy ready result became invisible")
	}
	if err := validateReadyPiAction(&snap, call); err != nil {
		t.Fatal(err)
	}
	snap, err = store.Update(id, func(s *Snapshot) error {
		settled, err := settleReadyPiResult(s, call.ID)
		if err != nil {
			return err
		}
		if !settled {
			t.Fatal("legacy result not settled")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Actions[0].RequestID != "" || snap.Actions[0].Status != "completed" || len(snap.Actions) != 1 || len(pendingToolCalls(&snap)) != 0 || len(snap.Evidence) != 1 || snap.Evidence[0].Verification == "verified" {
		t.Fatal("legacy ready settlement rewrote provenance or replayed")
	}
	if _, ok, err := readyPiResult(&snap, call.ID); err != nil || ok {
		t.Fatal("settled legacy ready result remained pending")
	}
}

// Save the old wire format directly, then migrate it through the real backend
// migration. Store.Update would already assign the identity this fixture lacks.
func rawManualBatchFixture(t *testing.T, backend string, action *Action) (*Agent, *Store, string, ToolCall) {
	t.Helper()
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	snap, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	call := ToolCall{ID: "raw-manual", Name: "external", Arguments: `{}`}
	snap.Run.Status = StatusWaitingTool
	snap.Messages = append(snap.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}})
	if action != nil {
		copy := *action
		copy.CallID, copy.Name, copy.Arguments = call.ID, call.Name, call.Arguments
		snap.Actions = []Action{copy}
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.root, id+".json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if backend == "sqlite" {
		if err = MigrateStoreToSQLite(store.root); err != nil {
			t.Fatal(err)
		}
	}
	store, err = NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	a, err = New(store, successProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	snap, err = store.Load(id)
	if err != nil || currentRequestID(&snap) != "" || snap.Messages[len(snap.Messages)-1].ID != "" {
		t.Fatal("raw fixture unexpectedly acquired message identity", err)
	}
	return a, store, id, call
}

func TestManualBatchRawLegacyExecutionMigratesBeforeLaunch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fixture")
	}
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, id, call := rawManualBatchFixture(t, backend, nil)
			release, err := store.AcquireRunLock(id)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			marker := filepath.Join(t.TempDir(), "launched")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			command := []string{"/bin/sh", "-c", `printf 'launched\n' >> "$1"; printf raw`, "sh", marker}
			snap, err := a.executeCommand(ctx, id, call.ID, command)
			if err != nil || len(snap.Actions) != 1 || snap.Actions[0].Result == nil || snap.Actions[0].Result.Stdout != "raw" || snap.Actions[0].Result.Error != "" {
				t.Fatalf("raw manual command did not execute: %+v %v", snap.Actions, err)
			}
			action := snap.Actions[0]
			request := currentRequestID(&snap)
			if !strings.HasPrefix(request, "manual-batch:") || action.RequestID != request || action.ResourceOwner != worker.ResourceOwner(id, request, call.ID) || action.Result.WorkerResourceOwner != action.ResourceOwner {
				t.Fatal("Action and launched resource owner differ from persisted batch")
			}
			if len(snap.Requests) != 0 || len(snap.Responses) != 0 || len(snap.Evidence) != 1 || snap.Evidence[0].Verification == "verified" {
				t.Fatal("message migration fabricated model provenance")
			}
			reopened, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			saved, err := reopened.Load(id)
			if err != nil || currentRequestID(&saved) != request || saved.Actions[0].RequestID != request {
				t.Fatal("message identity not durable", err)
			}
			if _, err = a.executeCommand(ctx, id, call.ID, command); err == nil {
				t.Fatal("completed raw manual call accepted twice")
			}
			raw, err := os.ReadFile(marker)
			if err != nil || string(raw) != "launched\n" {
				t.Fatalf("marker not exactly once: %q %v", raw, err)
			}
		})
	}
}

func TestManualBatchRawLegacyStartedNeverReplays(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, id, call := rawManualBatchFixture(t, backend, &Action{Adapter: "exec", Status: "started"})
			snap, err := a.executeCommand(context.Background(), id, call.ID, []string{"missing-must-not-launch"})
			if err != nil || len(snap.Actions) != 1 || snap.Actions[0].Status != "uncertain" || snap.Actions[0].Result != nil || snap.Actions[0].RequestID != "" || snap.Run.Status != StatusCreated || len(snap.Evidence) != 1 || snap.Evidence[0].Type != "runtime_observation" || currentRequestID(&snap) == "" {
				t.Fatalf("raw started action replayed or rewritten: %+v %v", snap.Actions, err)
			}
			if _, err = store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusWaitingTool; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err = a.recordManualTool(id, call.ID, "operator observation"); err == nil {
				t.Fatal("raw legacy observation accepted twice")
			}
			snap, err = store.Load(id)
			if err != nil || snap.Actions[0].Status != "uncertain" || snap.Actions[0].RequestID != "" || snap.Actions[0].ResourceCleanupStatus == "completed" || len(pendingToolCalls(&snap)) != 0 {
				t.Fatal("raw legacy observation lost cleanup boundary", err)
			}
		})
	}
}

func TestManualBatchRawLegacyReadySettlesWithoutReplay(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			_, store, id, call := rawManualBatchFixture(t, backend, &Action{Adapter: "exec", Status: "result_ready", Result: &ToolResult{Stdout: "retained"}})
			snap, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok, err := readyPiResult(&snap, call.ID); err != nil || !ok {
				t.Fatal("raw ready result invisible", err)
			}
			snap, err = store.Update(id, func(s *Snapshot) error {
				if err := validateReadyPiAction(s, call); err != nil {
					return err
				}
				settled, err := settleReadyPiResult(s, call.ID)
				if !settled {
					t.Error("raw ready result not settled")
				}
				return err
			})
			if err != nil || len(snap.Actions) != 1 || snap.Actions[0].Status != "completed" || snap.Actions[0].RequestID != "" || snap.Actions[0].Result.Stdout != "retained" || len(pendingToolCalls(&snap)) != 0 || currentRequestID(&snap) == "" {
				t.Fatal("raw ready result replayed or identity rewritten", err)
			}
			if len(snap.Requests) != 0 || len(snap.Responses) != 0 || len(snap.Evidence) != 1 || snap.Evidence[0].Verification == "verified" {
				t.Fatal("raw ready source promoted")
			}
			if _, ok, err := readyPiResult(&snap, call.ID); err != nil || ok {
				t.Fatal("raw result still ready after settlement", err)
			}
		})
	}
}

func legacyRepeatedBatchFixture(t *testing.T, backend, status, receipt string) (*Agent, *Store, string, ToolCall, string) {
	t.Helper()
	old := Action{Adapter: "exec", Status: status}
	if status != "started" {
		old.Result = &ToolResult{Stdout: "old observation"}
		if receipt == "exact" || receipt == "duplicate" {
			old.Result.EvidenceID = "legacy-result"
		}
	}
	_, store, id, call := rawManualBatchFixture(t, backend, &old)
	_, err := store.Update(id, func(s *Snapshot) error {
		if receipt != "none" {
			raw, _ := json.Marshal(s.Actions[0].Result)
			s.Messages = append(s.Messages, Message{Role: "tool", ToolCallID: call.ID, Content: string(raw)})
		}
		s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}})
		if receipt == "duplicate" {
			raw, _ := json.Marshal(s.Actions[0].Result)
			s.Messages = append(s.Messages, Message{Role: "tool", ToolCallID: call.ID, Content: string(raw)}, Message{Role: "assistant", ToolCalls: []ToolCall{call}})
		}
		s.Run.Status = StatusWaitingTool
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	root := store.root
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	a, err := New(reopened, successProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := reopened.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(snap.Actions[0])
	return a, reopened, id, call, string(raw)
}

func TestManualBatchLegacyReceiptDoesNotCaptureNewSameCall(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, status := range []string{"completed", "result_ready"} {
			t.Run(backend+"/"+status, func(t *testing.T) {
				a, store, id, call, original := legacyRepeatedBatchFixture(t, backend, status, "exact")
				snap, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				request := currentRequestID(&snap)
				if actionRequestMatchesCurrent(&snap, snap.Actions[0]) || actionRequestMayMatchCurrent(&snap, snap.Actions[0]) {
					t.Fatal("old receipt captured new batch")
				}
				if _, ready, err := readyPiResult(&snap, call.ID); err != nil || ready {
					t.Fatal("old ready result offered to new call", err)
				}
				if settled, err := settleReadyPiResult(&snap, call.ID); err != nil || settled {
					t.Fatal("old ready result settled into new batch", err)
				}
				ran := 0
				snap, err = a.executeAction(context.Background(), id, call.ID, "exec", nil, func() ToolResult { ran++; return ToolResult{Stdout: "new observation"} })
				if err != nil || ran != 1 || len(snap.Actions) != 2 || snap.Actions[1].RequestID != request || snap.Actions[1].Result == nil || snap.Actions[1].Result.Stdout != "new observation" {
					t.Fatalf("new batch blocked or reused old output: %+v %v", snap.Actions, err)
				}
				raw, _ := json.Marshal(snap.Actions[0])
				if string(raw) != original || len(snap.Requests) != 0 || len(snap.Responses) != 0 || len(snap.Evidence) != 1 || snap.Evidence[0].Verification == "verified" {
					t.Fatal("old action changed or source promoted")
				}
				if _, err = a.executeAction(context.Background(), id, call.ID, "exec", nil, func() ToolResult { ran++; return ToolResult{} }); err == nil || ran != 1 {
					t.Fatal("new batch executed more than once")
				}
			})
		}
	}
}

func TestManualBatchLegacyAmbiguityBlocksWithoutMutatingHistory(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, mode := range []struct{ status, receipt string }{{"started", "none"}, {"result_ready", "none"}, {"result_ready", "no_evidence_id"}, {"result_ready", "duplicate"}} {
			t.Run(backend+"/"+mode.status+"/"+mode.receipt, func(t *testing.T) {
				a, store, id, call, original := legacyRepeatedBatchFixture(t, backend, mode.status, mode.receipt)
				snap, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				if actionRequestMatchesCurrent(&snap, snap.Actions[0]) || !actionRequestMayMatchCurrent(&snap, snap.Actions[0]) {
					t.Fatal("ambiguous action accepted as known batch")
				}
				if _, ready, err := readyPiResult(&snap, call.ID); err != nil || ready {
					t.Fatal("ambiguous result exposed", err)
				}
				if settled, err := settleReadyPiResult(&snap, call.ID); err != nil || settled {
					t.Fatal("ambiguous result consumed", err)
				}
				if _, err = a.recordManualTool(id, call.ID, "new observation"); err == nil {
					t.Fatal("ambiguous action manually resolved as current batch")
				}
				ran := false
				snap, err = a.executeAction(context.Background(), id, call.ID, "exec", nil, func() ToolResult { ran = true; return ToolResult{} })
				if err != nil || ran || snap.Run.Status != StatusCreated || len(snap.Actions) != 1 || len(snap.Evidence) != 1 || snap.Evidence[0].Type != "runtime_observation" || len(pendingToolCalls(&snap)) != 0 {
					t.Fatalf("ambiguous batch did not return an observation: %+v %v", snap.Actions, err)
				}
				raw, _ := json.Marshal(snap.Actions[0])
				if string(raw) != original {
					t.Fatal("ambiguity rewrote original action")
				}
				loaded, err := store.Load(id)
				raw, _ = json.Marshal(loaded.Actions[0])
				if err != nil || string(raw) != original || len(loaded.Evidence) != 1 || strings.Contains(loaded.Evidence[0].Content, "old observation") {
					t.Fatal("ambiguous history changed or historical result consumed", err)
				}
			})
		}
	}
}
