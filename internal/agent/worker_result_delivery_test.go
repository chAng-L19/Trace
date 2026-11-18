package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func reportWorkerFixture(t *testing.T, backend string, translation func(*Agent, *Store, string, int, ModelRequest) error) (*Agent, *Store, string, AgentWorker, *atomic.Int32) {
	t.Helper()
	store, _ := u7Store(t, backend)
	return reportWorkerFixtureInStore(t, store, translation)
}

func reportWorkerFixtureInStore(t *testing.T, store *Store, translation func(*Agent, *Store, string, int, ModelRequest) error, legacyPause ...bool) (*Agent, *Store, string, AgentWorker, *atomic.Int32) {
	t.Helper()
	legacy := len(legacyPause) == 0 || legacyPause[0]
	path := filepath.Join(t.TempDir(), "proof.txt")
	const marker = "report-worker-proof"
	if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
		t.Fatal(err)
	}
	var a *Agent
	var parentID string
	calls := &atomic.Int32{}
	provider := settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		n := int(calls.Add(1))
		switch n {
		case 1:
			args, _ := json.Marshal(map[string]string{"path": path})
			return ModelResponse{ToolCalls: []ToolCall{{ID: "read-proof", Name: "read", Arguments: string(args)}}}, nil
		case 2:
			var result ToolResult
			if err := json.Unmarshal([]byte(lastConversationMessage(request).Content), &result); err != nil {
				return ModelResponse{}, err
			}
			claim := Result{Status: "success", Summary: "文件已经返回固定验证标记；保留本次读取的原始来源，双语文件报告尚待交付。", EvidenceIDs: []string{result.EvidenceID}, Answers: []Answer{{Kind: "marker", Value: marker, EvidenceIDs: []string{result.EvidenceID}}}}
			raw, _ := json.Marshal(map[string]any{"trace_result": claim})
			return ModelResponse{Content: string(raw)}, nil
		default:
			if len(request.Tools) != 0 {
				return ModelResponse{}, fmt.Errorf("translation exposed execution tools")
			}
			if translation != nil {
				if err := translation(a, store, parentID, n, request); err != nil {
					return ModelResponse{}, err
				}
			}
			if n == 3 && legacy {
				// Seed a legacy delivery pause to retain the recovery/settlement
				// boundary fixtures. New missing translations continue in pi.
				parent, err := store.Load(parentID)
				if err != nil {
					return ModelResponse{}, err
				}
				if _, err := store.Update(parent.Workers[0].RunID, func(s *Snapshot) error {
					s.Run.Status, s.Run.PauseReason = StatusPaused, "report_translation_missing"
					return nil
				}); err != nil {
					return ModelResponse{}, err
				}
				return ModelResponse{Content: "English translation remains pending."}, nil
			}
			if n == 3 {
				return ModelResponse{Content: "English translation remains pending."}, nil
			}
			return u7TranslationResponse(request)
		}
	})
	var err error
	a, err = New(store, provider, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.StopWorkers(context.Background()); a.CloseMCP() })
	a.SetRequireVerifiedCompletion(true)
	parent, err := a.Create("Read the marker and deliver substantive Chinese and English file reports.")
	if err != nil {
		t.Fatal(err)
	}
	parentID = parent.Run.ID
	if _, err = a.SetPlan(parentID, []PlanTask{{ID: "work", Goal: "Read " + path, SuccessContains: marker}, {ID: "next", Goal: "Consume the verified marker", Dependencies: []string{"work"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(parentID, func(s *Snapshot) error { s.Run.Status = StatusRunning; s.Plan.Capacity = 2; return nil }); err != nil {
		t.Fatal(err)
	}
	w, err := a.DispatchTask(context.Background(), parentID, "work")
	if err != nil {
		t.Fatal(err)
	}
	parent, err = waitAllTestWorkers(a, context.Background(), parentID, 10)
	wantCalls, wantStatus := int32(4), "completed"
	if legacy {
		wantCalls, wantStatus = 3, "paused"
	}
	if err != nil || calls.Load() != wantCalls || parent.Plan.Tasks[0].Status != "verified" || parent.Workers[0].Status != wantStatus || parent.Workers[0].CleanupStatus != "completed" || !hasString(parent.Plan.Frontier, "next") {
		t.Fatalf("report pause blocked business settlement: %+v workers=%+v calls=%d err=%v", parent.Plan, parent.Workers, calls.Load(), err)
	}
	child, err := store.Load(w.RunID)
	if err != nil || child.Result.Status != "success" || legacy && (child.Run.PauseReason != "report_translation_missing" || !reportDeliveryPending(&child) || !reportBusinessSettled(parentID, parent.Workers[0], &child)) || !legacy && (child.Run.Status != StatusCompleted || reportDeliveryPending(&child) || !a.reportFilesComplete(&child)) {
		t.Fatalf("missing durable report/business split: run=%+v result=%+v err=%v", child.Run, child.Result, err)
	}
	return a, store, parentID, parent.Workers[0], calls
}

func TestWorkerReportMissingTranslationContinuesAutomatically(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, _ := u7Store(t, backend)
			_, _, _, worker, calls := reportWorkerFixtureInStore(t, store, nil, false)
			child, err := store.Load(worker.RunID)
			if err != nil || calls.Load() != 4 || len(child.Actions) != 1 || child.Actions[0].CallID != "read-proof" || child.Run.PauseReason != "" || child.Run.Budget != (Budget{}) {
				t.Fatalf("automatic report repair replayed work, paused or added a hidden budget: %+v %v", child.Run, err)
			}
		})
	}
}

