package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvidenceChainsQualifiedReuseAndNoNewProof(t *testing.T) {
	a, store, source, _ := catalogFixture(t)
	target, err := a.Create("reuse original path")
	if err != nil {
		t.Fatal(err)
	}
	e := source.Evidence[0]
	chain, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "读取→核对", Summary: "保留原始响应与验证路径", Steps: []EvidenceChainStep{{EvidenceID: e.ID, Label: "读取响应"}}})
	if err != nil || chain.Verification != "verified" || len(chain.Operations) != 1 || chain.Operations[0].Action == nil {
		t.Fatalf("chain: %+v %v", chain, err)
	}
	ref := EvidenceChainReference{SourceRunID: source.Run.ID, ChainID: chain.Chain.ID, ChainHash: chain.Chain.ContentHash}
	if _, err = store.ReferenceEvidenceChain(target.Run.ID, ref); err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReferenceEvidenceChain(target.Run.ID, ref); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Load(target.Run.ID)
	if err != nil || len(snap.EvidenceChainRefs) != 1 || len(snap.Evidence) != 0 || len(snap.Actions) != 0 || snap.Result.Status == "success" {
		t.Fatalf("reference created proof: %+v %v", snap, err)
	}
	if _, err = evidenceReferences(&snap, []string{chain.Chain.ID}); err == nil {
		t.Fatal("chain accepted as executed stdout")
	}
	if !strings.Contains(chainContext(&snap), source.Run.ID) {
		t.Fatal("reuse missing from model context")
	}
	if _, err = a.CheckEvidence(context.Background(), target.Run.ID, "absent", []string{chain.Chain.ID}); err == nil {
		t.Fatal("reference satisfied local requirement")
	}
	if _, err = store.DeleteEvidence(source.Run.ID, e.ID); err == nil {
		t.Fatal("same-run chain allowed source deletion")
	}
	if err = store.DeleteEvidenceChain(source.Run.ID, chain.Chain.ID); err == nil {
		t.Fatal("referenced chain removed")
	}
	report, err := store.Report(target.Run.ID)
	if err != nil || !strings.Contains(report, chain.Chain.Name) || !strings.Contains(report, ref.ChainHash) {
		t.Fatalf("report missing chain: %s %v", report, err)
	}
	if _, err = store.DeleteWithEvidence(source.Run.ID, true); err != nil {
		t.Fatal(err)
	}
	read, err := store.GetEvidenceChain(source.Run.ID, chain.Chain.ID)
	if err != nil || !read.RunDeleted || read.Verification != "verified" {
		t.Fatalf("soft deleted chain: %+v %v", read, err)
	}
	if err = store.UnreferenceEvidenceChain(target.Run.ID, ref); err != nil {
		t.Fatal(err)
	}
	if err = store.DeleteEvidenceChain(source.Run.ID, chain.Chain.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.GetEvidence(source.Run.ID, e.ID); err != nil {
		t.Fatal("removing chain deleted raw operation", err)
	}
}

