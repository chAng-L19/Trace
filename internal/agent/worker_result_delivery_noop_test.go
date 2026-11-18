package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// Keep the real source ledger and finish path, without background executors or
// lease renewal writes in the publication measurement window.
func workerReportNoopFixture(t *testing.T, backend string) (*Agent, *Store, string, AgentWorker) {
	t.Helper()
	a, store := treeCapacityAgent(t, backend, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "read proof", SuccessContains: "report-proof"}, {ID: "next", Goal: "consume proof", Dependencies: []string{"work"}}}); err != nil {
		t.Fatal(err)
	}
	publisher, worker := capacityFixtureChild(t, a, id, "work")
	child, err := store.Update(worker.RunID, func(s *Snapshot) error {
		s.Run.SuccessContains = "report-proof"
		s.Run.RequireVerifiedCompletion = true
		resultValidationSource(t, s, "proof", "read", "builtin", "report-proof")
		resultValidationClaim(t, s, Result{Status: "success", Summary: "Source-verified business result; bilingual reports pending.", EvidenceIDs: []string{"proof"}, Answers: []Answer{{Kind: "marker", Value: "report-proof", EvidenceIDs: []string{"proof"}}}})
		if !continueForReport(s) {
			return fmt.Errorf("fixture did not enter report delivery")
		}
		return nil
	})
	if err != nil || !reportBusinessReady(&child) || child.Run.Status != StatusRunning {
		t.Fatalf("fixture lost business/report split: result=%+v run=%+v err=%v", child.Result, child.Run, err)
	}
	if _, err = store.Update(id, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "user", Content: strings.Repeat("retained parent history ", 4096)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return publisher, store, id, worker
}

func workerReportNoopRaw(t *testing.T, store *Store, id string) Snapshot {
	t.Helper()
	unlock, err := store.acquireStoreLock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	s, err := store.loadRawLocked(id)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWorkerResultDuplicateSkipsParentCommit(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, path := range []string{"direct", "finish-turn"} {
			t.Run(backend+"/"+path, func(t *testing.T) {
				publisher, store, id, worker := workerReportNoopFixture(t, backend)
				writes := 0
				store.beforePersist = func(got string) {
					if got == id {
						writes++
					}
				}
				defer func() { store.beforePersist = nil }()
				if err := publisher.publishWorkerResult(worker.RunID); err != nil || writes != 1 {
					t.Fatalf("first report must commit once: writes=%d err=%v", writes, err)
				}
				before := workerReportNoopRaw(t, store, id)
				childBefore, err := store.Load(worker.RunID)
				if err != nil || len(before.ChildReports) != 1 || len(before.WorkerMessages) != 1 {
					t.Fatalf("missing first receipt/message: %v", err)
				}
				writes = 0
				for i := 0; i < 4; i++ {
					if path == "finish-turn" {
						// Match a saved report-only assistant response, then use the
						// existing finish/continue path for each incomplete translation.
						if _, err := store.Update(worker.RunID, func(s *Snapshot) error {
							s.Messages = append(s.Messages, Message{Role: "assistant", ReportOnly: true, Content: "English translation remains pending.", CreatedAt: time.Now().UTC()})
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						result, err := publisher.finishPiTurn(worker.RunID, ModelRequest{}, ModelResponse{Content: "English translation remains pending."})
						if err != nil || result.(map[string]string)["action"] != "continue" {
							t.Fatalf("translation did not continue: result=%v err=%v", result, err)
						}
					} else if err := publisher.publishWorkerResult(worker.RunID); err != nil {
						t.Fatal(err)
					}
					after := workerReportNoopRaw(t, store, id)
					if writes != 0 || valueHash(before) != valueHash(after) || !before.Run.UpdatedAt.Equal(after.Run.UpdatedAt) || before.Run.EventSeq != after.Run.EventSeq {
						t.Fatalf("duplicate %d rewrote parent: writes=%d hash=%s/%s time=%v/%v events=%d/%d", i, writes, valueHash(before), valueHash(after), before.Run.UpdatedAt, after.Run.UpdatedAt, before.Run.EventSeq, after.Run.EventSeq)
					}
				}
				childAfter, err := store.Load(worker.RunID)
				if err != nil || workerBusinessSourceHash(&childBefore) != workerBusinessSourceHash(&childAfter) || !reportBusinessReady(&childAfter) || childAfter.Run.Status != StatusRunning || childAfter.Run.Budget != childBefore.Run.Budget || len(childAfter.Actions) != 1 {
					t.Fatalf("report continuation changed business/budget or replayed: %v", err)
				}
				if path == "finish-turn" && childAfter.Run.EventSeq != childBefore.Run.EventSeq+4 {
					t.Fatalf("missing natural report feedback: events=%d/%d", childBefore.Run.EventSeq, childAfter.Run.EventSeq)
				}
				other, err := NewStore(store.root)
				if err != nil {
					t.Fatal(err)
				}
				defer other.Close()
				persisted, err := other.Load(id)
				if err != nil || valueHash(before) != valueHash(persisted) || occupiedWorkerSlots(&persisted) != 1 || persisted.Plan.Tasks[0].Status != "running" || persisted.Workers[0].CleanupStatus != "pending" {
					t.Fatalf("duplicate changed durable parent or released active worker: %v", err)
				}
			})
		}
	}
}

func TestWorkerResultChangedSourceAndVisitStillCommit(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			publisher, store, id, worker := workerReportNoopFixture(t, backend)
			for i, change := range []string{"first", "source", "visit"} {
				if change == "source" {
					if _, err := store.Update(worker.RunID, func(s *Snapshot) error { s.Run.Goal += " changed source"; return nil }); err != nil {
						t.Fatal(err)
					}
				}
				if change == "visit" {
					if _, err := store.Update(id, func(s *Snapshot) error { s.Workers[0].Visit++; return nil }); err != nil {
						t.Fatal(err)
					}
					publisher.workerVisit++
				}
				writes := 0
				store.beforePersist = func(got string) {
					if got == id {
						writes++
					}
				}
				err := publisher.publishWorkerResult(worker.RunID)
				store.beforePersist = nil
				after := workerReportNoopRaw(t, store, id)
				if err != nil || writes != 1 || len(after.ChildReports) != i+1 || len(after.WorkerMessages) != i+1 {
					t.Fatalf("%s publication suppressed: writes=%d reports=%d messages=%d err=%v", change, writes, len(after.ChildReports), len(after.WorkerMessages), err)
				}
			}
		})
	}
}

