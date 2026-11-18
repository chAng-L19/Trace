package agent

import (
	"fmt"
	"strings"
	"testing"
)

// Muteki c60c8fee coordinator_reason.py binds decisions to the input watermark,
// not later graph changes. Trace already archives that boundary per request.
func TestPlannerSetPlanKeepsUnseenFactBatch(t *testing.T) {
	for _, succeeded := range []bool{false, true} {
		t.Run(fmt.Sprint(succeeded), func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id := createTestRun(t, a)
			tasks := []PlanTask{{ID: "task", Goal: "work"}}
			if _, err := a.SetPlan(id, tasks); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(id, func(s *Snapshot) error {
				for i := 0; i < 25; i++ {
					addEvent(s, "worker_finished", fmt.Sprintf("cursor-fact-%02d", i), nil)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_, through, err := a.plannerContextAt(id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.Update(id, func(s *Snapshot) error {
				s.Requests = append(s.Requests, ModelRequest{ID: "cursor-request", PlannerThrough: through})
				response := ResponseRecord{RequestID: "cursor-request"}
				if !succeeded {
					response.Error = "provider interrupted"
				}
				s.Responses = append(s.Responses, response)
				addEvent(s, "worker_finished", "cursor-late-fact", nil)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err = a.SetPlan(id, tasks); err != nil {
				t.Fatal(err)
			}
			text, retryThrough, err := a.plannerContextAt(id)
			if err != nil {
				t.Fatal(err)
			}
			if succeeded {
				if strings.Contains(text, "cursor-fact-19") || !strings.Contains(text, "cursor-fact-20") || !strings.Contains(text, "cursor-fact-24") || !strings.Contains(text, "cursor-late-fact") {
					t.Fatalf("plan update skipped unseen observations: %s", text)
				}
			} else if retryThrough != through || !strings.Contains(text, "cursor-fact-00") || !strings.Contains(text, "cursor-fact-19") || strings.Contains(text, "cursor-fact-20") {
				t.Fatalf("plan update acknowledged failed request: through=%d want=%d text=%s", retryThrough, through, text)
			}
		})
	}
}
