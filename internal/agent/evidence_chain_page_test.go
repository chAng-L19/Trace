package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestU8ChainPagePinsReferencesAndKeepsScopedReads(t *testing.T) {
	a, store, source, _ := catalogFixture(t)
	base, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "fixed invoice version", Summary: "comparison", Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	target, err := a.Create("consumer")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(target.Run.ID, func(s *Snapshot) error {
		s.EvidenceChainRefs = []EvidenceChainReference{{SourceRunID: source.Run.ID, ChainID: base.Chain.ID, ChainHash: base.Chain.ContentHash, Purpose: "invoice comparison"}, {SourceRunID: "missing", ChainID: "absent", ChainHash: "pinned-old", Purpose: "historical context"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.root, "unrelated.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	page, err := store.EvidenceChainPage(target.Run.ID, true, "", "", 1)
	if err != nil || page.Total != 2 || len(page.Items) != 1 || page.Items[0].Verification != "verified" || page.Items[0].ExpectedHash != base.Chain.ContentHash || page.Items[0].Purpose != "invoice comparison" || len(page.Items[0].Operations) > 0 || len(page.Items[0].ReferencedBy) > 0 {
		t.Fatalf("scoped authority: %+v %v", page, err)
	}
	missing, err := store.EvidenceChainPage(target.Run.ID, true, "", page.NextBefore, 1)
	if err != nil || len(missing.Items) != 1 || missing.Items[0].Verification != "invalid" || missing.Items[0].ExpectedHash != "pinned-old" || missing.Items[0].HistoryReadable {
		t.Fatalf("missing pin lost: %+v %v", missing, err)
	}
	_, err = store.Update(source.Run.ID, func(s *Snapshot) error { s.EvidenceChains[0].Summary += "edited"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	page, err = store.EvidenceChainPage(target.Run.ID, true, "invoice", "", 2)
	if err != nil || len(page.Items) != 1 || page.Items[0].Verification != "invalid" {
		t.Fatal("metadata hint became authority", err)
	}
	if _, err := store.EvidenceChainPage("", false, "", "", 1); err == nil {
		t.Fatal("global read silently ignored unreadable snapshot")
	}
}
