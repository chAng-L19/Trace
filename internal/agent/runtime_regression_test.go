package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type scriptedProvider struct {
	responses []ModelResponse
	requests  []ModelRequest
}

func (p *scriptedProvider) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	p.requests = append(p.requests, request)
	if len(p.responses) == 0 {
		return ModelResponse{}, fmt.Errorf("unexpected model request")
	}
	response := p.responses[0]
	p.responses = p.responses[1:]
	if request.Purpose == "" {
		response = fixtureModelResponse(response)
	}
	return response, nil
}

// Legacy execution fixtures supply the report in their existing final turn.
// Delivery-specific tests use their own provider and exercise missing bodies.
func fixtureReport(content string) string {
	if _, present := traceEnvelope[reportSubmission](content, "trace_report"); present {
		return content
	}
	if claim, present := traceResultEnvelope(content); present {
		if claim == nil || claim.Report != nil {
			return content
		}
		if _, raw := traceResultJSON(strings.TrimSpace(content)); raw {
			content = "```json\n" + content + "\n```"
		}
	}
	raw, _ := json.Marshal(map[string]any{"trace_report": u7Text})
	return content + "\n\n```json\n" + string(raw) + "\n```"
}

func fixtureModelResponse(response ModelResponse) ModelResponse {
	if len(response.ToolCalls) == 0 && strings.TrimSpace(response.Content) != "" {
		response.Content = fixtureReport(response.Content)
	}
	return response
}
func newTestAgent(t *testing.T, p Provider) (*Agent, *Store) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	agent, err := New(store, p, "")
	if err != nil {
		t.Fatal(err)
	}
	return agent, store
}

