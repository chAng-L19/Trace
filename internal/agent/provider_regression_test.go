package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProviderStreamingLifetimeUsesRunContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"still working\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p := &OpenAIProvider{BaseURL: server.URL, Model: "fixture"}
	seen := false
	_, err := p.CompleteStream(ctx, ModelRequest{}, func(d StreamDelta) error {
		if d.Text == "still working" {
			seen = true
			cancel()
		}
		return nil
	})
	if !seen || err == nil || ctx.Err() != context.Canceled {
		t.Fatalf("stream cancellation lost: seen=%v err=%v context=%v", seen, err, ctx.Err())
	}
}

func TestCompatibleProviderWireToolRoundTrip(t *testing.T) {
	requests := []map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("provider path/auth mismatch")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests = append(requests, body)
		if len(requests) == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"checking","tool_calls":[{"id":"call-1","type":"function","function":{"name":"external","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		} else {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"verified"},"finish_reason":"stop"}]}`)
		}
	}))
	defer server.Close()
	a, _ := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL + "/v1", APIKey: "fixture-token", Model: "fixture"})
	id := createTestRun(t, a)
	snap, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusCompleted || len(requests) != 2 {
		t.Fatal("wire loop incomplete")
	}
	tools, ok := requests[0]["tools"].([]any)
	if !ok || len(tools) != len(a.Registry().Definitions()) {
		t.Fatal("tools schema missing")
	}
	messages := requests[1]["messages"].([]any)
	toolIndex := -1
	for i, raw := range messages {
		message := raw.(map[string]any)
		if message["role"] == "tool" && message["tool_call_id"] == "call-1" {
			content, _ := message["content"].(string)
			if toolIndex >= 0 || !strings.Contains(content, "was not executed") {
				t.Fatalf("duplicate or bad tool wire: %+v", message)
			}
			toolIndex = i
		}
	}
	if toolIndex < 1 {
		t.Fatal("tool wire or preceding assistant missing")
	}
	assistant := messages[toolIndex-1].(map[string]any)
	if assistant["role"] != "assistant" || assistant["content"] != "checking" || assistant["tool_calls"] == nil {
		t.Fatal("assistant wire context lost")
	}
}

func TestProviderInvalidCompletionsNeverComplete(t *testing.T) {
	fixtures := map[string]string{
		"empty":      `{"choices":[{"message":{"content":""},"finish_reason":"stop"}]}`,
		"truncated":  `{"choices":[{"message":{"content":"partial"},"finish_reason":"length"}]}`,
		"unresolved": `{"choices":[{"message":{"content":"claims done"},"finish_reason":"tool_calls"}]}`,
		"duplicate":  `{"choices":[{"message":{"tool_calls":[{"id":"same","function":{"name":"external","arguments":"{}"}},{"id":"same","function":{"name":"external","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	for name, body := range fixtures {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
			defer server.Close()
			a, s := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "fixture"})
			id := createTestRun(t, a)
			if _, err := a.Run(context.Background(), id); err == nil {
				t.Fatal("invalid completion accepted")
			}
			snap, _ := s.Load(id)
			if snap.Run.Status != StatusFailed || len(snap.Responses) != 1 || snap.Run.Error == "" {
				t.Fatalf("failure not preserved: %+v", snap)
			}
		})
	}
}

func TestProviderHTTPFailureAndPromptCorruptionState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fixture outage", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	a, s := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "fixture"})
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(3)})
	id := createTestRun(t, a)
	snap, err := a.Run(context.Background(), id)
	if err != nil || snap.Run.Status != StatusPaused || snap.Run.PauseReason != "model request budget exhausted" || len(snap.Responses) != 3 || !strings.Contains(snap.Responses[2].Error, "503") {
		t.Fatalf("HTTP error lost: %v", err)
	}
	corruptID := createTestRun(t, a)
	s.Update(corruptID, func(s *Snapshot) error { s.Prompt = "tampered"; return nil })
	snap, err = a.Run(context.Background(), corruptID)
	if err == nil || snap.Run.Status != StatusFailed {
		t.Fatal("corruption left active state")
	}
}
