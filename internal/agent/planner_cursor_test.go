package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestPlainRootRequestKeepsReadWithoutPlanner(t *testing.T) {
	a, _ := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	req, err := a.preparePiRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if req.PlannerThrough != 0 || strings.Contains(requestRuntimeObservations(id, req), "Planner observations (data):") {
		t.Fatal("plain root request unexpectedly received a planner section or cursor")
	}
	for _, tool := range req.Tools {
		if tool.Function.Name == "read" {
			return
		}
	}
	t.Fatal("plain root request lost its direct read tool")
}

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

// New roots coordinate before dispatch; delegated Workers retain execution.
func TestPlannerRequestRoleFollowsDelegation(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			root, err := a.Create("coordinate collection")
			if err != nil {
				t.Fatal(err)
			}
			id := root.Run.ID
			if _, err := a.SetPlan(id, []PlanTask{{ID: "collect", Goal: "collect a local fixture", ResourceKeys: []string{"fixture:one"}}}); err != nil {
				t.Fatal(err)
			}
			request := func(agent *Agent, run string) string {
				t.Helper()
				req, err := agent.preparePiRequest(context.Background(), run)
				if err != nil {
					t.Fatal(err)
				}
				return requestRuntimeObservations(run, req)
			}
			if text := request(a, id); strings.Contains(text, "execute simple work directly") || !strings.Contains(text, "from the first turn") {
				t.Fatal("new root lacks coordinator role before dispatch")
			}
			local, worker := capacityFixtureChild(t, a, id, "collect")
			text := request(a, id)
			for _, required := range []string{"Coordinate delegated work", "resource_keys", "review outcomes and cleanup", "produce the final report", "worker_wait", "fixture:one"} {
				if !strings.Contains(text, required) {
					t.Fatalf("delegated request missing %q: %s", required, text)
				}
			}
			if strings.Contains(text, "execute simple work directly") {
				t.Fatal("delegated root still receives a conflicting execution role")
			}
			if _, err := local.SetPlan(worker.RunID, []PlanTask{{ID: "read", Goal: "read the assigned fixture"}}); err != nil {
				t.Fatal(err)
			}
			if text := request(local, worker.RunID); !strings.Contains(text, "execute simple work directly") || strings.Contains(text, "Coordinate delegated work") {
				t.Fatal("assigned worker lost its execution role")
			}
			saved, err := store.Load(id)
			if err != nil || saved.Plan.Tasks[0].ResourceKeys[0] != "fixture:one" || saved.Workers[0].RunID != worker.RunID {
				t.Fatal("request role changed task ownership")
			}
		})
	}
}
