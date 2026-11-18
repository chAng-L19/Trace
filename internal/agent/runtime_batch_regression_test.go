package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestLeaseRenewalDoesNotRepeatClaimAndDefaultCapacityFive(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	tasks := []PlanTask{}
	for i := 0; i < 6; i++ {
		tasks = append(tasks, PlanTask{ID: fmt.Sprint(i), Goal: "independent"})
	}
	if _, err := a.SetPlan(id, tasks); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := a.ClaimTask(id, fmt.Sprint(i), fmt.Sprint(i), 30); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.ClaimTask(id, "5", "5", 30); err == nil {
		t.Fatal("sixth worker admitted")
	}
	before, _ := store.Load(id)
	after, err := a.ClaimTask(id, "0", "0", 60)
	if err != nil || after.Run.EventSeq != before.Run.EventSeq || !after.Plan.Tasks[0].LeaseUntil.After(before.Plan.Tasks[0].LeaseUntil) {
		t.Fatalf("renewal duplicated claim or failed: %v", err)
	}
}

func TestWaitWorkersWakesForAnyWorkerOrDurableInput(t *testing.T) {
	for _, signal := range []string{"worker", "message", "mailbox"} {
		t.Run(signal, func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id := createTestRun(t, a)
			fast, slow := createTestRun(t, a), createTestRun(t, a)
			for _, childID := range []string{fast, slow} {
				release, err := store.AcquireRunLock(childID)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			_, err := store.Update(id, func(s *Snapshot) error {
				s.Workers = []AgentWorker{{ID: "fast", TaskID: "fast", RunID: fast, Status: "running"}, {ID: "slow", TaskID: "slow", RunID: slow, Status: "running"}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			go func() { _, err := a.WaitWorkers(ctx, id, 30); done <- err }()
			select {
			case err := <-done:
				t.Fatalf("unchanged workers woke wait: %v", err)
			case <-time.After(400 * time.Millisecond):
			}
			_, err = store.Update(id, func(s *Snapshot) error {
				switch signal {
				case "worker":
					s.Workers[0].Status = "completed"
				case "message":
					s.WorkerMessages = append(s.WorkerMessages, WorkerMessage{ID: "new", From: "fast", To: "parent", Content: "progress"})
				case "mailbox":
					s.Mailbox = append(s.Mailbox, QueuedMessage{Kind: "steer", Content: "new instruction"})
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("wait ignored durable change")
			}
		})
	}
}

func TestUsageCalibrationPreservesNewTailAndInvalidatesEditedPrefix(t *testing.T) {
	input := int64(500)
	prior := []Message{{Role: "system", Content: "system"}, {Role: "user", Content: strings.Repeat("中文需求", 1000)}}
	s := Snapshot{Requests: []ModelRequest{{ID: "r1", Messages: prior}}, Responses: []ResponseRecord{{RequestID: "r1", Response: ModelResponse{Usage: &TokenUsage{InputTokens: &input}}}}}
	current := append(append([]Message(nil), prior...), Message{Role: "assistant", Content: "observed", Thinking: "reasoning"}, Message{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("new output", 100)})
	want := int(input) + EstimateContextTokens(current[2:], nil)
	if got := calibratedContextTokens(&s, current, nil); got != want || got >= EstimateContextTokens(current, nil) {
		t.Fatalf("usage not anchored: got=%d want=%d", got, want)
	}
	current[0].Content = "改写" // Same six UTF-8 bytes as "system", different tokenization.
	if got := calibratedContextTokens(&s, current, nil); got != want+EstimateContextTokens(current[:1], nil) {
		t.Fatalf("equal-size system replacement not charged: %d", got)
	}
	current[1].Content = "different historical source"
	if got := calibratedContextTokens(&s, current, nil); got != EstimateContextTokens(current, nil) {
		t.Fatal("calibrated edited source")
	}
	current = append([]Message(nil), prior...)
	s.Responses[0].Error = "failed response"
	if got := calibratedContextTokens(&s, current, nil); got != EstimateContextTokens(current, nil) {
		t.Fatal("calibrated failed response")
	}
}

func TestWaitWorkersDoesNotSpinOnQueuedFollowUp(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	childID := createTestRun(t, a)
	release, err := store.AcquireRunLock(childID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Workers = []AgentWorker{{ID: "slow", RunID: childID, Status: "running"}}
		s.Mailbox = []QueuedMessage{{ID: "next", Kind: "follow_up", Content: "next turn"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
		_, err := a.WaitWorkers(ctx, id, 30)
		cancel()
		if err != context.DeadlineExceeded {
			t.Fatalf("queued follow-up woke active wait: %v", err)
		}
	}
}

func TestCompactPlanPreservesDirectDependencyContract(t *testing.T) {
	p := PlanState{Capacity: 5}
	for i := 0; i < 60; i++ {
		p.Tasks = append(p.Tasks, PlanTask{ID: fmt.Sprint(i), Goal: strings.Repeat("goal ", 200), Status: "pending"})
	}
	p.Tasks[0].Status = "verified"
	p.Tasks[0].EvidenceIDs = []string{"ev-source"}
	p.Tasks[0].EvidenceRunID = "child-source"
	p.Tasks[1].Dependencies = []string{"0"}
	refreshFrontier(&p)
	view := compactPlan(p)
	tasks := view["tasks"].([]PlanTask)
	if len(tasks) > 6 || tasks[0].ID != "0" || tasks[0].EvidenceRunID != "child-source" || tasks[1].Dependencies[0] != "0" {
		t.Fatalf("compact plan lost source contract: %+v", tasks)
	}
}

func TestPlannerFactsCursorKeepsConcurrentObservation(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "next", Goal: "work"}}); err != nil {
		t.Fatal(err)
	}
	_, observed, err := a.plannerContextAt(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error {
		addEvent(s, "worker_finished", "concurrent-unseen-observation", nil)
		s.Requests = append(s.Requests, ModelRequest{ID: "r1", PlannerThrough: observed})
		addEvent(s, "model_request", "request after observation", map[string]string{"request_id": "r1"})
		s.Responses = append(s.Responses, ResponseRecord{RequestID: "r1"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	text, _, err := a.plannerContextAt(id)
	if err != nil || !strings.Contains(text, "concurrent-unseen-observation") {
		t.Fatalf("lost concurrent planner observation: %s %v", text, err)
	}
}