func TestEvidenceChainsCrossRunIdentityAndInvalidation(t *testing.T) {
	a, store, source, _ := catalogFixture(t)
	target, err := a.Create("mixed source")
	if err != nil {
		t.Fatal(err)
	}
	e := source.Evidence[0]
	_, err = store.Update(target.Run.ID, func(s *Snapshot) error { s.Evidence = []Evidence{{ID: e.ID, Content: "different run"}}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	entry, err := store.CreateEvidenceChain(target.Run.ID, EvidenceChain{Name: "原始路径", Summary: "跨运行来源保留", Steps: []EvidenceChainStep{{RunID: source.Run.ID, EvidenceID: e.ID, ContentHash: e.ContentHash}}})
	if err != nil || entry.Operations[0].Evidence.Content != e.Content {
		t.Fatalf("run collision: %+v %v", entry, err)
	}
	if _, err = store.DeleteEvidence(source.Run.ID, e.ID); err == nil {
		t.Fatal("cross-run chain allowed deletion")
	}
	for _, mutate := range []func(*Snapshot){
		func(s *Snapshot) { s.Evidence[0].Content += "changed" },
		func(s *Snapshot) { s.Actions[0].Arguments += "changed" },
	} {
		original, _ := store.loadRawLocked(source.Run.ID)
		_, err = store.Update(source.Run.ID, func(s *Snapshot) error { mutate(s); return nil })
		if err != nil {
			t.Fatal(err)
		}
		read, err := store.GetEvidenceChain(target.Run.ID, entry.Chain.ID)
		if err != nil || read.Verification != "invalid" {
			t.Fatalf("tampered source accepted: %+v %v", read, err)
		}
		if err = store.saveLocked(original); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := json.Marshal(entry.Chain)
	_, err = store.Update(target.Run.ID, func(s *Snapshot) error { s.EvidenceChains[0].Summary = "changed"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	read, err := store.GetEvidenceChain(target.Run.ID, entry.Chain.ID)
	if err != nil || read.Verification != "invalid" {
		t.Fatal("chain hash not checked", err)
	}
	var original EvidenceChain
	if err = json.Unmarshal(before, &original); err != nil {
		t.Fatal(err)
	}
	if chainHash(original) != original.ContentHash {
		t.Fatal("stored hash mismatch")
	}
}

func TestEvidenceChainsRejectDuplicateMissingAndChangedSteps(t *testing.T) {
	_, store, source, _ := catalogFixture(t)
	e := source.Evidence[0]
	for _, steps := range [][]EvidenceChainStep{
		{{EvidenceID: "missing"}},
		{{EvidenceID: e.ID, ContentHash: "changed"}},
		{{EvidenceID: e.ID}, {EvidenceID: e.ID}},
	} {
		if _, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "chain", Summary: "purpose", Steps: steps}); err == nil {
			t.Fatal("invalid chain accepted", steps)
		}
	}
}

func TestEvidenceChainEmptyAncestryJSONRoundTrip(t *testing.T) {
	_, store, source, _ := catalogFixture(t)
	entry, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "browser chain", Summary: "empty parents", Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}, DerivedFrom: []EvidenceChainReference{}})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.GetEvidenceChain(source.Run.ID, entry.Chain.ID)
	if err != nil || loaded.Verification != "verified" || loaded.Chain.ContentHash != entry.Chain.ContentHash {
		t.Fatalf("empty ancestry changed after persistence: %+v %v", loaded, err)
	}
}

func TestEvidenceMethodEvolutionPreservesVariantsAndPinnedHistory(t *testing.T) {
	_, store, source, _ := catalogFixture(t)
	base, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "请求核对方法", Summary: "初次操作路径", Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	parent := EvidenceChainReference{SourceRunID: source.Run.ID, ChainID: base.Chain.ID, ChainHash: base.Chain.ContentHash}
	evolved := EvidenceChain{Name: base.Chain.Name, Summary: "吸收新观察，保留两种路径", Variant: "补充校验路径", Applicability: "存在完整返回时", Lesson: "检查原始返回，不依赖预览", DerivedFrom: []EvidenceChainReference{parent}, Steps: base.Chain.Steps}
	next, err := store.CreateEvidenceChain(source.Run.ID, evolved)
	if err != nil || next.Chain.MethodID != base.Chain.MethodID || next.Chain.ID == base.Chain.ID {
		t.Fatalf("evolution: %+v %v", next, err)
	}
	list, err := store.ListEvidenceChains()
	if err != nil || len(list) != 2 {
		t.Fatalf("variants lost: %+v %v", list, err)
	}
	old, err := store.GetEvidenceChain(source.Run.ID, base.Chain.ID)
	if err != nil || old.Chain.ContentHash != parent.ChainHash || old.Chain.Summary != base.Chain.Summary {
		t.Fatal("old version overwritten", err)
	}
	if err = store.DeleteEvidenceChain(source.Run.ID, base.Chain.ID); err == nil {
		t.Fatal("experience parent removed")
	}
	evolved.DerivedFrom[0].ChainHash = "changed"
	if _, err = store.CreateEvidenceChain(source.Run.ID, evolved); err == nil {
		t.Fatal("stale parent accepted")
	}
}

