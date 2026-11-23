package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

func TestDelegatedAcceptanceCleanupBindsOriginalProbeVisitJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, laterEffects := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/later_effects_%t", backend, laterEffects), func(t *testing.T) {
				first, last := delegatedTwoWorkers(t, backend)
				delegatedRootCheck(t, last)
				delegatedRootClaim(t, last)
				delegatedPauseForFollowup(t, last)
				if laterEffects {
					delegatedPauseForFollowup(t, first)
					first.a.provider = manualProvider(ToolCall{ID: "followup", Name: "exec", Arguments: `{"command":"printf later-effect"}`})
					delegatedResume(t, first, func(child *Agent, ctx context.Context, id string) (Snapshot, error) {
						waitTestRun(t, child, id)
						s, err := child.executeCommand(ctx, id, "followup", []string{"sh", "-c", "printf later-effect"})
						if err != nil {
							return s, err
						}
						proof := s.Evidence[len(s.Evidence)-1].ID
						return first.store.Update(id, func(s *Snapshot) error {
							resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: []string{proof}})
							return nil
						})
					})
				}
				// No new execution: formatting/report continuation cites the old
				// genuine probes, and must retain their original visit chronology.
				delegatedResume(t, last, func(_ *Agent, _ context.Context, id string) (Snapshot, error) {
					return last.store.Update(id, func(s *Snapshot) error {
						resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: last.ids})
						return nil
					})
				})
				s, err := last.a.CheckEvidence(context.Background(), last.root, delegatedRequirements()[1].ID, []string{last.qualified(1)})
				if err != nil {
					t.Fatal(err)
				}
				check := latestCheck(&s, delegatedRequirements()[1].ID)
				if check == nil || (check.Status == "pass") == laterEffects {
					t.Fatalf("old probe refreshed by a report continuation: %+v", s.Checks)
				}
				if laterEffects && s.Result.Status == "success" {
					t.Fatal("root result survived stale cleanup")
				}
				// A new exact probe in the later visit is usable without changing
				// the root tool, arguments hash, predicate or Action history.
				delegatedPauseForFollowup(t, last)
				last.a.provider = manualProvider(ToolCall{ID: "fresh-cleanup", Name: "exec", Arguments: `{"command":"printf clean"}`})
				delegatedResume(t, last, func(child *Agent, ctx context.Context, id string) (Snapshot, error) {
					waitTestRun(t, child, id)
					s, err := child.executeCommand(ctx, id, "fresh-cleanup", []string{"sh", "-c", "printf clean"})
					if err != nil {
						return s, err
					}
					last.ids[1] = s.Evidence[len(s.Evidence)-1].ID
					return last.store.Update(id, func(s *Snapshot) error {
						resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: last.ids})
						return nil
					})
				})
				delegatedRootCheck(t, last)
				delegatedRootClaim(t, last)
			})
		}
	}
}

func TestDelegatedAcceptanceCleanupUnknownVisitFloorJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, mutation := range []string{"missing", "mismatch", "future", "regressed"} {
			t.Run(backend+"/"+mutation, func(t *testing.T) {
				f := delegatedFixtureRun(t, backend, false)
				delegatedPauseForFollowup(t, f)
				delegatedResume(t, f, func(_ *Agent, _ context.Context, id string) (Snapshot, error) {
					return f.store.Update(id, func(s *Snapshot) error {
						resultValidationClaim(t, s, Result{Status: "success", EvidenceIDs: f.ids})
						return nil
					})
				})
				delegatedRootCheck(t, f)
				delegatedRootClaim(t, f)
				delegatedRawMutation(t, f, f.root, func(s *Snapshot) {
					for i := range s.Events {
						if s.Events[i].Type != "worker_resumed" {
							continue
						}
						raw, _ := json.Marshal(s.Events[i].Data)
						var w AgentWorker
						if err := json.Unmarshal(raw, &w); err != nil {
							t.Fatal(err)
						}
						switch mutation {
						case "missing":
							w.SourceEventFloor = nil
							s.Workers[0].SourceEventFloor = nil
						case "mismatch":
							floor := *w.SourceEventFloor + 1
							w.SourceEventFloor = &floor
						case "future":
							floor := *w.SourceEventFloor + 10000
							w.SourceEventFloor = &floor
							s.Workers[0].SourceEventFloor = &floor
						case "regressed":
							floor := -1
							w.SourceEventFloor = &floor
							s.Workers[0].SourceEventFloor = &floor
						}
						s.Events[i].Data = w
					}
				})
				s, err := f.a.CheckEvidence(context.Background(), f.root, delegatedRequirements()[1].ID, []string{f.qualified(1)})
				if err == nil && latestCheck(&s, delegatedRequirements()[1].ID).Status == "pass" {
					t.Fatal("unknown or corrupted visit boundary accepted")
				}
				if s.Result.Status == "success" {
					t.Fatal("result survived corrupted visit boundary")
				}
			})
		}
	}
}
