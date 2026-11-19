package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func modelPlan(a *Agent, id string, args any) error {
	raw, _ := json.Marshal(args)
	_, err := a.builtin(context.Background(), id, ToolCall{Name: "plan", Arguments: string(raw)})
	return err
}

func TestModelPlanCapacityAtomicAdmission(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			id := createTestRun(t, a)
			tasks := capacityTasks(5)
			if err := modelPlan(a, id, map[string]any{}); err != nil {
				t.Fatal("read plan:", err)
			}
			if err := modelPlan(a, id, map[string]any{"tasks": []PlanTask{}}); err != nil {
				t.Fatal("empty plan:", err)
			}
			empty, _ := store.Load(id)
			if empty.Plan.Capacity != 0 {
				t.Fatal("empty model plan silently configured admission")
			}
			before, _ := store.Load(id)
			for _, args := range []any{
				map[string]any{"tasks": tasks},
				map[string]any{"tasks": tasks, "capacity": 33},
				map[string]any{"tasks": tasks, "capacity": 0},
				map[string]any{"capacity": -1},
			} {
				if err := modelPlan(a, id, args); err == nil {
					t.Fatal("missing/invalid capacity accepted")
				}
				after, _ := store.Load(id)
				if !reflect.DeepEqual(before.Plan, after.Plan) || len(before.Events) != len(after.Events) {
					t.Fatal("rejected capacity published tasks or events")
				}
			}
			wake, unsubscribe := store.SubscribeScheduling(id)
			defer unsubscribe()
			if err := modelPlan(a, id, map[string]any{"tasks": tasks, "capacity": 3}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-wake:
			default:
				t.Fatal("atomic commit did not wake scheduling")
			}
			saved, _ := store.Load(id)
			if saved.Plan.Capacity != 3 || len(saved.Plan.Tasks) != 5 {
				t.Fatal("tasks and capacity not committed together")
			}
			for i := 0; i < 3; i++ {
				if _, err := a.ClaimTask(id, tasks[i].ID, fmt.Sprint("claim-", i), 30); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := a.ClaimTask(id, tasks[3].ID, "overflow", 30); !errors.Is(err, errWorkerCapacity) {
				t.Fatalf("fourth claim passed three-slot admission: %v", err)
			}
			saved, _ = store.Load(id)
			if saved.Plan.Tasks[3].Status != "pending" || len(saved.Requests) != 0 || len(saved.Workers) != 0 {
				t.Fatal("pending work created a waiting worker or model request")
			}
			if err := modelPlan(a, id, map[string]any{"mode": "append", "tasks": []PlanTask{{ID: "six", Goal: "next object"}}}); err != nil {
				t.Fatal(err)
			}
			saved, _ = store.Load(id)
			if saved.Plan.Capacity != 3 {
				t.Fatal("append reset capacity")
			}
		})
	}
}

func TestModelPlanCapacityRollbackAndNestedOwnership(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			id := createTestRun(t, a)
			if _, err := a.SetPlanCapacity(id, 2); err != nil {
				t.Fatal(err)
			}
			// A separately configured ceiling permits first-plan writes without widening it.
			if err := modelPlan(a, id, map[string]any{"tasks": capacityTasks(1)}); err != nil {
				t.Fatal(err)
			}
			before, _ := store.Load(id)
			if before.Plan.Capacity != 2 {
				t.Fatal("first plan overwrote configured ceiling")
			}
			if err := modelPlan(a, id, map[string]any{"mode": "append", "tasks": []PlanTask{{ID: "invalid-zero", Goal: "should not persist"}}, "capacity": 0}); err == nil {
				t.Fatal("explicit zero accepted on configured root")
			}
			if err := modelPlan(a, id, map[string]any{"mode": "invalid", "tasks": capacityTasks(2), "capacity": 3}); err == nil {
				t.Fatal("invalid plan accepted")
			}
			after, _ := store.Load(id)
			if !reflect.DeepEqual(before.Plan, after.Plan) || len(before.Events) != len(after.Events) {
				t.Fatal("invalid plan changed capacity/events")
			}
			child, _ := a.Create("nested task")
			if _, err := store.Update(child.Run.ID, func(s *Snapshot) error { s.Run.ParentRunID = id; s.Run.ParentTaskID = "0"; return nil }); err != nil {
				t.Fatal(err)
			}
			if err := modelPlan(a, child.Run.ID, map[string]any{"tasks": capacityTasks(1)}); err != nil {
				t.Fatal("nested plan incorrectly requires root capacity:", err)
			}
			nestedBefore, _ := store.Load(child.Run.ID)
			err := modelPlan(a, child.Run.ID, map[string]any{"mode": "append", "tasks": []PlanTask{{ID: "next", Goal: "next stage"}}, "capacity": 3})
			if err == nil || !strings.Contains(err.Error(), "root conversation") {
				t.Fatalf("nested capacity ownership lost: %v", err)
			}
			nestedAfter, _ := store.Load(child.Run.ID)
			if !reflect.DeepEqual(nestedBefore.Plan, nestedAfter.Plan) {
				t.Fatal("nested rejected capacity left a partial plan")
			}
		})
	}
}
