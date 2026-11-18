package web

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"trace/internal/agent"
)

func TestPausedConversationDeleteAfterControllerRestart(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, mode := range []string{"hide", "permanent"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				path := t.TempDir()
				if backend == "sqlite" {
					if err := agent.MigrateStoreToSQLite(path); err != nil {
						t.Fatal(err)
					}
				}
				store, err := agent.NewStore(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				var modelCalls atomic.Int32
				a, err := agent.New(store, providerFunc(func(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
					modelCalls.Add(1)
					return agent.ModelResponse{}, errors.New("delete must not invoke the model")
				}), "")
				if err != nil {
					t.Fatal(err)
				}
				root, err := a.Create("paused root after controller restart")
				if err != nil {
					t.Fatal(err)
				}
				child, err := a.Create("paused child after controller restart")
				if err != nil {
					t.Fatal(err)
				}
				w := agent.AgentWorker{ID: "worker", TaskID: "task", RunID: child.Run.ID, Status: "running", CleanupStatus: "pending", Visit: 1}
				_, err = store.Update(root.Run.ID, func(s *agent.Snapshot) error {
					s.Run.Status, s.Run.PauseReason = agent.StatusPaused, "operator_pause"
					s.Workers = []agent.AgentWorker{w}
					s.Plan.Tasks = []agent.PlanTask{{ID: w.TaskID, WorkerID: w.ID, ChildRunID: w.RunID, Status: "running", LeaseUntil: time.Now().Add(-time.Hour)}}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				before, err := store.Update(child.Run.ID, func(s *agent.Snapshot) error {
					s.Run.Status, s.Run.PauseReason = agent.StatusPaused, "parent_pause"
					s.Run.ParentRunID, s.Run.ParentTaskID = root.Run.ID, w.TaskID
					s.WorkerContext = &agent.WorkerContext{WorkerID: w.ID, RunID: w.RunID, ParentRunID: root.Run.ID, TaskID: w.TaskID}
					s.Actions = []agent.Action{{Adapter: "exec", RequestID: "old", CallID: "old", Status: "uncertain", ResourceCleanupStatus: "completed", Result: &agent.ToolResult{OutcomeUncertain: true}}}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				s, err := New(a, Options{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Close() })
				request(t, s.Handler(), "DELETE", "/api/runs/"+root.Run.ID, map[string]string{"mode": mode}, 200)
				request(t, s.Handler(), "GET", "/api/runs/"+root.Run.ID, nil, 404)
				if mode == "permanent" {
					if _, err = store.Load(child.Run.ID); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("child remains: %v", err)
					}
				} else {
					after, err := store.Load(child.Run.ID)
					if err != nil || !reflect.DeepEqual(after.Actions, before.Actions) || after.WorkerSettlement != nil || after.Run.Status != agent.StatusPaused {
						t.Fatalf("hide changed child execution: %v", err)
					}
					parent, err := store.Load(root.Run.ID)
					if err != nil || !reflect.DeepEqual(parent.Workers, []agent.AgentWorker{w}) || parent.Plan.Tasks[0].Status != "running" {
						t.Fatalf("hide forged settlement: %v", err)
					}
				}
				if modelCalls.Load() != 0 {
					t.Fatal("deletion started model execution")
				}
			})
		}
	}
}
