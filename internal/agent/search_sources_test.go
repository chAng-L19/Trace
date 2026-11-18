package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSourceSearchHTTPContractsCASAndCandidates(t *testing.T) {
	for _, source := range []string{"fofa", "quake", "shodan"} {
		t.Run(source, func(t *testing.T) {
			var received atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received.Add(1)
				switch source {
				case "fofa":
					query, _ := base64.StdEncoding.DecodeString(r.URL.Query().Get("qbase64"))
					if r.URL.Path != "/api/v1/search/all" || r.URL.Query().Get("key") != "api-fixture-secret" || string(query) != "domain=example.test" || r.URL.Query().Get("page") != "2" {
						t.Error("FOFA request mismatch")
					}
					fmt.Fprint(w, `{"error":false,"size":201,"results":[["https://example.test/?token=never-context","192.0.2.1",443,"https","api-fixture-secret","example.test"]]}`)
				case "quake":
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					if r.Method != "POST" || r.URL.Path != "/api/v3/search/quake_service" || r.Header.Get("X-QuakeToken") != "api-fixture-secret" || body["start"] != float64(100) {
						t.Error("Quake request mismatch")
					}
					fmt.Fprint(w, `{"code":0,"data":[{"ip":"192.0.2.1","port":443,"transport":"tcp","domain":"example.test","service":{"http":{"title":"page"}}}],"meta":{"pagination":{"total":201}}}`)
				case "shodan":
					if r.URL.Path != "/shodan/host/search" || r.URL.Query().Get("key") != "api-fixture-secret" || r.URL.Query().Get("page") != "2" {
						t.Error("Shodan request mismatch")
					}
					fmt.Fprint(w, `{"total":201,"matches":[{"ip_str":"192.0.2.1","port":443,"transport":"tcp","hostnames":["example.test"],"timestamp":"2026-11-09T00:00:00Z"}]}`)
				}
			}))
			defer server.Close()
			args := fmt.Sprintf(`{"source":%q,"query":"domain=example.test","page":2}`, source)
			a, store := newTestAgent(t, manualProvider(ToolCall{ID: "lookup", Name: "source_search", Arguments: args}))
			if err := a.ConfigureSearchSources(map[string]SearchSourceConfig{source: {Enabled: true, APIKey: "api-fixture-secret", BaseURL: server.URL}}); err != nil {
				t.Fatal(err)
			}
			id := createTestRun(t, a)
			s, err := a.Run(context.Background(), id)
			if err != nil || len(s.Actions) != 1 || s.Actions[0].Result == nil {
				t.Fatalf("ledger: %v %+v", err, s.Actions)
			}
			var result SourceSearchResult
			if err = json.Unmarshal([]byte(s.Actions[0].Result.Stdout), &result); err != nil {
				t.Fatal(err)
			}
			if received.Load() != 1 || result.Status != "partial" || result.NextPage != 3 || result.Total == nil || *result.Total != 201 || len(result.Items) != 1 || result.Items[0].Status != "candidate_not_verified" || result.Items[0].IP != "192.0.2.1" {
				t.Fatalf("page: %+v", result)
			}
			if len(s.Assets) != 0 || len(s.Actions[0].Result.Artifacts) != 1 || result.Raw == nil {
				t.Fatal("search fabricated target or lost raw source")
			}
			if strings.Contains(s.Actions[0].Result.Stdout, "api-fixture-secret") || strings.Contains(s.Actions[0].Result.Stdout, "never-context") {
				t.Fatal("credential entered normalized context")
			}
			f, err := store.OpenArtifact(result.Raw.Hash)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(f)
			f.Close()
			if err != nil || promptHash(string(raw)) != result.Raw.Hash {
				t.Fatal("raw CAS changed")
			}
			if _, err = a.builtin(context.Background(), id, ToolCall{Name: "artifact_read", Arguments: fmt.Sprintf(`{"hash":%q,"offset":1,"limit":1}`, result.Raw.Hash)}); err == nil {
				t.Fatal("search receipt became model-readable")
			}
			if s.SearchUsage[source].Requests != 1 || s.SearchUsage[source].ResultSlots != 100 {
				t.Fatalf("missing quota receipt: %+v", s.SearchUsage)
			}
		})
	}
}