func methodTestRef(entry EvidenceChainEntry) EvidenceChainReference {
	return EvidenceChainReference{SourceRunID: entry.RunID, ChainID: entry.Chain.ID, ChainHash: entry.Chain.ContentHash}
}

// Each node owns a different executed leaf, so ancestry cannot accidentally be
// validated merely by checking the descendant's own operation list.
func methodTestChain(t *testing.T, a *Agent, store *Store, name string, parents ...EvidenceChainEntry) EvidenceChainEntry {
	t.Helper()
	snap, err := a.Run(context.Background(), createTestRun(t, a))
	if err != nil {
		t.Fatal(err)
	}
	// This helper isolates manually arranged ancestry and deletion dependencies.
	for _, c := range snap.EvidenceChains {
		if strings.HasPrefix(c.MethodID, "auto-") {
			if err := store.DeleteEvidenceChain(snap.Run.ID, c.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	chain := EvidenceChain{Name: name, Summary: name + " observations", Variant: name, Applicability: "fixture response available", Lesson: "retain exact observed response", Steps: []EvidenceChainStep{{EvidenceID: snap.Evidence[0].ID}}}
	for _, parent := range parents {
		chain.DerivedFrom = append(chain.DerivedFrom, methodTestRef(parent))
	}
	entry, err := store.CreateEvidenceChain(snap.Run.ID, chain)
	if err != nil || entry.Verification != "verified" {
		t.Fatalf("create %s: %+v %v", name, entry, err)
	}
	return entry
}

func methodTestRestore(t *testing.T, store *Store, snap Snapshot) {
	t.Helper()
	unlock, err := store.acquireStoreLock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err = store.saveLocked(snap); err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceMethodAncestryInvalidatesAcrossDistinctLeaves(t *testing.T) {
	a, store := newTestAgent(t, resultProvider{})
	base := methodTestChain(t, a, store, "ancestor")
	middle := methodTestChain(t, a, store, "middle", base)
	last := methodTestChain(t, a, store, "last", middle)
	original, err := store.Load(base.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Snapshot)
	}{
		{"leaf_content", func(s *Snapshot) { s.Evidence[0].Content += "tampered" }},
		{"leaf_missing", func(s *Snapshot) { s.Evidence = nil }},
		{"action_arguments", func(s *Snapshot) { s.Actions[0].Arguments += "tampered" }},
		{"chain_missing", func(s *Snapshot) { s.EvidenceChains = nil }},
		{"chain_metadata", func(s *Snapshot) { s.EvidenceChains[0].Lesson += "tampered" }},
		{"rehashed_parent", func(s *Snapshot) {
			s.EvidenceChains[0].Lesson += "changed version"
			s.EvidenceChains[0].ContentHash = chainHash(s.EvidenceChains[0])
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer methodTestRestore(t, store, original)
			if _, err := store.Update(base.RunID, func(s *Snapshot) error { tc.mutate(s); return nil }); err != nil {
				t.Fatal(err)
			}
			for _, descendant := range []EvidenceChainEntry{middle, last} {
				read, err := store.GetEvidenceChain(descendant.RunID, descendant.Chain.ID)
				if err != nil || read.Verification != "invalid" {
					t.Fatalf("ancestry not revoked: %+v %v", read, err)
				}
				if len(read.Operations) != 1 || read.Operations[0].Verification != "verified" {
					t.Fatal("test changed descendant leaf instead of ancestor")
				}
			}
		})
	}
}

func TestEvidenceMethodDiamondAndHiddenDescendantDeletion(t *testing.T) {
	a, store := newTestAgent(t, resultProvider{})
	base := methodTestChain(t, a, store, "shared ancestor")
	left := methodTestChain(t, a, store, "left variant", base)
	right := methodTestChain(t, a, store, "right variant", base)
	merged := methodTestChain(t, a, store, "merged method", left, right)
	if merged.Chain.MethodID != base.Chain.MethodID {
		t.Fatal("method identity lost")
	}
	original, err := store.Load(right.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(right.RunID, func(s *Snapshot) error { s.Evidence[0].Content += "tampered"; return nil }); err != nil {
		t.Fatal(err)
	}
	read, err := store.GetEvidenceChain(merged.RunID, merged.Chain.ID)
	if err != nil || read.Verification != "invalid" {
		t.Fatalf("healthy parent masked invalid parent: %+v %v", read, err)
	}
	methodTestRestore(t, store, original)
	read, err = store.GetEvidenceChain(merged.RunID, merged.Chain.ID)
	if err != nil || read.Verification != "verified" {
		t.Fatalf("diamond mistaken for cycle: %+v %v", read, err)
	}
	if _, err = store.DeleteWithEvidence(merged.RunID, true); err != nil {
		t.Fatal(err)
	}
	if err = store.DeleteEvidenceChain(left.RunID, left.Chain.ID); err == nil {
		t.Fatal("hidden descendant lost parent deletion protection")
	}
	if _, err = store.DeleteEvidence(base.RunID, base.Chain.Steps[0].EvidenceID); err == nil {
		t.Fatal("ancestor leaf deleted while bound")
	}
	if err = store.DeleteEvidenceChain(merged.RunID, merged.Chain.ID); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []EvidenceChainEntry{base, left, right} {
		if _, err = store.GetEvidenceChain(entry.RunID, entry.Chain.ID); err != nil {
			t.Fatal("deleting descendant removed ancestor", err)
		}
	}
	if _, err = store.GetEvidence(merged.RunID, merged.Chain.Steps[0].EvidenceID); err != nil {
		t.Fatal("deleting method removed original operation", err)
	}
}

func TestEvidenceMethodRejectsMissingOrUnpinnedParents(t *testing.T) {
	a, store := newTestAgent(t, resultProvider{})
	base := methodTestChain(t, a, store, "base")
	valid := methodTestRef(base)
	for _, tc := range []struct {
		name   string
		mutate func(*EvidenceChainReference)
	}{
		{"empty_hash", func(r *EvidenceChainReference) { r.ChainHash = "" }},
		{"wrong_hash", func(r *EvidenceChainReference) { r.ChainHash = "changed" }},
		{"missing_chain", func(r *EvidenceChainReference) { r.ChainID = "chain-missing" }},
		{"missing_run", func(r *EvidenceChainReference) { r.SourceRunID = "missing-run" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := valid
			tc.mutate(&ref)
			chain := EvidenceChain{Name: "derived", Summary: "purpose", Variant: "variant", Applicability: "condition", Lesson: "lesson", Steps: base.Chain.Steps, DerivedFrom: []EvidenceChainReference{ref}}
			if _, err := store.CreateEvidenceChain(base.RunID, chain); err == nil {
				t.Fatal("invalid ancestry accepted")
			}
			entries, err := store.ListEvidenceChains()
			if err != nil || len(entries) != 1 {
				t.Fatalf("rejected creation persisted a chain: %+v %v", entries, err)
			}
		})
	}
}

func TestEvidenceChainPurposeUpdatePreservesPinnedSource(t *testing.T) {
	a, store := newTestAgent(t, resultProvider{})
	base := methodTestChain(t, a, store, "source method")
	target, err := a.Create("reference without executing")
	if err != nil {
		t.Fatal(err)
	}
	ref := methodTestRef(base)
	ref.Purpose = "support first conclusion"
	if _, err = store.ReferenceEvidenceChain(target.Run.ID, ref); err != nil {
		t.Fatal(err)
	}
	ref.Purpose = "support revised conclusion"
	if _, err = store.ReferenceEvidenceChain(target.Run.ID, ref); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Load(target.Run.ID)
	if err != nil || len(snap.EvidenceChainRefs) != 1 || snap.EvidenceChainRefs[0] != ref {
		t.Fatalf("purpose update was not saved: %+v %v", snap.EvidenceChainRefs, err)
	}
	if len(snap.Actions) != 0 || len(snap.Evidence) != 0 || snap.Result.Status == "success" {
		t.Fatal("purpose update created execution proof")
	}
	report, err := store.Report(target.Run.ID)
	if err != nil || !strings.Contains(report, ref.Purpose) || strings.Contains(report, "support first conclusion") {
		t.Fatalf("report uses stale purpose: %s %v", report, err)
	}
	source, err := store.GetEvidenceChain(base.RunID, base.Chain.ID)
	if err != nil || source.Chain.ContentHash != base.Chain.ContentHash || source.Verification != "verified" {
		t.Fatal("purpose update changed source chain", err)
	}
	// Purpose is citation metadata, not part of the source identity for removal.
	ref.Purpose = ""
	if err = store.UnreferenceEvidenceChain(target.Run.ID, ref); err != nil {
		t.Fatal(err)
	}
}

type methodFailureProvider struct{}

func (methodFailureProvider) Complete(ctx context.Context, request ModelRequest) (ModelResponse, error) {
	response, err := (resultProvider{}).Complete(ctx, request)
	if len(response.ToolCalls) > 0 {
		response.ToolCalls[0].Arguments = `{"command":"exit 7"}`
	}
	return response, err
}

func TestEvidenceMethodFailedParentRemainsUnverifiedAcrossGenerations(t *testing.T) {
	a, store := newTestAgent(t, methodFailureProvider{})
	// The deliberately unsupported success claim must not end execution.
	limit := 2
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
	source, err := a.Run(context.Background(), createTestRun(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if len(source.Evidence) != 1 || source.Evidence[0].Verification != "unverified" || source.Actions[0].Result.ExitCode != 7 {
		t.Fatalf("fixture did not execute a failed operation: %+v", source)
	}
	parent, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "failed attempt", Summary: "preserve negative experience", Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}})
	if err != nil || parent.Verification != "unverified" {
		t.Fatalf("failed parent promoted: %+v %v", parent, err)
	}
	// Subsequent leaves really succeed, but they do not repair ancestor evidence.
	a.provider = resultProvider{}
	for _, name := range []string{"child", "grandchild"} {
		snap, err := a.Run(context.Background(), createTestRun(t, a))
		if err != nil {
			t.Fatal(err)
		}
		entry, err := store.CreateEvidenceChain(snap.Run.ID, EvidenceChain{Name: name, Summary: "learn from failed attempt", Variant: name, Applicability: "successful leaf available", Lesson: "retain failed ancestor", Steps: []EvidenceChainStep{{EvidenceID: snap.Evidence[0].ID}}, DerivedFrom: []EvidenceChainReference{methodTestRef(parent)}})
		if err != nil || entry.Verification != "unverified" || len(entry.Operations) != 1 || entry.Operations[0].Verification != "verified" {
			t.Fatalf("ancestor status lost: %+v %v", entry, err)
		}
		parent = entry
	}
	target, err := a.Create("reuse negative experience")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ReferenceEvidenceChain(target.Run.ID, methodTestRef(parent)); err != nil {
		t.Fatal("negative experience should remain referenceable", err)
	}
	snap, err := store.Load(target.Run.ID)
	if err != nil || snap.Result.Status == "success" || len(snap.Checks) != 0 {
		t.Fatal("unverified experience satisfied target", err)
	}
}

func TestEvidenceMethodAncestorCASMissingOrCorrupt(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			a, store, source, artifact := catalogFixture(t)
			base, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "CAS ancestor", Summary: "archived output", Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}})
			if err != nil || base.Verification != "verified" {
				t.Fatalf("base: %+v %v", base, err)
			}
			child := methodTestChain(t, a, store, "independent leaf", base)
			last := methodTestChain(t, a, store, "independent grandchild", child)
			path := filepath.Join(store.root, "artifacts", artifact.Hash)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "missing" {
				err = os.Remove(path)
			} else {
				corrupt := append([]byte(nil), original...)
				corrupt[0] ^= 1 // Same size must still fail content verification.
				err = os.WriteFile(path, corrupt, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, descendant := range []EvidenceChainEntry{child, last} {
				read, err := store.GetEvidenceChain(descendant.RunID, descendant.Chain.ID)
				if err != nil || read.Verification != "invalid" || read.Operations[0].Verification != "verified" {
					t.Fatalf("ancestor CAS failure not isolated/propagated: %+v %v", read, err)
				}
			}
			if _, err = store.PutArtifact("stdout", original); err != nil {
				t.Fatal(err)
			}
			read, err := store.GetEvidenceChain(last.RunID, last.Chain.ID)
			if err != nil || read.Verification != "verified" || read.Chain.ContentHash != last.Chain.ContentHash {
				t.Fatalf("restored bytes did not restore fixed provenance: %+v %v", read, err)
			}
		})
	}
}

