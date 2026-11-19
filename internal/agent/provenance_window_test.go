package agent

import (
	"context"
	"fmt"
	"testing"
)

func TestCompletedChildRevalidatesRequiredBusinessSuccess(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, strict := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/strict=%t", backend, strict), func(t *testing.T) {
				a, store := treeCapacityAgent(t, backend, MockProvider{})
				a.SetRequireVerifiedCompletion(strict)
				root := createTestRun(t, a)
				if _, err := a.SetPlan(root, []PlanTask{{ID: "read", Goal: "read a fixture"}, {ID: "next", Goal: "review", Dependencies: []string{"read"}}}); err != nil {
					t.Fatal(err)
				}
				_, w := capacityFixtureChild(t, a, root, "read")
				if err := settlementOperation(t, a, store, root, w, func(s *Snapshot) {
					resultValidationSource(t, s, "proof", "read", "builtin", "fixture-ready")
					resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: []string{"proof"}, Answers: []Answer{{Value: "fixture-ready", EvidenceIDs: []string{"proof"}}}})
					if s.Result.Status != "success" {
						t.Fatalf("valid source did not establish child success: %+v", s.Result)
					}
				}); err != nil {
					t.Fatal(err)
				}
				before, err := store.Load(root)
				if err != nil || before.Plan.Tasks[0].Status != "completed" || !hasString(before.Plan.Frontier, "next") {
					t.Fatalf("valid child completion: %+v, %v", before.Plan, err)
				}
				child, err := store.Update(w.RunID, func(s *Snapshot) error {
					s.Evidence[0].Content += "changed"
					return nil
				})
				if err != nil || child.Run.Status != StatusCompleted || child.Result.Status != "incomplete" || child.Run.RequireVerifiedCompletion != strict {
					t.Fatalf("revoked source: run=%s result=%s strict=%t err=%v", child.Run.Status, child.Result.Status, child.Run.RequireVerifiedCompletion, err)
				}
				after, err := store.Load(root)
				want := "completed"
				if strict {
					want = "uncertain"
				}
				if err != nil || after.Plan.Tasks[0].Status != want || hasString(after.Plan.Frontier, "next") != (want == "completed") || after.Workers[0].CleanupStatus != "completed" {
					t.Fatalf("parent task should be %s with historical cleanup intact: %+v, %v", want, after.Plan, err)
				}
				again, err := store.Load(root)
				if err != nil || again.Run.EventSeq != after.Run.EventSeq || len(again.WorkerMessages) != len(before.WorkerMessages) {
					t.Fatalf("revalidation emitted a second settlement or invalidation: %v", err)
				}
			})
		}
	}
}

func TestProvenanceWindowRevalidatesRepeatedChildSources(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, resultProvider{})
			defer a.CloseMCP()
			root := createTestRun(t, a)
			if _, err := a.SetPlan(root, []PlanTask{{ID: "first", Goal: "first proof", SuccessContains: "flag{verified}"}, {ID: "second", Goal: "second proof", SuccessContains: "flag{verified}"}}); err != nil {
				t.Fatal(err)
			}
			var children []Snapshot
			for _, task := range []string{"first", "second"} {
				w, err := a.DispatchTask(context.Background(), root, task)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := waitAllTestWorkers(a, context.Background(), root, 10); err != nil {
					t.Fatal(err)
				}
				child, err := store.Load(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				children = append(children, child)
			}
			// Tasks read first->second; coverage then revisits first->second->first.
			for i, source := range []int{0, 1, 0} {
				child := children[source]
				if _, err := a.UpsertAsset(root, Asset{ID: fmt.Sprint("asset", i), Kind: "file", Value: fmt.Sprint(i), Coverage: "verified", Predicate: "flag{verified}", SourceRunID: child.Run.ID, EvidenceIDs: child.Result.EvidenceIDs}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := store.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			if before.Plan.Tasks[0].Status != "verified" || before.Plan.Tasks[1].Status != "verified" || Coverage(before).ByStatus["verified"] != 3 {
				t.Fatal("source window lost valid repeated sources")
			}
			if _, err := store.Update(children[0].Run.ID, func(s *Snapshot) error { s.Actions[0].Result.ExitCode = 7; return nil }); err != nil {
				t.Fatal(err)
			}
			after, err := store.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			if after.Plan.Tasks[0].Status != "uncertain" || after.Plan.Tasks[1].Status != "verified" || after.Assets[0].Coverage != "uncertain" || after.Assets[1].Coverage != "verified" || after.Assets[2].Coverage != "uncertain" {
				t.Fatal("changed source did not revoke all repeated references")
			}
			if _, err := store.Update(children[1].Run.ID, func(s *Snapshot) error { s.Run.ParentRunID = "unrelated-parent"; return nil }); err != nil {
				t.Fatal(err)
			}
			after, err = store.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			if after.Plan.Tasks[1].Status != "uncertain" || Coverage(after).ByStatus["uncertain"] != 3 {
				t.Fatal("mismatched child identity remained a valid source")
			}
			other, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			again, err := other.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			if again.Run.EventSeq != after.Run.EventSeq || Coverage(again).ByStatus["uncertain"] != 3 {
				t.Fatal("source revocation was not durable/idempotent")
			}
		})
	}
}
