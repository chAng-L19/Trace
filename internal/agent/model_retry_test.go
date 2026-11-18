package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type modelRetryProvider struct {
	responses []ModelResponse
	errors    []error
	requests  []ModelRequest
	onCall    func(int)
}

func (p *modelRetryProvider) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	i := len(p.requests)
	p.requests = append(p.requests, request)
	if p.onCall != nil {
		p.onCall(i + 1)
	}
	var response ModelResponse
	if i < len(p.responses) {
		response = p.responses[i]
	}
	if i < len(p.errors) {
		return response, p.errors[i]
	}
	return response, nil
}

func TestModelRetryRetainsAttemptsAndDoesNotReplayTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "single-write.txt")
	args, _ := json.Marshal(map[string]string{"path": path, "content": "one actual write"})
	outage := &ProviderHTTPError{StatusCode: 503, Status: "503 Service Unavailable"}
	p := &modelRetryProvider{
		errors:    []error{outage, outage, nil, outage, nil},
		responses: []ModelResponse{{}, {}, {ToolCalls: []ToolCall{{ID: "write-once", Name: "write", Arguments: string(args)}}, StopReason: "tool_calls"}, {}, {Content: "完成观察", StopReason: "stop"}},
	}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	p.onCall = func(n int) {
		if n == 4 {
			if _, err := a.QueueMessage(id, "steer", "继续原任务并说明结果"); err != nil {
				t.Fatal(err)
			}
		}
	}
	snap, err := a.Run(context.Background(), id)
	if err != nil || snap.Run.Status != StatusCompleted || len(snap.Requests) != 5 || len(snap.Responses) != 5 || len(snap.Actions) != 1 {
		t.Fatalf("retry/replay outcome: %+v %v", snap.Run, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "one actual write" || len(snap.Evidence) != 1 || snap.Evidence[0].Verification != "verified" {
		t.Fatalf("tool execution lost: %q %+v %v", data, snap.Evidence, err)
	}
	seen := map[string]bool{}
	for i, request := range snap.Requests {
		if seen[request.ID] || snap.Responses[i].RequestID != request.ID {
			t.Fatal("retry reused or lost an attempt identity")
		}
		seen[request.ID] = true
		if (snap.Responses[i].Error != "") != (i == 0 || i == 1 || i == 3) {
			t.Fatalf("failed attempt not archived at %d: %+v", i, snap.Responses[i])
		}
	}
	last := lastConversationMessage(p.requests[4])
	if last.Role != "user" || last.Content != "继续原任务并说明结果" || snap.Run.Usage.ModelRequests != 5 || snap.Run.Usage.ToolActions != 1 {
		t.Fatal("retry bypassed queued steering or budget accounting")
	}
	loaded, err := store.Load(id)
	if err != nil || len(loaded.Requests) != 5 || len(loaded.Actions) != 1 {
		t.Fatal("retry history was not durable", err)
	}
}

func TestModelRetryLimitsAndValidationFailures(t *testing.T) {
	for _, scenario := range []string{"auth", "continued", "budget", "truncated"} {
		t.Run(scenario, func(t *testing.T) {
			failure := &ProviderHTTPError{StatusCode: 503, Status: "503 Service Unavailable"}
			p := &modelRetryProvider{errors: []error{failure, failure, failure}}
			wantRequests, wantStatus := 1, StatusFailed
			if scenario == "auth" {
				p.errors[0] = &ProviderHTTPError{StatusCode: 401, Status: "401 Unauthorized"}
			}
			if scenario == "continued" {
				wantRequests, wantStatus = 4, StatusCompleted
				p.responses = []ModelResponse{{}, {}, {}, {Content: "provider recovered"}}
			}
			if scenario == "truncated" {
				p.errors = nil
				p.responses = []ModelResponse{{StopReason: "length", ToolCalls: []ToolCall{{ID: "partial", Name: "write", Arguments: `{"path":"never-execute"}`}}}}
			}
			a, _ := newTestAgent(t, p)
			if scenario == "budget" {
				a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(1)})
				wantStatus = StatusPaused
			}
			snap, err := a.Run(context.Background(), createTestRun(t, a))
			if snap.Run.Status != wantStatus || len(snap.Requests) != wantRequests || len(snap.Actions) != 0 || len(pendingToolCalls(&snap)) != 0 {
				t.Fatalf("unexpected retry outcome: %+v %v", snap.Run, err)
			}
			if (err == nil) != (scenario == "budget" || scenario == "continued") || len(snap.Responses) != wantRequests || (snap.Responses[len(snap.Responses)-1].Error == "") != (scenario == "continued") {
				t.Fatalf("failed response was not retained: %+v %v", snap.Responses, err)
			}
		})
	}
}