func TestEvidenceChainBuiltinsReuseWithoutExecutionProof(t *testing.T) {
	a, store, source, _ := catalogFixture(t)
	target, err := a.Create("read and reuse a method")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetVerificationRequirements(target.Run.ID, []VerificationRequirement{{ID: "local-proof", Description: "requires fresh local output", Category: "goal", Predicate: EvidencePredicate{Kind: "contains", Value: "flag{verified}"}}}); err != nil {
		t.Fatal(err)
	}
	call := func(name string, args any, wantError bool) any {
		t.Helper()
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		value, err := a.evidenceChainBuiltin(target.Run.ID, ToolCall{Name: name, Arguments: string(raw)})
		if (err != nil) != wantError {
			t.Fatalf("%s: %v", name, err)
		}
		snap, loadErr := store.Load(target.Run.ID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if len(snap.Actions) != 0 || len(snap.Evidence) != 0 || len(snap.Checks) != 0 || snap.Result.Status == "success" || len(unmetRequirements(&snap)) != 1 {
			t.Fatalf("%s promoted organization to execution: %+v", name, snap)
		}
		if observationAction(Action{Adapter: "builtin", Name: name}) {
			t.Fatalf("%s treated as observation", name)
		}
		return value
	}
	base, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "searchable-source", Summary: "source evidence", Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	found := call("evidence_chain_search", map[string]any{"query": "searchable-source", "limit": 10}, false).(map[string]any)
	matches, ok := found["chains"].([]EvidenceChainSearchHit)
	if !ok || len(matches) != 1 || matches[0].SourceRunID != source.Run.ID {
		t.Fatalf("cross-run search failed: %+v", found)
	}
	readArgs := map[string]string{"run_id": source.Run.ID, "chain_id": base.Chain.ID, "chain_hash": base.Chain.ContentHash}
	read := call("evidence_chain_read", readArgs, false).(EvidenceChainEntry)
	if len(read.Operations) != 1 || read.Operations[0].RunID != source.Run.ID || read.Operations[0].Evidence.ContentHash != source.Evidence[0].ContentHash {
		t.Fatal("read lost source identity")
	}
	readArgs["chain_hash"] = "wrong-hash"
	call("evidence_chain_read", readArgs, true)
	created := call("evidence_chain_create", EvidenceChain{Name: "composed-source", Summary: "retained source", Variant: "reference-only", Applicability: "source available", Lesson: "no new execution", DerivedFrom: []EvidenceChainReference{methodTestRef(base)}, Steps: base.Chain.Steps}, false).(EvidenceChainEntry)
	if created.RunID != target.Run.ID || created.Chain.Steps[0].RunID != source.Run.ID {
		t.Fatal("creation rewrote leaf provenance")
	}
	ref := methodTestRef(base)
	ref.Purpose = "support comparison"
	call("evidence_chain_reference", ref, false)
	snap, err := store.Load(target.Run.ID)
	if err != nil || len(snap.EvidenceChains) != 1 || len(snap.EvidenceChainRefs) != 1 {
		t.Fatal("builtin changes were not persisted", err)
	}
	if _, ok := verifiedOutput(&snap, source.Evidence[0].ID); ok {
		t.Fatal("foreign bare ID became local output")
	}
}
