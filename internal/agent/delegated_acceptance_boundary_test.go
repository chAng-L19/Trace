package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestDelegatedAcceptanceKeepsOriginalProbeAndPredicateJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, mismatch := range []string{"tool", "arguments", "predicate"} {
			t.Run(backend+"/"+mismatch, func(t *testing.T) {
				requirements := delegatedRequirements()
				switch mismatch {
				case "tool":
					requirements[0].ToolID = "different-original-tool"
				case "arguments":
					requirements[0].ArgumentsHash = promptHash(`{"command":"different-original-probe"}`)
				case "predicate":
					requirements[0].Predicate.Value = "different-original-output"
				}
				f := delegatedFixtureContract(t, backend, false, requirements)
				s, err := f.a.CheckEvidence(context.Background(), f.root, requirements[0].ID, []string{f.qualified(0)})
				if err != nil {
					t.Fatal(err)
				}
				check := latestCheck(&s, requirements[0].ID)
				if check == nil || check.Status == "pass" {
					t.Fatal("child success laundered original probe mismatch")
				}
				if s.Requirements[0].ToolID != requirements[0].ToolID || s.Requirements[0].ArgumentsHash != requirements[0].ArgumentsHash || valueHash(s.Requirements[0].Predicate) != valueHash(requirements[0].Predicate) {
					t.Fatal("original root probe was weakened")
				}
			})
		}
	}
}

func TestDelegatedAcceptanceAnswersAndRefsUseOriginalStdoutJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, invalid := range []string{"literal", "unqualified", "unknown_run", "nested_run"} {
			t.Run(backend+"/"+invalid, func(t *testing.T) {
				f := delegatedFixtureRun(t, backend, false)
				delegatedRootCheck(t, f)
				ref, answer := f.qualified(0), delegatedProof
				switch invalid {
				case "literal":
					answer = "fabricated-answer"
				case "unqualified":
					ref = f.ids[0]
				case "unknown_run":
					ref = "unregistered/" + f.ids[0]
				case "nested_run":
					ref = f.worker.RunID + "/another-run/" + f.ids[0]
				}
				s, err := f.store.Update(f.root, func(s *Snapshot) error {
					resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: []string{ref}, Answers: []Answer{{Value: answer, EvidenceIDs: []string{ref}, Verified: true}}})
					return nil
				})
				if err != nil || s.Result.Status == "success" || len(s.Result.Answers) == 0 || s.Result.Answers[0].Verified {
					t.Fatalf("fabricated answer/reference accepted: %+v %v", s.Result, err)
				}
			})
		}
	}
}

func TestDelegatedAcceptanceIndependentReviewUsesOriginalOutputJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			requirements := delegatedRequirements()
			requirements[0].RequireReview = true
			f := delegatedFixtureContract(t, backend, false, requirements)
			f.a.provider = &scriptedProvider{responses: []ModelResponse{{Content: `{"status":"pass","reason":"exact original output supports the goal"}`}}}
			s, err := f.a.CheckEvidence(context.Background(), f.root, requirements[0].ID, []string{f.qualified(0)})
			if err != nil {
				t.Fatal(err)
			}
			check := latestCheck(&s, requirements[0].ID)
			if check == nil || check.Status != "pass" || check.Review == nil || !reviewCurrent(&s, check.Review, valueHash(requirements[0]), check.References) {
				t.Fatalf("source-qualified review unavailable: %+v", s.Checks)
			}
			var input reviewInput
			for _, request := range s.Requests {
				if request.ID == check.Review.RequestID {
					if err := json.Unmarshal([]byte(request.Messages[1].Content), &input); err != nil {
						t.Fatal(err)
					}
				}
			}
			if len(input.Outputs) != 1 || input.Outputs[0] != delegatedProof || input.References[0].ID != f.qualified(0) || input.References[0].SourceHash == "" {
				t.Fatalf("review used child claim instead of original stdout: %+v", input)
			}
		})
	}
}

func delegatedTwoWorkers(t *testing.T, backend string) (delegatedFixture, delegatedFixture) {
	t.Helper()
	first := delegatedFixtureRun(t, backend, false)
	if _, err := first.a.UpdatePlan(first.root, []PlanTask{{ID: "last-cleanup", Goal: "repeat the original cleanup probe after all other Workers stop", Dependencies: []string{"work"}}}, "append"); err != nil {
		t.Fatal(err)
	}
	first.a.provider = manualProvider(ToolCall{ID: "goal", Name: "exec", Arguments: `{"command":"printf delegated-original-proof"}`}, ToolCall{ID: "cleanup", Name: "exec", Arguments: `{"command":"printf clean"}`})
	second := delegatedFixtureSettle(t, first.a, first.store, first.root, "last-cleanup", false)
	return first, second
}

func TestDelegatedAcceptanceCleanupUsesCausalDomainsJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			first, last := delegatedTwoWorkers(t, backend)
			// The first Worker's old probe predates the next target execution.
			s, err := first.a.CheckEvidence(context.Background(), first.root, delegatedRequirements()[1].ID, []string{first.qualified(1)})
			if err != nil || latestCheck(&s, delegatedRequirements()[1].ID).Status == "pass" {
				t.Fatalf("old cross-Worker cleanup accepted: %+v %v", s.Checks, err)
			}
			// The last leaf was dispatched after prior settlement. Its own
			// worker_finished event cannot permanently invalidate its probe.
			delegatedRootCheck(t, last)
			delegatedRootClaim(t, last)
		})
	}
}