func TestWorkerResultDeliveryBeforeTranslationAndResumption(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, id, w, calls := reportWorkerFixture(t, backend, func(a *Agent, store *Store, id string, n int, _ ModelRequest) error {
				parent, err := store.Load(id)
				if err != nil {
					return err
				}
				if n == 3 {
					if len(parent.ChildReports) != 1 || len(parent.WorkerMessages) != 1 || parent.Plan.Tasks[0].Status != "running" || occupiedWorkerSlots(&parent) != 1 {
						return fmt.Errorf("early result released active executor: %+v %+v", parent.Plan, parent.Workers)
					}
					r := parent.ChildReports[0]
					if !r.SettlementPending || !r.DeliveryPending || r.CleanupStatus != "pending" || r.ResultStatus != "success" || len(r.Answers) == 0 || !r.Answers[0].Verified || r.Answers[0].Value != "report-worker-proof" {
						return fmt.Errorf("early receipt lost verified answer or delivery boundary: %+v", r)
					}
					publisher := *a
					publisher.workerRunID, publisher.workerVisit = r.SourceRunID, r.Visit
					if err := publisher.publishWorkerResult(r.SourceRunID); err != nil {
						return err
					}
					parent, err = store.Load(id)
					if err != nil || len(parent.ChildReports) != 1 || len(parent.WorkerMessages) != 1 {
						return fmt.Errorf("same business source duplicated early receipt: %v", err)
					}
					rows, err := store.ChildReports(id)
					if err != nil || rows[0].SourceStatus != "available" {
						return fmt.Errorf("translation telemetry invalidated business source: %+v %v", rows, err)
					}
					return a.DeliverWorkerMessages(id)
				}
				if n != 4 || parent.Plan.Tasks[0].Status != "verified" || parent.Plan.Tasks[1].Status != "running" || parent.Workers[0].Status != "running" || occupiedWorkerSlots(&parent) != 2 {
					return fmt.Errorf("report resume revoked dependency or released capacity: %+v %+v", parent.Plan, parent.Workers)
				}
				if err := a.renewActiveWorkerLease(context.Background(), id, parent.Workers[0]); err != nil {
					return err
				}
				_, err = a.QueueMessage(parent.Workers[0].RunID, "follow_up", "Keep this input for a later business visit.")
				return err
			})
			_, err := store.Update(id, func(s *Snapshot) error {
				s.Run.Status = StatusCompleted
				_, err := a.deferForChildReportDelivery(s)
				return err
			})
			parent, _ := store.Load(id)
			if err != nil || parent.Run.Status != StatusRunning || parent.Run.PauseReason != "" {
				t.Fatalf("parent claimed full delivery: %+v %v", parent.Run, err)
			}
			if _, err = store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; s.Run.PauseReason = ""; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err = a.ClaimTask(id, "next", "consumer", 30); err != nil {
				t.Fatal(err)
			}
			if _, err = a.ResumeWorker(context.Background(), id, w.ID); err != nil {
				t.Fatal(err)
			}
			parent, err = waitAllTestWorkers(a, context.Background(), id, 10)
			child, childErr := store.Load(w.RunID)
			if err != nil || childErr != nil || calls.Load() != 4 || child.Run.Status != StatusCompleted || reportDeliveryPending(&child) || len(child.Actions) != 1 || parent.Plan.Tasks[0].Status != "verified" || parent.Plan.Tasks[1].Status != "running" || len(child.Mailbox) != 1 || !child.Mailbox[0].Pending() {
				t.Fatalf("report continuation replayed or consumed business input: parent=%+v child=%+v calls=%d err=%v/%v", parent.Plan, child.Run, calls.Load(), err, childErr)
			}
			if !a.reportFilesComplete(&child) {
				t.Fatal("substantive bilingual CAS files remain incomplete")
			}
			_, err = store.Update(id, func(s *Snapshot) error {
				if pending := a.pendingChildReportDeliveryLocked(s, map[string]bool{}); len(pending) != 0 {
					return fmt.Errorf("delivery did not clear: %v", pending)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWorkerReportBusinessSettlementInvalidation(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, change := range []string{"evidence", "cleanup", "uncertain", "scope", "visit", "requirements", "cancel", "delivery_exit"} {
			t.Run(backend+"/"+change, func(t *testing.T) {
				a, store, id, w, calls := reportWorkerFixture(t, backend, nil)
				if _, err := a.ClaimTask(id, "next", "consumer", 30); err != nil {
					t.Fatal(err)
				}
				if change == "visit" || change == "requirements" {
					_, err := store.Update(id, func(s *Snapshot) error {
						if change == "visit" {
							s.Workers[0].Visit++
						} else {
							s.Plan.Tasks[0].Requirements = []VerificationRequirement{taskRequirement("new-check")}
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
				} else {
					_, err := store.Update(w.RunID, func(s *Snapshot) error {
						switch change {
						case "evidence":
							s.Evidence[0].Content += " changed"
						case "cleanup":
							s.Actions[0].ResourceCleanupError = "cleanup not observed"
						case "uncertain":
							s.Actions = append(s.Actions, Action{RequestID: "unresolved", CallID: "uncertain", Adapter: "builtin", Status: "uncertain"})
						case "scope":
							s.WorkerContext.EvidenceScope = &EvidenceScope{ObjectID: "other", Version: "v2"}
						case "cancel":
							s.Run.Status = StatusCancelled
						case "delivery_exit":
							s.Run.Status, s.Run.PauseReason, s.ReportDelivery.Pending = StatusRunning, "", false
						}
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				parent, err := store.Load(id)
				if err != nil || taskDone(parent.Plan.Tasks[0]) || parent.Plan.Tasks[1].Status != "uncertain" || calls.Load() != 3 {
					t.Fatalf("invalid source kept dependency or replayed: %+v calls=%d err=%v", parent.Plan, calls.Load(), err)
				}
				rows, err := store.ChildReports(id)
				if err != nil || len(rows) < 2 || change != "requirements" && rows[0].SourceStatus == "available" {
					t.Fatalf("stale receipt presented as current: %+v %v", rows, err)
				}
			})
		}
	}
}

func TestWorkerReportResumeCrashRecovery(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, id, w, calls := reportWorkerFixture(t, backend, nil)
			child, _ := store.Load(w.RunID)
			unlock, err := store.AcquireRunLock(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			_, release, resumed, err := a.prepareWorkerResume(context.Background(), child)
			if err != nil {
				unlock()
				t.Fatal(err)
			}
			release()
			unlock()
			parent, _ := store.Load(id)
			if parent.Plan.Tasks[0].Status != "verified" || occupiedWorkerSlots(&parent) != 1 || resumed.Visit != w.Visit+1 {
				t.Fatalf("report admission lost completion/current capacity: %+v %+v", parent.Plan, parent.Workers)
			}
			if _, err = store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Minute); return nil }); err != nil {
				t.Fatal(err)
			}
			parent, err = a.RecoverWorkers(id)
			if err != nil || parent.Plan.Tasks[0].Status != "uncertain" || parent.Workers[0].CleanupStatus != "unknown" || occupiedWorkerSlots(&parent) != 1 || calls.Load() != 3 {
				t.Fatalf("crashed report visit reused old cleanup or replayed: %+v %+v err=%v", parent.Plan, parent.Workers, err)
			}
		})
	}
}

func TestWorkerReportFilesRequireCurrentSourceAndCAS(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, id, w, _ := reportWorkerFixture(t, backend, nil)
			if _, err := a.ResumeWorker(context.Background(), id, w.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := waitAllTestWorkers(a, context.Background(), id, 10); err != nil {
				t.Fatal(err)
			}
			child, _ := store.Load(w.RunID)
			if !a.reportFilesComplete(&child) {
				t.Fatal("complete files rejected")
			}
			selected := preferredReport(child)
			if selected == nil || len(selected.Files) != 2 {
				t.Fatal("missing report files")
			}
			if err := os.WriteFile(filepath.Join(store.root, "artifacts", selected.Files[1].Hash), []byte("tampered report"), 0600); err != nil {
				t.Fatal(err)
			}
			if a.reportFilesComplete(&child) {
				t.Fatal("corrupt English CAS file accepted")
			}
			child.Run.Goal += " changed source version"
			if a.reportFilesComplete(&child) {
				t.Fatal("old report accepted for a new source version")
			}
			_, err := store.Update(id, func(s *Snapshot) error {
				pending := a.pendingChildReportDeliveryLocked(s, map[string]bool{})
				if len(pending) == 0 || !strings.Contains(pending[0], w.ID) {
					return fmt.Errorf("missing CAS delivery gate: %v", pending)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Force the durable interleaving: the new child cleanup receipt has committed,
// but the parent still owns the running report visit and a running dependent.
func TestWorkerReportReceiptCommitGapKeepsDependency(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, change := range []string{"unchanged", "missing_continuity", "changed_source", "wrong_visit", "current_error"} {
			t.Run(backend+"/"+change, func(t *testing.T) {
				a, store, id, w, calls := reportWorkerFixture(t, backend, nil)
				if _, err := a.ClaimTask(id, "next", "consumer", 30); err != nil {
					t.Fatal(err)
				}
				// A paused report visit can retain a provider/bridge interruption
				// while its business proof and actual cleanup are both valid. The
				// next successful visit must not inherit that visit's RunError.
				child, err := store.Update(w.RunID, func(s *Snapshot) error {
					s.WorkerSettlement.RunError = "previous report delivery interrupted"
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				unlock, err := store.AcquireRunLock(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				_, release, resumed, err := a.prepareWorkerResume(context.Background(), child)
				if err != nil {
					unlock()
					t.Fatal(err)
				}
				release()
				unlock()
				_, err = store.Update(w.RunID, func(s *Snapshot) error {
					body := u7Text
					for _, c := range reportCandidates(s) {
						if c.source.Kind == "final" {
							body.SourceDigest = c.digest()
						}
					}
					raw, _ := json.Marshal(map[string]any{"trace_report": body})
					s.Messages = append(s.Messages, Message{Role: "assistant", ReportOnly: true, Content: string(raw)})
					s.Run.Status, s.Run.PauseReason = StatusCompleted, ""
					if continueForReport(s) {
						return fmt.Errorf("fixture report did not complete")
					}
					previous := w.Visit
					// Match runChildOperation: each visit produces a fresh lifecycle
					// receipt; only the source hash/continuation identity carry over.
					receipt := WorkerSettlement{ParentRunID: id, TaskID: resumed.TaskID, WorkerID: resumed.ID, RunID: resumed.RunID, Visit: resumed.Visit, CleanupStatus: "completed", CleanupVersion: 1, BusinessSourceHash: s.WorkerSettlement.BusinessSourceHash, BusinessContinuationVisit: &previous, ObservedAt: time.Now().UTC()}
					if change == "missing_continuity" {
						receipt.BusinessContinuationVisit = nil
					}
					if change == "wrong_visit" {
						receipt.Visit++
					}
					if change == "changed_source" {
						s.Run.Goal += " new business source"
					}
					if change == "current_error" {
						receipt.RunError = "current report visit failed"
					}
					s.WorkerSettlement = &receipt
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				if change == "current_error" {
					parent, err := a.RecoverWorkers(id)
					if err != nil || parent.Plan.Tasks[0].Status != "failed" || parent.Plan.Tasks[1].Status != "uncertain" || calls.Load() != 3 {
						t.Fatalf("current visit failure was hidden: %+v err=%v", parent.Plan, err)
					}
					return
				}
				parent, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				if change != "unchanged" {
					if taskDone(parent.Plan.Tasks[0]) || parent.Plan.Tasks[1].Status != "uncertain" {
						t.Fatal("invalid continuation preserved dependency")
					}
					return
				}
				if parent.Plan.Tasks[0].Status != "verified" || parent.Plan.Tasks[1].Status != "running" || occupiedWorkerSlots(&parent) != 2 || calls.Load() != 3 {
					t.Fatalf("receipt commit gap revoked dependency: %+v %+v", parent.Plan, parent.Workers)
				}
				parent, err = a.RecoverWorkers(id)
				if err != nil || parent.Plan.Tasks[0].Status != "verified" || parent.Plan.Tasks[1].Status != "running" || parent.Workers[0].Status != "completed" || occupiedWorkerSlots(&parent) != 1 || calls.Load() != 3 {
					t.Fatalf("receipt recovery replayed or revoked: %+v %+v err=%v", parent.Plan, parent.Workers, err)
				}
			})
		}
	}
}
