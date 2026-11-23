package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const delegatedProof = "delegated-original-proof"

func delegatedRequirements() []VerificationRequirement {
	return []VerificationRequirement{
		{ID: "original-goal", Description: "original exact goal probe", Category: "goal", ToolID: "exec", ArgumentsHash: promptHash(`{"command":"printf delegated-original-proof"}`), Predicate: EvidencePredicate{Kind: "equals", Value: delegatedProof}},
		{ID: "original-cleanup", Description: "original exact cleanup probe", Category: "cleanup", ToolID: "exec", ArgumentsHash: promptHash(`{"command":"printf clean"}`), Predicate: EvidencePredicate{Kind: "equals", Value: "clean"}},
	}
}

type delegatedFixture struct {
	a          *Agent
	store      *Store
	root       string
	worker     AgentWorker
	ids        []string
	artifactID string
}

// Production coordinator + real process execution + runChildOperation cleanup
// and durable settlement. No copied child ledger, legacy Create helper or
// fabricated pass check is used. The server's existing resource bridge applies.
func delegatedFixtureRun(t *testing.T, backend string, contracted bool) delegatedFixture {
	t.Helper()
	return delegatedFixtureContract(t, backend, contracted, delegatedRequirements())
}

func delegatedFixtureContract(t *testing.T, backend string, contracted bool, requirements []VerificationRequirement) delegatedFixture {
	t.Helper()
	a, store := responsibilityAgent(t, backend, manualProvider(
		ToolCall{ID: "goal", Name: "exec", Arguments: `{"command":"printf delegated-original-proof"}`},
		ToolCall{ID: "cleanup", Name: "exec", Arguments: `{"command":"printf clean"}`},
	))
	predicate := delegatedProof
	root, err := a.CreateConfigured("delegate original goal and cleanup probes", RunConfiguration{Requirements: requirements, SuccessContains: &predicate})
	if err != nil {
		t.Fatal(err)
	}
	if root.Run.Responsibility != ResponsibilityCoordinator {
		t.Fatal("fixture lost production coordinator")
	}
	task := PlanTask{ID: "work", Goal: "execute original probes and retain both observations"}
	if contracted {
		task.Requirements = delegatedRequirements()
		task.SuccessContains = delegatedProof
	}
	if _, err := a.SetPlan(root.Run.ID, []PlanTask{task}); err != nil {
		t.Fatal(err)
	}
	return delegatedFixtureSettle(t, a, store, root.Run.ID, "work", contracted)
}

func delegatedFixtureSettle(t *testing.T, a *Agent, store *Store, root, task string, contracted bool) delegatedFixture {
	t.Helper()
	return delegatedFixtureSettleArtifact(t, a, store, root, task, contracted, "")
}

