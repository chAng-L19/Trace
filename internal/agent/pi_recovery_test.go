package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"trace/internal/mcp"
)

func seedPiBatch(t *testing.T, a *Agent, store *Store, calls []ToolCall, tools []ToolDefinition) string {
	t.Helper()
	id := createTestRun(t, a)
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Run.RequestSeq = 1
		requestID := id + "-0001"
		s.Requests = append(s.Requests, ModelRequest{ID: requestID, Tools: tools, CreatedAt: time.Now().UTC()})
		s.Messages = append(s.Messages, Message{Role: "user", Content: s.Run.Goal}, Message{Role: "assistant", RequestID: requestID, ToolCalls: calls})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestPiAmbiguousJSONNeverExecutes(t *testing.T) {
	for _, args := range []string{`{"command":"echo first","command":"echo second"}`, `{"command":"echo first","Command":"echo second"}`, `{"command":"echo first","extra":{"key":1,"key":2}}`} {
		for _, recovery := range []bool{false, true} {
			t.Run(args+map[bool]string{false: "live", true: "recovery"}[recovery], func(t *testing.T) {
				call := ToolCall{ID: "ambiguous", Name: "exec", Arguments: args}
				p := manualProvider(call)
				a, store := newTestAgent(t, p)
				id := ""
				if recovery {
					id = seedPiBatch(t, a, store, []ToolCall{call}, []ToolDefinition{execToolDefinition()})
					p.responses = []ModelResponse{{Content: "continue after validation"}}
				} else {
					id = createTestRun(t, a)
				}
				s, err := a.Run(piMigrationContext(t), id)
				if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 0 || len(s.Evidence) != 1 || s.Evidence[0].Verification == "verified" {
					t.Fatalf("ambiguous JSON executed: status=%s actions=%d err=%v", s.Run.Status, len(s.Actions), err)
				}
			})
		}
	}
}

func TestPiRetryRefreshesSuccessfulRequestTools(t *testing.T) {
	server, calls, _ := integrationServer(t, "success")
	toolID := mcp.ToolID("fixture", "echo")
	a, _ := newTestAgent(t, MockProvider{})
	addIntegrationMCP(t, a, mcp.Config{Name: "fixture", URL: server.URL})
	id := createTestRun(t, a)
	if _, err := a.SelectTools(id, []string{}, true); err != nil {
		t.Fatal(err)
	}
	requests := 0
	a.provider = settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		requests++
		if requests == 1 {
			if _, err := a.SelectTools(id, []string{toolID}, true); err != nil {
				return ModelResponse{}, err
			}
			return ModelResponse{}, &ProviderHTTPError{StatusCode: 503}
		}
		if requests == 2 {
			return ModelResponse{ToolCalls: []ToolCall{{ID: "new-schema", Name: toolID, Arguments: `{"message":"selected during retry"}`}}}, nil
		}
		return ModelResponse{Content: fixtureReport("complete")}, nil
	})
	s, err := a.Run(piMigrationContext(t), id)
	if err != nil || s.Run.Status != StatusCompleted || requests != 3 || calls.Load() != 1 || len(s.Actions) != 1 {
		t.Fatalf("retry used stale tools: status=%s models=%d remote=%d actions=%d err=%v", s.Run.Status, requests, calls.Load(), len(s.Actions), err)
	}
}

func TestPiArgumentsCoercionNeverCrossesActionBoundary(t *testing.T) {
	for _, args := range []string{`{"command":42}`, `{"command":true}`} {
		t.Run(args, func(t *testing.T) {
			p := manualProvider(ToolCall{ID: "coerce", Name: "exec", Arguments: args})
			a, _ := newTestAgent(t, p)
			s, err := a.Run(piMigrationContext(t), createTestRun(t, a))
			if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 0 || len(s.Evidence) != 1 {
				t.Fatalf("coercion crossed boundary: status=%s actions=%d evidence=%d err=%v", s.Run.Status, len(s.Actions), len(s.Evidence), err)
			}
			if !strings.Contains(s.Evidence[0].Content, "schema") && !strings.Contains(s.Evidence[0].Content, "Validation") {
				t.Fatalf("missing explicit validation result: %s", s.Evidence[0].Content)
			}
		})
	}
}

func TestPiStrictArgumentsRejectOptionalNullAndBooleanString(t *testing.T) {
	for _, call := range []ToolCall{
		{ID: "number-string", Name: "tool_search", Arguments: `{"offset":"1"}`},
		{ID: "boolean-string", Name: "tool_select", Arguments: `{"ids":[],"replace":"true"}`},
		{ID: "optional-null", Name: "tool_search", Arguments: `{"query":null}`},
		{ID: "nested-invalid", Name: "plan", Arguments: `{"tasks":[{"id":"t","goal":"g","requirements":[{"id":"r","description":"d","category":"invalid","predicate":{"kind":"contains","value":"x"}}]}]}`},
	} {
		for _, recovery := range []bool{false, true} {
			t.Run(call.ID+map[bool]string{false: "live", true: "recovery"}[recovery], func(t *testing.T) {
				p := manualProvider(call)
				a, store := newTestAgent(t, p)
				id := ""
				if recovery {
					id = seedPiBatch(t, a, store, []ToolCall{call}, a.registry.Definitions())
					p.responses = []ModelResponse{{Content: "after validation"}}
				} else {
					id = createTestRun(t, a)
				}
				s, err := a.Run(piMigrationContext(t), id)
				if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 0 || len(s.Evidence) != 1 || s.Evidence[0].Verification == "verified" {
					t.Fatalf("invalid/coerced arguments executed: status=%s actions=%d evidence=%d err=%v", s.Run.Status, len(s.Actions), len(s.Evidence), err)
				}
			})
		}
	}
}

