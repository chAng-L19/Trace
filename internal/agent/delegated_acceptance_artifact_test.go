package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func delegatedArtifactFixture(t *testing.T, backend string) delegatedFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "deliverable.txt")
	if err := os.WriteFile(path, []byte("artifact-only-marker"), 0600); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(ArtifactRegistration{Path: path})
	a, store := responsibilityAgent(t, backend, manualProvider(
		ToolCall{ID: "goal", Name: "exec", Arguments: `{"command":"printf delegated-original-proof"}`},
		ToolCall{ID: "cleanup", Name: "exec", Arguments: `{"command":"printf clean"}`},
		ToolCall{ID: "deliver", Name: "artifact_register", Arguments: string(args)},
	))
	predicate := delegatedProof
	root, err := a.CreateConfigured("delegate original probes and archive deliverable", RunConfiguration{Requirements: delegatedRequirements(), SuccessContains: &predicate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{{ID: "work", Goal: "execute original probes and register original deliverable"}}); err != nil {
		t.Fatal(err)
	}
	return delegatedFixtureSettleArtifact(t, a, store, root.Run.ID, "work", false, path)
}

func TestDelegatedAcceptanceArtifactQualifiedRefsJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			f := delegatedArtifactFixture(t, backend)
			delegatedRootCheck(t, f)
			artifact := f.worker.RunID + "/" + f.artifactID
			s, err := f.store.Update(f.root, func(s *Snapshot) error {
				resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: []string{f.qualified(0)}, ArtifactEvidenceIDs: []string{artifact}, Answers: []Answer{{Value: delegatedProof, EvidenceIDs: []string{f.qualified(0)}}}})
				return nil
			})
			if err != nil || s.Result.Status != "success" || len(s.Result.ValidationIssues) != 0 {
				t.Fatalf("qualified artifact rejected: %+v %v", s.Result, err)
			}
			o, ok := delegatedObservationFor(&s, artifact)
			if !ok || !o.ArtifactOnly || !o.HasArtifacts || o.Reference.SourceHash == "" || o.Output != "" {
				t.Fatal("artifact acquired observation eligibility")
			}
			if _, issue := diagnoseResultEvidence(&s, artifact, true); issue != nil {
				t.Fatal(issue)
			}
			if _, issue := diagnoseResultEvidence(&s, artifact, false); issue == nil || issue.Code != "evidence_not_observation" {
				t.Fatal("artifact accepted as outcome stdout")
			}
			if _, ok := verifiedOutput(&s, artifact); ok {
				t.Fatal("artifact became SuccessContains/answer output")
			}
			if _, err := evidenceReferences(&s, []string{artifact}); err == nil {
				t.Fatal("artifact became predicate/review evidence")
			}
			if status, _ := checkPredicate(&s, EvidencePredicate{Kind: "contains", Value: ""}, []string{artifact}, "", "", 0); status == "pass" {
				t.Fatal("artifact passed an original predicate")
			}
			s.Run.SuccessContains = "deliverable.txt"
			if successPredicate(&s).(map[string]any)["matched"].(bool) {
				t.Fatal("artifact bookkeeping satisfied SuccessContains")
			}
			s, err = f.store.Update(f.root, func(s *Snapshot) error {
				resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: []string{artifact}, Answers: []Answer{{Value: "deliverable.txt", EvidenceIDs: []string{artifact}}}})
				return nil
			})
			if err != nil || s.Result.Status == "success" || s.Result.Answers[0].Verified {
				t.Fatalf("artifact laundered outcome/answer: %+v %v", s.Result, err)
			}
			if len(s.Actions) != 0 || len(s.Evidence) != 0 {
				t.Fatal("artifact projection copied the child ledger")
			}
		})
	}
}

func TestDelegatedAcceptanceArtifactSourceRevocationJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, mutation := range []string{"bytes", "ledger", "reference"} {
			t.Run(backend+"/"+mutation, func(t *testing.T) {
				f := delegatedArtifactFixture(t, backend)
				delegatedRootCheck(t, f)
				artifact := f.worker.RunID + "/" + f.artifactID
				if _, err := f.store.Update(f.root, func(s *Snapshot) error {
					resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: []string{f.qualified(0)}, ArtifactEvidenceIDs: []string{artifact}})
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if mutation == "bytes" {
					s, err := f.store.Load(f.worker.RunID)
					if err != nil {
						t.Fatal(err)
					}
					ref := s.Actions[len(s.Actions)-1].Result.Artifacts[0]
					if err := os.WriteFile(filepath.Join(f.store.root, "artifacts", ref.Hash), []byte("altered deliverable"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					delegatedRawMutation(t, f, f.worker.RunID, func(s *Snapshot) {
						if mutation == "ledger" {
							s.Actions[len(s.Actions)-1].Arguments = "{}"
						} else {
							resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: f.ids, Answers: []Answer{{Value: delegatedProof, EvidenceIDs: []string{f.ids[0]}}}})
							s.WorkerSettlement.BusinessSourceHash = workerBusinessSourceHash(s)
						}
					})
				}
				s, err := f.store.Load(f.root)
				if err != nil {
					t.Fatal(err)
				}
				if _, issue := diagnoseResultEvidence(&s, artifact, true); issue == nil {
					t.Fatal("changed or unbound artifact source remained eligible")
				}
				if s.Result.Status == "success" || latestCheck(&s, delegatedRequirements()[0].ID).Status == "pass" {
					t.Fatal("root validation survived changed Worker artifact binding")
				}
			})
		}
	}
}
