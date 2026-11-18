package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderProbeExplicitDraftAndSecretWithoutMutation(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer stored-key" {
			t.Errorf("wrong request path/auth")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "draft-model" || body["max_tokens"] != float64(64) || body["tools"] != nil || body["stream"] != nil {
			t.Errorf("unexpected probe payload: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()
	s, a := fixture(t, nil)
	config := s.config
	config.Provider = ProviderConfig{URL: upstream.URL, Model: "saved-model", APIKey: "stored-key"}
	request(t, s.Handler(), "PUT", "/api/config", config, 200)
	request(t, s.Handler(), "GET", "/api/config", nil, 200)
	if calls.Load() != 0 {
		t.Fatal("save/GET called provider")
	}
	before, err := os.ReadFile(s.options.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	live := s.a
	w := request(t, s.Handler(), "POST", "/api/provider/probe", ProviderConfig{URL: upstream.URL, Model: "draft-model", APIKey: hiddenSecret}, 200)
	var result providerProbeResult
	if err = json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "success" || calls.Load() != 1 || result.CheckedAt.IsZero() {
		t.Fatalf("probe result %+v calls %d", result, calls.Load())
	}
	if strings.Contains(w.Body.String(), "stored-key") || strings.Contains(w.Body.String(), upstream.URL) {
		t.Fatal("probe exposed credentials/config")
	}
	after, err := os.ReadFile(s.options.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || s.a != live || s.config.Provider.Model != "saved-model" {
		t.Fatal("probe mutated config/Agent")
	}
	runs, err := a.Store().List()
	if err != nil || len(runs) != 0 {
		t.Fatalf("probe created run/evidence: %v %v", runs, err)
	}
}

func TestProviderProbeResponsesAndNoSecretEcho(t *testing.T) {
	tests := []struct {
		name               string
		http               int
		body, status, code string
	}{
		{"unauthorized", 401, `key-secret url-secret`, "failed", "authentication"},
		{"notfound", 404, `key-secret url-secret`, "failed", "not_found"},
		{"rate", 429, `key-secret url-secret`, "failed", "rate_limit"},
		{"upstream", 500, `key-secret url-secret`, "failed", "http"},
		{"malformed", 200, `key-secret url-secret`, "failed", "response"},
		{"empty", 200, `{"choices":[{"message":{}}]}`, "failed", "response"},
		{"error", 200, `{"error":{"message":"key-secret url-secret"}}`, "failed", "response"},
		{"toolonly", 200, `{"choices":[{"message":{"tool_calls":[{"id":"call","function":{"name":"x","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, "failed", "response"},
		{"reasoning", 200, `{"choices":[{"message":{"reasoning_content":"thinking"},"finish_reason":"length"}]}`, "responded", "incomplete_reply"},
		{"length", 200, `{"choices":[{"message":{"content":"O"},"finish_reason":"length"}]}`, "responded", "incomplete_reply"},
		{"filtered", 200, `{"choices":[{"message":{"content":"blocked"},"finish_reason":"content_filter"}]}`, "failed", "response"},
		{"tool_finish", 200, `{"choices":[{"message":{"content":"call"},"finish_reason":"tool_calls"}]}`, "failed", "response"},
		{"cancelled_finish", 200, `{"choices":[{"message":{"content":"partial"},"finish_reason":"cancelled"}]}`, "failed", "response"},
		{"unknown_finish", 200, `{"choices":[{"message":{"content":"partial"},"finish_reason":"key-secret"}]}`, "failed", "response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(test.http); w.Write([]byte(test.body)) }))
			defer upstream.Close()
			s, _ := fixture(t, nil)
			live := s.a
			w := request(t, s.Handler(), "POST", "/api/provider/probe", ProviderConfig{URL: upstream.URL + "/?token=url-secret", Model: "m", APIKey: "key-secret"}, 200)
			var result providerProbeResult
			json.Unmarshal(w.Body.Bytes(), &result)
			if result.Status != test.status || result.Code != test.code {
				t.Fatalf("%+v", result)
			}
			if strings.Contains(w.Body.String(), "key-secret") || strings.Contains(w.Body.String(), "url-secret") {
				t.Fatal("upstream error secret leaked")
			}
			if s.a != live || s.config.Provider.URL != "" {
				t.Fatal("failed probe mutated active config")
			}
		})
	}
}

func TestProviderProbeMockConfigurationAndNetworkErrors(t *testing.T) {
	s, _ := fixture(t, nil)
	for _, test := range []struct {
		config       ProviderConfig
		status, code string
	}{
		{ProviderConfig{Model: "m"}, "mock", "mock"},
		{ProviderConfig{URL: "file:///key-secret", Model: "m"}, "failed", "configuration"},
		{ProviderConfig{URL: "http://localhost"}, "failed", "configuration"},
		{ProviderConfig{URL: "http://127.0.0.1:0/?token=url-secret", Model: "m"}, "failed", "network"},
	} {
		w := request(t, s.Handler(), "POST", "/api/provider/probe", test.config, 200)
		var result providerProbeResult
		json.Unmarshal(w.Body.Bytes(), &result)
		if result.Status != test.status || result.Code != test.code {
			t.Fatalf("%+v", result)
		}
		if strings.Contains(w.Body.String(), "key-secret") || strings.Contains(w.Body.String(), "url-secret") {
			t.Fatal("local error secret leaked")
		}
	}
}

func TestProviderProbeCancelAndServerClose(t *testing.T) {
	for _, mode := range []string{"request", "close"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				close(entered)
				<-r.Context().Done()
			}))
			defer upstream.Close()
			s, _ := fixture(t, nil)
			body, _ := json.Marshal(ProviderConfig{URL: upstream.URL, Model: "m"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest("POST", "/api/provider/probe", bytes.NewReader(body)).WithContext(ctx)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { s.Handler().ServeHTTP(w, r); close(done) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("probe did not start")
			}
			// A stalled probe does not hold the service mutex across its network call.
			getDone := make(chan struct{})
			go func() { request(t, s.Handler(), "GET", "/api/config", nil, 200); close(getDone) }()
			select {
			case <-getDone:
			case <-time.After(time.Second):
				t.Fatal("probe held configuration mutex")
			}
			if mode == "request" {
				cancel()
			} else {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("cancel/close did not drain probe")
			}
			var result providerProbeResult
			json.Unmarshal(w.Body.Bytes(), &result)
			if result.Code != "cancelled" {
				t.Fatalf("cancel %+v", result)
			}
			if mode == "close" {
				request(t, s.Handler(), "POST", "/api/provider/probe", ProviderConfig{URL: upstream.URL, Model: "m"}, 503)
			}
		})
	}
}
