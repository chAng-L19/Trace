package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"trace/internal/localtool"
	"trace/internal/mcp"
)

func TestManualToolCyclePublicAdaptersKeepPendingSibling(t *testing.T) {
	for _, adapter := range []string{"exec", "builtin", "local", "mcp"} {
		t.Run(adapter, func(t *testing.T) {
			provider := &scriptedProvider{}
			a, store := newTestAgent(t, provider)
			call := ToolCall{ID: "selected", Name: "external", Arguments: `{}`}
			switch adapter {
			case "builtin":
				path := filepath.Join(t.TempDir(), "source.txt")
				if err := os.WriteFile(path, []byte("manual read"), 0o600); err != nil {
					t.Fatal(err)
				}
				args, _ := json.Marshal(map[string]string{"path": path})
				call.Name, call.Arguments = "read", string(args)
			case "local":
				manifest := localManifest(t, "echo")
				if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
					t.Fatal(err)
				}
				call.Name = manifest.StableID()
			case "mcp":
				server, _, _ := integrationServer(t, "success")
				addIntegrationMCP(t, a, mcp.Config{Name: "fixture", URL: server.URL})
				call.Name = mcp.ToolID("fixture", "echo")
			}
			id := createTestRun(t, a)
			if _, err := store.Update(id, func(s *Snapshot) error {
				s.Run.Status = StatusWaitingTool
				s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{call, {ID: "sibling", Name: "external", Arguments: `{}`}}})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			execute := func() (Snapshot, error) {
				if adapter == "exec" {
					return a.ExecuteTool(context.Background(), id, call.ID, []string{os.Args[0], "-test.run=^TestAgentLocalFixture$"})
				}
				return a.ExecuteRegistered(context.Background(), id, call.ID, call.Name)
			}
			got, err := execute()
			if err != nil || got.Run.Status != StatusWaitingTool || len(got.Actions) != 1 || !got.Run.BudgetActiveSince.IsZero() {
				t.Fatalf("manual adapter result: status=%s actions=%d budget=%v err=%v", got.Run.Status, len(got.Actions), got.Run.BudgetActiveSince, err)
			}
			action := got.Actions[0]
			if action.Adapter != adapter || action.Status != "completed" || action.Result == nil || action.Result.ExitCode != 0 {
				t.Fatalf("manual adapter failed: %+v", action)
			}
			pending := pendingToolCalls(&got)
			if len(pending) != 1 || pending["sibling"].ID != "sibling" || len(provider.requests) != 0 {
				t.Fatal("manual adapter consumed sibling or resumed the model")
			}
			// An executed manual batch has no independent model request/response.
			// Existing source verification therefore marks tool_execution invalid.
			if len(got.Requests) != 0 || len(got.Responses) != 0 || len(got.Evidence) != 1 || got.Evidence[0].Type != "tool_execution" || got.Evidence[0].Verification != "invalid" {
				t.Fatalf("manual batch source contract changed: requests=%d responses=%d evidence=%+v", len(got.Requests), len(got.Responses), got.Evidence)
			}
			if _, err := execute(); err == nil {
				t.Fatal("completed manual call replayed")
			}
			saved, err := store.Load(id)
			if err != nil || len(saved.Actions) != 1 || len(provider.requests) != 0 {
				t.Fatal("rejected replay changed the execution ledger", err)
			}
		})
	}
}

