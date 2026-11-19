package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRuntimeAuditProviderRecoveryKeepsReportVisit(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, kind := range []string{"overflow", "length"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				store, _ := u7Store(t, backend)
				// The real child's report provider cannot return until this callback
				// returns. Inside it, settle the parent's response, then Observe the
				// committed root before executing its summary. Neither barrier polls.
				_, _, _, w, _ := reportWorkerFixtureInStore(t, store, func(a *Agent, store *Store, id string, n int, _ ModelRequest) error {
					if n != 3 {
						return nil
					}
					ctx := context.Background()
					_, err := store.Update(id, func(s *Snapshot) error {
						s.Run.Context = DefaultContextConfig()
						s.Messages = append(s.Messages, Message{Role: "user", Content: s.Run.Goal}, Message{Role: "assistant", Content: strings.Repeat("historical observation ", 200)}, Message{Role: "user", Content: "Keep the active report visit."})
						return nil
					})
					if err != nil {
						return err
					}
					failed, err := a.preparePiRequest(ctx, id)
					if err != nil || failed.Purpose != "" {
						return fmt.Errorf("expected normal parent request: purpose=%q err=%v", failed.Purpose, err)
					}
					var response ModelResponse
					var failure error = &ProviderHTTPError{StatusCode: 400, Body: "context_length_exceeded"}
					if kind == "length" {
						response = ModelResponse{StopReason: "length", ToolCalls: []ToolCall{{ID: "truncated", Name: "write", Arguments: "{"}}}
						failure = nil
					}
					retries := 0
					s, _, err := a.settlePiResponse(ctx, id, failed, response, failure, &retries)
					if err != nil {
						return err
					}
					view, err := store.Observe(id)
					if err != nil || view.Run.Status != StatusRunning || len(s.Responses) != 1 || s.Responses[0].RequestID != failed.ID || s.Responses[0].Error == "" || len(s.Compactions) != 1 || s.Compactions[0].Status != "pending" || len(s.Requests) != 2 || len(s.Actions)+len(pendingToolCalls(&s)) != 0 {
						return fmt.Errorf("failed response/summary admission not atomic: run=%s responses=%d compactions=%d requests=%d err=%v", view.Run.Status, len(s.Responses), len(s.Compactions), len(s.Requests), err)
					}
					root := *a
					root.provider = &modelRetryProvider{responses: []ModelResponse{{Content: "Preserved the original observation."}}}
					s, retry, err := root.recoverProviderContext(ctx, id, failed, response, failure)
					if err != nil || !retry || len(s.Compactions) != 1 || s.Compactions[0].Status != "completed" || len(s.Actions)+len(pendingToolCalls(&s)) != 0 {
						return fmt.Errorf("parent recovery did not finish without replay: retry=%v err=%v", retry, err)
					}
					return nil
				}, false)
				child, err := store.Load(w.RunID)
				if err != nil || w.Visit != 1 || w.CleanupStatus != "completed" || child.Result.Status != "success" || len(child.Actions) != 1 || child.Actions[0].CallID != "read-proof" {
					t.Fatalf("report lost or restarted its original visit: worker=%+v actions=%d err=%v", w, len(child.Actions), err)
				}
			})
		}
	}
}