func TestSourceSearchFailuresAndUncertainNeverReplay(t *testing.T) {
	for _, tc := range []struct {
		name, body, status string
		httpStatus         int
	}{{"auth", `{}`, "auth_error", 401}, {"rate", `{}`, "rate_limited", 429}, {"quota", `{"error":true,"errmsg":"quota exhausted api-fixture-secret"}`, "quota_exhausted", 200}, {"invalid", `not-json`, "invalid_response", 200}, {"empty", `{"error":false,"results":[],"size":0}`, "completed", 200}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "8")
				w.WriteHeader(tc.httpStatus)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			a, _ := newTestAgent(t, successProvider{})
			id := createTestRun(t, a)
			if err := a.ConfigureSearchSources(map[string]SearchSourceConfig{"fofa": {Enabled: true, APIKey: "api-fixture-secret", BaseURL: server.URL}}); err != nil {
				t.Fatal(err)
			}
			out, err := a.sourceSearch(context.Background(), id, ToolCall{ID: "one", Arguments: `{"source":"fofa","query":"x"}`})
			if err != nil || out.Status != tc.status || out.Raw == nil {
				t.Fatalf("status=%+v err=%v", out, err)
			}
			if tc.name == "rate" && (out.RetryAfterSeconds != 8 || a.SearchSourceStatus()[0].RetryAfterSeconds < 7) {
				t.Fatal("shared rate limit missing")
			}
		})
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer server.Close()
	args := `{"source":"fofa","query":"uncertain"}`
	a, store := newTestAgent(t, manualProvider(ToolCall{ID: "one", Name: "source_search", Arguments: args}))
	if err := a.ConfigureSearchSources(map[string]SearchSourceConfig{"fofa": {Enabled: true, APIKey: "api-fixture-secret", BaseURL: server.URL}}); err != nil {
		t.Fatal(err)
	}
	id := createTestRun(t, a)
	s, _ := a.Run(context.Background(), id)
	if len(s.Actions) != 1 || s.Actions[0].Status != "uncertain" || requests.Load() != 1 {
		t.Fatalf("unknown outcome not durable: %+v", s.Actions)
	}
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restart, err := New(reopened, successProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = restart.ConfigureSearchSources(map[string]SearchSourceConfig{"fofa": {Enabled: true, APIKey: "api-fixture-secret", BaseURL: server.URL}}); err != nil {
		t.Fatal(err)
	}
	out, err := restart.sourceSearch(context.Background(), id, ToolCall{ID: "different-call", Arguments: args})
	if err != nil || out.Status != "outcome_unknown" || requests.Load() != 1 {
		t.Fatal("uncertain search replayed across call IDs/restart")
	}
}

func TestSourceSearchRootQuotaAtomicAndSharedCredentialConcurrency(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	root := createTestRun(t, a)
	child := createTestRun(t, a)
	if _, err := store.Update(child, func(s *Snapshot) error { s.Run.ParentRunID, s.Run.ParentTaskID = root, "recon"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(root, func(s *Snapshot) error {
		s.Workers = append(s.Workers, AgentWorker{ID: "worker", RunID: child, TaskID: "recon"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	secondStore, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	secondAgent, err := New(secondStore, successProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	c := SearchSourceConfig{MaxRequests: 3, MaxResults: 250}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := root
			caller := a
			if i%2 == 0 {
				id = child
				caller = secondAgent
			}
			ok, err := caller.reserveSearchUsage(id, SourceSearchRequest{Source: "fofa", Query: fmt.Sprint(i), Page: 1, PageSize: 100}, c)
			if err != nil {
				t.Error(err)
			}
			if ok {
				admitted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	s, err := store.Load(root)
	if err != nil || admitted.Load() != 2 || s.SearchUsage["fofa"].Requests != 2 {
		t.Fatal("root quota bypassed by query/child/concurrency")
	}
	other := createTestRun(t, a)
	if ok, err := a.reserveSearchUsage(other, SourceSearchRequest{Source: "fofa", PageSize: 100}, c); err != nil || !ok {
		t.Fatal("independent root quota shared")
	}
	gate := &searchSourceGate{}
	config := SearchSourceConfig{MaxConcurrency: 1}
	_, _, release := gate.acquire(config)
	if release == nil {
		t.Fatal("initial gate")
	}
	if status, _, _ := gate.acquire(config); status != "busy" {
		t.Fatal("shared credential limit exceeded")
	}
	release()
	if _, _, release = gate.acquire(config); release == nil {
		t.Fatal("gate did not release")
	}
	release()
	gate.mu.Lock()
	gate.blockedUntil = time.Now().Add(time.Minute)
	gate.mu.Unlock()
	if status, _, _ := gate.acquire(config); status != "rate_limited" {
		t.Fatal("upstream cooldown bypassed")
	}
}
