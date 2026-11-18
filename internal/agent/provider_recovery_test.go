package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestProviderFailureClassificationIsNarrowAndBounded(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want providerFailure
	}{
		{"eof", io.EOF, providerTransient},
		{"unexpected_eof", fmt.Errorf("response body: %w", io.ErrUnexpectedEOF), providerTransient},
		{"connection_reset", fmt.Errorf("read: %w", syscall.ECONNRESET), providerTransient},
		{"incomplete_sse", &providerIncompleteError{detail: "missing DONE"}, providerTransient},
		{"throttle", &ProviderHTTPError{StatusCode: 429, RetryAfter: "7"}, providerTransient},
		{"cancel", errors.Join(io.EOF, context.Canceled), providerCancelled},
		{"deadline", context.DeadlineExceeded, providerCancelled},
		{"quota", &ProviderHTTPError{StatusCode: 429, Body: "insufficient_quota"}, providerQuota},
		{"billing", &ProviderHTTPError{StatusCode: 503, Body: "billing limit reached"}, providerQuota},
		{"auth", &ProviderHTTPError{StatusCode: 401, Body: "rate_limit_exceeded"}, providerPermanent},
		{"bad_request", &ProviderHTTPError{StatusCode: 400, Body: "malformed tool arguments"}, providerPermanent},
		{"bad_request_503", &ProviderHTTPError{StatusCode: 503, Body: "invalid_request_error: invalid tool schema"}, providerPermanent},
		{"overflow", &ProviderHTTPError{StatusCode: 400, Body: "context_length_exceeded"}, providerOverflow},
		{"api_overflow", &ProviderAPIError{Code: "context_length_exceeded"}, providerOverflow},
		{"api_quota", &ProviderAPIError{Code: "insufficient_quota"}, providerQuota},
		{"go_usage_quota", &ProviderHTTPError{StatusCode: 429, Body: "GoUsageLimitError"}, providerQuota},
		{"free_usage_quota", &ProviderAPIError{Type: "FreeUsageLimitError", Message: "rate_limit_exceeded"}, providerQuota},
		{"api_auth", &ProviderAPIError{Code: "invalid_api_key", Message: "rate_limit_exceeded"}, providerPermanent},
		{"ordinary_mock", errors.New("provider unavailable, context_length_exceeded, EOF"), providerPermanent},
		{"sink_error", errors.Join(io.EOF, &providerStreamSinkError{cause: errors.New("store unavailable")}), providerPermanent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyProviderFailure(tc.err); got != tc.want {
				t.Fatalf("classification: got %s want %s: %v", got, tc.want, tc.err)
			}
			if got := modelRetryDelay(tc.err, 1); (got > 0) != (tc.want == providerTransient) {
				t.Fatalf("retry mismatch: %v delay=%v", tc.err, got)
			}
			if got := modelRetryDelay(tc.err, 3); (got > 0) != (tc.want == providerTransient) {
				t.Fatalf("third retry classification changed: %v delay=%v", tc.err, got)
			}
		})
	}
	if modelRetryDelay(cases[4].err, 1) != 7*time.Second {
		t.Fatal("Retry-After changed")
	}
	if modelRetryDelay(io.EOF, 1000) != 30*time.Second || modelRetryDelay(&ProviderHTTPError{StatusCode: 429, RetryAfter: "90"}, 1000) != 90*time.Second {
		t.Fatal("retry backoff lost its cap or shortened Retry-After")
	}
}

