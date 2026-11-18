package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestProviderDiagnosticsSeparatesBodySemanticAndIdentity(t *testing.T) {
	for _, kind := range []string{"headers_only", "role_only", "thinking", "tool_id_only", "tool_arguments", "conflicting_model"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				if kind == "headers_only" {
					return
				}
				fmt.Fprint(w, "data: {\"id\":\"response-1\",\"model\":\"reported-b\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
				w.(http.Flusher).Flush()
				time.Sleep(25 * time.Millisecond)
				delta := `{}`
				switch kind {
				case "thinking":
					delta = `{"reasoning_content":"thinking"}`
				case "tool_id_only":
					delta = `{"tool_calls":[{"index":0,"id":"c"}]}`
				case "tool_arguments":
					delta = `{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}`
				case "conflicting_model":
					delta = `{"content":"ok"}`
				}
				model := "reported-b"
				if kind == "conflicting_model" {
					model = "reported-c"
				}
				fmt.Fprintf(w, "data: {\"model\":%q,\"choices\":[{\"delta\":%s,\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", model, delta)
			}))
			defer server.Close()
			p := &OpenAIProvider{BaseURL: server.URL, Model: "requested-a"}
			response, err := p.CompleteStream(context.Background(), ModelRequest{}, nil)
			if (err != nil) != (kind == "headers_only") {
				t.Fatalf("completion: %v", err)
			}
			d := response.Diagnostics
			if d == nil || d.ResponseMillis == nil {
				t.Fatalf("missing diagnostic: %+v", response)
			}
			if kind == "headers_only" {
				if d.FirstByteMillis != nil || d.FirstSemanticDeltaMillis != nil || response.ReportedModel != "" {
					t.Fatalf("headers became model output: %+v", response)
				}
				return
			}
			semantic := kind == "thinking" || kind == "tool_arguments" || kind == "conflicting_model"
			if d.FirstByteMillis == nil || (d.FirstSemanticDeltaMillis != nil) != semantic {
				t.Fatalf("semantic classification: %+v", d)
			}
			if semantic && (*d.FirstSemanticDeltaMillis < *d.FirstByteMillis || *d.FirstSemanticDeltaMillis > *d.ResponseMillis) {
				t.Fatalf("clock order: %+v", d)
			}
			if response.ReportedModel != "reported-b" || response.ProviderResponseID != "response-1" || response.IdentityConflict != (kind == "conflicting_model") || p.Model != "requested-a" {
				t.Fatalf("identity routing changed: %+v", response)
			}
		})
	}
}

