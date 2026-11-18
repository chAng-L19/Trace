package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"trace/internal/worker"
)

func TestHistoricalToolObservationPreservesResultAndCleanup(t *testing.T) {
	for _, hasResult := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "saved"}[hasResult], func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			call := ToolCall{ID: "old", Name: "external", Arguments: `{}`}
			id := seedPiBatch(t, a, store, []ToolCall{call}, nil)
			before, err := store.Update(id, func(s *Snapshot) error {
				action := Action{RequestID: currentRequestID(s), CallID: call.ID, Name: call.Name, Arguments: call.Arguments, Adapter: "exec", Status: "started", ResourceCleanupError: "cleanup remains unknown"}
				if hasResult {
					action.Result = &ToolResult{Stdout: "original", ExitCode: -1, OutcomeUncertain: true, WorkerCleanupError: "unreconciled", EvidenceID: "original-evidence"}
					action.FinishedAt = time.Now().UTC()
				}
				s.Actions = []Action{action}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			before, err = store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			s, err := a.recordUnavailableTool(id, call.ID, call.Name)
			if err != nil || s.Run.Status != StatusCreated || len(s.Actions) != 1 || len(s.Evidence) != 1 || s.Evidence[0].Type != "runtime_observation" || s.Evidence[0].Verification != "unverified" {
				t.Fatalf("missing runtime observation: %+v %v", s, err)
			}
			expected := before.Actions[0]
			if expected.Result == nil {
				expected.Status = "uncertain"
			}
			if !reflect.DeepEqual(expected, s.Actions[0]) || toolCleanupError(s) == nil {
				t.Fatal("observation rewrote execution result or cleanup ledger")
			}
			var result ToolResult
			if err := json.Unmarshal([]byte(s.Evidence[0].Content), &result); err != nil {
				t.Fatal(err)
			}
			if hasResult {
				raw, _ := json.Marshal(expected.Result)
				if string(raw) != s.Evidence[0].Content {
					t.Fatal("unavailable adapter replaced historical result")
				}
			} else if !result.OutcomeUncertain || strings.Contains(result.Error, "was not executed") {
				t.Fatal("missing history misrepresented as not executed")
			}
		})
	}
}

func TestUnavailableToolObservationIsNotExecution(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	call := ToolCall{ID: "unknown", Name: "absent", Arguments: `{}`}
	id := seedPiBatch(t, a, store, []ToolCall{call}, nil)
	s, err := a.recordUnavailableTool(id, call.ID, call.Name)
	if err != nil || len(s.Actions) != 0 || len(s.Evidence) != 1 || s.Evidence[0].Type != "runtime_observation" || s.Run.Status != StatusCreated {
		t.Fatalf("unavailable tool did not continue: %+v %v", s, err)
	}
	var result ToolResult
	if err := json.Unmarshal([]byte(s.Evidence[0].Content), &result); err != nil || result.WorkerStatus != worker.StatusNotStarted || result.OutcomeUncertain || !strings.Contains(result.Error, "was not executed") {
		t.Fatalf("unavailable result: %+v %v", result, err)
	}
	if _, err := a.recordUnavailableTool(id, call.ID, call.Name); err == nil {
		t.Fatal("settled call emitted twice")
	}
}

func TestHistoricalToolObservationStopsExecutorBeforeReplay(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	call := ToolCall{ID: "started", Name: "exec", Arguments: `{}`}
	id := seedPiBatch(t, a, store, []ToolCall{call}, nil)
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Actions = []Action{{RequestID: currentRequestID(s), CallID: call.ID, Name: call.Name, Arguments: call.Arguments, Status: "started"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ran := false
	s, err := a.executeAction(context.Background(), id, call.ID, "exec", nil, func() ToolResult { ran = true; return ToolResult{} })
	if err != nil || ran || len(s.Actions) != 1 || s.Actions[0].Result != nil || !s.Actions[0].FinishedAt.IsZero() || len(s.Evidence) != 1 {
		t.Fatalf("historical executor ran or fabricated result: %+v %v", s.Actions, err)
	}
}

func TestPiDeferredUnknownResultsRetainStatusAndDeclarationOrder(t *testing.T) {
	for _, status := range []string{"uncertain", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			calls := []ToolCall{{ID: "first", Name: "read", Arguments: `{}`}, {ID: "second", Name: "read", Arguments: `{}`}}
			id := seedPiBatch(t, a, store, calls, nil)
			ctx := context.WithValue(context.Background(), piDeferredResultKey{}, true)
			for _, call := range calls {
				if _, err := a.executeAction(ctx, id, call.ID, "builtin", nil, func() ToolResult {
					return ToolResult{Stdout: "saved", ExitCode: -1, Interrupted: status == "interrupted", OutcomeUncertain: status == "uncertain"}
				}); err != nil {
					t.Fatal(err)
				}
			}
			s, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := settleReadyPiResult(&s, calls[1].ID); err == nil {
				t.Fatal("deferred unknown result emitted out of order")
			}
			for _, call := range calls {
				raw, ready, err := readyPiResult(&s, call.ID)
				if err != nil || !ready || raw == "" {
					t.Fatalf("unknown result hidden from pi: %v", err)
				}
				if err := validateReadyPiAction(&s, call); err != nil {
					t.Fatal(err)
				}
				if settled, err := settleReadyPiResult(&s, call.ID); err != nil || !settled {
					t.Fatalf("unknown result not settled: %v", err)
				}
			}
			for i, action := range s.Actions {
				if action.Status != status || action.Result.EvidenceID != "" || s.Evidence[i].Type != "runtime_observation" || s.Evidence[i].Verification != "unverified" {
					t.Fatal("deferred unknown result promoted or status lost")
				}
				if _, ready, err := readyPiResult(&s, action.CallID); err != nil || ready {
					t.Fatal("settled unknown result still ready")
				}
				if settled, err := settleReadyPiResult(&s, action.CallID); err != nil || settled {
					t.Fatal("settled unknown result emitted twice")
				}
			}
		})
	}
}
