package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestWorkerCheckpointSurvivesReopenAndActualCompaction(t *testing.T) {
	a, store, parent := contextFixture(t, "source-proof")
	// This fixture tests durable checkpoint compaction, not the evolving full
	// production prompt size. Persist the fixed prompt when creating the child.
	const prompt = "Checkpoint compaction fixture: preserve exact user constraints and source identities."
	a.workerPrompt = prompt
	child := contextChild(t, a, store, parent)
	if child.Prompt != prompt || child.Run.PromptHash != promptHash(prompt) {
		t.Fatal("child did not persist the fixture prompt identity")
	}
	if _, err := store.Update(child.Run.ID, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	handoff := &WorkerHandoff{Instance: "fixture:80", Findings: []string{"saved observation"}, NegativeResults: []string{"failed attempt"}, NextAction: "continue without repeating failure"}
	saved, err := a.YieldWorkerWithHandoff(child.Run.ID, "checkpoint", "saved independently of history", handoff)
	if err != nil {
		t.Fatal(err)
	}
	cpHash := workerContextHash(saved.WorkerCheckpoint)
	if _, err := store.Update(child.Run.ID, func(s *Snapshot) error {
		s.Run.Context = ContextConfig{WindowTokens: 24000, OutputReserve: 500, KeepRecentTokens: 1000, CompactionMode: "semantic"}
		s.Messages = []Message{{Role: "user", Content: s.Run.Goal}}
		for i := 0; i < 16; i++ {
			s.Messages = append(s.Messages, Message{Role: "assistant", Content: strings.Repeat("old answer ", 600)})
		}
		s.Messages = append(s.Messages, Message{Role: "user", Content: "latest-constraint"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := New(reopened, successProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	restarted.registry = &ToolRegistry{tools: map[string]RegisteredTool{}}
	limit := 10
	restarted.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
	queued, err := restarted.QueueMessage(child.Run.ID, "follow_up", "resume-long-context")
	if err != nil {
		t.Fatal(err)
	}
	original := append([]Message(nil), queued.Messages...)
	if EstimateContextTokens(original, nil) <= 23500 {
		t.Fatal("fixture history fits without compaction")
	}
	result, err := restarted.Run(context.Background(), child.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Compactions) == 0 || workerContextHash(result.WorkerCheckpoint) != cpHash {
		t.Fatalf("actual compaction/checkpoint: status=%s reason=%s requests=%d compactions=%d checkpoint_match=%v", result.Run.Status, result.Run.PauseReason, len(result.Requests), len(result.Compactions), workerContextHash(result.WorkerCheckpoint) == cpHash)
	}
	if len(result.Messages) < len(original) || messageHash(result.Messages[:len(original)]) != messageHash(original) {
		t.Fatal("semantic compaction rewrote the raw history")
	}
	if result.Prompt != prompt || result.Run.PromptHash != promptHash(prompt) || len(result.Requests) > 10 {
		t.Fatal("restart changed persisted prompt identity or exceeded model budget")
	}
	var request ModelRequest
	summaries := 0
	for _, candidate := range result.Requests {
		if candidate.Purpose == "context_summary" {
			summaries++
		}
		if candidate.Purpose == "context_summary" || candidate.CompactionID != "" {
			if EstimateContextTokens(candidate.Messages, candidate.Tools) > 23500 || candidate.ContextTokens > 23500 {
				t.Fatal("actual summary/resumed request exceeded the unchanged window")
			}
		}
		if request.ID == "" && candidate.Purpose != "context_summary" && candidate.CompactionID != "" {
			request = candidate
		}
	}
	if request.ID == "" || summaries == 0 {
		t.Fatal("no actual agent request after semantic summary")
	}
	constraints := map[string]bool{child.Run.Goal: false, "latest-constraint": false}
	for _, message := range request.Messages {
		if message.Role == "user" {
			if _, ok := constraints[message.Content]; ok {
				constraints[message.Content] = true
			}
		}
	}
	for constraint, present := range constraints {
		if !present {
			t.Fatalf("resumed request lost exact constraint %q", constraint)
		}
	}
	actual := contextFromRequest(t, request)
	if actual.Continuation == nil || actual.Continuation.Status != "available" || workerContextHash(actual.Continuation.Checkpoint) != cpHash || actual.Continuation.SourceRunID != child.Run.ID {
		t.Fatalf("reopened request lost exact continuation: %+v", actual.Continuation)
	}
	// worker_yield resumes an unfinished turn. The first compacted request
	// precedes follow_up consumption; finishPiTurn queues it for the next turn.
	receipt := result.Mailbox[len(result.Mailbox)-1]
	if receipt.ID != queued.Mailbox[len(queued.Mailbox)-1].ID || receipt.Pending() || receipt.ConsumedAt.IsZero() || receipt.MessageID != receipt.ID {
		t.Fatalf("follow-up was not durably consumed: first_request=%s receipt=%+v", request.ID, receipt)
	}
	var followupRequest ModelRequest
	for _, candidate := range result.Requests {
		if candidate.Purpose == "context_summary" || candidate.CreatedAt.Before(receipt.ConsumedAt) {
			continue
		}
		followupRequest = candidate
		break
	}
	if followupRequest.ID == "" || followupRequest.CompactionID == "" {
		t.Fatalf("no next compacted request after follow-up consumption: first_request=%s next_request=%s consumed_at=%s requests=%d status=%s reason=%s", request.ID, followupRequest.ID, receipt.ConsumedAt, len(result.Requests), result.Run.Status, result.Run.PauseReason)
	}
	followupPresent := false
	for _, message := range followupRequest.Messages {
		if message.Role == "user" && message.ID == receipt.ID && message.Content == "resume-long-context" {
			followupPresent = true
		}
	}
	if !followupPresent {
		t.Fatalf("next actual request lost consumed follow-up: request=%s receipt=%s consumed_at=%s", followupRequest.ID, receipt.ID, receipt.ConsumedAt)
	}
	followup := contextFromRequest(t, followupRequest)
	if followup.Continuation == nil || followup.Continuation.Status != "available" || workerContextHash(followup.Continuation.Checkpoint) != cpHash || followup.Continuation.SourceRunID != child.Run.ID {
		t.Fatalf("follow-up request lost exact checkpoint: request=%s continuation=%+v", followupRequest.ID, followup.Continuation)
	}
}

func TestWorkerCheckpointProjectionRejectsChangedInstanceOrDeletedIdentity(t *testing.T) {
	for _, mutation := range []string{"instance", "source_deleted", "parent_deleted", "receiver_deleted", "source_worker", "source_task", "receiver_worker"} {
		t.Run(mutation, func(t *testing.T) {
			a, store, parent, source, _ := checkpointSourceFixture(t)
			if _, err := a.ClaimTask(parent, "receiver", "receiver-worker", 30); err != nil {
				t.Fatal(err)
			}
			snap, err := store.Load(parent)
			if err != nil {
				t.Fatal(err)
			}
			task, _ := planTask(&snap, "receiver")
			manifest, err := a.newWorkerContext(&snap, *task)
			if err != nil {
				t.Fatal(err)
			}
			manifest.WorkerID = "receiver-worker"
			receiver := registerBudgetChild(t, a, parent, "receiver", "receiver-worker")
			child, err := store.Update(receiver, func(s *Snapshot) error { s.WorkerContext = manifest; return nil })
			if err != nil {
				t.Fatal(err)
			}
			original := workerContextHash(child.WorkerContext)
			before := contextFromRequest(t, ModelRequest{Messages: []Message{{Role: "user", Content: a.workerContextProjection(&child)}}})
			if len(before.Handoffs) != 1 || before.Handoffs[0].Status != "available" {
				t.Fatal("positive handoff projection missing")
			}
			target := source
			if mutation == "parent_deleted" {
				target = parent
			}
			if mutation == "receiver_deleted" {
				target = receiver
			}
			if strings.HasSuffix(mutation, "_worker") || mutation == "source_task" {
				target = parent
			}
			if _, err := store.Update(target, func(s *Snapshot) error {
				switch mutation {
				case "instance":
					s.WorkerCheckpoint.Instance = "replacement-instance:90"
				case "source_worker", "receiver_worker":
					for i := range s.Workers {
						if (mutation == "source_worker" && s.Workers[i].TaskID == "source") || (mutation == "receiver_worker" && s.Workers[i].TaskID == "receiver") {
							s.Workers[i].ID = "changed-worker"
						}
					}
				case "source_task":
					ref, _ := planTask(s, "source")
					ref.WorkerID = "changed-worker"
				default:
					s.Run.Deleted = true
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			child, err = store.Load(receiver)
			if err != nil {
				t.Fatal(err)
			}
			after := contextFromRequest(t, ModelRequest{Messages: []Message{{Role: "user", Content: a.workerContextProjection(&child)}}})
			ref := after.Handoffs[0]
			if ref.Status == "available" || ref.Checkpoint != nil || len(ref.Evidence) != 0 || workerContextHash(child.WorkerContext) != original {
				t.Fatalf("changed/deleted source reused as current fact: %+v", ref)
			}
		})
	}
}

func TestWorkerCheckpointChildCannotCompleteParentTask(t *testing.T) {
	a, store, parent, child, _ := checkpointSourceFixture(t)
	before, err := store.Load(parent)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.builtin(context.Background(), child, ToolCall{Name: "task_complete", Arguments: `{"task_id":"source","worker_id":"source-worker","status":"completed"}`})
	if err == nil || !strings.Contains(err.Error(), "complete parent task source") || !strings.Contains(err.Error(), "report to the parent") {
		t.Fatalf("wrong parent-task diagnostic: %v", err)
	}
	after, err := store.Load(parent)
	if err != nil || workerContextHash(before.Plan) != workerContextHash(after.Plan) {
		t.Fatalf("child changed parent plan: %v", err)
	}
}

func TestWorkerCheckpointPrerequisiteWaitPreservesJoinedFailure(t *testing.T) {
	wait := &HandoffNotReadyError{TaskID: "receiver", Sources: []string{"source"}}
	if !dispatchPrerequisiteWait(wait) || !dispatchPrerequisiteWait(errors.Join(wait, &ResourceBusyError{})) {
		t.Fatal("pure prerequisite wait was not skippable")
	}
	for _, err := range []error{nil, errors.New("cleanup failed"), errors.Join(wait, errors.New("settlement fsync failed")), errors.Join(errors.Join(wait, &TaskBlockedError{}), errors.New("cancel failed"))} {
		if dispatchPrerequisiteWait(err) {
			t.Fatalf("real cleanup failure hidden by wait classification: %v", err)
		}
	}
}

func TestWorkerCheckpointTaskToolsManualClaimsAndLocalIdentity(t *testing.T) {
	a, store, parent, child, _ := checkpointSourceFixture(t)
	if _, err := store.Update(child, func(s *Snapshot) error { s.Run.Status = StatusRunning; return nil }); err != nil {
		t.Fatal(err)
	}
	// Task IDs are Run-local: a child may own a local task with the same ID.
	if _, err := a.SetPlan(child, []PlanTask{{ID: "source", Goal: "local child work"}}); err != nil {
		t.Fatal(err)
	}
	call := func(name, worker string) error {
		args, _ := json.Marshal(map[string]any{"task_id": "source", "worker_id": worker, "lease_seconds": 30, "status": "completed"})
		_, err := a.builtin(context.Background(), child, ToolCall{Name: name, Arguments: string(args)})
		return err
	}
	for _, runID := range []string{parent, child} {
		if err := call("task_claim", runID); err == nil || !strings.Contains(err.Error(), "not a Run ID") {
			t.Fatalf("Run ID accepted as worker: %v", err)
		}
	}
	if err := call("task_claim", "manual-owner"); err != nil {
		t.Fatalf("manual claim blocked: %v", err)
	}
	if err := call("task_complete", "wrong-owner"); err == nil || !strings.Contains(err.Error(), "lease not held") {
		t.Fatalf("wrong manual owner accepted: %v", err)
	}
	if err := call("task_complete", "manual-owner"); err != nil {
		t.Fatalf("manual completion blocked: %v", err)
	}
}
