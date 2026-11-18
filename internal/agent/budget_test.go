package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func intPointer(v int) *int       { return &v }
func int64Pointer(v int64) *int64 { return &v }
func silentExec(id string) ToolCall {
	command := "true"
	if runtime.GOOS == "windows" {
		command = "exit /b 0"
	}
	args, _ := json.Marshal(map[string]string{"command": command})
	return ToolCall{ID: id, Name: "exec", Arguments: string(args)}
}

func TestModelBudgetResumeNeedsIncrease(t *testing.T) {
	p := manualProvider(silentExec("one"))
	a, s := newTestAgent(t, p)
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(1)})
	id := createTestRun(t, a)
	snap, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusPaused || snap.Run.PauseReason != "model request budget exhausted" || len(snap.Requests) != 1 || len(snap.Actions) != 1 {
		t.Fatalf("model budget violated: %+v", snap.Run)
	}
	snap, err = a.Run(context.Background(), id)
	if err != nil || len(snap.Requests) != 1 {
		t.Fatal("resume bypassed exhausted budget")
	}
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(2)})
	snap, err = a.Run(context.Background(), id)
	if err != nil || snap.Run.Status != StatusCompleted || len(snap.Requests) != 2 {
		t.Fatalf("increased budget failed: %v %+v", err, snap.Run)
	}
	persisted, _ := s.Load(id)
	if persisted.Run.Usage.ModelRequests != 2 || persisted.Run.Usage.ModelResponses != 2 || persisted.Run.Usage.ToolActions != 1 {
		t.Fatal("count accounting inconsistent")
	}
}

func TestActionBudgetPreservesPendingAndDoesNotRepeat(t *testing.T) {
	p := manualProvider(silentExec("one"), silentExec("two"))
	a, _ := newTestAgent(t, p)
	a.ConfigureBudget(BudgetOverride{MaxActions: intPointer(1)})
	id := createTestRun(t, a)
	snap, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusPaused || len(snap.Actions) != 1 || len(pendingToolCalls(&snap)) != 1 {
		t.Fatalf("action budget violated: %+v", snap)
	}
	snap, err = a.Run(context.Background(), id)
	if err != nil || len(snap.Actions) != 1 {
		t.Fatal("exhausted action resumed")
	}
	a.ConfigureBudget(BudgetOverride{MaxActions: intPointer(2)})
	snap, err = a.Run(context.Background(), id)
	if err != nil || snap.Run.Status != StatusCompleted || len(snap.Actions) != 2 {
		t.Fatalf("action increase: %v %+v", err, snap.Run)
	}
}

func TestTimeBudgetCancelsAndRemainsExhausted(t *testing.T) {
	p := &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a, s := newTestAgent(t, p)
	a.ConfigureBudget(BudgetOverride{MaxSeconds: int64Pointer(1)})
	id := createTestRun(t, a)
	started := time.Now()
	snap, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 3*time.Second || snap.Run.Status != StatusPaused || snap.Run.PauseReason != "time budget exhausted" || snap.Run.ElapsedMillis < 950 {
		t.Fatalf("time budget did not interrupt: %+v", snap.Run)
	}
	a.provider = successProvider{}
	before := len(snap.Requests)
	snap, err = a.Run(context.Background(), id)
	if err != nil || len(snap.Requests) != before || snap.Run.Status != StatusPaused {
		t.Fatal("exhausted time bypassed")
	}
	a.ConfigureBudget(BudgetOverride{MaxSeconds: int64Pointer(3)})
	snap, err = a.Run(context.Background(), id)
	if err != nil || snap.Run.Status != StatusCompleted {
		t.Fatalf("time extension: %v %+v", err, snap.Run)
	}
	persisted, _ := s.Load(id)
	if persisted.Run.ElapsedMillis < snap.Run.ElapsedMillis {
		t.Fatal("elapsed return exceeds persistence")
	}
}