func TestManualToolCycleSettlesBeforeReturningAndPreservesGates(t *testing.T) {
	toolErr := errors.New("tool failed before result commit")
	for _, gate := range []string{"pending", "paused", "error", "cancelled"} {
		t.Run(gate, func(t *testing.T) {
			provider := &scriptedProvider{}
			a, store := newTestAgent(t, provider)
			id := createTestRun(t, a)
			parent, cancel := context.WithCancel(context.Background())
			defer cancel()
			var toolContext context.Context
			release, err := store.AcquireRunLock(id)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			got, err := a.executeManualToolCycle(parent, id, func(ctx context.Context) (Snapshot, error) {
				toolContext = ctx
				if gate == "cancelled" {
					cancel()
					if !errors.Is(ctx.Err(), context.Canceled) {
						t.Fatal("parent cancellation did not reach tool")
					}
				}
				snap, err := store.Update(id, func(s *Snapshot) error {
					s.Run.Goal = "committed tool state"
					s.Run.Status = StatusWaitingTool
					switch gate {
					case "pending":
						s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "sibling", Name: "external", Arguments: `{}`}}})
					case "paused":
						s.Run.Status = StatusPaused
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				// A returned projection may predate the final durable settlement.
				snap.Run.Goal = "stale projection"
				if gate == "error" {
					return snap, toolErr
				}
				if gate == "cancelled" {
					return snap, ctx.Err()
				}
				return snap, nil
			})
			var wantErr error
			if gate == "error" {
				wantErr = toolErr
			} else if gate == "cancelled" {
				wantErr = context.Canceled
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("error = %v, want %v", err, wantErr)
			}
			if got.Run.Goal != "committed tool state" || !got.Run.BudgetActiveSince.IsZero() {
				t.Fatal("returned stale or unsettled snapshot", got.Run)
			}
			if toolContext == nil || !errors.Is(toolContext.Err(), context.Canceled) {
				t.Fatal("manual tool context remained active")
			}
			if len(provider.requests) != 0 || len(got.Requests) != 0 {
				t.Fatal("a stop/error/pending gate resumed the model")
			}
			if gate == "pending" && len(pendingToolCalls(&got)) != 1 {
				t.Fatal("sibling pending call was consumed")
			}
		})
	}
}

func TestManualToolCycleReloadFailureKeepsOriginalError(t *testing.T) {
	toolErr := errors.New("original tool failure")
	for _, original := range []error{nil, toolErr} {
		name := "reload-error"
		if original != nil {
			name = "tool-error"
		}
		t.Run(name, func(t *testing.T) {
			a, store := newTestAgent(t, &scriptedProvider{})
			id := createTestRun(t, a)
			release, err := store.AcquireRunLock(id)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			got, err := a.executeManualToolCycle(context.Background(), id, func(context.Context) (Snapshot, error) {
				snap, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(store.root, id+".json")); err != nil {
					t.Fatal(err)
				}
				snap.Run.Status = StatusPaused
				return snap, original
			})
			want := original
			if want == nil {
				want = os.ErrNotExist
			}
			if !errors.Is(err, want) || got.Run.ID != id {
				t.Fatalf("reload changed original result/error: %s %v", got.Run.ID, err)
			}
		})
	}
}

func TestManualToolCycleResumesWithParentAfterStoppingToolContext(t *testing.T) {
	type contextKey struct{}
	parent := context.WithValue(context.Background(), contextKey{}, "parent")
	var toolContext context.Context
	requests := 0
	provider := settlementProviderFunc(func(ctx context.Context, request ModelRequest) (ModelResponse, error) {
		requests++
		if ctx.Err() != nil || ctx.Value(contextKey{}) != "parent" {
			t.Error("continuation did not inherit the live parent context")
		}
		if toolContext == nil || !errors.Is(toolContext.Err(), context.Canceled) {
			t.Error("model resumed before the manual tool context stopped")
		}
		return fixtureModelResponse(ModelResponse{Content: "verified response"}), nil
	})
	a, store := newTestAgent(t, provider)
	id := createTestRun(t, a)
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "manual", Name: "external", Arguments: `{}`}}})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	release, err := store.AcquireRunLock(id)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	got, err := a.executeManualToolCycle(parent, id, func(ctx context.Context) (Snapshot, error) {
		toolContext = ctx
		return a.recordManualTool(id, "manual", "operator observation")
	})
	if err != nil || got.Run.Status != StatusCompleted || requests != 1 || !got.Run.BudgetActiveSince.IsZero() {
		t.Fatalf("continuation: status=%s requests=%d budget=%v err=%v", got.Run.Status, requests, got.Run.BudgetActiveSince, err)
	}
	if len(got.Evidence) != 1 || got.Evidence[0].Verification != "unverified" {
		t.Fatal("manual result acquired execution provenance")
	}
}
