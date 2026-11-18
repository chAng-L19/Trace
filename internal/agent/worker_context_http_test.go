package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkerContextHTTPChildAndResume(t *testing.T) {
	a, store, id := contextFixture(t, "source-proof")
	requests := make(chan []wireMessage, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input struct {
			Messages []wireMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			http.Error(w, err.Error(), 400)
			return
		}
		requests <- input.Messages
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "fixture response"}, "finish_reason": "stop"}}})
	}))
	defer server.Close()
	a.provider = &OpenAIProvider{BaseURL: server.URL, Model: "worker-context-fixture"}
	child := contextChild(t, a, store, id)
	assertDelivery := func(messages []wireMessage, expected string) {
		t.Helper()
		if strings.Contains(messages[0].Content, "chosen-asset-value") {
			t.Fatal("source in system wire message")
		}
		count := 0
		for _, m := range messages {
			if strings.HasPrefix(m.Content, "Worker task context (") {
				count++
				if m.Role != "user" || !strings.Contains(m.Content, expected) {
					t.Fatalf("wrong wire context: %+v", m)
				}
			}
		}
		if count != 1 {
			t.Fatalf("wire context count %d", count)
		}
	}
	assertDelivery(<-requests, `"status":"source_verified"`)
	if _, err := store.Update(id, func(s *Snapshot) error { s.Evidence[0].Content += "tampered"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.QueueMessage(child.Run.ID, "follow_up", "resume"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), child.Run.ID); err != nil {
		t.Fatal(err)
	}
	assertDelivery(<-requests, `"status":"invalid"`)
}