func TestUsagePreservesKnownAndUnknown(t *testing.T) {
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		if count == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"first"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"content":"second"},"finish_reason":"stop"}]}`)
		}
	}))
	defer server.Close()
	a, _ := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "fixture"})
	id := createTestRun(t, a)
	snap, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Usage.KnownTotalTokens == nil || *snap.Run.Usage.KnownTotalTokens != 15 || snap.Run.Usage.MissingUsageResponses != 0 {
		t.Fatalf("known usage: %+v", snap.Run.Usage)
	}
	snap, err = a.Message(context.Background(), id, "followup")
	if err != nil {
		t.Fatal(err)
	}
	if *snap.Run.Usage.KnownTotalTokens != 15 || snap.Run.Usage.MissingUsageResponses != 1 {
		t.Fatalf("partial usage: %+v", snap.Run.Usage)
	}
	a, _ = newTestAgent(t, MockProvider{})
	snap, err = a.Run(context.Background(), createTestRun(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Usage.KnownTotalTokens != nil || snap.Run.Usage.MissingUsageResponses != 1 {
		t.Fatal("unknown tokens zero filled")
	}
}

func TestBudgetEnvironmentParsing(t *testing.T) {
	t.Setenv("TRACE_MAX_ROUNDS", "0")
	t.Setenv("TRACE_MAX_ACTIONS", "12")
	t.Setenv("TRACE_MAX_SECONDS", "3")
	limits, err := BudgetFromEnv()
	if err != nil || *limits.MaxModelRequests != 0 || *limits.MaxActions != 12 || *limits.MaxSeconds != 3 {
		t.Fatalf("env: %+v %v", limits, err)
	}
	t.Setenv("TRACE_MAX_ACTIONS", "-1")
	if _, err = BudgetFromEnv(); err == nil || !strings.Contains(err.Error(), "TRACE_MAX_ACTIONS") {
		t.Fatal("negative budget accepted")
	}
}

func TestTimeBudgetInterruptsToolAndArchivesEvidence(t *testing.T) {
	p := manualProvider(ToolCall{ID: "one", Name: "external", Arguments: "{}"})
	a, s := newTestAgent(t, p)
	id := createTestRun(t, a)
	waitTestRun(t, a, id)
	a.ConfigureBudget(BudgetOverride{MaxSeconds: int64Pointer(1)})
	t.Setenv("TRACE_TEST_CHILD", "blocking_tool")
	t.Setenv("TRACE_TEST_READY", t.TempDir()+string(os.PathSeparator)+"ready")
	snap, err := a.ExecuteTool(context.Background(), id, "one", []string{os.Args[0], "-test.run=^TestProcessFixture$"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusPaused || snap.Run.PauseReason != "time budget exhausted" || len(snap.Actions) != 1 || snap.Actions[0].Result == nil || !snap.Actions[0].Result.Interrupted || snap.Actions[0].Status != "uncertain" || !snap.Actions[0].Result.OutcomeUncertain || len(snap.Evidence) != 1 || snap.Evidence[0].Verification != "unverified" {
		t.Fatalf("time-limited tool incorrectly completed: %+v", snap)
	}
	persisted, _ := s.Load(id)
	if persisted.Run.ElapsedMillis < 950 {
		t.Fatal("tool elapsed budget not saved")
	}
	if len(p.requests) != 1 || len(persisted.Evidence) != 1 || persisted.Evidence[0].Content != snap.Evidence[0].Content || persisted.Actions[0].Status != "uncertain" {
		t.Fatal("explicit budget resumed the model or lost the interrupted observation")
	}
}

func TestCallerDeadlineIsNotBudgetExhaustion(t *testing.T) {
	p := &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a, _ := newTestAgent(t, p)
	a.ConfigureBudget(BudgetOverride{MaxSeconds: int64Pointer(3)})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	snap, err := a.Run(ctx, createTestRun(t, a))
	if err == nil || snap.Run.Status != StatusFailed || snap.Run.PauseReason == "time budget exhausted" {
		t.Fatalf("caller deadline misclassified: %v %+v", err, snap.Run)
	}
}

func TestBudgetRecoveryChargesInterruptedInterval(t *testing.T) {
	a, s := newTestAgent(t, MockProvider{})
	a.ConfigureBudget(BudgetOverride{MaxSeconds: int64Pointer(1)})
	id := createTestRun(t, a)
	s.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusRunning
		s.Run.BudgetActiveSince = time.Now().Add(-2 * time.Second)
		return nil
	})
	snap, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusPaused || len(snap.Requests) != 0 || snap.Run.ElapsedMillis < 1900 || !snap.Run.BudgetActiveSince.IsZero() {
		t.Fatalf("recovery erased active time: %+v", snap.Run)
	}
}
func TestPartialTokenUsagePreservesMissingFields(t *testing.T) {
	snapshot := Snapshot{Responses: []ResponseRecord{{Response: ModelResponse{Usage: &TokenUsage{InputTokens: int64Pointer(12)}}}}}
	refreshUsage(&snapshot)
	if snapshot.Run.Usage.KnownInputTokens == nil || *snapshot.Run.Usage.KnownInputTokens != 12 || snapshot.Run.Usage.KnownOutputTokens != nil || snapshot.Run.Usage.KnownTotalTokens != nil || snapshot.Run.Usage.MissingUsageResponses != 1 {
		t.Fatalf("partial usage fabricated fields: %+v", snapshot.Run.Usage)
	}
}
