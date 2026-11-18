package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSearchSourceProbeChecksAccountShapeAndNeverFollowsRedirect(t *testing.T) {
	for _, tc := range []struct{ source, path, body, status string }{{"fofa", "/api/v1/info/my", `{"error":false,"email":"account@example.test"}`, "authenticated"}, {"quake", "/api/v3/user/info", `{"code":0,"data":{"id":"account"}}`, "authenticated"}, {"shodan", "/api-info", `{"query_credits":42,"plan":"fixture"}`, "authenticated"}, {"fofa", "/api/v1/info/my", `{"error":false}`, "invalid_response"}} {
		t.Run(tc.source+tc.status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path || r.Method != "GET" {
					t.Error("not account-only GET")
				}
				if tc.source == "quake" && r.Header.Get("X-QuakeToken") != "account-key" {
					t.Error("missing account credential")
				}
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			a, _ := newTestAgent(t, successProvider{})
			if err := a.ConfigureSearchSources(map[string]SearchSourceConfig{tc.source: {Enabled: true, APIKey: "account-key", BaseURL: server.URL}}); err != nil {
				t.Fatal(err)
			}
			out, err := a.ProbeSearchSource(context.Background(), tc.source)
			if err != nil || out.Status != tc.status {
				t.Fatalf("probe=%+v err=%v", out, err)
			}
			if tc.source == "shodan" && (out.AvailableCredits == nil || *out.AvailableCredits != 42) {
				t.Fatal("explicit quota receipt missing")
			}
		})
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/redirect-with-key", http.StatusFound)
	}))
	defer server.Close()
	a, _ := newTestAgent(t, successProvider{})
	if err := a.ConfigureSearchSources(map[string]SearchSourceConfig{"fofa": {Enabled: true, APIKey: "account-key", BaseURL: server.URL}}); err != nil {
		t.Fatal(err)
	}
	out, err := a.ProbeSearchSource(context.Background(), "fofa")
	if err != nil || out.Status != "upstream_error" || calls.Load() != 1 {
		t.Fatal("account request followed redirect/retried")
	}
}

func TestSearchSourceConfigRejectsInvalidBoundsAndSeparatesCredentials(t *testing.T) {
	a, _ := newTestAgent(t, successProvider{})
	for _, c := range []SearchSourceConfig{{Enabled: true}, {APIKey: "[stored secret]"}, {BaseURL: "https://user:password@host.invalid"}, {BaseURL: "https://host.invalid?key=hidden"}, {BaseURL: "http://external.invalid"}, {MaxConcurrency: 6}, {MinIntervalMS: -1}, {MaxPages: 101}, {MaxRequests: -1}, {MaxResults: -1}} {
		if err := a.ConfigureSearchSources(map[string]SearchSourceConfig{"fofa": c}); err == nil {
			t.Fatalf("accepted invalid config: %+v", c)
		}
	}
	if err := a.ConfigureSearchSources(map[string]SearchSourceConfig{"unknown": {}}); err == nil {
		t.Fatal("unknown source accepted")
	}
	if err := a.ConfigureSearchSources(map[string]SearchSourceConfig{"fofa": {Enabled: true, APIKey: "data-key"}}); err != nil {
		t.Fatal(err)
	}
	if len(a.modelProviders) != 0 || a.SearchSourceStatus()[0].Source != "fofa" {
		t.Fatal("data source became a model provider")
	}
}
