package agent

import (
	"strings"
	"testing"
	"time"
)

func fixedReportFixture(t *testing.T) (*Store, Snapshot) {
	t.Helper()
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	snap, err := store.Update(id, func(s *Snapshot) error {
		s.EvidenceChains = []EvidenceChain{{ID: "chain", Name: "固定链路", Summary: "固定链路摘要", ContentHash: "fixed-chain-hash", Steps: []EvidenceChainStep{{RunID: id, EvidenceID: "ev-local", ContentHash: "ev-hash", Label: "观察"}}}}
		s.EvidenceChainRefs = []EvidenceChainReference{{SourceRunID: "external", ChainID: "external-chain", ChainHash: "external-hash", Purpose: "固定引用用途"}}
		s.Workers = []AgentWorker{{ID: "worker", TaskID: "task", RunID: "child", Visit: 1, Status: "completed", CleanupStatus: "completed"}}
		s.ChildReports = []ChildReport{{ParentRunID: id, SourceRunID: "child", WorkerID: "worker", TaskID: "task", Visit: 1, SourceHash: "child-hash", Summary: "固定子报告", SourceStatus: "available", Current: true}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, snap
}

func TestP0FixedReportHashCoversRenderedSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"goal", func(s *Snapshot) { s.Run.Goal += " changed" }},
		{"chain_name", func(s *Snapshot) { s.EvidenceChains[0].Name += " changed" }},
		{"chain_summary", func(s *Snapshot) { s.EvidenceChains[0].Summary += " changed" }},
		{"chain_step", func(s *Snapshot) { s.EvidenceChains[0].Steps[0].Label += " changed" }},
		{"chain_reference", func(s *Snapshot) { s.EvidenceChainRefs[0].ChainHash += " changed" }},
		{"reference_purpose", func(s *Snapshot) { s.EvidenceChainRefs[0].Purpose += " changed" }},
		{"child_report", func(s *Snapshot) { s.ChildReports[0].Summary += " changed" }},
		{"worker_visit", func(s *Snapshot) { s.Workers[0].Visit++ }},
		{"usage", func(s *Snapshot) { s.Requests = append(s.Requests, ModelRequest{ID: "usage-request"}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, snap := fixedReportFixture(t)
			hash := childReportHash(&snap)
			before, err := store.ReportVersion(snap.Run.ID, hash)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := store.Update(snap.Run.ID, func(s *Snapshot) error { tc.mutate(s); return nil })
			if err != nil {
				t.Fatal(err)
			}
			if childReportHash(&changed) == hash {
				t.Fatal("rendered mutation preserved source hash")
			}
			if _, err = store.ReportVersion(snap.Run.ID, hash); err == nil {
				t.Fatal("old hash accepted changed body")
			}
			after, err := store.ReportVersion(snap.Run.ID, childReportHash(&changed))
			if err != nil || before == after {
				t.Fatalf("mutation not reflected: %v", err)
			}
		})
	}
}

func TestP0FixedReportStableAndDoesNotRefreshSavedUsage(t *testing.T) {
	store, snap := fixedReportFixture(t)
	// Legacy persisted usage may differ from a fresh calculation. Settlement
	// and export must use the same loaded value rather than export-only repair.
	snap.Run.Usage.ModelRequests = 73
	unlock, err := store.acquireStoreLock()
	if err != nil {
		t.Fatal(err)
	}
	err = store.saveLocked(snap)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	hash := childReportHash(&loaded)
	first, err := store.ReportVersion(snap.Run.ID, hash)
	if err != nil || !strings.Contains(first, "73") {
		t.Fatalf("saved usage changed: %v", err)
	}
	for i := 0; i < 3; i++ {
		next, err := store.ReportVersion(snap.Run.ID, hash)
		if err != nil || first != next {
			t.Fatalf("same hash changed bytes: %v", err)
		}
	}
	loaded.Run.UpdatedAt = loaded.Run.UpdatedAt.Add(time.Hour)
	loaded.Run.Title = "unrendered title"
	if childReportHash(&loaded) != hash {
		t.Fatal("unrendered metadata invalidated version")
	}
	if strings.Contains(first, "来源状态：available") || !strings.Contains(first, "not_checked") {
		t.Fatal("fixed references claimed live verification")
	}
}

func TestP0FixedReportSettlementSurvivesSQLiteMigration(t *testing.T) {
	a, store, id, w := settlementFixture(t)
	if err := settlementOperation(t, a, store, id, w, func(s *Snapshot) { s.Run.Status = StatusCompleted }); err != nil {
		t.Fatal(err)
	}
	reports, err := store.ChildReports(id)
	if err != nil || len(reports) != 1 || reports[0].SourceStatus != "available" {
		t.Fatalf("settlement receipt: %+v %v", reports, err)
	}
	before, err := store.ReportVersion(w.RunID, reports[0].SourceHash)
	if err != nil {
		t.Fatal(err)
	}
	if err = MigrateStoreToSQLite(store.root); err != nil {
		t.Fatal(err)
	}
	migrated, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	after, err := migrated.ReportVersion(w.RunID, reports[0].SourceHash)
	if err != nil || before != after {
		t.Fatalf("SQLite changed fixed settlement version: %v", err)
	}
	reloaded, err := migrated.ChildReports(id)
	if err != nil || len(reloaded) != 1 || reloaded[0].SourceStatus != "available" || reloaded[0].SourceHash != reports[0].SourceHash {
		t.Fatalf("SQLite receipt mismatch: %+v %v", reloaded, err)
	}
}

func TestP0ChildReportHashCoversCheckpointFallback(t *testing.T) {
	snap := Snapshot{Run: Run{ID: "child", Status: StatusPaused}, WorkerCheckpoint: &WorkerCheckpoint{Summary: "original checkpoint"}}
	hash := childReportHash(&snap)
	snap.WorkerCheckpoint.Summary = "changed checkpoint"
	if childReportHash(&snap) == hash {
		t.Fatal("receipt fallback summary changed without source digest")
	}
}

func TestP0FixedReportExternalChangesDoNotRewriteBody(t *testing.T) {
	a, store, source, _ := catalogFixture(t)
	chain, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "外源链路", Summary: "original external summary", Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	target, err := a.Create("fixed external reference")
	if err != nil {
		t.Fatal(err)
	}
	ref := EvidenceChainReference{SourceRunID: source.Run.ID, ChainID: chain.Chain.ID, ChainHash: chain.Chain.ContentHash}
	target, err = store.ReferenceEvidenceChain(target.Run.ID, ref)
	if err != nil {
		t.Fatal(err)
	}
	hash := childReportHash(&target)
	before, err := store.ReportVersion(target.Run.ID, hash)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(before, chain.Chain.Summary) || !strings.Contains(before, ref.ChainHash) || !strings.Contains(before, "not_checked") {
		t.Fatal("external reference expanded or claimed current provenance")
	}
	if _, err = store.Update(source.Run.ID, func(s *Snapshot) error {
		s.Evidence[0].Content += "tampered"
		s.EvidenceChains[0].Summary = "new live summary"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	after, err := store.ReportVersion(target.Run.ID, hash)
	if err != nil || before != after {
		t.Fatalf("external mutation rewrote fixed body: %v", err)
	}
	live, err := store.Report(target.Run.ID)
	if err != nil || !strings.Contains(live, "new live summary") || !strings.Contains(live, "invalid") {
		t.Fatalf("ordinary report skipped source revocation: %v", err)
	}
	current, err := store.Load(target.Run.ID)
	if err != nil || len(current.Evidence) != 0 || current.Result.Status == "success" {
		t.Fatal("external summary became local execution proof")
	}
}

func TestP0FixedReportChildSourcesRemainDetached(t *testing.T) {
	a, store, id, w := settlementFixture(t)
	if err := settlementOperation(t, a, store, id, w, func(s *Snapshot) { s.Run.Status = StatusPaused; s.Run.PauseReason = "checkpoint" }); err != nil {
		t.Fatal(err)
	}
	parent, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	hash := childReportHash(&parent)
	before, err := store.ReportVersion(id, hash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(w.RunID, func(s *Snapshot) error { s.Run.Goal += " changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := store.ReportVersion(id, hash)
	if err != nil || before != after {
		t.Fatalf("child live status rewrote fixed body: %v", err)
	}
	live, err := store.Report(id)
	if err != nil || !strings.Contains(live, "来源状态：changed") {
		t.Fatalf("live child status not rechecked: %v", err)
	}
}

func TestP0FixedReportSourceRevocationStillGatesSuccess(t *testing.T) {
	a, store, id, w := settlementFixture(t)
	if err := settlementOperation(t, a, store, id, w, func(s *Snapshot) { s.Run.Status = StatusCompleted }); err != nil {
		t.Fatal(err)
	}
	proof := stageScopeSnapshot(t, nil)
	parent, err := store.Update(id, func(s *Snapshot) error {
		s.RootContract = nil
		s.Run.Status, s.Run.SuccessContains = StatusCompleted, "true"
		s.Plan.Tasks = s.Plan.Tasks[:1]
		s.Requests, s.Responses, s.Actions, s.Evidence, s.Events = proof.Requests, proof.Responses, proof.Actions, proof.Evidence, proof.Events
		s.Run.EventSeq = proof.Run.EventSeq
		s.Messages = append(proof.Messages, Message{Role: "assistant", Content: "Observed accepted output."})
		return nil
	})
	if err != nil || parent.Result.Status != "success" {
		t.Fatalf("positive control failed: %+v %v", parent.Result, err)
	}
	hash := childReportHash(&parent)
	if _, err = store.ReportVersion(id, hash); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(w.RunID, func(s *Snapshot) error { s.Run.Status = StatusFailed; s.Run.Error = "source revoked"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReportVersion(id, hash); err == nil {
		t.Fatal("revoked source still exported old verified result")
	}
	parent, err = store.Load(id)
	if err != nil || parent.Result.Status == "success" || parent.Plan.Tasks[0].Status != "uncertain" {
		t.Fatalf("source revocation bypassed: %+v %v", parent.Result, err)
	}
}

func TestP0ManualToolSubmissionDoesNotProveCleanup(t *testing.T) {
	for _, checked := range []bool{false, true} {
		a, store := newTestAgent(t, MockProvider{})
		id := createTestRun(t, a)
		snap := stageScopeSnapshot(t, nil)
		snap.Run.ID, snap.Run.Status = id, StatusWaitingTool
		if checked {
			requirement := taskRequirement("already-observed")
			snap.Requirements = []VerificationRequirement{requirement}
			refs, err := evidenceReferences(&snap, []string{snap.Evidence[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			snap.Checks = []VerificationCheck{{RequirementID: requirement.ID, RequirementHash: valueHash(requirement), References: refs, EvidenceIDs: []string{snap.Evidence[0].ID}}}
		} else {
			snap.Run.SuccessContains = "true"
		}
		call := ToolCall{ID: "unresolved", Name: "exec", Arguments: "{}"}
		snap.Requests = append(snap.Requests, ModelRequest{ID: "unresolved-request"})
		snap.Responses = append(snap.Responses, ResponseRecord{RequestID: "unresolved-request", Response: ModelResponse{ToolCalls: []ToolCall{call}}})
		snap.Messages = append(snap.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{call}})
		snap.Actions = append(snap.Actions, Action{Adapter: "exec", RequestID: "unresolved-request", CallID: call.ID, Name: call.Name, Arguments: call.Arguments, Status: "uncertain"})
		if _, err := store.Update(id, func(s *Snapshot) error { *s = snap; return nil }); err != nil {
			t.Fatal(err)
		}
		// Exercise the same persistence path as SubmitTool without invoking a
		// provider; only the genuine cleanup receipt below may open the gate.
		manual, err := a.recordManualTool(id, call.ID, "operator observed exit")
		if err != nil || manual.Actions[1].Status != "resolved_manually" || len(pendingToolCalls(&manual)) != 0 {
			t.Fatalf("manual settlement: %v", err)
		}
		manual.Run.Status = StatusCompleted
		manual.Messages = append(manual.Messages, Message{Role: "assistant", Content: "Task complete."})
		refreshResult(&manual)
		if manual.Result.Status == "success" || !strings.Contains(manual.Result.TerminalReason, "cleanup") {
			t.Fatalf("manual result bypassed cleanup: %+v", manual.Result)
		}
		manual.Actions[1].ResourceCleanupStatus = "completed"
		refreshResult(&manual)
		if manual.Result.Status != "success" {
			t.Fatalf("cleanup receipt failed positive control: %+v", manual.Result)
		}
	}
}