func TestPiRecoveryUsesArchivedSchemaAndRequest(t *testing.T) {
	for _, args := range []string{`{}`, `{"command":"not-allowed"}`, `{"command":42}`} {
		t.Run(args, func(t *testing.T) {
			p := &scriptedProvider{responses: []ModelResponse{{Content: "recovered"}}}
			a, store := newTestAgent(t, p)
			d := execToolDefinition()
			d.Function.Parameters = map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string", "enum": []string{"allowed"}}}, "required": []string{"command"}}
			id := seedPiBatch(t, a, store, []ToolCall{{ID: "invalid", Name: "exec", Arguments: args}}, []ToolDefinition{d})
			s, err := a.Run(piMigrationContext(t), id)
			if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 0 || len(s.Evidence) != 1 || len(p.requests) != 1 {
				t.Fatalf("recovery skipped validation: status=%s actions=%d evidence=%d model=%d err=%v", s.Run.Status, len(s.Actions), len(s.Evidence), len(p.requests), err)
			}
			if s.Evidence[0].RequestID != id+"-0001" || s.Evidence[0].Verification == "verified" {
				t.Fatalf("wrong recovery provenance: %+v", s.Evidence[0])
			}
		})
	}
}

func TestPiRecoveryUnknownReturnsToModelAndContinuesBatch(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-execute")
	args, _ := json.Marshal(map[string]string{"command": `echo unexpected > "` + marker + `"`})
	p := &scriptedProvider{responses: []ModelResponse{{Content: "after recovery"}}}
	a, store := newTestAgent(t, p)
	id := seedPiBatch(t, a, store, []ToolCall{{ID: "unknown", Name: "manual_external", Arguments: `{}`}, {ID: "later", Name: "exec", Arguments: string(args)}}, []ToolDefinition{execToolDefinition()})
	s, err := a.Run(piMigrationContext(t), id)
	if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 1 || len(p.requests) != 1 || len(s.Evidence) != 2 || s.Evidence[0].Type != "runtime_observation" {
		t.Fatalf("resolved batch failed: status=%s actions=%d model=%d err=%v", s.Run.Status, len(s.Actions), len(p.requests), err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("later tool did not execute: %v", err)
	}
	if s.Actions[0].RequestID != id+"-0001" {
		t.Fatalf("recovery action moved request: %+v", s.Actions[0])
	}
}

func TestPiRecoveryDoesNotReplayUncertainAction(t *testing.T) {
	p := &scriptedProvider{responses: []ModelResponse{{Content: "inspect unknown outcome"}}}
	a, store := newTestAgent(t, p)
	id := seedPiBatch(t, a, store, []ToolCall{{ID: "started", Name: "exec", Arguments: `{"command":"echo must-not-replay"}`}}, []ToolDefinition{execToolDefinition()})
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Actions = append(s.Actions, Action{RequestID: id + "-0001", CallID: "started", Name: "exec", Status: "started"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.Run(piMigrationContext(t), id)
	if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 1 || s.Actions[0].Status != "uncertain" || s.Actions[0].Result != nil || !s.Actions[0].FinishedAt.IsZero() || len(s.Evidence) != 1 || s.Evidence[0].Type != "runtime_observation" || len(p.requests) != 1 {
		t.Fatalf("uncertain action replayed: status=%s actions=%+v evidence=%d model=%d err=%v", s.Run.Status, s.Actions, len(s.Evidence), len(p.requests), err)
	}
}

func TestPiLargeIntegerArgumentsNeverExecuteRoundedValue(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "recovery"}[recovery], func(t *testing.T) {
			call := ToolCall{ID: "large-int", Name: "exec", Arguments: `{"command":"echo must-not-execute","ticket":9007199254740993}`}
			p := manualProvider(call)
			a, store := newTestAgent(t, p)
			d := execToolDefinition()
			d.Function.Parameters = map[string]any{"type": "object", "properties": map[string]any{"command": map[string]string{"type": "string"}, "ticket": map[string]any{"type": "integer", "enum": []any{json.Number("9007199254740992")}}}, "required": []string{"command", "ticket"}}
			tool := a.registry.tools["exec"]
			tool.Definition = d
			a.registry.tools["exec"] = tool
			id := ""
			if recovery {
				id = seedPiBatch(t, a, store, []ToolCall{call}, []ToolDefinition{d})
				p.responses = []ModelResponse{{Content: "continue after integer validation"}}
			} else {
				id = createTestRun(t, a)
			}
			s, err := a.Run(piMigrationContext(t), id)
			if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 0 || len(s.Evidence) != 1 || !strings.Contains(s.Evidence[0].Content, "arguments differ") || s.Evidence[0].Verification == "verified" {
				t.Fatalf("rounded integer executed: status=%s actions=%d err=%v", s.Run.Status, len(s.Actions), err)
			}
		})
	}
}

