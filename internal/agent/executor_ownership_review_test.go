package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestExecutorOwnershipExplicitReview(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, mode := range []string{"delivered", "pending", "ordinary_manual", "process_error"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				a, local, store, root, w := executorFixture(t, backend)
				call := ToolCall{ID: "sent", Name: "exec", Arguments: `{}`}
				responsibilityPending(t, local, w.RunID, call)
				ctx := context.Background()
				if mode == "pending" || mode == "ordinary_manual" {
					ctx = context.WithValue(ctx, piDeferredResultKey{}, true)
				}
				executions := 0
				snap, err := local.executeAction(ctx, w.RunID, call.ID, "mcp", nil, func() ToolResult {
					executions++
					if _, err := store.Update(root, func(s *Snapshot) error { s.Run.Status = StatusPaused; return nil }); err != nil {
						t.Fatal(err)
					}
					return ToolResult{Stdout: "remote receipt; effects require separate observation"}
				})
				if err != nil || len(snap.Actions) != 1 {
					t.Fatalf("setup: %v", err)
				}
				original := *snap.Actions[0].Result
				request := snap.Actions[0].RequestID
				changed := 0
				for _, e := range snap.Events {
					if e.Type == "action_executor_changed" {
						changed = e.Sequence
					}
				}
				if changed == 0 {
					t.Fatal("missing original visit event")
				}
				if _, err = store.Update(w.RunID, func(s *Snapshot) error {
					s.Run.Status = StatusPaused
					s.WorkerSettlement = &WorkerSettlement{ParentRunID: root, TaskID: w.TaskID, WorkerID: w.ID, RunID: w.RunID, Visit: w.Visit, CleanupVersion: 1, CleanupStatus: "uncertain", ObservedAt: time.Now().UTC()}
					if mode == "process_error" {
						s.Actions[0].ResourceCleanupError = "actual process cleanup not observed"
					}
					local.refreshWorkerCleanupReceiptLocked(s)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				encode := func(sequence int, reconciled bool) string {
					raw, err := json.Marshal(map[string]any{"executor_ownership_review": executorOwnershipReview{ChangedEventSequence: sequence, EffectsReconciled: reconciled, Observation: "Inspected the remote object; effects reconciled, no replay requested."}})
					if err != nil {
						t.Fatal(err)
					}
					return string(raw)
				}
				for _, invalid := range []struct{ request, content string }{{"foreign", encode(changed, true)}, {request, encode(changed+1, true)}, {request, encode(changed, false)}} {
					if _, err := a.RecordToolObservation(w.RunID, invalid.request, call.ID, invalid.content); err == nil {
						t.Fatal("unbound review accepted")
					}
				}
				if mode == "ordinary_manual" {
					ordinary, err := a.RecordToolObservation(w.RunID, request, call.ID, "ordinary tool observation")
					if err != nil || ordinary.Actions[0].ExecutorOwnershipError == "" || ordinary.WorkerSettlement.CleanupStatus != "uncertain" {
						t.Fatalf("ordinary string cleared hold: %v", err)
					}
				}
				before, err := store.Load(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				beforeResults := 0
				for _, m := range before.Messages {
					if m.Role == "tool" {
						beforeResults++
					}
				}
				reviewed, err := a.RecordToolObservation(w.RunID, request, call.ID, encode(changed, true))
				if err != nil {
					t.Fatal(err)
				}
				action := reviewed.Actions[0]
				if action.ExecutorOwnershipError != "" || action.Status != "resolved_manually" || !reflect.DeepEqual(*action.Result, original) || executions != 1 {
					t.Fatalf("review altered/replayed original: %+v", action)
				}
				afterResults := 0
				for _, m := range reviewed.Messages {
					if m.Role == "tool" {
						afterResults++
					}
				}
				want := beforeResults
				if mode == "pending" {
					want++
				}
				if afterResults != want {
					t.Fatalf("historical result duplicated or pending observation lost: %d want %d", afterResults, want)
				}
				if mode == "process_error" {
					if action.ResourceCleanupError != "actual process cleanup not observed" || toolCleanupError(reviewed) == nil || reviewed.WorkerSettlement.CleanupStatus != "uncertain" {
						t.Fatal("external review waived real process cleanup")
					}
					return
				}
				if toolCleanupError(reviewed) != nil || reviewed.WorkerSettlement.CleanupStatus != "completed" {
					t.Fatalf("review did not refresh cleanup: %+v", reviewed.WorkerSettlement)
				}
				parent, err := store.Update(root, func(s *Snapshot) error { return a.applyWorkerSettlementLocked(s, s.Workers[0]) })
				if err != nil || occupiedWorkerSlots(&parent) != 0 || taskDone(parent.Plan.Tasks[0]) {
					t.Fatalf("review did not permit normal paused retirement, or fabricated success: %+v %v", parent.Plan, err)
				}
				for _, e := range reviewed.Evidence {
					if e.Verification == "verified" {
						t.Fatal("manual review promoted unknown original evidence")
					}
				}
			})
		}
	}
}