func TestProviderErrorUsageSurvivesHTTPAndSSE(t *testing.T) {
	for _, transport := range []string{"http", "sse", "json"} {
		t.Run(transport, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "insufficient_quota", "message": "no remaining credits"}, "usage": map[string]int{"prompt_tokens": 8, "completion_tokens": 1, "total_tokens": 9}})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if transport == "http" {
					w.WriteHeader(429)
				} else if transport == "sse" {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: %s\n\n", body)
					return
				}
				w.Write(body)
			}))
			defer server.Close()
			p := &OpenAIProvider{BaseURL: server.URL, Model: "fixture"}
			answer, err := p.CompleteStream(context.Background(), ModelRequest{}, nil)
			if classifyProviderFailure(err) != providerQuota || answer.Usage == nil || *answer.Usage.TotalTokens != 9 {
				t.Fatalf("error usage/class lost: %+v %v", answer, err)
			}
		})
	}
}

func recoveryHistory(t *testing.T, a *Agent, store *Store) (string, []Message) {
	t.Helper()
	a.prompt, a.promptID = "fixture system", promptHash("fixture system")
	a.registry = &ToolRegistry{tools: map[string]RegisteredTool{}}
	if err := a.ConfigureContext(ContextConfig{WindowTokens: 32000, OutputReserve: 1000, CompactionMode: "semantic"}); err != nil {
		t.Fatal(err)
	}
	id := createTestRun(t, a)
	snap, err := store.Update(id, func(s *Snapshot) error {
		s.Messages = []Message{{Role: "user", Content: "keep exact objective"},
			{Role: "assistant", Content: strings.Repeat("archived observation ", 100)},
			{Role: "user", Content: "keep latest instruction"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return id, snap.Messages
}

func TestProviderOverflowAndLengthCompactOnceWithoutTools(t *testing.T) {
	for _, kind := range []string{"overflow", "length"} {
		t.Run(kind, func(t *testing.T) {
			failure := &ProviderHTTPError{StatusCode: 400, Status: "400 Bad Request", Body: "context_length_exceeded"}
			first := ModelResponse{Content: "failed partial", Usage: &TokenUsage{TotalTokens: int64Pointer(5)}}
			var callErr error = failure
			if kind == "length" {
				callErr = nil
				first.StopReason = "length"
				first.ToolCalls = []ToolCall{{ID: "partial-write", Name: "write", Arguments: "{}"}}
			}
			p := &modelRetryProvider{errors: []error{callErr, nil, nil}, responses: []ModelResponse{first,
				{Content: "Preserved historical observations.", StopReason: "stop", Usage: &TokenUsage{TotalTokens: int64Pointer(7)}},
				{Content: "done", StopReason: "stop", Usage: &TokenUsage{TotalTokens: int64Pointer(3)}}}}
			a, store := newTestAgent(t, p)
			id, original := recoveryHistory(t, a, store)
			snap, err := a.Run(context.Background(), id)
			if err != nil || snap.Run.Status != StatusCompleted || len(snap.Requests) != 3 || len(snap.Responses) != 3 || len(snap.Compactions) != 1 {
				t.Fatalf("recovery incomplete: %+v requests=%d err=%v", snap.Run, len(snap.Requests), err)
			}
			if len(snap.Actions) != 0 || len(pendingToolCalls(&snap)) != 0 || len(snap.Messages) != len(original)+1 || messageHash(snap.Messages[:len(original)]) != messageHash(original) {
				t.Fatal("failed response/summary changed source history or executed a tool")
			}
			if snap.Requests[1].Purpose != "context_summary" || snap.Requests[2].CompactionID == "" || snap.Responses[0].Error == "" || *snap.Run.Usage.KnownTotalTokens != 15 {
				t.Fatal("summary/failed attempt escaped archive or budget")
			}
			if EstimateContextTokens(snap.Requests[2].Messages, nil) >= EstimateContextTokens(snap.Requests[0].Messages, nil) {
				t.Fatal("recovery retried unchanged context")
			}
		})
	}
}

func TestProviderContextRecoveryStopsRepeatedOverflowAndPersistsLimit(t *testing.T) {
	failure := &ProviderHTTPError{StatusCode: 400, Body: "context_length_exceeded"}
	p := &modelRetryProvider{errors: []error{failure, nil, failure}, responses: []ModelResponse{{}, {Content: "short summary", StopReason: "stop"}, {}}}
	a, store := newTestAgent(t, p)
	id, _ := recoveryHistory(t, a, store)
	snap, err := a.Run(context.Background(), id)
	if err == nil || snap.Run.Status != StatusFailed || len(snap.Requests) != 3 || len(snap.Compactions) != 1 || !strings.Contains(snap.Run.Error, "already used") {
		t.Fatalf("overflow spun: %+v requests=%d err=%v", snap.Run, len(snap.Requests), err)
	}
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(reopened, p, "")
	if err != nil {
		t.Fatal(err)
	}
	snap, retry, err := restarted.recoverProviderContext(context.Background(), id, snap.Requests[2], ModelResponse{}, failure)
	if err != nil || retry || len(snap.Requests) != 3 || len(snap.Compactions) != 1 {
		t.Fatalf("restart reset recovery bound: %+v %v", snap.Run, err)
	}
}

func TestProviderRecoverySummaryFailuresContinueThroughPi(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, kind := range []string{"length", "tool_calls", "empty", "no_reduction", "transient"} {
			t.Run(backend+"/"+kind, func(t *testing.T) {
				bad := ModelResponse{Content: "partial summary", StopReason: "length"}
				failures := 3
				var summaryErr error
				switch kind {
				case "tool_calls":
					bad = ModelResponse{ToolCalls: []ToolCall{{ID: "never-dispatch", Name: "write", Arguments: `{}`}}}
				case "empty":
					bad = ModelResponse{}
				case "no_reduction":
					bad = ModelResponse{Content: strings.Repeat("long summary ", 500)}
				case "transient":
					failures, summaryErr = 1, &ProviderHTTPError{StatusCode: 503}
				}
				p := &modelRetryProvider{
					responses: []ModelResponse{{Usage: &TokenUsage{TotalTokens: int64Pointer(1)}}},
					errors:    []error{&ProviderHTTPError{StatusCode: 400, Body: "context_length_exceeded"}},
				}
				for i := 0; i < failures; i++ {
					bad.Usage = &TokenUsage{TotalTokens: int64Pointer(1)}
					p.responses = append(p.responses, bad)
					p.errors = append(p.errors, summaryErr)
				}
				p.responses = append(p.responses,
					ModelResponse{Content: "Exact objective and historical observations retained.", Usage: &TokenUsage{TotalTokens: int64Pointer(1)}},
					ModelResponse{Content: "done", Usage: &TokenUsage{TotalTokens: int64Pointer(1)}})
				p.errors = append(p.errors, nil, nil)
				store, _ := u7Store(t, backend)
				a, err := New(store, p, "")
				if err != nil {
					t.Fatal(err)
				}
				id, original := recoveryHistory(t, a, store)
				snap, err := a.Run(context.Background(), id)
				want := failures + 3
				if err != nil || snap.Run.Status != StatusCompleted || len(snap.Requests) != want || len(snap.Responses) != want || len(snap.Compactions) != failures+1 {
					t.Fatalf("summary correction stopped: %+v requests=%d err=%v", snap.Run, len(snap.Requests), err)
				}
				if len(snap.Actions) != 0 || len(pendingToolCalls(&snap)) != 0 || len(snap.Messages) != len(original)+1 || messageHash(snap.Messages[:len(original)]) != messageHash(original) || *snap.Run.Usage.KnownTotalTokens != int64(want) {
					t.Fatal("summary correction rewrote history, dispatched tools or escaped usage accounting")
				}
				for i := 0; i < failures; i++ {
					if snap.Compactions[i].Status != "failed" || snap.Responses[i+1].Error == "" || snap.Requests[i+1].Purpose != "context_summary" {
						t.Fatal("failed summary or its archived response was replaced")
					}
				}
				lastSummary := snap.Requests[want-2]
				started, ready := 0, 0
				for _, event := range snap.Events {
					if event.Type == "context_recovery_started" {
						started++
						origin := recoveryEvent(event)
						if origin.RequestID != snap.Requests[0].ID || origin.SourceHash != messageHash(original) {
							t.Fatal("summary retry lost the original failure/source identity")
						}
					}
					if event.Type == "context_recovery_ready" {
						ready++
						if recoveryEvent(event).SummaryRequestID != lastSummary.ID {
							t.Fatal("a failed summary was marked ready")
						}
					}
				}
				last := snap.Requests[want-1]
				if started != failures+1 || ready != 1 || last.CompactionID != lastSummary.CompactionID || EstimateContextTokens(last.Messages, last.Tools) >= EstimateContextTokens(snap.Requests[0].Messages, snap.Requests[0].Tools) {
					t.Fatal("summary retry bypassed source-bound complete-context reduction")
				}
			})
		}
	}
}

func TestProviderContextRecoveryPreservesExplicitBudgetAndCancellation(t *testing.T) {
	for _, kind := range []string{"budget", "summary_failure", "no_reduction", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			failure := &ProviderHTTPError{StatusCode: 400, Body: "context_length_exceeded"}
			p := &modelRetryProvider{errors: []error{failure, nil}, responses: []ModelResponse{{}, {Content: "summary", StopReason: "stop"}}}
			a, store := newTestAgent(t, p)
			id, original := recoveryHistory(t, a, store)
			wantRequests, wantStatus := 2, StatusPaused
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "budget":
				a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(1)})
				wantRequests = 1
			case "summary_failure":
				p.responses[1].StopReason = "length"
				a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(2)})
			case "no_reduction":
				p.responses[1].Content = strings.Repeat("long summary ", 500)
				a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(2)})
			case "cancel":
				wantStatus = StatusFailed
				p.onCall = func(n int) {
					if n == 2 {
						cancel()
					}
				}
			}
			snap, err := a.Run(ctx, id)
			if snap.Run.Status != wantStatus || len(snap.Requests) != wantRequests || len(snap.Responses) != wantRequests || messageHash(snap.Messages) != messageHash(original) {
				t.Fatalf("non-progressing recovery: %+v requests=%d err=%v", snap.Run, len(snap.Requests), err)
			}
			if (kind == "summary_failure" || kind == "no_reduction") && snap.Compactions[0].Status != "failed" {
				t.Fatal("truncated summary was accepted")
			}
			if kind != "cancel" && snap.Run.PauseReason != "model request budget exhausted" {
				t.Fatal("technical summary failure introduced an implicit pause")
			}
		})
	}
}

