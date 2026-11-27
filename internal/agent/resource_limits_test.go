package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func TestResourceLimitsCreationValidation(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			for _, tc := range []struct {
				name    string
				ceiling WorkerCeiling
				role    string
			}{
				{"zero", WorkerCeiling{0, "user:request", "root-tree"}, ""},
				{"negative", WorkerCeiling{-1, "user:request", "root-tree"}, ""},
				{"out-of-range", WorkerCeiling{33, "user:request", "root-tree"}, ""},
				{"missing-source", WorkerCeiling{3, "  ", "root-tree"}, ""},
				{"missing-scope", WorkerCeiling{3, "user:request", ""}, ""},
				{"external-instance-scope", WorkerCeiling{3, "managed:pool", "external-instances"}, ""},
				{"child-override", WorkerCeiling{3, "user:request", "root-tree"}, "worker"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if _, err := a.CreateConfigured("invalid limit", RunConfiguration{ResourceLimits: &ResourceLimits{WorkerCeiling: &tc.ceiling}, role: tc.role}); err == nil {
						t.Fatal("invalid explicit worker ceiling accepted")
					}
					runs, err := store.List()
					if err != nil || len(runs) != 0 {
						t.Fatalf("rejected creation persisted a run: %+v %v", runs, err)
					}
				})
			}
		})
	}
}

func TestResourceLimitsPlanCapacityAtomicAndDurable(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			ceiling := WorkerCeiling{Capacity: 3, Source: "user:create-request", Scope: "root-tree"}
			root, err := a.CreateConfigured("declared capacity", RunConfiguration{ResourceLimits: &ResourceLimits{WorkerCeiling: &ceiling}})
			if err != nil {
				t.Fatal(err)
			}
			ceiling.Capacity, ceiling.Source = 32, "changed caller memory"
			if root.Plan.Capacity != 3 || root.Run.ResourceLimits.WorkerCeiling.Capacity != 3 || root.Run.ResourceLimits.WorkerCeiling.Source != "user:create-request" {
				t.Fatalf("creation did not pin the explicit declaration: %+v", root.Run.ResourceLimits)
			}
			for _, attempt := range []func() error{
				func() error { _, err := a.SetPlanCapacity(root.Run.ID, 5); return err },
				func() error {
					return modelPlan(a, root.Run.ID, map[string]any{"tasks": capacityTasks(4), "capacity": 5})
				},
			} {
				before, err := store.Load(root.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				var conflict *WorkerCeilingConflictError
				if err := attempt(); !errors.As(err, &conflict) || conflict.Capacity != 5 || conflict.Ceiling.Capacity != 3 || conflict.Ceiling.Source != "user:create-request" || conflict.Ceiling.Scope != "root-tree" {
					t.Fatalf("plan capacity escaped declared ceiling or lost provenance: %v", err)
				}
				after, err := store.Load(root.Run.ID)
				if err != nil || !reflect.DeepEqual(before.Plan, after.Plan) || len(before.Events) != len(after.Events) {
					t.Fatalf("rejected plan/capacity write was not atomic: %v", err)
				}
			}
			if err := modelPlan(a, root.Run.ID, map[string]any{"tasks": capacityTasks(4)}); err != nil {
				t.Fatal("declared capacity should permit the first plan:", err)
			}
			if _, err := a.SetPlanCapacity(root.Run.ID, 2); err != nil {
				t.Fatal(err)
			}
			other, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { other.Close() })
			reopened, err := New(other, MockProvider{}, "")
			if err != nil {
				t.Fatal(err)
			}
			state, err := reopened.WorkerCapacity(root.Run.ID)
			if err != nil || state.Capacity != 2 || state.WorkerCeiling == nil || state.WorkerCeiling.Capacity != 3 || state.WorkerCeiling.Source != "user:create-request" || state.WorkerCeiling.Scope != "root-tree" {
				t.Fatalf("reopened capacity projection lost ceiling: %+v %v", state, err)
			}
			if _, err := reopened.SetPlanCapacity(root.Run.ID, 3); err != nil {
				t.Fatal("raising plan capacity within the pinned ceiling:", err)
			}
			var conflict *WorkerCeilingConflictError
			if _, err := reopened.SetPlanCapacity(root.Run.ID, 4); !errors.As(err, &conflict) {
				t.Fatalf("reopened store forgot the ceiling: %v", err)
			}
		})
	}
}

