package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEvidenceListsRunScopeRetainsHiddenReferences(t *testing.T) {
	a, store, source, _ := catalogFixture(t)
	target, err := a.Create("scoped reader")
	if err != nil {
		t.Fatal(err)
	}
	chain, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "path", Summary: "source retained", Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	ref := EvidenceChainReference{SourceRunID: source.Run.ID, ChainID: chain.Chain.ID, ChainHash: chain.Chain.ContentHash}
	if _, err = store.ReferenceEvidenceChain(target.Run.ID, ref); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DeleteWithEvidence(source.Run.ID, true); err != nil {
		t.Fatal(err)
	}
	// An unrelated damaged Run must not be scanned by a task-scoped read.
	if err = os.WriteFile(filepath.Join(store.root, "unrelated.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := store.ListRunEvidenceChains(target.Run.ID)
	if err != nil || len(entries) != 1 || !entries[0].RunDeleted || entries[0].Verification != "verified" || len(entries[0].Operations) != 0 {
		t.Fatalf("scoped chains: %+v %v", entries, err)
	}
	rows, err := store.ListRunEvidence(source.Run.ID)
	if err != nil || len(rows) != 1 || !rows[0].RunDeleted {
		t.Fatalf("scoped evidence: %+v %v", rows, err)
	}
	if _, err = store.ListEvidenceChains(); err == nil {
		t.Fatal("global list hid damaged Run")
	}
	if _, err = store.ListRunEvidence(""); err == nil {
		t.Fatal("empty scope became global scan")
	}
	if _, err = store.ListRunEvidenceChains("../escape"); err == nil {
		t.Fatal("invalid scope accepted")
	}
}
