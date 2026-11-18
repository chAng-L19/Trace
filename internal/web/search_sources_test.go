package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"trace/internal/agent"
)

func TestSearchSourceConfigSecretsAndRolePersistence(t *testing.T) {
	c := Config{Provider: ProviderConfig{APIKey: "model-secret"}, RoleModels: agent.RoleModels{Recon: &agent.ModelSelection{Provider: "default", Model: "collector"}, StageRoutes: []agent.ModelRoute{{Provider: "default", Model: "allowed"}}}, SearchSources: map[string]agent.SearchSourceConfig{"fofa": {Enabled: true, APIKey: "search-secret", Email: "private-email", MaxRequests: 4, MaxResults: 200}}}
	redacted := redactConfig(c)
	raw, _ := json.Marshal(redacted)
	if strings.Contains(string(raw), "model-secret") || strings.Contains(string(raw), "search-secret") || strings.Contains(string(raw), "private-email") {
		t.Fatal("credentials exposed")
	}
	restoreSecrets(&redacted, c)
	if redacted.SearchSources["fofa"].APIKey != "search-secret" || redacted.Provider.APIKey != "model-secret" || redacted.SearchSources["fofa"].Email != "private-email" {
		t.Fatal("model/source secrets crossed or dropped")
	}
	path := filepath.Join(t.TempDir(), "config.json")
	if err := saveConfig(path, redacted); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path, Config{}, "")
	if err != nil || loaded.Config.RoleModels.Recon.Model != "collector" || len(loaded.Config.RoleModels.StageRoutes) != 1 || loaded.Config.SearchSources["fofa"].MaxRequests != 4 {
		t.Fatal("roundtrip dropped recon/source policy")
	}
	source := redacted.SearchSources["fofa"]
	source.APIKey = ""
	redacted.SearchSources["fofa"] = source
	restoreSecrets(&redacted, c)
	if redacted.SearchSources["fofa"].APIKey != "" {
		t.Fatal("explicit secret clear ignored")
	}
}

func TestSearchSourceProbeUsesSavedAccountEndpointWithoutRun(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/info/my" || r.URL.Query().Get("key") != "account-secret" {
			t.Error("probe performed wrong call")
		}
		fmt.Fprint(w, `{"error":false,"email":"private@example.test","username":"private-name"}`)
	}))
	defer upstream.Close()
	s, a := fixture(t, nil)
	if err := a.ConfigureSearchSources(map[string]agent.SearchSourceConfig{"fofa": {Enabled: true, APIKey: "account-secret", BaseURL: upstream.URL}}); err != nil {
		t.Fatal(err)
	}
	response := request(t, s.Handler(), "POST", "/api/search-sources/fofa/probe", map[string]any{}, 200)
	if !strings.Contains(response.Body.String(), `"status":"authenticated"`) || strings.Contains(response.Body.String(), "account-secret") || strings.Contains(response.Body.String(), "private@example.test") || calls.Load() != 1 {
		t.Fatal("probe misreported authentication or leaked account")
	}
	runs, err := a.Store().List()
	if err != nil || len(runs) != 0 {
		t.Fatal("connection probe created a run")
	}
	request(t, s.Handler(), "POST", "/api/search-sources/missing/probe", map[string]any{}, 400)
}
