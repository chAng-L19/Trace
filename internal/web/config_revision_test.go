package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"trace/internal/agent"
)

// Unlike request(), this deliberately sends exactly the supplied precondition.
func revisionRequest(t *testing.T, h http.Handler, method, path, match string, body any) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	if match != "" {
		r.Header.Set("If-Match", match)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestConfigurationRevisionRejectsStaleEditorAndKeepsSecrets(t *testing.T) {
	s, _ := fixture(t, nil)
	h := s.Handler()
	loaded := request(t, h, "GET", "/api/config", nil, 200)
	var old Config
	if err := json.Unmarshal(loaded.Body.Bytes(), &old); err != nil || len(old.Revision) != 48 {
		t.Fatalf("invalid revision: %v", err)
	}
	etag := loaded.Header().Get("ETag")
	if etag != `"`+old.Revision+`"` {
		t.Fatal("config and HTTP revision differ")
	}
	a := cloneConfig(old)
	a.Provider = ProviderConfig{Model: "new-model", APIKey: "rotated-secret"}
	accepted := revisionRequest(t, h, "PUT", "/api/config", etag, a)
	if accepted.Code != 200 || s.config.Revision == old.Revision {
		t.Fatalf("save did not advance revision: %d %s", accepted.Code, accepted.Body.String())
	}
	current := s.config.Revision
	live := s.a
	before, err := os.ReadFile(s.options.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	old.Provider.APIKey = hiddenSecret
	stale := revisionRequest(t, h, "PUT", "/api/config", etag, old)
	var conflict map[string]any
	_ = json.Unmarshal(stale.Body.Bytes(), &conflict)
	after, _ := os.ReadFile(s.options.ConfigPath)
	if stale.Code != 409 || conflict["code"] != "config_revision_conflict" || s.a != live || s.config.Revision != current || s.config.Provider.APIKey != "rotated-secret" || !bytes.Equal(before, after) {
		t.Fatalf("stale save mutated committed state: %d %s", stale.Code, stale.Body.String())
	}
	for _, item := range []struct{ method, path string }{
		{"PUT", "/api/config"},
		{"POST", "/api/integrations/import"},
		{"POST", "/api/integrations/update"},
		{"PUT", "/api/skills/fixture/availability"},
	} {
		body := any(map[string]any{})
		if item.path == "/api/config" {
			body = a
		} else if item.method == "PUT" {
			body = map[string]bool{"disabled": true}
		}
		for _, match := range []string{etag, "", "*"} {
			w := revisionRequest(t, h, item.method, item.path, match, body)
			want := 409
			if match == "" {
				want = 428
			}
			if w.Code != want || s.config.Revision != current || s.a != live {
				t.Fatalf("%s %s precondition %q: %d", item.method, item.path, match, w.Code)
			}
		}
	}
}

func TestConfigurationRevisionFailureAndRestart(t *testing.T) {
	s, _ := fixture(t, nil)
	h := s.Handler()
	initial := s.config.Revision
	bad := cloneConfig(s.config)
	bad.Provider.URL = "invalid-url"
	if w := revisionRequest(t, h, "PUT", "/api/config", `"`+initial+`"`, bad); w.Code != 409 || s.config.Revision != initial {
		t.Fatal("failed build advanced revision")
	}
	good := cloneConfig(s.config)
	good.Provider.Model = "saved-model"
	if w := revisionRequest(t, h, "PUT", "/api/config", `"`+initial+`"`, good); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	loaded, err := LoadConfig(s.options.ConfigPath, Config{}, "")
	if err != nil || loaded.Config.Revision != s.config.Revision {
		t.Fatalf("revision was not atomically persisted: %v", err)
	}
	newAgent, err := agent.New(s.store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(newAgent, Options{InitialConfig: loaded.Config})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if restarted.config.Revision == loaded.Config.Revision {
		t.Fatal("pre-restart editor revision still current")
	}
	// Persistence failure must discard the candidate without advancing the revision.
	previous, previousAgent := s.config.Revision, s.a
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	s.options.ConfigPath = filepath.Join(blocker, "config.json")
	if w := revisionRequest(t, h, "PUT", "/api/config", `"`+previous+`"`, good); w.Code != 409 || s.config.Revision != previous || s.a != previousAgent {
		t.Fatal("failed persistence changed live config")
	}
}

func TestConfigurationRevisionBusyDoesNotStartSecondBuild(t *testing.T) {
	s, _ := fixture(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	s.options.BuildAgent = func(ctx context.Context, _ Config) (*agent.Agent, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return agent.New(s.store, nil, "")
	}
	old := cloneConfig(s.config)
	match := `"` + old.Revision + `"`
	h := s.Handler()
	done := make(chan *httptest.ResponseRecorder, 1)
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("PUT", "/api/config", bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("If-Match", match)
	go func() {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		done <- w
	}()
	<-entered
	w := revisionRequest(t, h, "PUT", "/api/config", match, old)
	close(release)
	first := <-done
	if w.Code != 409 || first.Code != 200 {
		t.Fatalf("busy/build results: %d/%d", w.Code, first.Code)
	}
}