func TestPiRecoveryValidationCannotSettleHistoricalAction(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-execute")
	args, _ := json.Marshal(map[string]string{"command": `echo unexpected > "` + marker + `"`})
	p := &scriptedProvider{responses: []ModelResponse{{Content: "inspect old invalid call"}}}
	a, store := newTestAgent(t, p)
	id := seedPiBatch(t, a, store, []ToolCall{{ID: "old-invalid", Name: "exec", Arguments: `{}`}, {ID: "later", Name: "exec", Arguments: string(args)}}, []ToolDefinition{execToolDefinition()})
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Actions = append(s.Actions, Action{RequestID: id + "-0001", CallID: "old-invalid", Name: "exec", Status: "started"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.Run(piMigrationContext(t), id)
	if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 2 || s.Actions[0].Status != "uncertain" || s.Actions[0].Result != nil || !s.Actions[0].FinishedAt.IsZero() || len(s.Evidence) != 2 || s.Evidence[0].Type != "runtime_observation" || len(pendingToolCalls(&s)) != 0 || len(p.requests) != 1 {
		t.Fatalf("validation settled historical action: status=%s actions=%+v evidence=%d pending=%d model=%d err=%v", s.Run.Status, s.Actions, len(s.Evidence), len(pendingToolCalls(&s)), len(p.requests), err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("later action did not execute: %v", err)
	}
}

func TestPiLoopErrorKeepsFailureAcrossStoppedAndBudgetStates(t *testing.T) {
	disk := errors.New("fixture persistence failure")
	for _, status := range []Status{StatusRunning, StatusPaused, StatusCancelled} {
		t.Run(string(status), func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id := createTestRun(t, a)
			_, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = status; return nil })
			if err != nil {
				t.Fatal(err)
			}
			snap, err := a.settlePiLoopError(context.Background(), id, errors.Join(errPiHalt, disk))
			if !errors.Is(err, disk) {
				t.Fatalf("real failure hidden by %s: %v", status, err)
			}
			if status == StatusRunning {
				if snap.Run.Status != StatusFailed {
					t.Fatal("running error did not fail")
				}
			} else if snap.Run.Status != status {
				t.Fatal("operator stop state changed")
			}
		})
	}
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprint("budget-mixed-", mixed), func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id := createTestRun(t, a)
			_, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; s.Run.Budget.MaxSeconds = 1; return nil })
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(&timeBudgetCause{RunID: id})
			cause := errors.Join(context.Cause(ctx), ctx.Err(), errPiHalt)
			if mixed {
				cause = errors.Join(cause, disk)
			}
			snap, err := a.settlePiLoopError(ctx, id, cause)
			if snap.Run.Status != StatusPaused {
				t.Fatalf("budget state lost: %+v", snap.Run)
			}
			if mixed && !errors.Is(err, disk) || !mixed && err != nil {
				t.Fatalf("budget error classification: %v", err)
			}
		})
	}
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	_, err := store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusPaused; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.settlePiLoopError(context.Background(), id, fmt.Errorf("boundary: %w", errPiHalt)); err != nil {
		t.Fatalf("pure halt failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = a.settlePiLoopError(ctx, id, errors.Join(context.Cause(ctx), errPiHalt)); err != nil {
		t.Fatalf("pure cancellation failed: %v", err)
	}
}

func TestPiBudgetPauseRenameFailureRemainsError(t *testing.T) {
	for _, boundary := range []string{"budget", "prepare", "settle"} {
		t.Run(boundary, func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id := createTestRun(t, a)
			before, err := store.Update(id, func(s *Snapshot) error {
				s.Run.Status = StatusRunning
				s.Run.Budget.MaxSeconds = 1
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(&timeBudgetCause{RunID: id})
			restore, failed := settlementRenameFailure(t, store, id)
			paused := false
			switch boundary {
			case "budget":
				paused, err = a.pauseIfBudgetTime(id, ctx)
			case "prepare":
				_, err = a.preparePiRequest(ctx, id)
			case "settle":
				_, err = a.settlePiLoopError(ctx, id, errors.Join(context.Cause(ctx), errPiHalt))
			}
			restore()
			var renameErr *os.LinkError
			if !*failed || paused || !errors.As(err, &renameErr) || renameErr.Op != "rename" {
				t.Fatalf("budget persistence failure hidden: failed=%v paused=%v err=%v", *failed, paused, err)
			}
			after, loadErr := store.Load(id)
			if loadErr != nil || after.Run.Status != before.Run.Status || after.Run.EventSeq != before.Run.EventSeq || after.Run.PauseReason != before.Run.PauseReason {
				t.Fatalf("failed pause published state: before=%+v after=%+v err=%v", before.Run, after.Run, loadErr)
			}
		})
	}
}