func TestProviderOptionalMetadataNeverChangesResponseAcceptance(t *testing.T) {
	for _, metadata := range []string{
		`"id":"r","model":"reported","service_tier":"standard","reported_cost":{"amount":0,"currency":"USD","unit":"currency"}`,
		`"id":7,"model":{},"service_tier":false,"reported_cost":17`,
		`"usage":{"cost":0,"cost_currency":"USD","cost_unit":"currency"}`,
		`"usage":{"cost":0,"cost_currency":{},"cost_unit":17}`,
	} {
		response, err := decodeResponse(strings.NewReader(`{` + metadata + `,"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
		if err != nil || response.Content != "ok" || response.ReportedCost == nil {
			t.Fatalf("optional metadata changed acceptance: %+v %v", response, err)
		}
	}
	legacy, err := decodeResponse(strings.NewReader(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))
	if err != nil || legacy.ReportedModel != "" || legacy.ReportedCost != nil {
		t.Fatalf("missing became reported: %+v %v", legacy, err)
	}
}

func TestProviderReportedCostStreamUpdatesAreNotAdded(t *testing.T) {
	var response ModelResponse
	for _, raw := range []string{
		`{"usage":{"cost":0.1}}`,
		`{"usage":{"cost":0.2,"cost_currency":"USD","cost_unit":"currency"}}`,
		`{"usage":{"cost":0.3}}`,
	} {
		var wire wireResponse
		if err := json.Unmarshal([]byte(raw), &wire); err != nil {
			t.Fatal(err)
		}
		response.observeMetadata(wire)
	}
	if response.ReportedCost == nil || response.ReportedCost.Amount != "0.3" || response.ReportedCost.Currency != "USD" || response.ReportedCost.Unit != "currency" || response.ReportedCost.Conflict {
		t.Fatalf("stream cost accumulated or metadata lost: %+v", response.ReportedCost)
	}
	var conflict wireResponse
	if err := json.Unmarshal([]byte(`{"usage":{"cost":0.3,"cost_currency":"EUR"}}`), &conflict); err != nil {
		t.Fatal(err)
	}
	response.observeMetadata(conflict)
	if !response.ReportedCost.Conflict {
		t.Fatal("currency conflict accepted")
	}
}

func TestProviderFailureRetainsDiagnosticMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"id":"failed-response","model":"reported-b","error":{"message":"rate limit"},"usage":{"cost":0,"cost_currency":"USD","cost_unit":"currency"}}`)
	}))
	defer server.Close()
	p := &OpenAIProvider{BaseURL: server.URL, Model: "requested-a"}
	for _, stream := range []bool{false, true} {
		var response ModelResponse
		var err error
		if stream {
			response, err = p.CompleteStream(context.Background(), ModelRequest{}, nil)
		} else {
			response, err = p.Complete(context.Background(), ModelRequest{})
		}
		if err == nil || response.ReportedModel != "reported-b" || response.ProviderResponseID != "failed-response" || response.ReportedCost == nil || response.ReportedCost.Amount != "0" || response.Diagnostics == nil || response.Diagnostics.FirstByteMillis == nil || response.Diagnostics.FirstSemanticDeltaMillis != nil {
			t.Fatalf("failed response diagnostics missing: %+v %v", response, err)
		}
	}
}

func TestPiDiagnosticsPersistsActualProviderWithoutChangingRoute(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Model string `json:"model"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || request.Model != "requested-a" {
					t.Error("request route changed")
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"model": "reported-b", "id": "reply-1", "choices": []any{map[string]any{"message": map[string]any{"content": fixtureReport("diagnostic fixture")}, "finish_reason": "stop"}}})
			}))
			defer server.Close()
			var store *Store
			var err error
			if backend == "sqlite" {
				store = sqliteTestStore(t)
			} else {
				store, err = NewStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
			}
			a, err := New(store, &OpenAIProvider{BaseURL: server.URL, Model: "requested-a"}, "")
			if err != nil {
				t.Fatal(err)
			}
			snap, err := a.Run(piMigrationContext(t), createTestRun(t, a))
			if err != nil || snap.Run.Status != StatusCompleted || len(snap.Requests) != 1 || len(snap.Responses) != 1 {
				t.Fatalf("pi path: requests=%d responses=%d err=%v", len(snap.Requests), len(snap.Responses), err)
			}
			before, err := store.readUsageSnapshot(snap.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			projection, err := store.Diagnostics(snap.Run.ID, "USD", 40)
			if err != nil || len(projection.Requests) != 1 {
				t.Fatalf("diagnostic projection: %+v %v", projection, err)
			}
			row := projection.Requests[0]
			if row.RequestedModel != "requested-a" || row.ReportedModel != "reported-b" || row.IdentityStatus != "reported" || row.Timing == nil || row.Timing.PrepareMillis == nil || row.Timing.ArchiveMillis == nil || row.Timing.ManifestStoreMillis == nil || row.Timing.FirstSemanticDeltaMillis == nil || row.Cost.Amount != nil {
				t.Fatalf("real request metadata missing: %+v", row)
			}
			after, err := store.readUsageSnapshot(snap.Run.ID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("diagnostics mutated persisted execution")
			}
		})
	}
}
