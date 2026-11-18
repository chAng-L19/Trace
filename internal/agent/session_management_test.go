package agent

import (
	"context"
	"strings"
	"testing"
)

func TestMessageOperationSurvivesLostACKEditCancelAndRestart(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	_, first, created, err := a.QueueMessageOperation(id, "steer", "op-1", "original")
	if err != nil || !created {
		t.Fatalf("queue: %+v %v", first, err)
	}
	otherStore, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	other, err := New(otherStore, successProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, duplicate, created, err := other.QueueMessageOperation(id, "steer", "op-1", "original")
	if err != nil || created || duplicate.ID != first.ID {
		t.Fatalf("ACK retry duplicated input: %+v %v", duplicate, err)
	}
	if _, _, _, err = other.QueueMessageOperation(id, "steer", "op-1", "different"); err == nil {
		t.Fatal("operation identity reused for different input")
	}
	edited := "edited"
	if _, err = store.EditQueuedMessage(id, first.ID, first.Revision, &edited, false); err != nil {
		t.Fatal(err)
	}
	if _, err = store.EditQueuedMessage(id, first.ID, first.Revision, &edited, false); err == nil {
		t.Fatal("stale edit accepted")
	}
	_, duplicate, created, err = other.QueueMessageOperation(id, "steer", "op-1", "original")
	if err != nil || created || duplicate.Content != edited {
		t.Fatalf("retry overwrote edited input: %+v %v", duplicate, err)
	}
	if _, err = store.EditQueuedMessage(id, first.ID, duplicate.Revision, nil, true); err != nil {
		t.Fatal(err)
	}
	_, duplicate, created, err = other.QueueMessageOperation(id, "steer", "op-1", "original")
	if err != nil || created || duplicate.CancelledAt.IsZero() {
		t.Fatalf("retry resurrected cancellation: %+v %v", duplicate, err)
	}
	_, second, _, err := a.QueueMessageOperation(id, "steer", "op-2", "second")
	if err != nil {
		t.Fatal(err)
	}
	_, third, _, err := a.QueueMessageOperation(id, "steer", "op-3", "third")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.OrderQueuedMessages(id, []string{second.ID, second.ID}); err == nil {
		t.Fatal("duplicate ordering accepted")
	}
	if _, err = store.OrderQueuedMessages(id, []string{third.ID, second.ID}); err != nil {
		t.Fatal(err)
	}
	snap, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Requests) != 1 {
		t.Fatalf("unexpected model requests: %d", len(snap.Requests))
	}
	messages, observations := splitContextObservations(id, snap.Requests[0].Messages)
	if len(observations) == 0 || len(messages) < 3 || messages[len(messages)-2].Content != "third" || messages[len(messages)-1].Content != "second" {
		t.Fatalf("queue order not delivered: %+v", messages)
	}
	for _, m := range snap.Messages {
		if m.Content == "original" || m.Content == "edited" {
			t.Fatal("cancelled message delivered")
		}
	}
	if _, err = store.EditQueuedMessage(id, second.ID, second.Revision, &edited, false); err == nil {
		t.Fatal("delivered input editable")
	}
	_, duplicate, created, err = other.QueueMessageOperation(id, "steer", "op-2", "second")
	if err != nil || created || duplicate.ConsumedAt.IsZero() {
		t.Fatalf("delivered retry lost receipt: %+v %v", duplicate, err)
	}
	final, _ := store.Load(id)
	if final.Run.Status != StatusCompleted || len(final.Requests) != 1 || len(final.Mailbox) != 3 {
		t.Fatalf("retry reactivated run: %+v", final.Run)
	}
}

func TestRunMetadataSearchAndAcceptanceRemainSeparate(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	proof := "proof"
	config := RunConfiguration{Title: "original title", Budget: &Budget{MaxModelRequests: 2}, SuccessContains: &proof, Requirements: []VerificationRequirement{{ID: "proof", Description: "exact proof", Category: "goal", Predicate: EvidencePredicate{Kind: "equals", Value: proof}}}}
	snap, err := a.CreateConfigured("immutable goal", config)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Run.BudgetConfigured || snap.Run.Budget.MaxModelRequests != 2 || snap.Run.SuccessContains != proof || len(snap.Requirements) != 1 {
		t.Fatalf("creation omitted contract: %+v", snap)
	}
	release, err := store.AcquireRunLock(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetAcceptanceContract(snap.Run.ID, nil, nil); err == nil {
		t.Error("active execution lock ignored")
	}
	release()
	snap, err = store.Update(snap.Run.ID, func(s *Snapshot) error {
		s.Requests = []ModelRequest{{ID: "request-1"}}
		s.Messages = []Message{{ID: "message-1", RequestID: "request-1", Role: "assistant", Content: strings.Repeat("前", 500) + "NEEDLE" + strings.Repeat("后", 500)}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetAcceptanceContract(snap.Run.ID, nil, nil); err == nil {
		t.Fatal("executed contract changed")
	}
	title, archived := "renamed", true
	changed, err := store.UpdateRunMetadata(snap.Run.ID, &title, &archived)
	if err != nil || changed.Run.Goal != "immutable goal" || changed.Run.Status != snap.Run.Status || changed.Run.SuccessContains != proof || len(changed.Requirements) != 1 {
		t.Fatalf("metadata changed execution: %+v %v", changed.Run, err)
	}
	results, err := store.SearchMessages("needle", false, 50)
	if err != nil || len(results.Matches) != 0 {
		t.Fatalf("archived run included: %+v %v", results, err)
	}
	results, err = store.SearchMessages("needle", true, 50)
	if err != nil || len(results.Matches) != 1 || results.Matches[0].MessageID != "message-1" || results.Matches[0].RequestID != "request-1" || !strings.Contains(results.Matches[0].Content, "NEEDLE") {
		t.Fatalf("body search/link identity failed: %+v %v", results, err)
	}
}

func TestInvalidCreationContractLeavesNoRun(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	for _, config := range []RunConfiguration{{Budget: &Budget{MaxActions: -1}}, {Requirements: []VerificationRequirement{{ID: "invalid"}}}} {
		if _, err := a.CreateConfigured("reject", config); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
	runs, err := store.List()
	if err != nil || len(runs) != 0 {
		t.Fatalf("invalid creation persisted an incomplete Run: %+v %v", runs, err)
	}
}
