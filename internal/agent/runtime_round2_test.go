package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"trace/internal/localtool"
)

// Explicit tool resolution is still child execution. Parent controls and worker
// capacity must be checked before starting the process, not after its result.
func TestRuntimeRound2ChildToolResolutionHonorsParentAdmission(t *testing.T) {
	for _, adapter := range []string{"exec", "registered"} {
		for _, gate := range []string{"paused", "cancelled", "capacity"} {
			t.Run(adapter+"/"+gate, func(t *testing.T) {
				root := t.TempDir()
				t.Setenv("TRACE_TEST_CHILD", "sideeffect")
				t.Setenv("TRACE_TEST_ROOT", root)
				manifest := localtool.Manifest{
					Name: "round2-sideeffect", Command: os.Args[0],
					Args:        []string{"-test.run=^TestProcessFixture$"},
					InputSchema: json.RawMessage(`{"type":"object"}`),
				}
				call := ToolCall{ID: "pending", Name: "external", Arguments: `{}`}
				if adapter == "registered" {
					// Register only after the initial child has stopped on its
					// unresolved call, just like installing a missing local tool.
					call.Name = manifest.StableID()
				}
				a, store, id, worker := resumeWorkerFixture(t, manualProvider(call))
				parent, err := waitAllTestWorkers(a, context.Background(), id, 5)
				if err != nil {
					t.Fatal(err)
				}
				child, err := store.Load(worker.RunID)
				if err != nil || child.Run.Status != StatusWaitingTool || len(child.Actions) != 0 || parent.Workers[0].Status != "uncertain" || parent.Workers[0].CleanupStatus != "completed" {
					t.Fatalf("pending child fixture: child=%+v workers=%+v err=%v", child.Run, parent.Workers, err)
				}
				if adapter == "registered" {
					if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
						t.Fatal(err)
					}
				}
				switch gate {
				case "paused":
					_, err = a.Pause(id)
				case "cancelled":
					_, err = a.Cancel(id)
				case "capacity":
					_, err = a.SetPlan(id, []PlanTask{
						{ID: "work", Goal: "continue original task"},
						{ID: "next", Goal: "consume dependency", Dependencies: []string{"work"}},
						{ID: "other", Goal: "occupy another worker slot"},
					})
					if err != nil {
						t.Fatal(err)
					}
					// The unresolved original child retains its slot. Start the
					// second worker within capacity before tightening admission.
					if _, err = a.SetPlanCapacity(id, 2); err != nil {
						t.Fatal(err)
					}
					blocked := &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
					a.provider = blocked
					if _, err = a.DispatchTask(context.Background(), id, "other"); err != nil {
						t.Fatal(err)
					}
					select {
					case <-blocked.entered:
					case <-time.After(5 * time.Second):
						t.Fatal("capacity fixture did not start")
					}
					_, err = a.SetPlanCapacity(id, 1)
				}
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if adapter == "registered" {
					_, err = a.ExecuteRegistered(ctx, worker.RunID, call.ID, call.Name)
				} else {
					_, err = a.ExecuteTool(ctx, worker.RunID, call.ID, []string{os.Args[0], "-test.run=^TestProcessFixture$"})
				}
				if err == nil || gate == "capacity" && !errors.Is(err, errWorkerCapacity) {
					t.Fatalf("child execution did not reject %s admission: %v", gate, err)
				}
				child, loadErr := store.Load(worker.RunID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				files, readErr := os.ReadDir(root)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if len(child.Actions) != 0 || len(files) != 0 {
					t.Fatalf("child crossed %s admission before rejection: actions=%d side_effect_files=%d error=%v", gate, len(child.Actions), len(files), err)
				}
				if len(pendingToolCalls(&child)) != 1 {
					t.Fatal("rejected child execution consumed its pending call")
				}
			})
		}
	}
}

// No runChild goroutine remains after a paused/waiting-tool worker has settled.
// Cancelling it must therefore settle the parent ledger in the control path.
func TestRuntimeRound2CancelSettledWorkerUpdatesParent(t *testing.T) {
	for _, state := range []string{"paused", "uncertain"} {
		t.Run(state, func(t *testing.T) {
			call := ToolCall{ID: "pending", Name: "external", Arguments: `{}`}
			if state == "paused" {
				call = ToolCall{ID: "input", Name: "request_input", Arguments: `{"reason":"provide target"}`}
			}
			a, store, id, worker := resumeWorkerFixture(t, manualProvider(call))
			before, err := waitAllTestWorkers(a, context.Background(), id, 5)
			if err != nil {
				t.Fatal(err)
			}
			if before.Workers[0].Status != state || before.Workers[0].CleanupStatus != "completed" {
				t.Fatalf("unsettled fixture: %+v", before.Workers)
			}
			if _, err := a.CancelWorker(id, worker.ID); err != nil {
				t.Fatal(err)
			}
			parent, err := waitAllTestWorkers(a, context.Background(), id, 5)
			if err != nil {
				t.Fatal(err)
			}
			child, err := store.Load(worker.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if child.Run.Status != StatusCancelled || parent.Workers[0].Status != "cancelled" || parent.Plan.Tasks[0].Status != "cancelled" {
				t.Fatalf("cancelled child has stale parent settlement: child=%s worker=%s task=%s", child.Run.Status, parent.Workers[0].Status, parent.Plan.Tasks[0].Status)
			}
			if hasString(parent.Plan.Frontier, "next") || parent.Workers[0].CleanupStatus != "completed" {
				t.Fatal("cancellation released a dependency or lost settled cleanup")
			}
		})
	}
}
