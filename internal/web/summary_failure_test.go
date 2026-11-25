package web

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"trace/internal/agent"
)

func TestAsynchronousSummaryFailurePreservesCommittedError(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			root := t.TempDir()
			if backend == "sqlite" {
				if err := agent.MigrateStoreToSQLite(root); err != nil {
					t.Fatal(err)
				}
			}
			store, err := agent.NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			providerFailure := errors.New("permanent web summary failure")
			calls := 0
			a, err := agent.New(store, providerFunc(func(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
				calls++
				if request.Purpose != "context_summary" {
					return agent.ModelResponse{}, fmt.Errorf("expected summary, got %q", request.Purpose)
				}
				return agent.ModelResponse{}, providerFailure
			}), "")
			if err != nil {
				t.Fatal(err)
			}
			if err := a.ConfigureContext(agent.ContextConfig{WindowTokens: 128000, OutputReserve: 1000, CompactionMode: "semantic"}); err != nil {
				t.Fatal(err)
			}
			s, err := New(a, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			created, err := a.Create("preserve the committed failure and operation diagnostics")
			if err != nil {
				t.Fatal(err)
			}
			id := created.Run.ID
			before, err := store.Update(id, func(snap *agent.Snapshot) error {
				snap.Messages = []agent.Message{{Role: "user", Content: "keep exact objective"}}
				for i := 0; i < 8; i++ {
					call := fmt.Sprintf("old-%d", i)
					snap.Messages = append(snap.Messages,
						agent.Message{Role: "assistant", ToolCalls: []agent.ToolCall{{ID: call, Name: "external", Arguments: `{}`}}},
						agent.Message{Role: "tool", ToolCallID: call, Content: strings.Repeat("observation ", 2000)})
				}
				snap.Messages = append(snap.Messages, agent.Message{Role: "user", Content: "retain original identifiers"})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			request(t, s.Handler(), "POST", "/api/runs/"+id+"/resume", map[string]any{}, 200)
			s.wg.Wait()
			saved, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || saved.Run.Status != agent.StatusFailed || saved.Run.Error != providerFailure.Error() {
				t.Fatalf("web overwrote committed failure: calls=%d status=%s pause=%q saved=%q", calls, saved.Run.Status, saved.Run.PauseReason, saved.Run.Error)
			}
			if len(saved.Responses) != 1 || saved.Responses[0].Error != providerFailure.Error() || len(saved.Compactions) != 1 || saved.Compactions[0].Status != "failed" || len(saved.Actions) != 0 || len(saved.Messages) != len(before.Messages) {
				t.Fatal("summary failure changed provider receipt, history, or execution")
			}
			event := saved.Events[len(saved.Events)-1]
			raw := fmt.Sprint(event.Data)
			if event.Type != "web_operation_error" || !strings.Contains(raw, providerFailure.Error()) {
				t.Fatalf("operation diagnostics missing: %+v", event)
			}
		})
	}
}
