package agent

import (
	"errors"
	"testing"
)

func TestSchedulingResourceProjectionReadsEachOwnerOnce(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			id := createTestRun(t, a)
			tasks := []PlanTask{
				{ID: "first", Goal: "first owner", ResourceKeys: []string{"one", "two", "three"}},
				{ID: "second", Goal: "second owner", ResourceKeys: []string{"one", "two", "three"}},
				{ID: "candidate", Goal: "next write", ResourceKeys: []string{"unshared", "two", "one", "three"}},
			}
			if _, err := a.SetPlan(id, tasks); err != nil {
				t.Fatal(err)
			}
			registerBudgetChild(t, a, id, "first", "first-worker")
			child := registerBudgetChild(t, a, id, "second", "second-worker")
			loadSamples := func() int64 {
				for _, stage := range store.StoreDiagnostics().Stages {
					if stage.Stage == "load_verify" {
						return stage.Samples
					}
				}
				return 0
			}
			check := func(t *testing.T, busy bool) {
				t.Helper()
				var reads int64
				_, err := store.Update(id, func(s *Snapshot) error {
					before := loadSamples()
					err := a.taskResourcesLocked(s, tasks[2])
					reads = loadSamples() - before
					return err
				})
				if reads != 2 {
					t.Fatalf("verified source reads = %d, want one per owner", reads)
				}
				if !busy {
					if err != nil {
						t.Fatal(err)
					}
					return
				}
				want := ResourceBusyError{TaskID: "candidate", OwnerTaskID: id + "/second", Key: "two"}
				var got *ResourceBusyError
				if !errors.As(err, &got) || *got != want || err.Error() != want.Error() {
					t.Fatalf("resource error = %v, want %+v", err, want)
				}
			}
			check(t, false)
			for _, test := range []struct {
				name   string
				action Action
				busy   bool
			}{
				{"started", Action{Status: "started"}, true},
				{"uncertain", Action{Status: "uncertain"}, true},
				{"result_ready", Action{Status: "result_ready"}, true},
				{"cleanup_pending", Action{Status: "completed", Adapter: "exec", Result: &ToolResult{WorkerCleanupError: "pending"}}, true},
				{"cleanup_completed", Action{Status: "completed", Adapter: "exec", Result: &ToolResult{}, ResourceCleanupStatus: "completed"}, false},
			} {
				t.Run(test.name, func(t *testing.T) {
					if _, err := store.Update(child, func(s *Snapshot) error {
						s.Actions = []Action{test.action}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					check(t, test.busy)
				})
			}
			if _, err := store.Update(id, func(s *Snapshot) error {
				s.Plan.Tasks[1].ChildRunID = "missing-source"
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			check(t, true)
		})
	}
}
