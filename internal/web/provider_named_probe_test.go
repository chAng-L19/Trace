package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProviderProbeNamedCredentialOwnership(t *testing.T) {
	var calls atomic.Int32
	var authorization atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		authorization.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()
	s, _ := fixture(t, nil)
	config := s.config
	config.Provider = ProviderConfig{URL: upstream.URL, Model: "default-model", APIKey: "default-key"}
	config.Providers = map[string]ProviderConfig{"named": {URL: upstream.URL, Model: "named-model", APIKey: "named-key"}}
	request(t, s.Handler(), "PUT", "/api/config", config, 200)
	for _, test := range []struct {
		name, key, wantAuth, wantStatus string
		wantCall                        bool
	}{
		{"named", hiddenSecret, "Bearer named-key", "success", true},
		{"named", "", "", "success", true},
		{"new-profile", "draft-key", "Bearer draft-key", "success", true},
		{"missing", hiddenSecret, "", "failed", false},
	} {
		t.Run(test.name+test.wantAuth, func(t *testing.T) {
			before := calls.Load()
			w := request(t, s.Handler(), "POST", "/api/provider/probe", map[string]any{"provider": test.name, "url": upstream.URL, "model": "draft-model", "api_key": test.key}, 200)
			var result providerProbeResult
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Status != test.wantStatus || (calls.Load() > before) != test.wantCall {
				t.Fatalf("status=%s callDelta=%d", result.Status, calls.Load()-before)
			}
			if test.wantCall && authorization.Load() != test.wantAuth {
				t.Fatal("probe used the wrong credential")
			}
			if strings.Contains(w.Body.String(), "-key") || strings.Contains(w.Body.String(), upstream.URL) {
				t.Fatal("probe exposed configuration")
			}
		})
	}
	if s.config.ActiveProvider != "" || s.config.Providers["named"].APIKey != "named-key" {
		t.Fatal("probe changed saved config or active profile")
	}
}
