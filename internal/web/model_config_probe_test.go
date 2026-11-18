package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"trace/internal/mcp"
)

func TestModelConfigurationRetainsInFlightMCPProbeOnlyForSameRegistry(t *testing.T) {
	for _, fullRebuild := range []bool{false, true} {
		t.Run(map[bool]string{false: "model-only", true: "full-rebuild"}[fullRebuild], func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseProbe := func() { once.Do(func() { close(release) }) }
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				var rpc struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
					t.Error(err)
					return
				}
				if rpc.ID == 0 {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				result := any(map[string]any{})
				if rpc.Method == "initialize" {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					result = map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}}
				} else if rpc.Method == "tools/list" {
					result = map[string]any{"tools": []any{}}
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
			}))
			t.Cleanup(remote.Close)
			s, original := modelConfigFixture(t)
			s.config.MCP = []mcp.Config{{Name: "fixture", URL: remote.URL}}
			t.Cleanup(releaseProbe)
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				r := httptest.NewRequest(http.MethodPost, "/api/mcp/probe", strings.NewReader(`{"name":"fixture"}`))
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				s.Handler().ServeHTTP(w, r)
				done <- w
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("probe did not reach the barrier")
			}
			next := redactConfig(s.config)
			next.ActiveProvider = "b"
			if fullRebuild {
				next.Context.WindowTokens++
			}
			request(t, s.Handler(), http.MethodPut, "/api/config", next, http.StatusOK)
			if (s.a.Registry() == original.Registry()) == fullRebuild {
				t.Fatal("registry identity did not reflect the publication path")
			}
			releaseProbe()
			select {
			case w := <-done:
				if w.Code != http.StatusOK {
					t.Fatal("probe did not complete")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("probe did not exit")
			}
			var rows []mcpServiceStatus
			w := request(t, s.Handler(), http.MethodGet, "/api/mcp/status", nil, http.StatusOK)
			if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 1 {
				t.Fatal("invalid MCP status response")
			}
			if fullRebuild {
				if rows[0].LastAttempt != nil {
					t.Fatal("stale probe overwrote rebuilt registry status")
				}
			} else if rows[0].LastAttempt == nil || rows[0].LastAttempt.State != "connected" {
				t.Fatal("model-only save discarded a current MCP probe result")
			}
		})
	}
}
