package agent

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestPiRecoveryMissingArchiveDoesNotUseCurrentCatalog(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, name := range []string{"exec", "manual_external"} {
			t.Run(backend+"/"+name, func(t *testing.T) {
				p := &scriptedProvider{}
				a, store := newTestAgent(t, p)
				if backend == "sqlite" {
					store = sqliteTestStore(t)
					a.store = store
				}
				id := createTestRun(t, a)
				before, err := store.Update(id, func(s *Snapshot) error {
					s.Run.Status = StatusWaitingTool
					s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "pending", Name: name, Arguments: `{"command":"must-not-execute"}`}}})
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				recovery, pending, err := a.preparePiRecovery(id)
				if !errors.Is(err, errPiRecoveryArchiveMissing) || pending || len(recovery.Tools) != 0 {
					t.Fatalf("current catalog substituted for missing archive: %+v %v %v", recovery, pending, err)
				}
				after, loadErr := store.Load(id)
				if loadErr != nil || !reflect.DeepEqual(before, after) || len(p.requests) != 0 || len(after.Actions) != 0 || len(after.Evidence) != 0 {
					t.Fatalf("failed recovery changed history or executed: %v", loadErr)
				}
				// Explicit operator resolution remains available for legacy batches.
				resolved, err := a.recordManualTool(id, "pending", "operator inspected; no execution")
				if err != nil || len(pendingToolCalls(&resolved)) != 0 || len(resolved.Requests) != 0 || len(resolved.Actions) != 0 || len(resolved.Evidence) != 1 || resolved.Evidence[0].Verification == "verified" {
					t.Fatalf("legacy manual resolution lost or promoted provenance: %v", err)
				}
			})
		}
	}
}

func TestPiRecoveryMissingArchiveContinuesWithoutExecutingOldCall(t *testing.T) {
	p := &scriptedProvider{responses: []ModelResponse{{Content: "continue after diagnostic"}}}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Messages = append(s.Messages, Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "pending", Name: "exec", Arguments: `{"command":"must-not-execute"}`}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Run(context.Background(), id)
	if err != nil || snap.Run.Status != StatusCompleted || len(p.requests) != 1 || len(snap.Actions) != 0 || len(snap.Evidence) != 1 || len(pendingToolCalls(&snap)) != 0 || snap.Evidence[0].Type != "runtime_observation" || snap.Evidence[0].Verification != "unverified" || !strings.Contains(snap.Evidence[0].Content, "not executed") {
		t.Fatalf("missing schema crossed execution boundary: %+v %v", snap.Run, err)
	}
}

func TestPiRecoveryBlockedPersistenceErrorIsReturned(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	before, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	restore, failed := settlementRenameFailure(t, store, id)
	_, err = a.observeMissingPiArchive(id)
	restore()
	var rename *os.LinkError
	if !*failed || !errors.As(err, &rename) {
		t.Fatalf("recovery block hid persistence failure: %v", err)
	}
	after, err := store.Load(id)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed recovery block published state: %v", err)
	}
}

func TestPiRecoveryMissingArchiveManualReadyResolution(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, id, call := rawManualBatchFixture(t, backend, &Action{Adapter: "exec", Status: "result_ready", Result: &ToolResult{Stdout: "retained"}})
			before, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = a.preparePiRecovery(id)
			if !errors.Is(err, errPiRecoveryArchiveMissing) {
				t.Fatal("missing archive accepted", err)
			}
			snap, err := a.recordManualTool(id, call.ID, "operator observation")
			if err != nil || len(pendingToolCalls(&snap)) != 0 || len(snap.Actions) != 1 || snap.Actions[0].Status != "resolved_manually" || snap.Actions[0].FinishedAt.IsZero() {
				t.Fatalf("manual ready result stayed unsettled: %+v %v", snap.Actions, err)
			}
			original := before.Actions[0]
			original.Status, original.FinishedAt = snap.Actions[0].Status, snap.Actions[0].FinishedAt
			if !reflect.DeepEqual(original, snap.Actions[0]) || len(snap.Requests) != 0 || len(snap.Responses) != 0 || len(snap.Evidence) != 1 || snap.Evidence[0].Type != "manual_observation" || snap.Evidence[0].Verification != "unverified" {
				t.Fatal("manual resolution changed original result or promoted provenance")
			}
			if _, ready, err := readyPiResult(&snap, call.ID); err != nil || ready {
				t.Fatal("resolved manual result still offered to pi", err)
			}
		})
	}
}