func delegatedFixtureSettleArtifact(t *testing.T, a *Agent, store *Store, root, task string, contracted bool, artifactPath string) delegatedFixture {
	t.Helper()
	_, w := capacityFixtureChild(t, a, root, task)
	if _, err := store.Update(root, func(s *Snapshot) error {
		// Different sequence domains: the parent floor is intentionally much
		// larger than all original child execution sequence numbers.
		for i := 0; i < 80; i++ {
			addEvent(s, "fixture_bookkeeping", "parent-only sequence", nil)
		}
		addEvent(s, "worker_started", "registered execution visit", w)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if contracted {
		// capacityFixtureChild links the production-created child as running;
		// no request/action exists yet, so configure its frozen contract in the
		// normal pre-execution state before starting this visit.
		if _, err := store.Update(w.RunID, func(s *Snapshot) error { s.Run.Status = StatusCreated; return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := a.SetVerificationRequirements(w.RunID, delegatedRequirements()); err != nil {
			t.Fatal(err)
		}
	}
	unlock, err := store.AcquireRunLock(w.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var ids []string
	artifactID := ""
	_, err = a.runChildOperation(context.Background(), root, w, func(child *Agent, ctx context.Context, id string) (Snapshot, error) {
		waitTestRun(t, child, id)
		for _, probe := range []struct{ call, command string }{{"goal", "printf delegated-original-proof"}, {"cleanup", "printf clean"}} {
			s, err := child.executeCommand(ctx, id, probe.call, []string{"sh", "-c", probe.command})
			if err != nil {
				return s, err
			}
			if probe.call == "goal" {
				s = delegatedArchiveStdout(t, store, s)
			}
			ids = append(ids, s.Evidence[len(s.Evidence)-1].ID)
		}
		if contracted {
			for i, requirement := range delegatedRequirements() {
				s, err := child.CheckEvidence(ctx, id, requirement.ID, []string{ids[i]})
				if err != nil {
					return s, err
				}
				if latestCheck(&s, requirement.ID).Status != "pass" {
					return s, fmt.Errorf("child exact probe rejected: %+v", s.Checks)
				}
			}
		}
		artifactIDs := []string{}
		if artifactPath != "" {
			args, _ := json.Marshal(ArtifactRegistration{Path: artifactPath})
			s, err := child.executeBuiltin(ctx, id, ToolCall{ID: "deliver", Name: "artifact_register", Arguments: string(args)})
			if err != nil {
				return s, err
			}
			artifactID = s.Evidence[len(s.Evidence)-1].ID
			artifactIDs = append(artifactIDs, artifactID)
		}
		return store.Update(id, func(s *Snapshot) error {
			resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: ids, ArtifactEvidenceIDs: artifactIDs, Answers: []Answer{{Value: delegatedProof, EvidenceIDs: []string{ids[0]}}}})
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := store.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Workers[len(parent.Workers)-1].CleanupStatus != "completed" || !taskDone(parent.Plan.Tasks[len(parent.Plan.Tasks)-1]) {
		t.Fatalf("genuine Worker did not settle: %+v", parent)
	}
	return delegatedFixture{a: a, store: store, root: root, worker: w, ids: ids, artifactID: artifactID}
}

func (f delegatedFixture) qualified(index int) string { return f.worker.RunID + "/" + f.ids[index] }

func delegatedRootCheck(t *testing.T, f delegatedFixture) Snapshot {
	t.Helper()
	var s Snapshot
	for i, requirement := range delegatedRequirements() {
		var err error
		s, err = f.a.CheckEvidence(context.Background(), f.root, requirement.ID, []string{f.qualified(i)})
		if err != nil {
			t.Fatal(err)
		}
		if check := latestCheck(&s, requirement.ID); check == nil || check.Status != "pass" || check.References[0].SourceHash == "" {
			t.Fatalf("original root probe did not pass: %+v", s.Checks)
		}
	}
	return s
}

func delegatedRootClaim(t *testing.T, f delegatedFixture) Snapshot {
	t.Helper()
	s, err := f.store.Update(f.root, func(s *Snapshot) error {
		resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: []string{f.qualified(0)}, Answers: []Answer{{Value: delegatedProof, EvidenceIDs: []string{f.qualified(0)}}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Result.Status != "success" || s.Result.CleanupStatus != "completed" || len(s.Result.Answers) == 0 || !s.Result.Answers[0].Verified {
		t.Fatalf("root original result rejected: %+v", s.Result)
	}
	return s
}

func TestDelegatedAcceptanceOriginalContractJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, contracted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/child_contract_%t", backend, contracted), func(t *testing.T) {
				f := delegatedFixtureRun(t, backend, contracted)
				before, err := f.store.Load(f.root)
				if err != nil {
					t.Fatal(err)
				}
				if len(before.Actions) != 0 || len(before.Evidence) != 0 || len(before.Checks) != 0 {
					t.Fatal("source projection fabricated root execution/checks")
				}
				if !contracted && (before.Plan.Tasks[0].Status != "completed" || len(before.Plan.Tasks[0].EvidenceIDs) != 0) {
					t.Fatal("ordinary task contract was rewritten")
				}
				if _, ok := verifiedOutput(&before, f.ids[0]); ok {
					t.Fatal("unqualified child ID accepted")
				}
				status, handled, err := f.a.verificationBuiltin(context.Background(), f.root, ToolCall{Name: "verification_status", Arguments: `{}`})
				if err != nil || !handled || !strings.Contains(fmt.Sprint(status), f.qualified(0)) {
					t.Fatalf("qualified source not discoverable: %v %v", status, err)
				}
				// Exercise the model's actual evidence_check tool, not a pass claim.
				args, _ := json.Marshal(map[string]any{"requirement_id": delegatedRequirements()[0].ID, "evidence_ids": []string{f.qualified(0)}})
				if _, handled, err := f.a.verificationBuiltin(context.Background(), f.root, ToolCall{Name: "evidence_check", Arguments: string(args)}); err != nil || !handled {
					t.Fatalf("evidence_check: %v", err)
				}
				delegatedRootCheck(t, f)
				after := delegatedRootClaim(t, f)
				if !reflect.DeepEqual(before.Requirements, after.Requirements) || !reflect.DeepEqual(before.RootContract, after.RootContract) || before.Run.SuccessContains != after.Run.SuccessContains || len(after.Actions) != 0 || len(after.Evidence) != 0 {
					t.Fatal("root contract or raw ledger changed")
				}
				if predicate := successPredicate(&after); !strings.Contains(fmt.Sprint(predicate), f.qualified(0)) {
					t.Fatalf("SuccessContains lost original source: %v", predicate)
				}
				// Reopen verifies sources through the same JSON/SQLite backend.
				reader, err := NewStore(f.store.root)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				current, err := reader.Load(f.root)
				if err != nil || current.Result.Status != "success" {
					t.Fatalf("reopened source validation: %+v %v", current.Result, err)
				}
				// Store.Update's callback uses only the already resolved view.
				done := make(chan error, 1)
				go func() {
					_, err := f.store.Update(f.root, func(s *Snapshot) error { refreshVerification(s); refreshResult(s); return nil })
					done <- err
				}()
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("nested Store load deadlock")
				}
			})
		}
	}
}

func TestDelegatedAcceptanceBranchUsesOnlyOwnWorkersJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			ancestor := delegatedFixtureRun(t, backend, false)
			parent, err := ancestor.store.Update(ancestor.root, func(s *Snapshot) error {
				s.Messages = append(s.Messages, Message{Role: "user", Content: "branch this intent"})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			branch, err := ancestor.a.Branch(ancestor.root, parent.Messages[len(parent.Messages)-1].ID)
			if err != nil {
				t.Fatal(err)
			}
			if branch.Run.Responsibility != ResponsibilityCoordinator || branch.Run.ParentRunID != ancestor.root || branch.Run.ParentTaskID != "" {
				t.Fatal("independent production branch lost root responsibility")
			}
			if _, err := ancestor.a.CheckEvidence(context.Background(), branch.Run.ID, delegatedRequirements()[0].ID, []string{ancestor.qualified(0)}); err == nil {
				t.Fatal("branch inherited historical parent's sources")
			}
			if _, err := ancestor.a.SetPlan(branch.Run.ID, []PlanTask{{ID: "own", Goal: "collect branch-owned observations"}}); err != nil {
				t.Fatal(err)
			}
			ancestor.a.provider = manualProvider(ToolCall{ID: "goal", Name: "exec", Arguments: `{"command":"printf delegated-original-proof"}`}, ToolCall{ID: "cleanup", Name: "exec", Arguments: `{"command":"printf clean"}`})
			f := delegatedFixtureSettle(t, ancestor.a, ancestor.store, branch.Run.ID, "own", false)
			delegatedRootCheck(t, f)
			delegatedRootClaim(t, f)
		})
	}
}

func TestDelegatedAcceptanceLegacyDoesNotReadChildrenJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			f := delegatedFixtureRun(t, backend, true)
			if _, err := f.store.Update(f.root, func(s *Snapshot) error { s.Run.Responsibility = ""; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := f.a.CheckEvidence(context.Background(), f.root, delegatedRequirements()[0].ID, []string{f.qualified(0)}); err == nil {
				t.Fatal("legacy root accepted delegated source")
			}
			s, err := f.store.Update(f.root, func(s *Snapshot) error {
				resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: []string{f.qualified(0)}, Answers: []Answer{{Value: delegatedProof, EvidenceIDs: []string{f.qualified(0)}}}})
				return nil
			})
			if err != nil || s.Result.Status == "success" || len(s.delegatedAcceptance) != 0 || strings.Contains(fmt.Sprint(successPredicate(&s)), "matched:true") {
				t.Fatalf("legacy source acceptance changed: %+v %v", s.Result, err)
			}
		})
	}
}

