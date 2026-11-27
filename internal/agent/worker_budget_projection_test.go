package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWorkerBudgetProjectionUnlimited(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, deadline := range []time.Time{{}, now.Add(-time.Second)} {
		// Even an elapsed caller deadline is not a time budget without a budget cause.
		view := projectWorkerBudget(1, "worker", deadline, nil, now)
		raw, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		const want = `{"estimated_seconds":1,"estimate_is_deadline":false,"effective_budget":{"limited":false,"source":"none"}}`
		if string(raw) != want {
			t.Fatalf("unlimited projection = %s, want %s", raw, want)
		}
	}
}

func TestWorkerBudgetProjectionCountsDownSelectedDeadline(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	deadline := start.Add(10 * time.Second)
	for _, tc := range []struct{ runID, source string }{{"worker", "own"}, {"root", "ancestor"}} {
		t.Run(tc.source, func(t *testing.T) {
			cause := &timeBudgetCause{RunID: tc.runID}
			first := projectWorkerBudget(1, "worker", deadline, cause, start)
			for _, at := range []struct {
				elapsed time.Duration
				left    int64
			}{{2250 * time.Millisecond, 7750}, {10 * time.Second, 0}, {11 * time.Second, 0}} {
				view := projectWorkerBudget(1, "worker", deadline, cause, start.Add(at.elapsed))
				b := view.EffectiveBudget
				if view.EstimatedSeconds != 1 || view.EstimateIsDeadline || !b.Limited || b.Source != tc.source || b.SourceRunID != tc.runID || b.Deadline == nil || !b.Deadline.Equal(deadline) || b.RemainingMillis == nil || *b.RemainingMillis != at.left {
					t.Fatalf("elapsed %s: projection = %+v, budget = %+v; want %d ms from %s", at.elapsed, view, b, at.left, tc.runID)
				}
			}
			if first.EffectiveBudget.RemainingMillis == nil || *first.EffectiveBudget.RemainingMillis != 10000 {
				t.Fatal("a later projection rewrote the first observation")
			}
		})
	}
}

func TestWorkerBudgetProjectionArchivedRounds(t *testing.T) {
	for _, limited := range []bool{false, true} {
		name := "unlimited"
		if limited {
			name = "ancestor"
		}
		t.Run(name, func(t *testing.T) {
			a, store := newTestAgent(t, MockProvider{})
			root := createTestRun(t, a)
			task := PlanTask{ID: "stage", Goal: "review the supplied notes", EstimatedSeconds: 1}
			if _, err := a.SetPlan(root, []PlanTask{task}); err != nil {
				t.Fatal(err)
			}
			parent, err := a.ClaimTask(root, task.ID, "worker", 30)
			if err != nil {
				t.Fatal(err)
			}
			child := registerBudgetChild(t, a, root, task.ID, "worker")
			manifest, err := a.newWorkerContext(&parent, parent.Plan.Tasks[0])
			if err != nil {
				t.Fatal(err)
			}
			manifest.RunID, manifest.WorkerID = child, "worker"
			manifestHash := workerContextHash(manifest)
			if _, err := store.Update(child, func(s *Snapshot) error {
				s.WorkerContext = manifest
				s.Run.ElapsedMillis = 3000 // Past the one-second estimate before either request.
				if limited {
					s.Run.Budget.MaxSeconds = 180
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if limited {
				if _, err := store.Update(root, func(s *Snapshot) error {
					// The larger nominal cap wins because only 90 seconds remain.
					s.Run.Budget.MaxSeconds, s.Run.ElapsedMillis = 600, 510000
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			caller, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			earliest := time.Now().Add(90 * time.Second)
			ctx, stop, err := a.beginBudgetContext(caller, child)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			latest := time.Now().Add(90 * time.Second)
			first, err := a.preparePiRequest(ctx, child)
			if err != nil {
				t.Fatal(err)
			}
			// Break only the source binding. The frozen estimate is still 1, but
			// the next revalidated WorkerContext must no longer return it.
			if _, err := store.Update(root, func(s *Snapshot) error {
				s.Plan.Tasks[0].WorkerID = "other-worker"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(child, func(s *Snapshot) error {
				s.Responses = append(s.Responses, ResponseRecord{RequestID: first.ID, Response: ModelResponse{Content: "notes reviewed"}})
				s.Messages = append(s.Messages, Message{Role: "assistant", Content: "notes reviewed"})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.preparePiRequest(ctx, child); err != nil {
				t.Fatal(err)
			}
			saved, err := store.Load(child)
			if err != nil {
				t.Fatal(err)
			}
			if len(saved.Requests) != 2 || workerContextHash(saved.Requests[0]) != workerContextHash(first) || workerContextHash(saved.WorkerContext) != manifestHash {
				t.Fatal("expected two archived rounds, with the original request and manifest unchanged")
			}
			var deadline time.Time
			for i, req := range saved.Requests {
				_, raw, ok := strings.Cut(requestRuntimeObservations(child, req), workerBudgetHeading)
				var view workerBudgetProjection
				if !ok || json.Unmarshal([]byte(raw), &view) != nil {
					t.Fatalf("round %d missing budget JSON in archived runtime observations: %q", i, raw)
				}
				wantEstimate := []int64{1, 0}[i]
				b := view.EffectiveBudget
				if view.EstimatedSeconds != wantEstimate || view.EstimateIsDeadline || b.Limited != limited {
					t.Fatalf("round %d: %s", i, raw)
				}
				if !limited {
					if b.Source != "none" || b.RemainingMillis != nil || b.Deadline != nil {
						t.Fatalf("caller deadline leaked into unlimited budget: %s", raw)
					}
					continue
				}
				if b.Source != "ancestor" || b.SourceRunID != root || b.Deadline == nil || b.Deadline.Before(earliest) || b.Deadline.After(latest) || b.RemainingMillis == nil || *b.RemainingMillis != b.Deadline.Sub(req.CreatedAt).Milliseconds() {
					t.Fatalf("round %d did not sample the real ancestor budget at request creation: %s", i, raw)
				}
				if i > 0 && !b.Deadline.Equal(deadline) {
					t.Fatal("recomputed a new deadline from the inactive ancestor's fixed balance")
				}
				deadline = *b.Deadline
			}
			for _, m := range saved.Messages {
				if strings.Contains(m.Content, workerBudgetHeading) || m.ID == child+"-runtime-observations" {
					t.Fatal("request budget was appended to durable conversation history")
				}
			}
		})
	}
}