// createTestRun preserves legacy execution semantics for low-level tool fixtures.
// Coordinator integration tests use Create/CreateConfigured directly.
func createTestRun(t *testing.T, a *Agent) string {
	t.Helper()
	snap, err := a.Create("test objective")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.Update(snap.Run.ID, func(s *Snapshot) error {
		s.Run.Responsibility = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return snap.Run.ID
}
func manualProvider(calls ...ToolCall) *scriptedProvider {
	return &scriptedProvider{responses: []ModelResponse{{Content: "inspection", ToolCalls: calls}, {Content: "verified response"}}}
}
func waitTestRun(t *testing.T, a *Agent, id string) {
	t.Helper()
	// Archive one real provider turn, then retain the legacy pending boundary
	// for explicit manual API fixtures without invoking automatic tool dispatch.
	retries := 0
	request, err := a.preparePiConversation(context.Background(), id, &retries)
	if err != nil {
		t.Fatal(err)
	}
	response, callErr := a.complete(context.Background(), id, request)
	snap, _, err := a.settlePiResponse(context.Background(), id, request, response, callErr, &retries)
	if err != nil || snap.Run.Status != StatusWaitingTool {
		t.Fatalf("waiting: %s %v", snap.Run.Status, err)
	}
}
func TestActiveControlCancelsProvider(t *testing.T) {
	for _, control := range []string{"pause", "cancel"} {
		t.Run(control, func(t *testing.T) {
			p := &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
			a, s := newTestAgent(t, p)
			id := createTestRun(t, a)
			done := make(chan error, 1)
			go func() { _, err := a.Run(context.Background(), id); done <- err }()
			<-p.entered
			other, _ := NewStore(s.root)
			controller, _ := New(other, MockProvider{}, "")
			var err error
			expected := StatusPaused
			if control == "pause" {
				_, err = controller.Pause(id)
			} else {
				_, err = controller.Cancel(id)
				expected = StatusCancelled
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				close(p.release)
				t.Fatal("control did not cancel provider")
			}
			snap, _ := s.Load(id)
			if snap.Run.Status != expected || len(snap.Responses) != 1 {
				t.Fatalf("control response not archived: %+v", snap)
			}
		})
	}
}

func TestPausedPendingToolsResumeReturnsUnavailableToModel(t *testing.T) {
	p := manualProvider(ToolCall{ID: "one", Name: "external", Arguments: "{}"})
	a, s := newTestAgent(t, p)
	id := createTestRun(t, a)
	waitTestRun(t, a, id)
	if _, err := a.Pause(id); err != nil {
		t.Fatal(err)
	}
	snap, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusWaitingTool || len(p.requests) != 1 || len(snap.Actions) != 0 || len(snap.Evidence) != 0 {
		t.Fatalf("operator resume crossed the paused pending-action boundary: %+v", snap.Run)
	}
	snap, err = a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusCompleted || len(p.requests) != 2 || len(snap.Actions) != 0 || len(snap.Evidence) != 1 || snap.Evidence[0].Type != "runtime_observation" {
		t.Fatalf("resume did not return unavailable tool to model: %+v", snap.Run)
	}
	stored, _ := s.Load(id)
	if len(pendingToolCalls(&stored)) != 0 {
		t.Fatal("pending result not resolved")
	}
}

func TestToolBatchIdentityAndRolePersistence(t *testing.T) {
	p := manualProvider(ToolCall{ID: "one", Name: "external", Arguments: "{}"}, ToolCall{ID: "two", Name: "external", Arguments: "{}"})
	a, s := newTestAgent(t, p)
	id := createTestRun(t, a)
	waitTestRun(t, a, id)
	before, _ := s.Load(id)
	if _, err := a.SubmitTool(context.Background(), id, "forged", "bad"); err == nil {
		t.Fatal("unknown call accepted")
	}
	after, _ := s.Load(id)
	if len(after.Messages) != len(before.Messages) {
		t.Fatal("invalid call mutated transcript")
	}
	snap, err := a.SubmitTool(context.Background(), id, "one", "UNTRUSTED OUTPUT")
	if err != nil || snap.Run.Status != StatusWaitingTool || len(p.requests) != 1 {
		t.Fatalf("partial batch: %+v %v", snap.Run, err)
	}
	if _, err := a.SubmitTool(context.Background(), id, "one", "duplicate"); err == nil {
		t.Fatal("duplicate call accepted")
	}
	snap, err = a.SubmitTool(context.Background(), id, "two", "second")
	if err != nil || snap.Run.Status != StatusCompleted {
		t.Fatalf("batch completion: %+v %v", snap.Run, err)
	}
	for _, m := range p.requests[1].Messages {
		if m.Role == "user" && m.Content == "UNTRUSTED OUTPUT" {
			t.Fatal("tool output became user message")
		}
	}
	if !strings.Contains(fmt.Sprint(p.requests[1].Messages), "inspection") {
		t.Fatal("assistant content discarded")
	}
}

func TestRejectedMessagesLeaveTranscriptUnchanged(t *testing.T) {
	for _, status := range []Status{StatusCancelled, StatusWaitingTool, StatusRunning} {
		t.Run(string(status), func(t *testing.T) {
			a, s := newTestAgent(t, MockProvider{})
			id := createTestRun(t, a)
			s.Update(id, func(s *Snapshot) error { s.Run.Status = status; return nil })
			if _, err := a.Message(context.Background(), id, "rejected"); err == nil {
				t.Fatal("invalid state accepted")
			}
			snap, _ := s.Load(id)
			if len(snap.Messages) != 0 {
				t.Fatal("rejected message mutated transcript")
			}
		})
	}
}

func TestPreparedPromptResourceAndReceiptLimits(t *testing.T) {
	a, _ := newTestAgent(t, MockProvider{})
	root, err := a.Create("coordinate a concrete task")
	if err != nil {
		t.Fatal(err)
	}
	req, err := a.preparePiRequest(context.Background(), root.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Messages) == 0 || req.Messages[0].Role != "system" {
		t.Fatal("missing system prompt in prepared request")
	}
	for _, rule := range []string{
		"From the first turn, the root Run only", "If dispatch is blocked, report the cause; the root never takes over execution",
		"concurrency ceiling, not a dispatch target", "first nonempty root plan, specify `capacity`",
		"external acceptance when the original goal requires it", "Local results need no external receipt",
		"cleanup proves neither delivery nor acceptance", "Do not add independent Worker solving time/token budgets or deadlines",
		"Uncertain stays unknown", "no Astra for simple work",
		"every successful stdout it cites", "zero tools", "source_digest",
		"<child_run_id>/<evidence_id>", "registered, settled Worker sources listed by verification_status",
	} {
		if !strings.Contains(req.Messages[0].Content, rule) {
			t.Errorf("prepared system prompt missing rule: %q", rule)
		}
	}
	for _, obsolete := range []string{"Simple work without delegated tasks can execute directly", "Execute simple tasks directly", "Cross-Run sources are not local acceptance evidence"} {
		if strings.Contains(req.Messages[0].Content, obsolete) {
			t.Errorf("prepared prompt retains direct root execution: %q", obsolete)
		}
	}
}

func TestCanonicalPromptAndHistoricalSnapshot(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "prompts", "agent.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != DefaultPrompt {
		t.Fatal("embedded prompt differs")
	}
	root := t.TempDir()
	path := filepath.Join(root, "runtime.md")
	os.WriteFile(path, []byte("old runtime prompt"), 0600)
	s, _ := NewStore(filepath.Join(root, "state"))
	a, _ := New(s, MockProvider{}, path)
	id := createTestRun(t, a)
	os.WriteFile(path, []byte("new runtime prompt"), 0600)
	a, _ = New(s, MockProvider{}, path)
	snap, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Requests[0].Messages[0].Content != "old runtime prompt"+runtimeDataHint {
		t.Fatal("historical prompt changed")
	}
	if strings.Contains(snap.Prompt, "Trace 项目 Agent 约定") {
		t.Fatal("maintainer prompt entered request")
	}
}

func TestAutomaticExecAndBoundedRounds(t *testing.T) {
	command := "printf 'out'; printf 'err' >&2; exit 7"
	if runtime.GOOS == "windows" {
		command = "echo out & echo err 1>&2 & exit /b 7"
	}
	args, _ := json.Marshal(map[string]string{"command": command})
	p := manualProvider(ToolCall{ID: "exec-1", Name: "exec", Arguments: string(args)})
	a, _ := newTestAgent(t, p)
	id := createTestRun(t, a)
	snap, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusCompleted || len(snap.Actions) != 1 || len(p.requests) != 2 {
		t.Fatalf("automatic loop incomplete: %+v", snap)
	}
	result := snap.Actions[0].Result
	if result == nil || result.ExitCode != 7 || !strings.Contains(result.Stdout, "out") || !strings.Contains(result.Stderr, "err") {
		t.Fatalf("structured result: %+v", result)
	}
	if snap.Actions[0].StartedAt.IsZero() || snap.Actions[0].FinishedAt.IsZero() {
		t.Fatal("action timing missing")
	}
	silent := "true"
	if runtime.GOOS == "windows" {
		silent = "exit /b 0"
	}
	args, _ = json.Marshal(map[string]string{"command": silent})
	p = &scriptedProvider{}
	const explicitRoundLimit = 8
	for i := 0; i < explicitRoundLimit+1; i++ {
		p.responses = append(p.responses, ModelResponse{ToolCalls: []ToolCall{{ID: fmt.Sprint(i), Name: "exec", Arguments: string(args)}}})
	}
	a, _ = newTestAgent(t, p)
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(explicitRoundLimit)})
	snap, err = a.Run(context.Background(), createTestRun(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusPaused || len(p.requests) != explicitRoundLimit {
		t.Fatalf("round bound not enforced: requests=%d %+v", len(p.requests), snap.Run)
	}
}

func TestUncertainActionNeverReexecutes(t *testing.T) {
	p := manualProvider(ToolCall{ID: "one", Name: "external", Arguments: "{}"})
	a, s := newTestAgent(t, p)
	id := createTestRun(t, a)
	waitTestRun(t, a, id)
	s.Update(id, func(s *Snapshot) error {
		s.Actions = append(s.Actions, Action{RequestID: currentRequestID(s), CallID: "one", Name: "external", Status: "started", StartedAt: time.Now()})
		return nil
	})
	snap, err := a.ExecuteTool(context.Background(), id, "one", []string{"nonexistent-command-must-not-run"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusCompleted || snap.Actions[0].Status != "uncertain" || snap.Actions[0].Result != nil || !snap.Actions[0].FinishedAt.IsZero() || len(p.requests) != 2 || len(snap.Evidence) != 1 || snap.Evidence[0].Type != "runtime_observation" {
		t.Fatalf("unknown action replayed or observation lost: %+v", snap)
	}
}

func TestMessageBeforeFirstRunPreservesOriginalGoal(t *testing.T) {
	p := &scriptedProvider{responses: []ModelResponse{{Content: "done"}}}
	a, _ := newTestAgent(t, p)
	snap, err := a.Create("original goal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Message(context.Background(), snap.Run.ID, "extra context"); err != nil {
		t.Fatal(err)
	}
	if len(p.requests) != 1 {
		t.Fatalf("unexpected model requests: %d", len(p.requests))
	}
	messages, observations := splitContextObservations(snap.Run.ID, p.requests[0].Messages)
	if len(observations) == 0 || len(messages) != 3 || messages[1].Content != "original goal" || messages[2].Content != "extra context" {
		t.Fatalf("original goal lost: %+v", p.requests)
	}
}