func TestDelegatedAcceptanceCleanupRejectsUnorderedOrStaleSourcesJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, mutation := range []string{"missing_dispatch", "missing_finish", "later_other_worker", "missing_dependency", "root_effect", "later_child_effect", "stale_local_start"} {
			t.Run(backend+"/"+mutation, func(t *testing.T) {
				var f, earlier delegatedFixture
				if mutation == "later_other_worker" || mutation == "missing_dependency" {
					earlier, f = delegatedTwoWorkers(t, backend)
				} else {
					f = delegatedFixtureRun(t, backend, false)
				}
				delegatedRootCheck(t, f)
				delegatedRootClaim(t, f)
				target := f.root
				if mutation == "later_child_effect" || mutation == "stale_local_start" {
					target = f.worker.RunID
				}
				delegatedRawMutation(t, f, target, func(s *Snapshot) {
					switch mutation {
					case "missing_dispatch", "missing_finish":
						for i := range s.Events {
							if mutation == "missing_dispatch" && s.Events[i].Type == "worker_started" || mutation == "missing_finish" && s.Events[i].Type == "worker_finished" {
								s.Events[i].Type = "removed_boundary"
							}
						}
					case "later_other_worker":
						addEvent(s, "worker_finished", "later other execution fence", map[string]string{"worker_id": earlier.worker.ID, "run_id": earlier.worker.RunID, "cleanup_status": "completed"})
					case "missing_dependency":
						s.Plan.Tasks[1].Dependencies = nil
					case "root_effect":
						resultValidationSource(t, s, "root-effect", "exec", "exec", "a root mutation is not delegated cleanup")
					case "later_child_effect":
						resultValidationSource(t, s, "later-effect", "exec", "exec", "later target mutation")
						// Even an updated retirement hash cannot make the old
						// probe fresh after a genuine later ledger observation.
						refreshResult(s)
						s.WorkerSettlement.BusinessSourceHash = workerBusinessSourceHash(s)
					case "stale_local_start":
						for i := range s.Events {
							if s.Events[i].Type == "action_started" {
								var fields map[string]string
								raw, _ := json.Marshal(s.Events[i].Data)
								json.Unmarshal(raw, &fields)
								if fields["call_id"] == "cleanup" {
									s.Events[i].Type = "missing_probe_start"
								}
							}
						}
					}
				})
				s, err := f.store.Load(f.root)
				if err != nil || s.Result.Status == "success" || latestCheck(&s, delegatedRequirements()[1].ID).Status == "pass" {
					t.Fatalf("invalid cleanup remained accepted: %+v %v", s.Result, err)
				}
				// A fresh CheckEvidence must reject it too; persisted pass
				// invalidation alone is not a usable acceptance boundary.
				s, err = f.a.CheckEvidence(context.Background(), f.root, delegatedRequirements()[1].ID, []string{f.qualified(1)})
				if err == nil && latestCheck(&s, delegatedRequirements()[1].ID).Status == "pass" {
					t.Fatal("fresh check laundered stale cleanup")
				}
			})
		}
	}
}

func TestDelegatedAcceptanceCleanupAllowsRetiredStageJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			_, last := delegatedTwoWorkers(t, backend)
			delegatedRawMutation(t, last, last.root, func(s *Snapshot) {
				s.Plan.Tasks[0].Stage, s.Plan.Tasks[0].Status = true, "abandoned"
				s.Plan.Tasks[0].DispositionReason = "retired execution stage; final leaf independently probes cleanup"
				s.Plan.Tasks[1].Dependencies = nil
			})
			delegatedRootCheck(t, last)
			delegatedRootClaim(t, last)
		})
	}
}

func TestDelegatedAcceptanceRawReportReadRevokesWithoutCommitJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			f := delegatedFixtureRun(t, backend, true)
			delegatedRootCheck(t, f)
			delegatedRootClaim(t, f)
			delegatedRawMutation(t, f, f.worker.RunID, func(s *Snapshot) { s.Evidence[0].ContentHash = strings.Repeat("0", 64) })
			detached, err := f.store.readReportSource(f.root)
			if err != nil || detached.Result.Status == "success" {
				t.Fatalf("detached report trusted stale source: %+v %v", detached.Result, err)
			}
			f.store.mu.Lock()
			raw, err := f.store.loadRawLocked(f.root)
			f.store.mu.Unlock()
			if err != nil || raw.Result.Status != "success" {
				t.Fatalf("read-only report committed revocation: %+v %v", raw.Result, err)
			}
			current, err := f.store.Load(f.root)
			if err != nil || current.Result.Status == "success" {
				t.Fatalf("ordinary load did not commit revocation: %+v %v", current.Result, err)
			}
			if len(current.Actions) != 0 || len(current.Evidence) != 0 {
				t.Fatal("revocation fabricated a root source ledger")
			}
		})
	}
}

func TestDelegatedAcceptanceLegacyDisposedSourceNotReadJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			f := delegatedFixtureRun(t, backend, false)
			delegatedRawMutation(t, f, f.root, func(s *Snapshot) {
				s.Run.Responsibility = ""
				s.Plan.Tasks[0].Status, s.Plan.Tasks[0].DispositionReason = "abandoned", "legacy retired source"
			})
			delegatedRawMutation(t, f, f.worker.RunID, func(s *Snapshot) { s.Evidence[0].ContentHash = strings.Repeat("0", 64) })
			f.store.mu.Lock()
			before, err := f.store.loadRawLocked(f.worker.RunID)
			f.store.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			parent, err := f.store.Load(f.root)
			if err != nil {
				t.Fatal(err)
			}
			f.store.mu.Lock()
			after, err := f.store.loadRawLocked(f.worker.RunID)
			f.store.mu.Unlock()
			if err != nil || valueHash(before) != valueHash(after) || len(parent.delegatedAcceptance) != 0 || parent.delegatedDescendantsRetired {
				t.Fatal("legacy load introduced delegated source validation or commits")
			}
		})
	}
}