func TestWorkerResultDuplicatePiTranslationsSkipParentCommit(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			publisher, store, id, worker := workerReportNoopFixture(t, backend)
			if err := publisher.publishWorkerResult(worker.RunID); err != nil {
				t.Fatal(err)
			}
			before := workerReportNoopRaw(t, store, id)
			writes, calls := 0, 0
			store.beforePersist = func(got string) {
				if got == id {
					writes++
				}
			}
			defer func() { store.beforePersist = nil }()
			publisher.provider = settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
				calls++
				parent, err := store.Load(id)
				if err != nil {
					return ModelResponse{}, err
				}
				if len(request.Tools) != 0 || writes != 0 || valueHash(parent) != valueHash(before) {
					return ModelResponse{}, fmt.Errorf("translation %d exposed tools or rewrote parent: tools=%d writes=%d", calls, len(request.Tools), writes)
				}
				if calls <= 4 {
					return ModelResponse{Content: "English translation remains pending."}, nil
				}
				return u7TranslationResponse(request)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			// Run the existing pi loop under its normal child run lock. The
			// fixture owns no background lease/settlement goroutine, so only
			// publishWorkerResult can write the parent during these turns.
			child, err := publisher.Run(ctx, worker.RunID)
			if err != nil || calls != 5 || writes != 0 || child.Run.Status != StatusCompleted || child.Result.Status != "success" || reportDeliveryPending(&child) || len(child.Actions) != 1 || !publisher.reportFilesComplete(&child) {
				t.Fatalf("natural pi report continuation: calls=%d writes=%d run=%+v result=%+v err=%v", calls, writes, child.Run, child.Result, err)
			}
			after := workerReportNoopRaw(t, store, id)
			if valueHash(before) != valueHash(after) || !before.Run.UpdatedAt.Equal(after.Run.UpdatedAt) || before.Run.EventSeq != after.Run.EventSeq || occupiedWorkerSlots(&after) != 1 {
				t.Fatal("natural pi translations mutated parent or released unsettled worker")
			}
		})
	}
}

