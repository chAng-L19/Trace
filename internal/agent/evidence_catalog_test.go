package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func catalogFixture(t *testing.T) (*Agent, *Store, Snapshot, ArtifactRef) {
	t.Helper()
	a, store := newTestAgent(t, resultProvider{})
	snap, err := a.Run(context.Background(), createTestRun(t, a))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.PutArtifact("stdout", []byte("flag{verified}"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err = store.Update(snap.Run.ID, func(s *Snapshot) error {
		e := &s.Evidence[0]
		old := e.Content
		for i := range s.Actions {
			if s.Actions[i].CallID == e.CallID && s.Actions[i].RequestID == e.RequestID {
				s.Actions[i].Result.Stdout = ""
				s.Actions[i].Result.Artifacts = []ArtifactRef{ref}
				raw, _ := json.Marshal(s.Actions[i].Result)
				e.Content, e.ContentHash = string(raw), promptHash(string(raw))
			}
		}
		for i := range s.Messages {
			if s.Messages[i].Role == "tool" && s.Messages[i].Content == old {
				s.Messages[i].Content = e.Content
			}
		}
		for i := range s.Requests {
			for j := range s.Requests[i].Messages {
				if s.Requests[i].Messages[j].Role == "tool" && s.Requests[i].Messages[j].Content == old {
					s.Requests[i].Messages[j].Content = e.Content
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Evidence[0].Verification != "verified" {
		t.Fatalf("invalid fixture: %+v", snap.Evidence)
	}
	// Catalog/deletion tests arrange their own organization dependencies.
	// U8 production auto-chains are exercised separately, not hidden in them.
	for _, chain := range snap.EvidenceChains {
		if strings.HasPrefix(chain.MethodID, "auto-") {
			if err := store.DeleteEvidenceChain(snap.Run.ID, chain.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	snap, err = store.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a, store, snap, ref
}

func TestEvidenceCatalogRetainedConversationAndSourceInvalidation(t *testing.T) {
	_, store, snap, ref := catalogFixture(t)
	if _, err := store.DeleteWithEvidence(snap.Run.ID, true); err != nil {
		t.Fatal(err)
	}
	entries, err := store.ListEvidence()
	if err != nil || len(entries) != 1 || !entries[0].RunDeleted || entries[0].Evidence.Verification != "verified" || entries[0].Action == nil {
		t.Fatalf("catalog: %+v %v", entries, err)
	}
	if err = os.Remove(filepath.Join(store.root, "artifacts", ref.Hash)); err != nil {
		t.Fatal(err)
	}
	entry, err := store.GetEvidence(snap.Run.ID, snap.Evidence[0].ID)
	if err != nil || entry.Evidence.Verification == "verified" {
		t.Fatalf("missing source remained verified: %+v %v", entry, err)
	}
	entries, err = store.ListEvidence()
	if err != nil || entries[0].Evidence.Verification == "verified" {
		t.Fatal("catalog trusted persisted verification", err)
	}
}

func TestEvidenceDeletionRemovesSourceAndExclusiveCAS(t *testing.T) {
	for _, conversation := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "conversation"}[conversation], func(t *testing.T) {
			_, store, snap, ref := catalogFixture(t)
			var result EvidenceDeleteResult
			var err error
			if conversation {
				result, err = store.DeleteWithEvidence(snap.Run.ID, false)
			} else {
				result, err = store.DeleteEvidence(snap.Run.ID, snap.Evidence[0].ID)
			}
			if err != nil || !result.Deleted || result.ArtifactsDeleted != 1 {
				t.Fatalf("delete: %+v %v", result, err)
			}
			if _, err := store.GetEvidence(snap.Run.ID, snap.Evidence[0].ID); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("deleted evidence readable", err)
			}
			if _, err := os.Stat(filepath.Join(store.root, "artifacts", ref.Hash)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("CAS not removed", err)
			}
			after, err := store.Load(snap.Run.ID)
			if err != nil || len(after.Evidence) != 0 || after.Result.Status == "success" || after.Run.Deleted != conversation {
				t.Fatalf("stale proof: %+v %v", after.Result, err)
			}
			for _, action := range after.Actions {
				if action.Result != nil {
					t.Fatal("source action result retained")
				}
			}
			for _, message := range after.Messages {
				if message.Role == "tool" && message.Content != `{"evidence_deleted":true}` {
					t.Fatal("raw tool output retained")
				}
			}
			if len(pendingToolCalls(&after)) != 0 {
				t.Fatal("deletion reopened a tool call")
			}
		})
	}
}

func TestEvidenceDeletionKeepsSharedInputCAS(t *testing.T) {
	_, store, snap, ref := catalogFixture(t)
	input, err := store.PutInputAttachment("original.txt", strings.NewReader("flag{verified}"))
	if err != nil || input.Hash != ref.Hash {
		t.Fatal("shared input fixture", err)
	}
	result, err := store.DeleteEvidence(snap.Run.ID, snap.Evidence[0].ID)
	if err != nil || result.ArtifactsDeleted != 0 || result.ArtifactsRetained != 1 {
		t.Fatalf("shared deletion: %+v %v", result, err)
	}
	f, err := store.OpenArtifact(ref.Hash)
	if err != nil {
		t.Fatal("input CAS lost", err)
	}
	f.Close()
}

func TestEvidenceDeleteMixedReferencesIsAtomic(t *testing.T) {
	a, store, snap, ref := catalogFixture(t)
	// An otherwise removable item precedes the referenced item in the batch.
	_, err := store.Update(snap.Run.ID, func(s *Snapshot) error {
		s.Evidence = append([]Evidence{{ID: "free", Content: "free", ContentHash: promptHash("free")}}, s.Evidence...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	other, err := a.Create("dependent run")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(other.Run.ID, func(s *Snapshot) error {
		s.Assets = []Asset{{ID: "linked", SourceRunID: snap.Run.ID, EvidenceIDs: []string{snap.Evidence[0].ID}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(store.root, snap.Run.ID+".json"))
	if _, err = store.DeleteWithEvidence(snap.Run.ID, false); err == nil || !strings.Contains(err.Error(), other.Run.ID) || !strings.Contains(err.Error(), "linked") {
		t.Fatalf("missing concrete dependency: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(store.root, snap.Run.ID+".json"))
	if string(before) != string(after) {
		t.Fatal("failed batch changed snapshot")
	}
	if _, err = os.Stat(filepath.Join(store.root, "artifacts", ref.Hash)); err != nil {
		t.Fatal("failed batch changed CAS", err)
	}
	if _, err = store.DeleteEvidence(snap.Run.ID, "free"); err != nil {
		t.Fatal("unrelated item should remain independently removable", err)
	}
}

func TestEvidenceDeletionRejectsActiveAndWorkerOwners(t *testing.T) {
	for _, scenario := range []string{"lock", "running", "worker"} {
		t.Run(scenario, func(t *testing.T) {
			_, store, snap, _ := catalogFixture(t)
			if scenario == "lock" {
				release, err := store.AcquireRunLock(snap.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			} else {
				_, err := store.Update(snap.Run.ID, func(s *Snapshot) error {
					if scenario == "running" {
						s.Run.Status = StatusRunning
					} else {
						s.Workers = []AgentWorker{{ID: "active", Status: "running", CleanupStatus: "unknown"}}
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.DeleteEvidence(snap.Run.ID, snap.Evidence[0].ID); err == nil {
				t.Fatal("deleted active source")
			}
			if _, err := store.GetEvidence(snap.Run.ID, snap.Evidence[0].ID); err != nil {
				t.Fatal("rejection removed source", err)
			}
		})
	}
}

func TestEvidenceDeletionRemovesStreamingOutputAndRevokesChecks(t *testing.T) {
	a, store, snap, _ := catalogFixture(t)
	e := snap.Evidence[0]
	_, err := store.Update(snap.Run.ID, func(s *Snapshot) error {
		s.Requirements = []VerificationRequirement{{ID: "goal", Description: "marker", Category: "goal", Predicate: EvidencePredicate{Kind: "contains", Value: "flag{verified}"}}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	checked, err := a.CheckEvidence(context.Background(), snap.Run.ID, "goal", []string{e.ID})
	if err != nil || len(checked.Checks) == 0 || checked.Checks[0].Status != "pass" {
		t.Fatalf("check fixture: %+v %v", checked.Checks, err)
	}
	_, err = a.SubmitFinding(snap.Run.ID, Finding{ID: "finding", Title: "source", Severity: "info", RootCause: "fixture", Impact: "observed", Remediation: "fixture", ReproductionSteps: []string{"probe"}, EvidenceIDs: []string{e.ID}, Predicate: EvidencePredicate{Kind: "contains", Value: "flag{verified}"}})
	if err != nil {
		t.Fatal(err)
	}
	snap, err = store.Update(snap.Run.ID, func(s *Snapshot) error {
		for i := range s.Events {
			if s.Events[i].Sequence >= e.SourceEventSequence {
				s.Events[i].Sequence++
			}
		}
		s.Evidence[0].SourceEventSequence++
		s.Run.EventSeq++
		s.Events = append(s.Events, Event{Sequence: e.SourceEventSequence, Type: "tool_output", Data: map[string]string{"call_id": e.CallID, "content": "raw stream"}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A new finding can create a fresh automatic chain after catalogFixture.
	// Preserve the reference guard before explicitly removing its organization.
	if _, err = store.DeleteEvidence(snap.Run.ID, e.ID); err == nil || !strings.Contains(err.Error(), "原始操作仍被") {
		t.Fatalf("automatic chain did not protect its source: %v", err)
	}
	if _, err = store.GetEvidence(snap.Run.ID, e.ID); err != nil {
		t.Fatal("reference rejection removed source", err)
	}
	for _, chain := range snap.EvidenceChains {
		if strings.HasPrefix(chain.MethodID, "auto-") {
			if err = store.DeleteEvidenceChain(snap.Run.ID, chain.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = store.DeleteEvidence(snap.Run.ID, e.ID); err != nil {
		t.Fatal(err)
	}
	after, err := store.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range after.Events {
		if event.Type == "tool_output" || event.Type == "tool_result" {
			t.Fatal("raw event retained")
		}
	}
	if after.Result.Status == "success" || after.Findings[0].SourceStatus == "verified" {
		t.Fatalf("derived proof not revoked: %+v %+v", after.Result, after.Findings)
	}
	if after.Checks[0].Status == "pass" {
		t.Fatal("deleted evidence still passes check")
	}
}

func TestEvidenceDeletionProtectsBranchAndSharedRunCAS(t *testing.T) {
	a, store, snap, ref := catalogFixture(t)
	branch, err := a.Branch(snap.Run.ID, snap.Messages[len(snap.Messages)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DeleteEvidence(snap.Run.ID, snap.Evidence[0].ID); err == nil || !strings.Contains(err.Error(), branch.Run.ID) {
		t.Fatalf("branch lineage not protected: %v", err)
	}
	if _, err = store.DeleteWithEvidence(snap.Run.ID, true); err != nil {
		t.Fatal("retention should allow branch", err)
	}
	f, err := store.OpenArtifact(ref.Hash)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestEvidenceDeletionSeparatesIdenticalReusedCallResults(t *testing.T) {
	_, store, snap, _ := catalogFixture(t)
	first := snap.Evidence[0]
	snap, err := store.Update(snap.Run.ID, func(s *Snapshot) error {
		action := s.Actions[0]
		action.RequestID = "repeat-request"
		s.Actions = append(s.Actions, action)
		s.Requests = append(s.Requests, ModelRequest{ID: action.RequestID})
		var response ModelResponse
		for _, record := range s.Responses {
			if record.RequestID == first.RequestID {
				response = record.Response
				break
			}
		}
		s.Responses = append(s.Responses, ResponseRecord{RequestID: action.RequestID, Response: response})
		s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: response.ToolCalls}, Message{Role: "tool", ToolCallID: first.CallID, Content: first.Content})
		addEvent(s, "tool_result", "identical second source", map[string]string{"call_id": first.CallID, "evidence_id": "repeat-evidence"})
		second := first
		second.ID, second.RequestID, second.SourceEventSequence = "repeat-evidence", action.RequestID, s.Run.EventSeq
		s.Evidence = append(s.Evidence, second)
		return nil
	})
	if err != nil || snap.Evidence[1].Verification != "verified" {
		t.Fatalf("repeat fixture: %+v %v", snap.Evidence, err)
	}
	secondMessageID := snap.Messages[len(snap.Messages)-1].ID
	_, err = store.Update(snap.Run.ID, func(s *Snapshot) error {
		s.Requests = append(s.Requests, ModelRequest{ID: "audit-copy", Messages: append([]Message(nil), s.Messages...)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.DeleteEvidence(snap.Run.ID, first.ID)
	if err != nil || result.ArtifactsRetained != 1 {
		t.Fatalf("delete: %+v %v", result, err)
	}
	after, err := store.Load(snap.Run.ID)
	if err != nil || len(after.Evidence) != 1 || after.Evidence[0].ID != "repeat-evidence" || after.Evidence[0].Verification != "verified" {
		t.Fatalf("other proof damaged: %+v %v", after.Evidence, err)
	}
	for _, messages := range [][]Message{after.Messages, after.Requests[len(after.Requests)-1].Messages} {
		found := false
		for _, message := range messages {
			if message.ID == secondMessageID {
				found = true
				if message.Content != first.Content {
					t.Fatal("identical other request result erased")
				}
			}
		}
		if !found {
			t.Fatal("second tool message missing")
		}
	}
}