func TestModelRetryWaitHonorsPauseCancelAndTimeBudget(t *testing.T) {
	for _, control := range []string{"pause", "cancel", "time"} {
		t.Run(control, func(t *testing.T) {
			p := &modelRetryProvider{errors: []error{&ProviderHTTPError{StatusCode: 429, Status: "429 Too Many Requests", RetryAfter: "60"}}}
			a, store := newTestAgent(t, p)
			if control == "time" {
				a.ConfigureBudget(BudgetOverride{MaxSeconds: int64Pointer(1)})
			}
			id := createTestRun(t, a)
			done := make(chan error, 1)
			go func() { _, err := a.Run(context.Background(), id); done <- err }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				s, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				ready := false
				for _, event := range s.Events {
					ready = ready || event.Type == "model_retry"
				}
				if ready {
					break
				}
				if time.Now().After(deadline) {
					a.Cancel(id)
					t.Fatal("retry was not durably scheduled")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if control == "pause" {
				if _, err := a.Pause(id); err != nil {
					t.Fatal(err)
				}
			} else if control == "cancel" {
				if _, err := a.Cancel(id); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				a.Cancel(id)
				t.Fatal("Retry-After wait ignored persisted control")
			}
			snap, err := store.Load(id)
			wantStatus := StatusPaused
			if control == "cancel" {
				wantStatus = StatusCancelled
			}
			if err != nil || snap.Run.Status != wantStatus || len(snap.Requests) != 1 || snap.Run.Usage.ModelRequests != 1 || snap.Responses[0].Error == "" {
				t.Fatalf("retry control lost: %+v %v", snap.Run, err)
			}
			if control == "pause" {
				a.provider = successProvider{}
				snap, err = a.Run(context.Background(), id)
				if err != nil || snap.Run.Status != StatusCompleted || len(snap.Requests) != 2 {
					t.Fatalf("resume lost failed-attempt accounting: %+v %v", snap.Run, err)
				}
			}
		})
	}
}

func TestModelRetryHTTPDelayAndPermanentFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		http.Error(w, "temporarily busy", http.StatusTooManyRequests)
	}))
	defer server.Close()
	provider := &OpenAIProvider{BaseURL: server.URL, Model: "fixture"}
	_, err := provider.Complete(context.Background(), ModelRequest{})
	if err == nil || modelRetryDelay(err, 1) != 7*time.Second || modelRetryDelay(err, 3) != 7*time.Second || modelRetryDelay(err, 100) != 30*time.Second {
		t.Fatalf("HTTP retry instruction lost: %v", err)
	}
	for _, failure := range []error{errors.New("malformed stream"), context.Canceled, &ProviderHTTPError{StatusCode: 400}, &ProviderHTTPError{StatusCode: 401}} {
		if modelRetryDelay(failure, 1) != 0 {
			t.Fatalf("permanent error retried: %v", failure)
		}
	}
	date := time.Now().Add(time.Minute).UTC().Format(http.TimeFormat)
	delay := modelRetryDelay(&ProviderHTTPError{StatusCode: 503, RetryAfter: date}, 1)
	if delay < 58*time.Second || delay > time.Minute {
		t.Fatalf("HTTP-date Retry-After ignored: %s", delay)
	}
}
