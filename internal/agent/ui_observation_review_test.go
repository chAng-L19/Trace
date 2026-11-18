package agent

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func observationReviewFixture(t *testing.T, backend string) (*Agent, *Store, string, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	a, store := treeCapacityAgent(t, backend, settlementProviderFunc(func(context.Context, ModelRequest) (ModelResponse, error) {
		calls.Add(1)
		return ModelResponse{}, errors.New("manual observation must not call provider")
	}))
	id := seedPiBatch(t, a, store, []ToolCall{{ID: "call", Name: "exec", Arguments: `{"command":"must-not-run"}`}}, []ToolDefinition{execToolDefinition()})
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusPaused
		s.Run.PauseReason = "tool outcome uncertain; inspect side effects and submit its result manually"
		s.Actions = []Action{{RequestID: id + "-0001", CallID: "call", Name: "exec", Adapter: "exec", Status: "uncertain", StartedAt: time.Unix(100, 0).UTC(), FinishedAt: time.Unix(120, 0).UTC(), ResourceOwner: "original-owner", ResourceRoot: "/original-root", ResourceRootIdentity: "original-identity", Result: &ToolResult{ExitCode: 1, Error: "thread limit", OutcomeUncertain: true}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, store, id, calls
}

func TestToolObservationReviewRejectsForeignBatchWithoutMutation(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, fault := range []string{"old_pending", "foreign_declaration", "duplicate_declaration"} {
			t.Run(backend+"/"+fault, func(t *testing.T) {
				a, store, id, calls := observationReviewFixture(t, backend)
				_, err := store.Update(id, func(s *Snapshot) error {
					request := id + "-0002"
					s.Run.RequestSeq = 2
					s.Requests = append(s.Requests, ModelRequest{ID: request})
					declared := []ToolCall{{ID: "new-call", Name: "exec", Arguments: `{}`}}
					if fault != "old_pending" {
						declared[0].ID = "call"
					}
					if fault == "foreign_declaration" {
						request = id + "-0001"
					}
					if fault == "duplicate_declaration" {
						declared = append(declared, declared[0])
					}
					s.Messages = append(s.Messages, Message{Role: "assistant", RequestID: request, ToolCalls: declared})
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				before, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = a.RecordToolObservation(id, id+"-0002", "call", "observed"); err == nil {
					t.Fatal("foreign or ambiguous call accepted")
				}
				after, err := store.Load(id)
				if err != nil || valueHash(before) != valueHash(after) || calls.Load() != 0 {
					t.Fatal("rejected observation mutated state or called provider", err)
				}
			})
		}
	}
}

func TestToolObservationReviewPreservesExecutionAndCleanup(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, reason := range []string{"clean", "cleanup", "operator_pause"} {
			t.Run(backend+"/"+reason, func(t *testing.T) {
				a, store, id, calls := observationReviewFixture(t, backend)
				before, err := store.Update(id, func(s *Snapshot) error {
					if reason == "cleanup" {
						s.Actions[0].Result.WorkerCleanupError = "owned process remains"
					}
					if reason == "operator_pause" {
						s.Run.PauseReason = reason
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				after, err := a.RecordToolObservation(id, id+"-0001", "call", "Observed failure; external state remains unknown")
				if err != nil {
					t.Fatal(err)
				}
				want := before.Actions[0]
				want.Status = "resolved_manually"
				if valueHash(after.Actions[0]) != valueHash(want) || after.Run.Status != StatusPaused || calls.Load() != 0 || len(after.Requests) != len(before.Requests) || len(pendingToolCalls(&after)) != 0 {
					t.Fatal("changed execution history, failed to resolve pending call, or auto-started")
				}
				if len(after.Evidence) != 1 || after.Evidence[0].Type != "manual_observation" || after.Evidence[0].Verification != "unverified" || after.Evidence[0].RequestID != id+"-0001" {
					t.Fatal("observation source or verification changed", after.Evidence)
				}
				if reason == "clean" {
					if after.Run.PauseReason != "tool outcomes reviewed; awaiting operator resume" {
						t.Fatal("clean observation did not explain explicit resume", after.Run.PauseReason)
					}
				} else if after.Run.PauseReason != before.Run.PauseReason {
					t.Fatal("cleanup or operator pause reason replaced", after.Run.PauseReason)
				}
				if (toolCleanupError(after) != nil) != (reason == "cleanup") {
					t.Fatal("manual observation changed cleanup gate")
				}
			})
		}
	}
}

func TestToolObservationReviewCurrentAndLegacyBatchOwnership(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, mode := range []string{"reused_call_id", "manual_batch"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				a, store, id, calls := observationReviewFixture(t, backend)
				before, err := store.Update(id, func(s *Snapshot) error {
					if mode == "manual_batch" {
						s.Requests = nil
						s.Run.RequestSeq = 0
						s.Actions[0].RequestID = ""
						for i := range s.Messages {
							s.Messages[i].RequestID = ""
						}
						return nil
					}
					request := id + "-0002"
					s.Run.RequestSeq = 2
					s.Requests = append(s.Requests, ModelRequest{ID: request})
					s.Messages = append(s.Messages, Message{Role: "tool", ToolCallID: "call", Content: "old observation"}, Message{Role: "assistant", RequestID: request, ToolCalls: []ToolCall{{ID: "call", Name: "exec", Arguments: `{}`}}})
					current := s.Actions[0]
					current.RequestID = request
					s.Actions = append(s.Actions, current)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				request := currentRequestID(&before)
				after, err := a.RecordToolObservation(id, request, "call", "current observation")
				if err != nil || calls.Load() != 0 || len(after.Evidence) != 1 || after.Evidence[0].RequestID != request || len(pendingToolCalls(&after)) != 0 {
					t.Fatal("current or legacy declaration rejected or misbound", err)
				}
				index := len(before.Actions) - 1
				want := before.Actions[index]
				want.Status = "resolved_manually"
				if valueHash(after.Actions[index]) != valueHash(want) || index > 0 && valueHash(after.Actions[0]) != valueHash(before.Actions[0]) {
					t.Fatal("observation changed another batch's action")
				}
			})
		}
	}
}

func TestToolObservationReviewConcurrentDuplicateCommitsOnce(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, id, calls := observationReviewFixture(t, backend)
			start := make(chan struct{})
			results := make(chan error, 2)
			var group sync.WaitGroup
			for range 2 {
				group.Add(1)
				go func() {
					defer group.Done()
					<-start
					_, err := a.RecordToolObservation(id, id+"-0001", "call", "observed")
					results <- err
				}()
			}
			close(start)
			group.Wait()
			close(results)
			accepted := 0
			for err := range results {
				if err == nil {
					accepted++
				}
			}
			snap, err := store.Load(id)
			if err != nil || accepted != 1 || len(snap.Evidence) != 1 || calls.Load() != 0 || snap.Run.Status != StatusPaused {
				t.Fatalf("duplicate observation settlement: accepted=%d evidence=%d error=%v", accepted, len(snap.Evidence), err)
			}
		})
	}
}

func TestToolObservationReviewKeepsWorkerReceiptAndParentHistory(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, childID, calls := observationReviewFixture(t, backend)
			root := createTestRun(t, a)
			visit := uint64(2)
			w := AgentWorker{ID: "worker", TaskID: "task", RunID: childID, Visit: visit, SettledVisit: &visit, Status: "paused", CleanupStatus: "completed"}
			child, err := store.Update(childID, func(s *Snapshot) error {
				s.Run.ParentRunID, s.Run.ParentTaskID = root, w.TaskID
				s.WorkerSettlement = &WorkerSettlement{ParentRunID: root, TaskID: w.TaskID, WorkerID: w.ID, RunID: childID, Visit: visit, CleanupStatus: "completed", ObservedAt: time.Unix(130, 0).UTC()}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			parent, err := store.Update(root, func(s *Snapshot) error {
				s.Run.Status = StatusPaused
				s.Workers = []AgentWorker{w}
				s.Plan.Tasks = []PlanTask{{ID: w.TaskID, WorkerID: w.ID, ChildRunID: childID, Status: "paused"}}
				s.ChildReports = []ChildReport{newChildReport(root, w, &child, "completed")}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			reports, err := store.ChildReports(root)
			if err != nil || len(reports) != 1 || reports[0].SourceStatus != "available" {
				t.Fatal("initial source report not current", reports, err)
			}
			after, err := a.RecordToolObservation(childID, childID+"-0001", "call", "observed failure")
			if err != nil || valueHash(after.WorkerSettlement) != valueHash(child.WorkerSettlement) || calls.Load() != 0 {
				t.Fatal("observation rewrote lifecycle receipt or called provider", err)
			}
			got, err := store.Load(root)
			if err != nil || valueHash(got.Workers) != valueHash(parent.Workers) || valueHash(got.Plan) != valueHash(parent.Plan) || valueHash(got.ChildReports) != valueHash(parent.ChildReports) {
				t.Fatal("observation changed parent task or fixed report history", err)
			}
			reports, err = store.ChildReports(root)
			if err != nil || len(reports) != 1 || reports[0].SourceStatus != "changed" {
				t.Fatal("old report accepted as current observation source", reports, err)
			}
		})
	}
}

func TestToolObservationReviewDeleteWaitingChildBackends(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, op := range []string{"hide", "purge"} {
			for _, fault := range []string{"none", "child_lock", "receipt_owner", "settled_visit", "cleanup"} {
				t.Run(backend+"/"+op+"/"+fault, func(t *testing.T) {
					a, store, childID, _ := observationReviewFixture(t, backend)
					root := createTestRun(t, a)
					visit, stale := uint64(2), uint64(1)
					w := AgentWorker{ID: "worker", TaskID: "task", RunID: childID, Visit: visit, SettledVisit: &visit, Status: "uncertain", CleanupStatus: "completed"}
					if fault == "settled_visit" {
						w.SettledVisit = &stale
					}
					parent, err := store.Update(root, func(s *Snapshot) error {
						s.Run.Status = StatusPaused
						s.Workers = []AgentWorker{w}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					child, err := store.Update(childID, func(s *Snapshot) error {
						s.Run.Status = StatusWaitingTool
						s.Run.ParentRunID, s.Run.ParentTaskID = root, w.TaskID
						s.WorkerSettlement = &WorkerSettlement{ParentRunID: root, TaskID: w.TaskID, WorkerID: w.ID, RunID: childID, Visit: visit, CleanupStatus: "completed", ObservedAt: time.Unix(130, 0).UTC()}
						if fault == "receipt_owner" {
							s.WorkerSettlement.WorkerID = "other-worker"
						}
						if fault == "cleanup" {
							s.Actions[0].Result.WorkerCleanupError = "owned process remains"
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
					if fault == "child_lock" {
						unlock, lockErr := store.AcquireRunLock(childID)
						if lockErr != nil {
							t.Fatal(lockErr)
						}
						defer unlock()
					}
					if op == "hide" {
						err = store.Delete(root)
					} else {
						_, err = store.PurgeRun(root)
					}
					if (err == nil) != (fault == "none") {
						t.Fatal("deletion guard", err)
					}
					if fault == "none" && op == "purge" {
						for _, id := range []string{root, childID} {
							if _, err = store.Load(id); !errors.Is(err, os.ErrNotExist) {
								t.Fatal("purged record remains", id, err)
							}
						}
						return
					}
					got, err := store.Load(childID)
					if err != nil || valueHash(got) != valueHash(child) {
						t.Fatal("deletion changed retained child ledger", err)
					}
					got, err = store.Load(root)
					if err != nil || fault != "none" && valueHash(got) != valueHash(parent) || fault == "none" && !got.Run.Deleted {
						t.Fatal("root deletion state mismatch", err)
					}
				})
			}
		}
	}
}
