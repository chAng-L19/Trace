package agent

import (
	"context"
	"encoding/json"
	"testing"
)

// Short stdout is normally inline. Archive the exact bytes produced by the
// genuine exec before any checks/settlement, preserving its tool/args/predicate.
func delegatedArchiveStdout(t *testing.T, store *Store, original Snapshot) Snapshot {
	t.Helper()
	e := original.Evidence[len(original.Evidence)-1]
	output, ok := verifiedOutput(&original, e.ID)
	if !ok {
		t.Fatal("original exec stdout unavailable")
	}
	ref, err := store.PutArtifact("stdout", []byte(output))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Update(original.Run.ID, func(s *Snapshot) error {
		for i := range s.Actions {
			a := &s.Actions[i]
			if a.RequestID != e.RequestID || a.CallID != e.CallID {
				continue
			}
			a.Result.Stdout = ""
			a.Result.Artifacts = append(a.Result.Artifacts, ref)
			raw, err := json.Marshal(a.Result)
			if err != nil {
				return err
			}
			for j := range s.Evidence {
				if s.Evidence[j].ID == e.ID {
					s.Evidence[j].Content, s.Evidence[j].ContentHash = string(raw), promptHash(string(raw))
				}
			}
			for j := range s.Messages {
				if s.Messages[j].Role == "tool" && s.Messages[j].ToolCallID == e.CallID && s.Messages[j].Content == e.Content {
					s.Messages[j].Content = string(raw)
				}
			}
			if verifyEvidence(s, &s.Evidence[len(s.Evidence)-1]) != "verified" {
				t.Fatal("CAS fixture lost original ledger binding")
			}
			return nil
		}
		t.Fatal("original exec action unavailable")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Resume through production admission and lifecycle settlement; only the
// callback's business observations are fixture-controlled.
func delegatedResume(t *testing.T, f delegatedFixture, operation func(*Agent, context.Context, string) (Snapshot, error)) AgentWorker {
	t.Helper()
	unlock, err := f.store.AcquireRunLock(f.worker.RunID)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	child, err := f.store.Load(f.worker.RunID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, release, current, err := f.a.prepareWorkerResume(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if current.SourceEventFloor == nil || *current.SourceEventFloor < child.Run.EventSeq {
		t.Fatal("resume omitted its server-owned source watermark")
	}
	if _, err = f.a.runChildOperation(ctx, f.root, current, func(child *Agent, ctx context.Context, id string) (Snapshot, error) {
		if _, err := f.store.Update(id, func(s *Snapshot) error { s.Run.Status, s.Run.PauseReason = StatusRunning, ""; return nil }); err != nil {
			return Snapshot{}, err
		}
		return operation(child, ctx, id)
	}); err != nil {
		t.Fatal(err)
	}
	return current
}

func delegatedPauseForFollowup(t *testing.T, f delegatedFixture) {
	t.Helper()
	if _, err := f.store.Update(f.root, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Update(f.worker.RunID, func(s *Snapshot) error { s.Run.Status, s.Run.PauseReason = StatusPaused, "awaiting_input"; return nil }); err != nil {
		t.Fatal(err)
	}
}
