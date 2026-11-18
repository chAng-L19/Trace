package agent

import (
	"context"
	"strings"
	"testing"
)

func TestArchiveDeleteCreatedRunStaysHiddenAndStopped(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	if err := store.Delete(id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), id); err == nil || !strings.Contains(err.Error(), "deleted") {
		t.Fatalf("deleted run restarted: %v", err)
	}
	if _, err := a.QueueMessage(id, "follow_up", "restart"); err == nil {
		t.Fatal("deleted source accepted mutation")
	}
	if _, err := a.Message(context.Background(), id, "restart"); err == nil {
		t.Fatal("deleted source accepted a CLI message")
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Deleted = false; return nil }); err == nil {
		t.Fatal("generic update resurrected deleted source")
	}
	// A fresh store must preserve the marker after Load refreshes derived state.
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := reopened.Load(id)
	if err != nil || !snap.Run.Deleted || snap.Run.Status != StatusCreated {
		t.Fatalf("source unreadable or resumed: %+v %v", snap.Run, err)
	}
	for i := 0; i < 2; i++ {
		runs, err := reopened.List()
		if err != nil || len(runs) != 0 {
			t.Fatalf("deleted run returned to archive: %+v %v", runs, err)
		}
	}
}

func TestArchiveDeleteRetainsVerifiedExecutionEvidence(t *testing.T) {
	a, store := newTestAgent(t, resultProvider{})
	source, err := a.Run(context.Background(), createTestRun(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(source.Run.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(source.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Run.Deleted || loaded.Result.Status != "success" || len(loaded.Evidence) != 1 || loaded.Evidence[0].Verification != "verified" {
		t.Fatalf("archive deletion changed source proof: %+v", loaded.Result)
	}
	if !evidenceSupports(&loaded, []string{source.Evidence[0].ID}, "flag{verified}") {
		t.Fatal("archived source no longer supports the original evidence")
	}
}

func TestArchiveDeleteRejectsExecutionAndUnfinishedWorkers(t *testing.T) {
	for _, scenario := range []string{"run-lock", "running", "waiting_tool", "worker-running", "worker-cleanup", "child"} {
		t.Run(scenario, func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id := createTestRun(t, a)
			_, err := store.Update(id, func(s *Snapshot) error {
				switch scenario {
				case "running", "waiting_tool":
					s.Run.Status = Status(scenario)
				case "worker-running":
					s.Workers = []AgentWorker{{Status: "running", CleanupStatus: "completed"}}
				case "worker-cleanup":
					s.Workers = []AgentWorker{{Status: "completed", CleanupStatus: "unknown"}}
				case "child":
					s.Run.ParentTaskID = "task"
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "run-lock" {
				release, err := store.AcquireRunLock(id)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			if err := store.Delete(id); err == nil {
				t.Fatal("deleted a protected run")
			}
			snap, err := store.Load(id)
			if err != nil || snap.Run.Deleted {
				t.Fatalf("failed delete changed source: %v", err)
			}
		})
	}
}

func TestArchiveDeletePreservesBranchSourceAndArtifacts(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	input, err := store.PutInputAttachment("source.txt", strings.NewReader("source evidence bytes"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := a.Create("source", input)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := a.Branch(source.Run.ID, source.Messages[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.PutArtifact("stdout", []byte("retained CAS bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(source.Run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), branch.Run.ID); err != nil {
		t.Fatalf("branch lost original attachment provenance: %v", err)
	}
	message, err := a.ReadMessage(source.Run.ID, source.Messages[0].ID)
	if err != nil || len(message.Attachments) != 1 || message.Attachments[0].SourceRunID != source.Run.ID {
		t.Fatalf("deleted source message not readable: %+v %v", message, err)
	}
	page, err := store.ReadArtifact(artifact.Hash, 0, 100)
	if err != nil || page.Content != "retained CAS bytes" {
		t.Fatalf("shared CAS data lost: %+v %v", page, err)
	}
	runs, err := store.List()
	if err != nil || len(runs) != 1 || runs[0].ID != branch.Run.ID {
		t.Fatalf("source returned to archive: %+v %v", runs, err)
	}
}
