package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"trace/internal/agent"
	"trace/internal/mcp"
)

func TestModelConfigurationDuringHTTPRunKeepsBindingAndRotatesOnResume(t *testing.T) {
	type observedRequest struct {
		Path, Authorization, Model, Effort string
	}
	observed := make(chan observedRequest, 8)
	releaseFirst := make(chan struct{})
	var modelCalls, mcpCalls, mcpClosed atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/mcp" {
			if r.Method == http.MethodDelete {
				mcpClosed.Add(1)
				w.WriteHeader(http.StatusNoContent)
				return
			}
			mcpCalls.Add(1)
			var rpc struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if rpc.ID == 0 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			result := any(map[string]any{})
			switch rpc.Method {
			case "initialize":
				w.Header().Set("Mcp-Session-Id", "runtime-profile-fixture")
				result = map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}}
			case "tools/list":
				result = map[string]any{"tools": []any{}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
			return
		}
		var body struct {
			Model  string `json:"model"`
			Effort string `json:"reasoning_effort"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		first := modelCalls.Add(1) == 1
		observed <- observedRequest{r.URL.Path, r.Header.Get("Authorization"), body.Model, body.Effort}
		if first {
			select {
			case <-releaseFirst:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"profile-search","type":"function","function":{"name":"tool_search","arguments":"{\"query\":\"profile-fixture-no-such-tool\"}"}}]},"finish_reason":"tool_calls"}]}`))
			case <-r.Context().Done():
			}
			return
		}
		<-r.Context().Done()
	}))
	t.Cleanup(remote.Close)
	s, _ := modelConfigFixture(t)
	profileA := s.config.Providers["a"]
	profileA.URL, profileA.APIKey, profileA.ImageInput = remote.URL+"/a", "a-old-fixture", true
	s.config.Providers["a"] = profileA
	profileB := s.config.Providers["b"]
	profileB.URL = remote.URL + "/b"
	s.config.Providers["b"] = profileB
	s.config.MCP = []mcp.Config{{Name: "retained", URL: remote.URL + "/mcp"}}
	provider, providers, roles, err := s.config.ModelConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	s.a, err = s.a.WithModelConfiguration(provider, providers, roles)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.a.AddMCP(context.Background(), s.config.MCP[0]); err != nil {
		t.Fatal(err)
	}
	if err = saveConfig(s.options.ConfigPath, s.config); err != nil {
		t.Fatal(err)
	}
	captured, initialMCPCalls := s.a, mcpCalls.Load()
	h := s.Handler()
	waitRequest := func(want observedRequest) {
		t.Helper()
		select {
		case got := <-observed:
			if got != want {
				t.Fatalf("provider request = %+v, want %+v", got, want)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("provider request did not arrive")
		}
	}
	pause := func(id string) agent.Snapshot {
		t.Helper()
		request(t, h, "POST", "/api/runs/"+id+"/pause", map[string]any{}, http.StatusOK)
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			s.mu.Lock()
			active := s.active[id]
			s.mu.Unlock()
			if !active {
				snapshot, err := s.store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				if snapshot.Run.Status != agent.StatusPaused || snapshot.Run.PauseReason != "operator_pause" {
					t.Fatalf("unexpected paused state: %+v", snapshot.Run)
				}
				return snapshot
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("paused Web execution did not exit")
		return agent.Snapshot{}
	}

	created := request(t, h, "POST", "/api/runs", map[string]any{
		"goal": "model profile runtime fixture", "start": false,
		"model_selection": agent.ModelSelection{Provider: "a", Model: "model-a", ReasoningEffort: "high"},
	}, http.StatusCreated)
	var original agent.Snapshot
	if err = json.Unmarshal(created.Body.Bytes(), &original); err != nil {
		t.Fatal(err)
	}
	if original.Run.ModelSelection == nil || original.Run.ModelSelection.ProviderID == "" || !original.Run.ModelSelection.ImageInput {
		t.Fatal("initial provider identity/capability was not pinned")
	}
	path := "/api/runs/" + original.Run.ID
	request(t, h, "POST", path+"/run", map[string]any{}, http.StatusOK)
	waitRequest(observedRequest{"/a/chat/completions", "Bearer a-old-fixture", "model-a", "high"})

	next := redactConfig(s.config)
	next.ActiveProvider = "b"
	profileA = next.Providers["a"]
	profileA.APIKey = "a-new-fixture"
	next.Providers["a"] = profileA
	saved := request(t, h, "PUT", "/api/config", next, http.StatusOK)
	var result struct {
		ModelOnly bool `json:"model_only"`
	}
	if err = json.Unmarshal(saved.Body.Bytes(), &result); err != nil || !result.ModelOnly || s.a == captured || s.a.Store() != captured.Store() || s.a.Registry() != captured.Registry() {
		t.Fatalf("in-flight model save replaced runtime: %v", err)
	}
	close(releaseFirst)
	// The old loop makes another real request after publication of the new Agent.
	waitRequest(observedRequest{"/a/chat/completions", "Bearer a-old-fixture", "model-a", "high"})
	paused := pause(original.Run.ID)
	if !reflect.DeepEqual(paused.Run.ModelSelection, original.Run.ModelSelection) || len(paused.Requests) != 2 || len(paused.Actions) != 1 || paused.Actions[0].Name != "tool_search" || paused.Actions[0].Result == nil {
		t.Fatal("in-flight switch changed binding or lost completed tool/request history")
	}

	request(t, h, "POST", path+"/resume", map[string]any{}, http.StatusOK)
	waitRequest(observedRequest{"/a/chat/completions", "Bearer a-new-fixture", "model-a", "high"})
	resumed := pause(original.Run.ID)
	if !reflect.DeepEqual(resumed.Run.ModelSelection, original.Run.ModelSelection) || len(resumed.Requests) != 3 || !reflect.DeepEqual(resumed.Requests[:2], paused.Requests) || !reflect.DeepEqual(resumed.Actions, paused.Actions) {
		t.Fatal("resume changed the pinned route or replayed/replaced existing history")
	}
	for _, recorded := range resumed.Requests {
		if recorded.ProviderID != original.Run.ModelSelection.ProviderID || recorded.ProviderModel != "model-a" || recorded.ReasoningEffort != "high" {
			t.Fatal("archived request lost model provenance")
		}
	}
	created = request(t, h, "POST", "/api/runs", map[string]any{"goal": "new active profile fixture", "start": true}, http.StatusCreated)
	var fresh agent.Snapshot
	if err = json.Unmarshal(created.Body.Bytes(), &fresh); err != nil || fresh.Run.ModelSelection == nil || fresh.Run.ModelSelection.Provider != "b" || fresh.Run.ModelSelection.Model != "model-b" {
		t.Fatalf("new Web run did not select profile b: %v", err)
	}
	waitRequest(observedRequest{"/b/chat/completions", "Bearer b-fixture", "model-b", ""})
	if mcpCalls.Load() != initialMCPCalls || mcpClosed.Load() != 0 {
		t.Fatal("model-only save reconnected or closed shared MCP")
	}
	closed := make(chan error, 2)
	go func() { closed <- s.Close() }()
	go func() { closed <- s.Close() }()
	for i := 0; i < 2; i++ {
		select {
		case err = <-closed:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Close did not finish the active Web execution")
		}
	}
	if mcpClosed.Load() != 1 {
		t.Fatalf("shared MCP closed %d times, want once", mcpClosed.Load())
	}
}