func TestResourceLimitsTreeAdmission(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			provider := &dispatchFailureProvider{}
			a, store := treeCapacityAgent(t, backend, provider)
			limits := &ResourceLimits{WorkerCeiling: &WorkerCeiling{Capacity: 3, Source: "managed:research-pool", Scope: "root-tree"}}
			root, err := a.CreateConfigured("shared executor slots", RunConfiguration{ResourceLimits: limits})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.SetPlan(root.Run.ID, capacityTasks(4)); err != nil {
				t.Fatal(err)
			}
			_, child := capacityFixtureChild(t, a, root.Run.ID, "0")
			if _, err := a.SetPlan(child.RunID, capacityTasks(2)); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ClaimTask(child.RunID, "0", "nested-claim", 3600); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ClaimTask(root.Run.ID, "1", "root-claim", 3600); err != nil {
				t.Fatal(err)
			}
			if _, err := a.ClaimTask(child.RunID, "1", "overflow", 3600); !errors.Is(err, errWorkerCapacity) {
				t.Fatalf("nested claim escaped the root-tree ceiling: %v", err)
			}
			if _, err := a.DispatchTask(context.Background(), root.Run.ID, "2"); !errors.Is(err, errWorkerCapacity) {
				t.Fatalf("dispatch escaped the same ceiling: %v", err)
			}
			state, err := a.WorkerCapacity(child.RunID)
			if err != nil || state.RootRunID != root.Run.ID || state.Capacity != 3 || state.Active != 3 || !reflect.DeepEqual(state.WorkerCeiling, limits.WorkerCeiling) {
				t.Fatalf("nested tree view diverged from admission: %+v %v", state, err)
			}
			for _, id := range []string{root.Run.ID, child.RunID} {
				saved, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				taskID := "2"
				if id == child.RunID {
					taskID = "1"
				}
				task, err := planTask(&saved, taskID)
				if err != nil || task.Status != "pending" || task.WorkerID != "" || task.ChildRunID != "" || len(saved.Requests) != 0 {
					t.Fatalf("full ceiling started pending work: %+v %v", task, err)
				}
			}
			if provider.calls.Load() != 0 {
				t.Fatal("blocked dispatch called the model")
			}
			independent, err := a.Create("the prose says capacity 3; configuration is absent")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.SetPlan(independent.Run.ID, capacityTasks(4)); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 4; i++ {
				if _, err := a.ClaimTask(independent.Run.ID, fmt.Sprint(i), fmt.Sprintf("independent-%d", i), 3600); err != nil {
					t.Fatal("unrelated root inherited a foreign or inferred ceiling:", err)
				}
			}
			state, err = a.WorkerCapacity(independent.Run.ID)
			if err != nil || state.Capacity != 5 || state.Active != 4 || state.WorkerCeiling != nil {
				t.Fatalf("undeclared limit changed the existing default: %+v %v", state, err)
			}
		})
	}
}

func TestResourceLimitsPersistedCapacityConflict(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			provider := &dispatchFailureProvider{}
			a, store := treeCapacityAgent(t, backend, provider)
			root, err := a.CreateConfigured("persisted conflict", RunConfiguration{ResourceLimits: &ResourceLimits{WorkerCeiling: &WorkerCeiling{3, "user:request", "root-tree"}}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.SetPlan(root.Run.ID, capacityTasks(2)); err != nil {
				t.Fatal(err)
			}
			// Simulate a mismatched saved plan; admission must use the pinned fact.
			before, err := store.Update(root.Run.ID, func(s *Snapshot) error { s.Plan.Capacity = 5; return nil })
			if err != nil {
				t.Fatal(err)
			}
			for _, attempt := range []func() error{
				func() error { _, err := a.SetPlan(root.Run.ID, capacityTasks(2)); return err },
				func() error { _, err := a.ClaimTask(root.Run.ID, "0", "conflict", 30); return err },
				func() error { _, err := a.DispatchTask(context.Background(), root.Run.ID, "0"); return err },
				func() error { _, err := a.WorkerCapacity(root.Run.ID); return err },
			} {
				var conflict *WorkerCeilingConflictError
				if err := attempt(); !errors.As(err, &conflict) {
					t.Fatalf("persisted plan conflict was silently accepted: %v", err)
				}
			}
			after, err := store.Load(root.Run.ID)
			if err != nil || !reflect.DeepEqual(before.Plan, after.Plan) || len(before.Events) != len(after.Events) || len(after.Workers) != 0 || len(after.Requests) != 0 || provider.calls.Load() != 0 {
				t.Fatalf("rejected admission changed plan/execution: %v", err)
			}
			runs, err := store.List()
			if err != nil || len(runs) != 1 {
				t.Fatalf("rejected dispatch created a child run: %+v %v", runs, err)
			}
			if _, err := a.SetPlanCapacity(root.Run.ID, 3); err != nil {
				t.Fatal("correcting the unstarted plan failed:", err)
			}
			if _, err := a.ClaimTask(root.Run.ID, "0", "corrected", 30); err != nil {
				t.Fatal("corrected plan did not become admissible:", err)
			}
		})
	}
}

func TestResourceLimitsHigherCeilingKeepsExistingDefault(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, _ := treeCapacityAgent(t, backend, MockProvider{})
			root, err := a.CreateConfigured("explicit maximum is not a dispatch target", RunConfiguration{ResourceLimits: &ResourceLimits{WorkerCeiling: &WorkerCeiling{7, "user:request", "root-tree"}}})
			if err != nil {
				t.Fatal(err)
			}
			if root.Plan.Capacity != 5 {
				t.Fatalf("higher ceiling widened the existing default: %+v", root.Plan)
			}
			if _, err := a.SetPlanCapacity(root.Run.ID, 7); err != nil {
				t.Fatal("explicit capacity within a higher declared ceiling rejected:", err)
			}
		})
	}
}
