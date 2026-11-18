package web

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"trace/internal/agent"
)

// Uses only the established HTTP API so the same file can benchmark old and
// new source trees. Correctness and response size accompany allocation/latency.
func BenchmarkWorkspaceSnapshot(b *testing.B) {
	for _, count := range []int{20000, 50000} {
		b.Run(fmt.Sprintf("events_%d", count), func(b *testing.B) {
			store, err := agent.NewStore(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			a, err := agent.New(store, nil, "")
			if err != nil {
				b.Fatal(err)
			}
			app, err := New(a, Options{})
			if err != nil {
				b.Fatal(err)
			}
			defer app.Close()
			snap, err := a.Create("workspace capacity")
			if err != nil {
				b.Fatal(err)
			}
			snap, err = store.Update(snap.Run.ID, func(v *agent.Snapshot) error {
				v.Messages = []agent.Message{{ID: "answer", Role: "assistant", RequestID: "request", Content: "answer"}}
				v.Requests = []agent.ModelRequest{{ID: "request", Messages: []agent.Message{{Role: "user", Content: strings.Repeat("history ", 1<<20)}}}}
				v.Responses = []agent.ResponseRecord{{RequestID: "request", Response: agent.ModelResponse{Content: "answer"}}}
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
			events := make([]agent.Event, count)
			for i := range events {
				events[i] = agent.Event{Type: "model_stream", Data: map[string]string{"request_id": "request", "content": strings.Repeat("x", 700)}}
			}
			if err = store.AppendEvents(snap.Run.ID, events); err != nil {
				b.Fatal(err)
			}
			for _, view := range []string{"workspace", "full"} {
				b.Run(view, func(b *testing.B) {
					path := "/api/runs/" + snap.Run.ID
					if view == "workspace" {
						path += "?view=workspace"
					}
					req := httptest.NewRequest("GET", path, nil)
					w := httptest.NewRecorder()
					app.Handler().ServeHTTP(w, req)
					var result agent.Snapshot
					if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Run.EventSeq < count || len(result.Messages) != 1 {
						b.Fatalf("invalid workspace response: %d", w.Code)
					}
					if view == "workspace" && (len(result.Events) > 200 || len(result.Requests[0].Messages) != 0) {
						b.Fatal("workspace retained archived payload")
					}
					b.ReportAllocs()
					b.ResetTimer()
					b.ReportMetric(float64(w.Body.Len()), "response-bytes")
					for i := 0; i < b.N; i++ {
						w = httptest.NewRecorder()
						app.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
						if w.Code != 200 {
							b.Fatalf("workspace status: %d", w.Code)
						}
					}
				})
			}
		})
	}
}