func TestWorkerResultDuplicatePersistsRevocationAndErrors(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, scenario := range []string{"revocation", "revocation-failure", "first-report-failure"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				publisher, store, id, worker := workerReportNoopFixture(t, backend)
				if scenario != "first-report-failure" {
					if err := publisher.publishWorkerResult(worker.RunID); err != nil {
						t.Fatal(err)
					}
				}
				before := workerReportNoopRaw(t, store, id)
				if scenario != "first-report-failure" {
					// Persist stale source-derived state so verified Load must write
					// its revocation before the duplicate callback can return.
					before.Assets = []Asset{{ID: "stale", Coverage: "verified", Predicate: "proof", EvidenceIDs: []string{"missing-proof"}}}
					unlock, err := store.acquireStoreLock()
					if err != nil {
						t.Fatal(err)
					}
					err = store.saveLocked(before)
					unlock()
					if err != nil {
						t.Fatal(err)
					}
					before = workerReportNoopRaw(t, store, id)
				}
				fail := strings.HasSuffix(scenario, "failure")
				if fail && backend == "sqlite" {
					db, err := store.sqliteDB()
					if err != nil {
						t.Fatal(err)
					}
					if _, err = db.Exec("CREATE TRIGGER reject_report_commit BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'report commit fixture failure'); END"); err != nil {
						t.Fatal(err)
					}
				}
				writes := 0
				var restore func()
				store.beforePersist = func(got string) {
					if got == id {
						writes++
						if fail && backend == "json" && restore == nil {
							restore = dispatchBlockRename(t, store, id)
						}
					}
				}
				err := publisher.publishWorkerResult(worker.RunID)
				store.beforePersist = nil
				if restore != nil {
					restore()
				}
				if writes != 1 {
					t.Fatalf("expected only the real state transition: writes=%d err=%v", writes, err)
				}
				other, openErr := NewStore(store.root)
				if openErr != nil {
					t.Fatal(openErr)
				}
				defer other.Close()
				persisted := workerReportNoopRaw(t, other, id)
				if fail {
					var pathErr *os.LinkError
					if err == nil || backend == "json" && !errors.As(err, &pathErr) || backend == "sqlite" && !strings.Contains(err.Error(), "report commit fixture failure") {
						t.Fatalf("original persistence error was hidden: %v", err)
					}
					if valueHash(before) != valueHash(persisted) {
						t.Fatal("failed commit changed durable parent")
					}
				} else if err != nil || persisted.Assets[0].Coverage != "uncertain" || persisted.Run.EventSeq <= before.Run.EventSeq || len(persisted.ChildReports) != 1 || len(persisted.WorkerMessages) != 1 {
					t.Fatalf("source revocation was lost or duplicate was appended: %v", err)
				}
			})
		}
	}
}

func TestWorkerResultDuplicateRechecksSourceAndVisit(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, change := range []string{"evidence", "cleanup", "uncertain", "requirements", "scope", "visit", "stopped"} {
			t.Run(backend+"/"+change, func(t *testing.T) {
				publisher, store, id, worker := workerReportNoopFixture(t, backend)
				if err := publisher.publishWorkerResult(worker.RunID); err != nil {
					t.Fatal(err)
				}
				if change == "requirements" || change == "visit" || change == "stopped" {
					if _, err := store.Update(id, func(s *Snapshot) error {
						switch change {
						case "requirements":
							s.Plan.Tasks[0].Requirements = []VerificationRequirement{taskRequirement("new-check")}
						case "visit":
							s.Workers[0].Visit++
						case "stopped":
							s.Run.Status = StatusCancelled
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				} else if _, err := store.Update(worker.RunID, func(s *Snapshot) error {
					switch change {
					case "evidence":
						s.Evidence[0].Content += " changed"
					case "cleanup":
						s.Actions[0].ResourceCleanupError = "cleanup not observed"
					case "uncertain":
						s.Actions = append(s.Actions, Action{RequestID: "unresolved", CallID: "uncertain", Adapter: "builtin", Status: "uncertain"})
					case "scope":
						s.WorkerContext.EvidenceScope = &EvidenceScope{ObjectID: "changed", Version: "v2"}
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := publisher.publishWorkerResult(worker.RunID); err != nil {
					t.Fatal(err)
				}
				after := workerReportNoopRaw(t, store, id)
				if len(after.ChildReports) != 1 || len(after.WorkerMessages) != 1 || after.Plan.Tasks[0].Status != "running" || after.Workers[0].CleanupStatus != "pending" {
					t.Fatal("invalid report source bypassed settlement or publication boundary")
				}
			})
		}
	}
}
