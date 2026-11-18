package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func u8Source(s *Snapshot, id string) {
	call := "call-" + id
	content := "observed document " + id
	addEvent(s, "tool_result", "observed", map[string]string{"call_id": call, "evidence_id": id})
	s.Evidence = append(s.Evidence, Evidence{ID: id, CallID: call, Content: content, ContentHash: promptHash(content), Type: "manual_observation", SourceEventSequence: s.Run.EventSeq})
	s.Messages = append(s.Messages, Message{Role: "tool", ToolCallID: call, Content: content})
}

func u8Claim(s *Snapshot, summary string, ids []string) {
	raw, _ := json.Marshal(map[string]any{"trace_result": Result{Status: "failed", Summary: summary, EvidenceIDs: ids}})
	s.Messages = append(s.Messages, Message{Role: "assistant", Content: string(raw)})
	s.Run.Status = StatusCompleted
}

func TestU8AutomaticChainVersionsNoNewProofAndDeletion(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, initial := u7Store(t, backend)
			s, err := store.Update(initial.Run.ID, func(s *Snapshot) error {
				s.Run.Goal = "Invoice parser document comparison"
				u8Source(s, "doc")
				u8Claim(s, "Observed a mismatch in the supplied invoice document", []string{"doc"})
				return nil
			})
			if err != nil || len(s.EvidenceChains) != 1 {
				t.Fatalf("auto chain: %+v %v", s.EvidenceChains, err)
			}
			original := s.EvidenceChains[0]
			if chainHash(original) != original.ContentHash || !strings.Contains(original.Name, s.Run.Goal) || strings.Contains(original.Name, "漏洞") || original.Steps[0].RunID != s.Run.ID {
				t.Fatalf("identity/name: %+v", original)
			}
			if len(s.Actions) != 0 || len(s.Checks) != 0 || s.Result.Status != "failed" {
				t.Fatal("organization created execution proof")
			}
			s, err = store.Update(s.Run.ID, func(s *Snapshot) error { addEvent(s, "telemetry", "no fact change", nil); return nil })
			if err != nil || len(s.EvidenceChains) != 1 {
				t.Fatal("telemetry created new chain", err)
			}
			s, err = store.Update(s.Run.ID, func(s *Snapshot) error {
				u8Source(s, "doc-2")
				u8Claim(s, "The second document confirms the same comparison boundary", []string{"doc", "doc-2"})
				return nil
			})
			if err != nil || len(s.EvidenceChains) != 2 || !reflect.DeepEqual(s.EvidenceChains[0], original) {
				t.Fatal("new fact overwrote history", err)
			}
			// Source integrity and conclusion freshness remain separate.
			entry, err := store.GetEvidenceChain(s.Run.ID, original.ID)
			if err != nil || entry.Verification != "unverified" || !entry.HistoryReadable {
				t.Fatalf("source meaning: %+v %v", entry, err)
			}
			latest := s.EvidenceChains[1]
			if err := store.DeleteEvidenceChain(s.Run.ID, latest.ID); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			s, err = reopened.Update(s.Run.ID, func(*Snapshot) error { return nil })
			if err != nil || len(s.EvidenceChains) != 1 {
				t.Fatalf("deleted auto chain resurrected: %d %v", len(s.EvidenceChains), err)
			}
			_, err = reopened.Update(s.Run.ID, func(s *Snapshot) error { s.Evidence[0].Content += " changed"; return nil })
			if err != nil {
				t.Fatal(err)
			}
			entry, err = reopened.GetEvidenceChain(s.Run.ID, original.ID)
			if err != nil || entry.Verification != "invalid" || entry.SourceReason == "" || entry.Chain.Steps[0].ContentHash != original.Steps[0].ContentHash {
				t.Fatalf("changed source silently replaced: %+v %v", entry, err)
			}
		})
	}
}

func TestU8AutomaticChainBoundsAndPinnedMissingSources(t *testing.T) {
	s := Snapshot{Run: Run{ID: "bounded", Goal: "document review", Status: StatusCompleted}}
	ids := []string{}
	for i := 0; i < 70; i++ {
		id := fmt.Sprintf("e-%02d", i)
		u8Source(&s, id)
		ids = append(ids, id)
	}
	s.Result = Result{Status: "incomplete", Summary: "Comparison observations", EvidenceIDs: ids}
	archiveConclusionChains(&s)
	if len(s.EvidenceChains) != 1 || len(s.EvidenceChains[0].Steps) != 64 || !strings.Contains(s.EvidenceChains[0].Summary, "64/70") {
		t.Fatalf("bound hidden: %+v", s.EvidenceChains)
	}
	first := s.EvidenceChains[0]
	// Rewriting a source never rebinds the unchanged conclusion.
	s.Evidence[69].Content += "v2"
	s.Evidence[69].ContentHash = promptHash(s.Evidence[69].Content)
	s.Messages[69].Content = s.Evidence[69].Content
	archiveConclusionChains(&s)
	if len(s.EvidenceChains) != 1 || !reflect.DeepEqual(first, s.EvidenceChains[0]) {
		t.Fatal("old conclusion silently rebound to rewritten source")
	}
	s.Result.Summary += " with a new explicit conclusion"
	archiveConclusionChains(&s)
	if len(s.EvidenceChains) != 2 {
		t.Fatal("new conclusion did not create a version")
	}
	s.Result = Result{}
	s.WorkerCheckpoint = &WorkerCheckpoint{WorkerHandoff: WorkerHandoff{Progress: "fixed checkpoint", EvidenceIDs: []string{ids[0]}, EvidenceHashes: map[string]string{ids[0]: "old-hash"}}}
	archiveConclusionChains(&s)
	if len(s.EvidenceChains) != 2 {
		t.Fatal("stale checkpoint source replaced")
	}
	s.WorkerCheckpoint.EvidenceIDs = []string{"missing"}
	archiveConclusionChains(&s)
	if len(s.EvidenceChains) != 2 {
		t.Fatal("missing source became proof")
	}
	// Original Name/Summary stay in the hash contract.
	first.Name += " new title"
	if chainHash(first) == first.ContentHash {
		t.Fatal("name excluded from immutable version")
	}
}

func TestU8SourceFreshnessProjectionKeepsVerificationSeparate(t *testing.T) {
	_, store, s, _ := catalogFixture(t)
	_, err := store.Update(s.Run.ID, func(s *Snapshot) error {
		for i := 0; i < 2; i++ {
			c := EvidenceChain{Name: fmt.Sprintf("fact %d", i), Summary: "source", MethodID: "auto-test", CreatedAt: time.Unix(int64(i+1), 0), Steps: []EvidenceChainStep{{RunID: s.Run.ID, EvidenceID: s.Evidence[0].ID, ContentHash: s.Evidence[0].ContentHash}}}
			c.ContentHash = chainHash(c)
			c.ID = "chain-" + c.ContentHash
			s.EvidenceChains = append(s.EvidenceChains, c)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err = store.Load(s.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := store.GetEvidenceChain(s.Run.ID, s.EvidenceChains[0].ID)
	if err != nil || entry.Verification != "verified" || entry.SourceStatus != "superseded" {
		t.Fatalf("freshness promoted/demoted integrity: %+v %v", entry, err)
	}
}