func TestProviderRecoveryRetryRejectsChangedHistory(t *testing.T) {
	p := &modelRetryProvider{
		errors: []error{&ProviderHTTPError{StatusCode: 400, Body: "context_length_exceeded"}, nil, nil},
		responses: []ModelResponse{{}, {Content: "truncated summary", StopReason: "length"},
			{Content: "compact source-bound summary"}},
	}
	a, store := newTestAgent(t, p)
	id, original := recoveryHistory(t, a, store)
	p.onCall = func(n int) {
		if n == 3 {
			if _, err := store.Update(id, func(s *Snapshot) error {
				s.Messages = append(s.Messages, Message{Role: "user", Content: "new constraint while the retry was pending"})
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	snap, err := a.Run(context.Background(), id)
	if err == nil || snap.Run.Status != StatusFailed || len(snap.Requests) != 3 || len(snap.Compactions) != 2 || snap.Compactions[1].Status != "failed" || len(snap.Actions) != 0 {
		t.Fatalf("changed retry source was accepted: %+v err=%v", snap.Run, err)
	}
	if messageHash(snap.Messages[:len(original)]) != messageHash(original) || len(snap.Messages) != len(original)+1 || snap.Responses[2].Error == "" {
		t.Fatal("source failure lost the original history or failed response")
	}
	for _, event := range snap.Events {
		if event.Type == "context_recovery_ready" {
			t.Fatal("changed retry source received a ready receipt")
		}
	}
}

func TestProviderStreamSinkEOFDoesNotBecomeNetworkRetry(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "partial"}}}})
		fmt.Fprintf(w, "data: %s\n\n", chunk)
	}))
	defer server.Close()
	p := &OpenAIProvider{BaseURL: server.URL}
	_, err := p.CompleteStream(context.Background(), ModelRequest{}, func(StreamDelta) error { return io.EOF })
	if !errors.Is(err, io.EOF) || modelRetryDelay(err, 1) != 0 {
		t.Fatalf("sink failure retried as transport: %v", err)
	}
}