// Mutate the durable raw ledger directly: neither backend's ordinary save
// preparation nor a derived pass claim may hide missing/corrupt raw sources.
func delegatedRawMutation(t *testing.T, f delegatedFixture, id string, mutate func(*Snapshot)) {
	t.Helper()
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	unlock, err := f.store.acquireStoreLock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	s, err := f.store.loadRawLocked(id)
	if err != nil {
		t.Fatal(err)
	}
	mutate(&s)
	if err := f.store.savePreparedLocked(s); err != nil {
		t.Fatal(err)
	}
}

func TestDelegatedAcceptanceRevokesChangedSourcesJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, mutation := range []string{"ledger", "bytes", "ancestry", "task_ancestry", "worker_context", "task_binding", "task_evidence", "evidence_hash", "check_hash", "check_ids", "worker_visit", "receipt_visit", "worker_cleanup", "receipt_cleanup", "child_deleted", "child_missing"} {
			t.Run(backend+"/"+mutation, func(t *testing.T) {
				f := delegatedFixtureRun(t, backend, true)
				delegatedRootCheck(t, f)
				delegatedRootClaim(t, f)
				target := f.worker.RunID
				if strings.HasPrefix(mutation, "task_") && mutation != "task_ancestry" || strings.HasPrefix(mutation, "check_") || strings.HasPrefix(mutation, "worker_") && mutation != "worker_context" {
					target = f.root
				}
				if mutation == "bytes" {
					s, err := f.store.Load(f.worker.RunID)
					if err != nil {
						t.Fatal(err)
					}
					ref := s.Actions[0].Result.Artifacts[0]
					if err := os.WriteFile(filepath.Join(f.store.root, "artifacts", ref.Hash), []byte("changed original bytes"), 0600); err != nil {
						t.Fatal(err)
					}
				} else if mutation == "child_missing" {
					if backend == "sqlite" {
						if _, err := f.store.sqlitePool.Exec("DELETE FROM runs WHERE id = ?", f.worker.RunID); err != nil {
							t.Fatal(err)
						}
					} else if err := os.Remove(filepath.Join(f.store.root, f.worker.RunID+".json")); err != nil {
						t.Fatal(err)
					}
				} else {
					delegatedRawMutation(t, f, target, func(s *Snapshot) {
						switch mutation {
						case "ledger":
							s.Actions[0].Arguments = `{"changed":true}`
						case "ancestry":
							s.Run.ParentRunID = "other-parent"
						case "task_ancestry":
							s.Run.ParentTaskID = "other-task"
						case "worker_context":
							s.WorkerContext.WorkerID = "other-worker"
						case "task_binding":
							s.Plan.Tasks[0].WorkerID = "other-worker"
						case "task_evidence":
							s.Plan.Tasks[0].EvidenceIDs = []string{f.ids[1]}
						case "evidence_hash":
							s.Evidence[0].ContentHash = strings.Repeat("0", 64)
						case "check_hash":
							s.Checks[0].References[0].SourceHash = strings.Repeat("0", 64)
						case "check_ids":
							s.Checks[0].EvidenceIDs = []string{f.qualified(1)}
						case "worker_visit":
							s.Workers[0].Visit++
						case "receipt_visit":
							s.WorkerSettlement.Visit++
						case "worker_cleanup":
							s.Workers[0].CleanupStatus = "uncertain"
						case "receipt_cleanup":
							s.WorkerSettlement.CleanupStatus = "uncertain"
						case "child_deleted":
							s.Run.Deleted = true
						}
					})
				}
				s, err := f.store.Load(f.root)
				if err != nil {
					t.Fatal(err)
				}
				if s.Result.Status == "success" || len(unmetRequirements(&s)) == 0 {
					t.Fatalf("source change retained root success: %+v", s.Result)
				}
				for _, c := range s.Checks {
					if c.RequirementID == delegatedRequirements()[0].ID && c.Status == "pass" {
						t.Fatal("goal check survived revoked source")
					}
				}
				reader, err := NewStore(f.store.root)
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				current, err := reader.Load(f.root)
				if err != nil || current.Result.Status == "success" {
					t.Fatalf("revocation lost on reopen: %+v %v", current.Result, err)
				}
			})
		}
	}
}
