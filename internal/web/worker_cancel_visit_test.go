package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"trace/internal/agent"
)

func TestWorkerCancelAPIExpectedVisit(t *testing.T) {
	for _, test := range []struct {
		name, body string
		status     int
		cancelled  bool
	}{
		{"matching", "{\"expected_visit\":2}", http.StatusOK, true},
		{"stale", "{\"expected_visit\":1}", http.StatusConflict, false},
		{"explicit-zero", "{\"expected_visit\":0}", http.StatusConflict, false},
		{"compatibility", "{}", http.StatusOK, true},
		{"negative", "{\"expected_visit\":-1}", http.StatusBadRequest, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, a := fixture(t, nil)
			parent, err := a.Create("cancel observed worker")
			if err != nil {
				t.Fatal(err)
			}
			child, err := a.Create("cancel child fixture")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.Store().Update(child.Run.ID, func(snapshot *agent.Snapshot) error {
				snapshot.Run.ParentRunID, snapshot.Run.ParentTaskID, snapshot.Run.Status = parent.Run.ID, "work", agent.StatusPaused
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Store().Update(parent.Run.ID, func(snapshot *agent.Snapshot) error {
				snapshot.Workers = []agent.AgentWorker{{ID: "worker", TaskID: "work", RunID: child.Run.ID, Visit: 2, Status: "paused", CleanupStatus: "completed"}}
				snapshot.Plan.Tasks = []agent.PlanTask{{ID: "work", Goal: "cancel observed worker", WorkerID: "worker", ChildRunID: child.Run.ID, Status: "running"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			s.registerPlannerRoutes(mux)
			path := fmt.Sprintf("/api/runs/%s/workers/worker/cancel", parent.Run.ID)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(test.body)))
			if response.Code != test.status {
				t.Fatalf("want HTTP %d, got %d: %s", test.status, response.Code, response.Body.String())
			}
			if test.status == http.StatusConflict && !strings.Contains(response.Body.String(), "stale_visit") {
				t.Fatal("missing stale_visit conflict")
			}
			child, err = a.Store().Load(child.Run.ID)
			if err != nil || (child.Run.Status == agent.StatusCancelled) != test.cancelled {
				t.Fatalf("unexpected cancellation: %+v, %v", child.Run, err)
			}
		})
	}
}
