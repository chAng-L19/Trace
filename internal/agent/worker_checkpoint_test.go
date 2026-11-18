package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func checkpointSourceFixture(t *testing.T) (*Agent, *Store, string, string, *dispatchFailureProvider) {
	t.Helper()
	p := &dispatchFailureProvider{}
	a, store := newTestAgent(t, p)
	parent := createTestRun(t, a)
	if _, err := a.SetPlan(parent, []PlanTask{{ID: "source", Goal: "saved source"}, {ID: "receiver", Goal: "inherit source", HandoffTaskIDs: []string{"source"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ClaimTask(parent, "source", "source-worker", 30); err != nil {
		t.Fatal(err)
	}
	source := registerBudgetChild(t, a, parent, "source", "source-worker")
	if _, err := a.YieldWorker(source, "handoff", "continue from saved source"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.StopWorkers(ctx); err != nil {
			t.Error(err)
		}
	})
	return a, store, parent, source, p
}

func TestWorkerCheckpointLegacyEvidencePinRejectsVerifiedReplacement(t *testing.T) {
	_, store, id := contextFixture(t, "original-checkpoint-output")
	source, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	ev := &source.Evidence[0]
	raw, err := json.Marshal(WorkerHandoff{NextAction: "continue", EvidenceIDs: []string{ev.ID}})
	if err != nil {
		t.Fatal(err)
	}
	addEvent(&source, "worker_yielded", "legacy fixture", map[string]string{"reason": "handoff", "checkpoint": string(raw)})
	pinned := projectCheckpoint(&source, "source", "source-worker", "")
	if pinned.Status != "available" || len(pinned.Evidence) != 1 || pinned.Evidence[0].ContentHash == "" || len(pinned.Checkpoint.EvidenceHashes) != 0 {
		t.Fatalf("legacy positive control: %+v", pinned)
	}
	pinnedHash := workerContextHash(pinned)
	// Keep all provenance counterparts coherent: the negative control must
	// depend on the dispatch pin, not the existing corrupt-ledger guard.
	oldContent := ev.Content
	changed := false
	for i := range source.Actions {
		action := &source.Actions[i]
		if action.RequestID == ev.RequestID && action.CallID == ev.CallID && action.Result != nil {
			action.Result.Stdout = "replacement-checkpoint-output"
			data, marshalErr := json.Marshal(action.Result)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			ev.Content, ev.ContentHash = string(data), promptHash(string(data))
			changed = true
		}
	}
	for i := range source.Messages {
		message := &source.Messages[i]
		if message.Role == "tool" && message.ToolCallID == ev.CallID && message.Content == oldContent {
			message.Content = ev.Content
		}
	}
	if !changed || verifyEvidence(&source, ev) != "verified" || ev.ContentHash == pinned.Evidence[0].ContentHash {
		t.Fatal("replacement must remain independently verified with a different content hash")
	}
	fresh := projectCheckpoint(&source, "source", "source-worker", pinned.CheckpointHash)
	if fresh.Status != "available" || fresh.Evidence[0].ContentHash != ev.ContentHash {
		t.Fatalf("fresh legacy projection positive control failed: %+v", fresh)
	}
	checked := reprojectCheckpoint(&source, pinned)
	if checked.Status != "changed" || checked.Checkpoint != nil || len(checked.Evidence) != 0 || workerContextHash(pinned) != pinnedHash {
		t.Fatalf("legacy dispatch pin drifted or leaked replacement: %+v", checked)
	}
}

func TestWorkerCheckpointRegistrationRechecksFixedManifest(t *testing.T) {
	for _, mutation := range []string{"checkpoint", "ancestry"} {
		t.Run(mutation, func(t *testing.T) {
			a, store, parent, source, p := checkpointSourceFixture(t)
			var childID string
			store.mu.Lock()
			store.beforePersist = func(got string) {
				if got == parent || got == source || childID != "" {
					return
				}
				childID = got // Manifest already captured; parent not registered.
				hook := store.beforePersist
				store.beforePersist = nil
				defer func() { store.beforePersist = hook }()
				snap, err := store.loadRawLocked(source)
				if err != nil {
					panic(err)
				}
				if mutation == "checkpoint" {
					snap.WorkerCheckpoint.NextAction = "new source checkpoint after manifest"
				} else {
					snap.Run.ParentTaskID = "different-task"
				}
				if err := store.saveLocked(snap); err != nil {
					panic(err)
				}
			}
			store.mu.Unlock()
			_, err := a.DispatchTask(context.Background(), parent, "receiver")
			store.mu.Lock()
			store.beforePersist = nil
			store.mu.Unlock()
			var notReady *HandoffNotReadyError
			rejected := errors.As(err, &notReady)
			if mutation == "ancestry" {
				// Tree admission now rejects the changed source identity before
				// registration reaches the fixed handoff-manifest recheck.
				rejected = err != nil && err.Error() == "worker tree identity mismatch: "+source
			}
			if childID == "" || !rejected {
				t.Fatalf("registration accepted stale manifest: child=%s err=%v", childID, err)
			}
			snap, err := store.Load(parent)
			if err != nil {
				t.Fatal(err)
			}
			task, err := planTask(&snap, "receiver")
			if err != nil || task.Status != "failed" || task.ChildRunID != "" || !task.LeaseUntil.IsZero() || len(snap.Workers) != 1 || occupiedWorkerSlots(&snap) != 0 {
				t.Fatalf("rejected dispatch remained registered or claimed: %+v %v", task, err)
			}
			for _, event := range snap.Events {
				if event.Type == "worker_started" {
					t.Fatal("stale manifest published worker_started")
				}
			}
			child, err := store.Load(childID)
			if err != nil || child.Run.Status != StatusCancelled || len(child.Requests) != 0 || len(child.Actions) != 0 || a.HasActiveWorkers() || p.calls.Load() != 0 {
				t.Fatalf("stale child executed or remained active: %+v calls=%d err=%v", child.Run, p.calls.Load(), err)
			}
		})
	}
}

func TestWorkerCheckpointRetryRechecksHandoffDespiteStartedContract(t *testing.T) {
	a, store, parent, source, p := checkpointSourceFixture(t)
	if _, err := a.ClaimTask(parent, "receiver", "failed-claim", 30); err != nil {
		t.Fatal(err)
	}
	if err := a.finishWorkerFailure(parent, "receiver", "failed-claim", errors.New("zero-action dispatch failure")); err != nil {
		t.Fatal(err)
	}
	retried, err := a.RetryTask(parent, "receiver")
	if err != nil {
		t.Fatal(err)
	}
	task, err := planTask(&retried, "receiver")
	if err != nil || !task.Started || task.Status != "pending" || !taskStarted(&retried, *task) {
		t.Fatalf("retry fixture lost the started immutable contract: %+v %v", task, err)
	}
	before := workerContextHash(task)
	if _, err := store.Update(source, func(s *Snapshot) error {
		s.WorkerCheckpoint = nil
		events := s.Events[:0]
		for _, event := range s.Events {
			if event.Type != "worker_yielded" {
				events = append(events, event)
			}
		}
		s.Events = events
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err = a.ClaimTask(parent, "receiver", "retry-claim", 30)
	var notReady *HandoffNotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("retried direct claim bypassed handoff prerequisite: %v", err)
	}
	after, err := store.Load(parent)
	if err != nil {
		t.Fatal(err)
	}
	task, err = planTask(&after, "receiver")
	if err != nil || workerContextHash(task) != before || p.calls.Load() != 0 {
		t.Fatalf("rejected retry changed task or invoked model: %+v %v", task, err)
	}
}

func TestWorkerCheckpointRebuildsHashesBeforeFinalSizeLimit(t *testing.T) {
	_, store, id := contextFixture(t, "checkpoint-size-proof")
	source, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	ev := source.Evidence[0]
	newCheckpoint := func() *WorkerCheckpoint {
		return &WorkerCheckpoint{WorkerHandoff: WorkerHandoff{NextAction: "continue", Progress: "x", EvidenceIDs: []string{ev.ID}}, Reason: "yield", Summary: "bounded fixture"}
	}
	cp := newCheckpoint()
	cp.EvidenceHashes = map[string]string{ev.ID: "forged", "unused": "caller-controlled"}
	if err := validateCheckpoint(&source, cp); err != nil || len(cp.EvidenceHashes) != 1 || cp.EvidenceHashes[ev.ID] != ev.ContentHash {
		t.Fatalf("server hashes retained caller fields: %+v %v", cp.EvidenceHashes, err)
	}
	cp = newCheckpoint()
	raw, _ := json.Marshal(cp)
	cp.Progress = strings.Repeat("x", 1+16384-len(raw))
	raw, _ = json.Marshal(cp)
	if len(raw) != 16384 {
		t.Fatalf("pre-hash size fixture=%d", len(raw))
	}
	if err := validateCheckpoint(&source, cp); err == nil || !strings.Contains(err.Error(), "16 KiB") {
		t.Fatalf("post-hash oversized checkpoint accepted: %v", err)
	}
	cp = newCheckpoint()
	cp.EvidenceHashes = map[string]string{ev.ID: ev.ContentHash}
	raw, _ = json.Marshal(cp)
	cp.Progress = strings.Repeat("x", 1+16384-len(raw))
	if err := validateCheckpoint(&source, cp); err != nil {
		t.Fatalf("exact final 16 KiB positive control rejected: %v", err)
	}
	raw, _ = json.Marshal(cp)
	if len(raw) != 16384 {
		t.Fatalf("final bound not exact: %d", len(raw))
	}
	cp = newCheckpoint()
	cp.EvidenceIDs = nil
	cp.EvidenceHashes = map[string]string{"unused": strings.Repeat("x", 20000)}
	if err := validateCheckpoint(&source, cp); err != nil || len(cp.EvidenceHashes) != 0 {
		t.Fatalf("unused caller hash map was not discarded: %v", err)
	}
}

func TestWorkerCheckpointCorruptSourceIsNotNotReady(t *testing.T) {
	for _, entry := range []string{"claim", "context", "frontier"} {
		t.Run(entry, func(t *testing.T) {
			a, store, parent, source, p := checkpointSourceFixture(t)
			snap, err := store.Load(parent)
			if err != nil {
				t.Fatal(err)
			}
			task, err := planTask(&snap, "receiver")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(store.root, source+".json"), []byte("{broken"), 0600); err != nil {
				t.Fatal(err)
			}
			switch entry {
			case "claim":
				_, err = a.ClaimTask(parent, task.ID, "new-worker", 30)
			case "context":
				_, err = a.newWorkerContext(&snap, *task)
			case "frontier":
				_, err = a.DispatchFrontier(context.Background(), parent)
			}
			var notReady *HandoffNotReadyError
			var syntax *json.SyntaxError
			if err == nil || !errors.As(err, &syntax) || errors.As(err, &notReady) || p.calls.Load() != 0 || a.HasActiveWorkers() {
				t.Fatalf("corrupt source swallowed as ordinary handoff wait: %v", err)
			}
		})
	}
}
