package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestPromptCacheUsageJSONAndStream(t *testing.T) {
	for _, raw := range []string{
		`{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}`,
		`{"prompt_tokens":100,"completion_tokens":7,"total_tokens":107,"prompt_tokens_details":{"cached_tokens":80,"cache_write_tokens":5}}`,
		`{"prompt_tokens":100,"cached_tokens":80}`,
		`{"prompt_tokens":100,"prompt_tokens_details":{"cached_tokens":0}}`,
		`{"prompt_tokens":100}`,
		`{"prompt_tokens":null,"prompt_cache_hit_tokens":null}`,
	} {
		for _, mode := range []string{"json", "sse", "http_error", "sse_error"} {
			t.Run(mode+raw, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch mode {
					case "json":
						fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":`+raw+`}`)
					case "http_error":
						w.WriteHeader(http.StatusTooManyRequests)
						fmt.Fprint(w, `{"error":{"message":"fixture"},"usage":`+raw+`}`)
					case "sse_error":
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"error\":{\"message\":\"fixture\"},\"usage\":"+raw+"}\n\n")
					default:
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":"+raw+"}\n\ndata: [DONE]\n\n")
					}
				}))
				defer server.Close()
				p := &OpenAIProvider{BaseURL: server.URL, Model: "fixture"}
				var response ModelResponse
				var err error
				if mode == "sse" || mode == "sse_error" {
					response, err = p.CompleteStream(context.Background(), ModelRequest{}, nil)
				} else {
					response, err = p.Complete(context.Background(), ModelRequest{})
				}
				if (err != nil) != (mode == "http_error" || mode == "sse_error") {
					t.Fatalf("unexpected result: %v", err)
				}
				var expected TokenUsage
				if err = json.Unmarshal([]byte(raw), &expected); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(response.Usage, &expected) {
					t.Fatalf("usage dropped: %+v != %+v", response.Usage, expected)
				}
				encoded, _ := json.Marshal(response.Usage)
				var restored TokenUsage
				if err = json.Unmarshal(encoded, &restored); err != nil || !reflect.DeepEqual(restored, expected) {
					t.Fatalf("round trip: %s %v", encoded, err)
				}
			})
		}
	}
}

func TestPromptCacheStatsCoverageAndInvalid(t *testing.T) {
	var stats PromptCacheStats
	for _, raw := range []string{
		`{"prompt_tokens":100,"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}`,
		`{"prompt_tokens":50,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":10}}`,
		`{"prompt_tokens":900}`,
		`{"prompt_cache_hit_tokens":20}`,
		`{"prompt_tokens":10,"cached_tokens":11}`,
		`{"prompt_tokens":10,"cached_tokens":-1}`,
		`{"prompt_tokens":10,"cached_tokens":5,"prompt_cache_hit_tokens":6}`,
		`{"prompt_tokens":10,"prompt_cache_hit_tokens":5,"prompt_cache_miss_tokens":4}`,
		`{"prompt_tokens":10,"prompt_tokens_details":{"cached_tokens":8,"cache_write_tokens":3}}`,
	} {
		var usage TokenUsage
		if err := json.Unmarshal([]byte(raw), &usage); err != nil {
			t.Fatal(err)
		}
		stats.add(&usage)
	}
	stats.add(nil)
	if stats.ObservedResponses != 2 || stats.MissingResponses != 3 || stats.InvalidResponses != 5 || stats.KnownReadTokens == nil || *stats.KnownReadTokens != 80 || *stats.ObservedInputTokens != 150 || *stats.KnownWriteTokens != 10 || stats.ReadRatio == nil || *stats.ReadRatio != float64(80)/150 {
		t.Fatalf("incorrect cache stats: %+v", stats)
	}
	var zero PromptCacheStats
	zero.add(&TokenUsage{InputTokens: int64Pointer(0), CachedTokens: int64Pointer(0)})
	if zero.ObservedResponses != 1 || zero.ReadRatio != nil || zero.KnownReadTokens == nil || *zero.KnownReadTokens != 0 {
		t.Fatalf("zero denominator: %+v", zero)
	}
}

func TestPromptCacheUsageRunAndTreeProjection(t *testing.T) {
	a, store := newTestAgent(t, nil)
	root, err := a.Create("cache usage")
	if err != nil {
		t.Fatal(err)
	}
	child, err := a.Create("worker")
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{root.Run.ID, child.Run.ID} {
		_, err = store.Update(id, func(s *Snapshot) error {
			if i == 1 {
				s.Run.ParentRunID, s.Run.ParentTaskID = root.Run.ID, "task"
			}
			s.Requests = []ModelRequest{{ID: "r1"}, {ID: "r2"}}
			s.Responses = []ResponseRecord{
				{RequestID: "r1", Response: ModelResponse{Usage: &TokenUsage{InputTokens: int64Pointer(100), OutputTokens: int64Pointer(2), TotalTokens: int64Pointer(102), PromptCacheHitTokens: int64Pointer(80), PromptCacheMissTokens: int64Pointer(20)}}},
				{RequestID: "r2", Response: ModelResponse{Usage: &TokenUsage{InputTokens: int64Pointer(50)}}},
			}
			refreshUsage(s)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	snap, err := store.Load(root.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if *snap.Run.Usage.KnownInputTokens != 150 || *snap.Run.Usage.PromptCache.KnownReadTokens != 80 || snap.Run.Usage.PromptCache.MissingResponses != 1 {
		t.Fatalf("cache changed inclusive input: %+v", snap.Run.Usage)
	}
	report, err := store.UsageReport(root.Run.ID, CostConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c := report.Usage.PromptCache
	if *report.Usage.KnownInputTokens != 300 || *c.KnownReadTokens != 160 || *c.ObservedInputTokens != 200 || c.ObservedResponses != 2 || c.MissingResponses != 2 || *c.ReadRatio != 0.8 {
		t.Fatalf("incorrect tree denominator: %+v", report.Usage)
	}
}
