package agent

import (
	"context"
	"reflect"
	"testing"
)

func TestMessageResumesBudgetPauseWithoutResetOrReplay(t *testing.T) {
	for _, kind := range []string{"model", "action"} {
		t.Run(kind, func(t *testing.T) {
			calls := []ToolCall{silentExec("one")}
			limit := BudgetOverride{MaxModelRequests: intPointer(1)}
			if kind == "action" {
				calls = append(calls, silentExec("two"))
				limit = BudgetOverride{MaxActions: intPointer(1)}
			}
			p := manualProvider(calls...)
			a, _ := newTestAgent(t, p)
			a.ConfigureBudget(limit)
			id := createTestRun(t, a)
			before, err := a.Run(context.Background(), id)
			if err != nil || before.Run.Status != StatusPaused || len(before.Actions) != 1 {
				t.Fatalf("initial budget pause: %v %+v", err, before.Run)
			}
			first := "finish cleanup before reporting"
			snap, err := a.Message(context.Background(), id, first)
			if err != nil || snap.Run.Status != StatusPaused || len(p.requests) != 1 || len(snap.Actions) != 1 || snap.Run.Budget != before.Run.Budget {
				t.Fatalf("message bypassed exhausted budget: %v %+v", err, snap.Run)
			}
			if len(snap.Mailbox) != 1 || snap.Mailbox[0].Content != first || !snap.Mailbox[0].ConsumedAt.IsZero() {
				t.Fatal("paused instruction was not durably queued")
			}
			if kind == "action" {
				a.ConfigureBudget(BudgetOverride{MaxActions: intPointer(2)})
			} else {
				a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(2)})
			}
			second := "include the verified cleanup result"
			snap, err = a.Message(context.Background(), id, second)
			if err != nil || snap.Run.Status != StatusCompleted || snap.Run.PauseReason != "" || len(p.requests) != 2 || len(snap.Actions) != len(calls) {
				t.Fatalf("message did not resume: %v %+v", err, snap.Run)
			}
			if !reflect.DeepEqual(snap.Actions[0], before.Actions[0]) || snap.Run.ElapsedMillis < before.Run.ElapsedMillis || snap.Run.Usage.ModelRequests != 2 || snap.Run.Usage.ToolActions != len(calls) {
				t.Fatal("resume replayed the original action or reset cumulative usage")
			}
			lastTool := -1
			positions := map[string]int{}
			for i, message := range p.requests[1].Messages {
				if message.Role == "tool" {
					lastTool = i
				}
				if message.Role == "user" && (message.Content == first || message.Content == second) {
					if _, duplicate := positions[message.Content]; duplicate {
						t.Fatal("instruction delivered twice")
					}
					positions[message.Content] = i
				}
			}
			if len(positions) != 2 || positions[first] <= lastTool || positions[second] <= positions[first] {
				t.Fatal("resumed model request lost instructions or broke tool-result ordering")
			}
		})
	}
}

func TestMessageDoesNotResumeCancelledBudgetPause(t *testing.T) {
	p := manualProvider(silentExec("one"))
	a, store := newTestAgent(t, p)
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(1)})
	id := createTestRun(t, a)
	if snap, err := a.Run(context.Background(), id); err != nil || snap.Run.Status != StatusPaused {
		t.Fatalf("initial budget pause: %v %+v", err, snap.Run)
	}
	before, err := a.Cancel(id)
	if err != nil {
		t.Fatal(err)
	}
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(2)})
	if _, err := a.Message(context.Background(), id, "resume cancelled run"); err == nil {
		t.Fatal("cancelled run accepted a resume message")
	}
	after, err := store.Load(id)
	if err != nil || after.Run.Status != StatusCancelled || len(p.requests) != 1 || !reflect.DeepEqual(after.Messages, before.Messages) || len(after.Mailbox) != 0 || !reflect.DeepEqual(after.Actions, before.Actions) {
		t.Fatalf("cancelled run was changed: %v %+v", err, after.Run)
	}
}

func TestMessagePreservesPausedPendingToolGate(t *testing.T) {
	p := manualProvider(ToolCall{ID: "pending", Name: "external", Arguments: `{}`})
	a, _ := newTestAgent(t, p)
	id := createTestRun(t, a)
	waitTestRun(t, a, id)
	if _, err := a.Pause(id); err != nil {
		t.Fatal(err)
	}
	snap, err := a.Message(context.Background(), id, "continue after resolving the pending tool")
	if err != nil || snap.Run.Status != StatusWaitingTool || len(p.requests) != 1 || len(snap.Actions) != 0 || len(pendingToolCalls(&snap)) != 1 {
		t.Fatalf("message bypassed paused tool resolution: %v %+v", err, snap.Run)
	}
	if len(snap.Mailbox) != 1 || !snap.Mailbox[0].ConsumedAt.IsZero() || snap.Messages[len(snap.Messages)-1].Role != "assistant" {
		t.Fatal("message was inserted before its pending tool result")
	}
}
