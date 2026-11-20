package agent

import (
	"context"
	"testing"
	"time"

	"trace/internal/mcp"
)

func TestWorkerReportCloneFailureRetainsSettledBusiness(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, id, w, calls := reportWorkerFixture(t, backend, nil)
			if _, err := a.ClaimTask(id, "next", "consumer", 30); err != nil {
				t.Fatal(err)
			}
			// Fail the real Clone path before any remote request or resource exists.
			a.registry.SetMCPConfig("bad-report-clone", mcp.Config{Name: "bad-report-clone", URL: "://invalid"})
			if _, err := a.ResumeWorker(context.Background(), id, w.ID); err != nil {
				t.Fatal(err)
			}
			parent, err := waitAllTestWorkers(a, context.Background(), id, 10)
			child, loadErr := store.Load(w.RunID)
			if err != nil || loadErr != nil || calls.Load() != 3 || len(child.Actions) != 1 || parent.Plan.Tasks[0].Status != "verified" || parent.Plan.Tasks[1].Status != "running" || parent.Workers[0].Status != "paused" || parent.Workers[0].CleanupStatus != "completed" {
				t.Fatalf("report connection failure revoked proof or replayed: %+v %+v err=%v/%v", parent.Plan, parent.Workers, err, loadErr)
			}
			if child.WorkerSettlement.RunError == "" || child.WorkerSettlement.BusinessContinuationVisit == nil || *child.WorkerSettlement.BusinessContinuationVisit != w.Visit || !reportDeliveryPending(&child) || !reportBusinessSettled(id, parent.Workers[0], &child) {
				t.Fatalf("failure lost delivery/continuity receipt: %+v", child.WorkerSettlement)
			}
		})
	}
}

func TestWorkerNestedReportDeliveryDoesNotUnlockParent(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, reason := range []string{"child_report_delivery_pending", "report_translation_missing"} {
			t.Run(backend+"/"+reason, func(t *testing.T) {
				a, store, id, w, calls := reportWorkerFixture(t, backend, nil)
				_, _, otherID, grandchildWorker, grandchildCalls := reportWorkerFixtureInStore(t, store, nil)
				other, err := store.Load(otherID)
				if err != nil {
					t.Fatal(err)
				}
				_, err = store.Update(w.RunID, func(child *Snapshot) error {
					grandchild, err := store.loadLocked(grandchildWorker.RunID)
					if err != nil {
						return err
					}
					// Form P -> C -> G from two independently source-verified runs.
					grandchild.Run.ParentRunID = child.Run.ID
					grandchild.WorkerSettlement.ParentRunID = child.Run.ID
					grandchild.WorkerSettlement.BusinessSourceHash = workerBusinessSourceHash(&grandchild)
					if err := store.saveLocked(grandchild); err != nil {
						return err
					}
					child.Plan.Tasks = append([]PlanTask(nil), other.Plan.Tasks[:1]...)
					child.Workers = []AgentWorker{grandchildWorker}
					child.ChildReports = []ChildReport{newChildReport(child.Run.ID, grandchildWorker, &grandchild, "completed")}
					child.Run.Status, child.Run.PauseReason = StatusPaused, reason
					// Own translation and descendant delivery may both be pending.
					child.ReportDelivery.Pending = true
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				child, err := store.Load(w.RunID)
				if err != nil || child.Result.Status != "success" {
					t.Fatalf("fixture lost core proof: %+v %v", child.Result, err)
				}
				_, err = store.Update(id, func(parent *Snapshot) error {
					parent.Plan.Tasks[0].Status = "running"
					parent.Workers[0].Status, parent.Workers[0].CleanupStatus = "running", "pending"
					parent.Workers[0].SettledVisit = nil
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				unlock, err := store.AcquireRunLock(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				_, err = store.Update(id, func(parent *Snapshot) error { return a.applyWorkerSettlementLocked(parent, w) })
				if err != nil {
					unlock()
					t.Fatal(err)
				}
				parent, err := store.Load(id)
				if err != nil || taskDone(parent.Plan.Tasks[0]) || parent.Plan.Tasks[0].Status != "paused" || parent.Plan.Tasks[1].Status != "pending" || hasString(parent.Plan.Frontier, "next") {
					unlock()
					t.Fatalf("nested delivery unlocked parent dependency: %+v %v", parent.Plan, err)
				}
				_, release, _, err := a.prepareWorkerResume(context.Background(), child)
				if err != nil {
					unlock()
					t.Fatal(err)
				}
				release()
				unlock()
				parent, err = store.Load(id)
				if err != nil || parent.Plan.Tasks[1].Status != "pending" || calls.Load() != 3 || grandchildCalls.Load() != 3 {
					t.Fatalf("nested continuation revoked a previously unlocked dependency or replayed: %+v %v", parent.Plan, err)
				}
			})
		}
	}
}

func TestWorkerEarlyReportFindingSourceBinding(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			_, store, id, w, _ := reportWorkerFixture(t, backend, nil)
			child, err := store.Update(w.RunID, func(s *Snapshot) error {
				s.Findings = append(s.Findings, Finding{ID: "finding", Title: "Source-bound finding", RootCause: "Observed marker", EvidenceIDs: append([]string(nil), s.Result.EvidenceIDs...)})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := store.ChildReports(id)
			if err != nil || rows[0].SourceStatus != "changed" {
				t.Fatalf("finding business change retained old source: %+v %v", rows, err)
			}
			early := newChildReport(id, w, &child, "pending")
			if _, err := store.Update(id, func(s *Snapshot) error { appendChildReport(s, early); return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Update(w.RunID, func(s *Snapshot) error {
				body := u7Text
				s.Findings[0].Report, s.Findings[0].UpdatedAt = &body, time.Now().UTC()
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			rows, err = store.ChildReports(id)
			if err != nil || rows[len(rows)-1].SourceStatus != "available" {
				t.Fatalf("translation changed finding business source: %+v %v", rows, err)
			}
			if _, err := store.Update(w.RunID, func(s *Snapshot) error { s.Findings[0].Title += " changed"; return nil }); err != nil {
				t.Fatal(err)
			}
			rows, err = store.ChildReports(id)
			if err != nil || rows[len(rows)-1].SourceStatus != "changed" {
				t.Fatalf("changed title reused early hash: %+v %v", rows, err)
			}
		})
	}
}
