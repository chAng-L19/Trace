package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Scripted providers consume the last conversation item, not request-only data.
func lastConversationMessage(request ModelRequest) Message {
	runID := request.ID
	if at := strings.LastIndexByte(runID, '-'); at >= 0 {
		runID = runID[:at]
	}
	history, _ := splitContextObservations(runID, request.Messages)
	return history[len(history)-1]
}

func TestRuntimeObservationsPreserveWirePrefixAndFreshWindow(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	if _, err := store.Update(id, func(s *Snapshot) error {
		s.Assets = []Asset{{ID: "cache-window"}}
		s.Run.Budget.MaxSeconds = 3600
		s.Messages = []Message{{Role: "user", Content: strings.Repeat("stable history ", 100)}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "completed-probe", Name: "read", Arguments: "{}"}}}, {Role: "tool", ToolCallID: "completed-probe", Content: "stable tool result"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	first, err := a.preparePiRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	if _, err = store.Update(id, func(s *Snapshot) error {
		s.Run.ElapsedMillis += 1000
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: "new observation"})
		s.Responses = append(s.Responses, ResponseRecord{RequestID: first.ID, Response: ModelResponse{Content: "prior request completed"}})
		addEvent(s, "asset_updated", "fresh-tail-fact", map[string]string{"id": "cache-window"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	second, err := a.preparePiRequest(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	aWire, bWire := wireMessages(first.Messages), wireMessages(second.Messages)
	for i := 0; i < 4; i++ {
		x, _ := json.Marshal(aWire[i])
		y, _ := json.Marshal(bWire[i])
		if !bytes.Equal(x, y) {
			t.Fatalf("stable wire prefix changed at message %d", i)
		}
	}
	window := func(q ModelRequest) int64 {
		t.Helper()
		text := requestRuntimeObservations(id, q)
		_, body, ok := strings.Cut(text, "\n")
		var data struct {
			Window struct {
				Remaining int64 `json:"remaining_millis"`
			} `json:"scheduling_window"`
		}
		if !ok || json.NewDecoder(strings.NewReader(body)).Decode(&data) != nil {
			t.Fatal("missing planner window")
		}
		return data.Window.Remaining
	}
	if window(second) >= window(first) {
		t.Fatal("runtime budget observation froze")
	}
	if second.Messages[len(second.Messages)-1].ID != id+"-runtime-observations" {
		t.Fatal("runtime observation not last")
	}
	saved, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	archived, _ := json.Marshal(saved.Requests[0])
	if !bytes.Equal(firstJSON, archived) {
		t.Fatal("new projection rewrote archived request")
	}
	if strings.Contains(requestRuntimeObservations(id, first), "fresh-tail-fact") || !strings.Contains(requestRuntimeObservations(id, second), "fresh-tail-fact") || strings.Contains(second.Messages[0].Content, "fresh-tail-fact") {
		t.Fatal("new planner fact did not update only runtime tail")
	}
	for _, m := range saved.Messages {
		if m.ID == id+"-runtime-observations" {
			t.Fatal("projection persisted into history")
		}
	}
	x, _ := json.Marshal(aWire)
	y, _ := json.Marshal(bWire)
	lcp := 0
	for lcp < len(x) && lcp < len(y) && x[lcp] == y[lcp] {
		lcp++
	}
	t.Logf("wire prefix=%d bytes first=%d second=%d remaining=%d->%d", lcp, len(x), len(y), window(first), window(second))
	var bodies [][]json.RawMessage
	var tools []json.RawMessage
	var payloads [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Model    string            `json:"model"`
			Messages []json.RawMessage `json:"messages"`
			Tools    json.RawMessage   `json:"tools"`
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if err := json.Unmarshal(payload, &input); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, input.Messages)
		tools = append(tools, input.Tools)
		payloads = append(payloads, payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	provider := &OpenAIProvider{BaseURL: server.URL, Model: "cache-fixture"}
	for _, request := range []ModelRequest{first, second} {
		if _, err := provider.Complete(context.Background(), request); err != nil {
			t.Fatal(err)
		}
	}
	if len(bodies) != 2 || !bytes.Equal(bodies[0][0], bodies[1][0]) || !bytes.Equal(bodies[0][1], bodies[1][1]) {
		t.Fatal("HTTP request prefix changed")
	}
	if !bytes.Equal(tools[0], tools[1]) {
		t.Fatal("tool schemas changed during prefix comparison")
	}
	for i := 0; i < 4; i++ {
		if !bytes.Equal(bodies[0][i], bodies[1][i]) {
			t.Fatal("complete HTTP history batch changed")
		}
	}
	common := func(a, b []byte) int {
		n := 0
		for n < len(a) && n < len(b) && a[n] == b[n] {
			n++
		}
		return n
	}
	legacy := func(q ModelRequest) []byte {
		messages, _ := splitContextObservations(id, q.Messages)
		messages[0].Content = strings.TrimSuffix(messages[0].Content, runtimeDataHint) + requestRuntimeObservations(id, q)
		value, _ := json.Marshal(struct {
			Model    string               `json:"model"`
			Messages []requestWireMessage `json:"messages"`
			Tools    []ToolDefinition     `json:"tools"`
		}{"cache-fixture", wireMessages(messages), q.Tools})
		return value
	}
	t.Logf("HTTP captured payload LCP legacy_layout=%d stable_layout=%d bytes; system_equal=true tools_equal=true; synthetic byte comparison, not provider KV hit rate", common(legacy(first), legacy(second)), common(payloads[0], payloads[1]))
	response, err := (MockProvider{}).Complete(context.Background(), second)
	if err != nil || !strings.Contains(response.Content, "stable history") || strings.Contains(response.Content, "remaining_millis") {
		t.Fatal("mock confused runtime data with goal", err)
	}
}

func TestRuntimeObservationsRequiredTailAcrossCompaction(t *testing.T) {
	for _, mode := range []string{"projection", "semantic"} {
		t.Run(mode, func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id, _ := longHistory(t, a, store)
			s, err := store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			s.Run.Context.CompactionMode = mode
			s.workerContextMessage = "fresh source unavailable"
			runtime := "Planner observations (data):\n{\"plan\":{\"tasks\":[]}}"
			if mode == "semantic" {
				end := len(s.Messages) - 1
				ids := make([]string, end)
				for i := range ids {
					ids[i] = s.Messages[i].ID
				}
				s.Compactions = []Compaction{{ID: "summary", Method: "llm_semantic", Status: "completed", SourceMessageIDs: ids, SourceHash: messageHash(s.Messages[:end]), Summary: "historical summary"}}
			}
			original := messageHash(s.Messages)
			messages, n, _, err := projectContext(&s, nil, "", runtime)
			if err != nil {
				t.Fatal(err)
			}
			if n > contextInputLimit(s.Run.Context) || messageHash(s.Messages) != original {
				t.Fatal("budget or history changed")
			}
			if messages[len(messages)-1].Content != runtime || messages[len(messages)-2].Content != s.workerContextMessage {
				t.Fatal("required tail lost")
			}
			calls := map[string]bool{}
			for _, m := range messages {
				for _, call := range m.ToolCalls {
					calls[call.ID] = true
				}
				if m.Role == "tool" && !calls[m.ToolCallID] {
					t.Fatal("runtime tail split tool batch")
				}
			}
			manifest := buildContextManifest(&s, ModelRequest{Messages: messages})
			found := false
			for _, section := range manifest.Sections {
				if section.Name == "planner" && section.Required && section.MessageIndex == len(messages)-1 {
					found = true
				}
			}
			if !found {
				t.Fatal("runtime planner missing from manifest")
			}
			if _, _, _, err = projectContext(&s, nil, "", strings.Repeat("mandatory", 2000)); err == nil {
				t.Fatal("oversized runtime dropped")
			}
		})
	}
}

func TestRuntimeObservationMarkersInPromptRemainExact(t *testing.T) {
	s := Snapshot{Run: Run{ID: "literal", Context: ContextConfig{WindowTokens: 20000, OutputReserve: 500}}, Prompt: "literal Planner observations (data):\nRuntime verification contract (data):", Messages: []Message{{Role: "user", Content: "Planner observations (data): is quoted user text"}}}
	messages, _, _, err := projectContext(&s, nil, "", "current observations")
	if err != nil {
		t.Fatal(err)
	}
	if messages[0].Content != s.Prompt+runtimeDataHint || messages[1].Content != s.Messages[0].Content {
		t.Fatal("literal markers changed prompt/history")
	}
}