// Seed the durable point after recovery admission but before summary response
// settlement. This is also the on-disk state after a process interruption.
func pendingRecovery(t *testing.T, a *Agent, store *Store) (string, ModelRequest) {
	t.Helper()
	id, _ := recoveryHistory(t, a, store)
	var summary ModelRequest
	_, err := store.Update(id, func(s *Snapshot) error {
		messages, estimate, _, err := projectContext(s, nil, "")
		if err != nil {
			return err
		}
		failed := ModelRequest{ID: id + "-0001", PromptHash: s.Run.PromptHash, Messages: messages, ContextTokens: estimate, CreatedAt: time.Now().UTC()}
		_, _, _, err = projectSemanticContextMode(s, nil, "", true)
		var plan *summaryNeeded
		if !errors.As(err, &plan) {
			return fmt.Errorf("missing recovery plan: %v", err)
		}
		summary = ModelRequest{ID: id + "-0002", PromptHash: s.Run.PromptHash, Purpose: "context_summary", Messages: plan.messages, CompactionID: "compact-" + id + "-0002", CreatedAt: time.Now().UTC()}
		plan.record.RequestID, plan.record.ID = summary.ID, summary.CompactionID
		s.Requests = append(s.Requests, failed, summary)
		s.Responses = append(s.Responses, ResponseRecord{RequestID: failed.ID, Error: "context_length_exceeded"})
		s.Compactions = append(s.Compactions, plan.record)
		s.Run.RequestSeq, s.Run.Status, s.Run.PauseReason = 2, StatusPaused, "operator pause"
		addEvent(s, "context_recovery_started", "fixture recovery admitted", map[string]string{"request_id": failed.ID, "summary_request_id": summary.ID, "source_hash": messageHash(s.Messages)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return id, summary
}

func TestProviderRecoveryPauseCommitsReductionBeforeSummaryReuse(t *testing.T) {
	for _, reduce := range []bool{true, false} {
		t.Run(fmt.Sprint(reduce), func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id, summary := pendingRecovery(t, a, store)
			text := "compact verified history"
			if !reduce {
				text = strings.Repeat("oversized summary ", 500)
			}
			retries := 0
			snap, _, err := a.settlePiResponse(context.Background(), id, summary, ModelResponse{Content: text, StopReason: "stop"}, nil, &retries)
			if err != nil || snap.Run.Status != StatusPaused {
				t.Fatalf("pause lost: %+v %v", snap.Run, err)
			}
			ready := false
			for _, event := range snap.Events {
				ready = ready || event.Type == "context_recovery_ready"
			}
			if ready != reduce || (snap.Compactions[0].Status == "completed") != reduce || (snap.Responses[1].Error == "") != reduce {
				t.Fatalf("summary persisted before reduction decision: compaction=%+v ready=%v", snap.Compactions[0], ready)
			}
			reopened, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			restarted, err := New(reopened, successProvider{}, "")
			if err != nil {
				t.Fatal(err)
			}
			restarted.registry = a.registry
			snap, err = restarted.Run(context.Background(), id)
			wantRequests, wantCompactions := 4, 2
			if reduce {
				wantRequests, wantCompactions = 3, 1
			}
			if err != nil || snap.Run.Status != StatusCompleted || len(snap.Requests) != wantRequests || len(snap.Compactions) != wantCompactions {
				t.Fatalf("restart reused an unready summary or stopped correction: %+v requests=%d err=%v", snap.Run, len(snap.Requests), err)
			}
		})
	}
}

func TestProviderRecoveryInterruptedOrUncheckedSummaryCannotRecompactOnRestart(t *testing.T) {
	for _, legacyCompleted := range []bool{false, true} {
		t.Run(fmt.Sprint(legacyCompleted), func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id, _ := pendingRecovery(t, a, store)
			if legacyCompleted {
				_, err := store.Update(id, func(s *Snapshot) error {
					// Old release's crash window: completed persisted, no ready event.
					s.Compactions[0].Status, s.Compactions[0].Summary = "completed", "unchecked summary"
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			restarted, err := New(reopened, successProvider{}, "")
			if err != nil {
				t.Fatal(err)
			}
			restarted.registry = a.registry
			snap, err := restarted.Run(context.Background(), id)
			if err == nil || snap.Run.Status != StatusFailed || snap.Run.PauseReason != "" || len(snap.Requests) != 2 || len(snap.Responses) != 1 || len(snap.Compactions) != 1 || !strings.Contains(snap.Run.Error, "did not commit") {
				t.Fatalf("recovery restarted hidden summary/attempt: %+v requests=%d err=%v", snap.Run, len(snap.Requests), err)
			}
			if err := providerRecoveryProjectionError(&snap); !errors.Is(err, errProviderRecoveryLedger) {
				t.Fatalf("recovery provenance error lost its admission classification: %v", err)
			}
		})
	}
}

func TestProviderRecoveryRejectsHistoryChangedDuringSummary(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id, summary := pendingRecovery(t, a, store)
	_, err := store.Update(id, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "user", Content: "new constraint during summary"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	retries := 0
	snap, _, err := a.settlePiResponse(context.Background(), id, summary, ModelResponse{Content: "short summary", StopReason: "stop"}, nil, &retries)
	if err != nil || snap.Compactions[0].Status != "failed" || snap.Responses[1].Error == "" || snap.Run.Status != StatusPaused {
		t.Fatalf("changed source accepted recovery: %+v %v", snap.Compactions, err)
	}
	for _, event := range snap.Events {
		if event.Type == "context_recovery_ready" {
			t.Fatal("changed history marked ready")
		}
	}
}

func TestProviderSSEInterruptionArchivesUsageWithoutDispatch(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			call := map[string]any{"index": 0, "id": "partial", "function": map[string]string{"name": "write", "arguments": "{}"}}
			chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "discarded partial", "tool_calls": []any{call}}}}, "usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12}})
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			return
		}
		chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": "complete"}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 9, "completion_tokens": 1, "total_tokens": 10}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
	}))
	defer server.Close()
	a, _ := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "recovery-fixture"})
	snap, err := a.Run(context.Background(), createTestRun(t, a))
	if err != nil || snap.Run.Status != StatusCompleted || len(snap.Responses) != 2 || len(snap.Actions) != 0 || len(pendingToolCalls(&snap)) != 0 {
		t.Fatalf("interruption outcome: %+v responses=%d err=%v", snap.Run, len(snap.Responses), err)
	}
	if snap.Responses[0].Error == "" || snap.Responses[0].Response.Content != "discarded partial" || len(snap.Responses[0].Response.ToolCalls) != 1 || *snap.Run.Usage.KnownTotalTokens != 22 {
		t.Fatalf("lost partial response/usage: %+v", snap.Responses)
	}
	for _, message := range snap.Messages {
		if message.Content == "discarded partial" || len(message.ToolCalls) != 0 {
			t.Fatal("failed stream advanced assistant/tool history")
		}
	}
}
