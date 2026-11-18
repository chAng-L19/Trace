package web

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"trace/internal/agent"
)

func TestNullConfigurationRetainsSavedFileAndAgent(t *testing.T) {
	s, live := fixture(t, nil)
	s.config.Provider = ProviderConfig{URL: "https://saved.invalid", Model: "saved-model", APIKey: "saved-key"}
	if err := saveConfig(s.options.ConfigPath, s.config); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.options.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	builds := 0
	s.options.BuildAgent = func(context.Context, Config) (*agent.Agent, error) {
		builds++
		return agent.New(live.Store(), nil, "")
	}
	request(t, s.Handler(), "PUT", "/api/config", json.RawMessage("null"), 400)
	after, err := os.ReadFile(s.options.ConfigPath)
	if err != nil || !bytes.Equal(before, after) || s.a != live || builds != 0 {
		t.Fatal("null configuration rebuilt the Agent or overwrote saved settings")
	}
	path := filepath.Join(t.TempDir(), "null-config.json")
	if err := os.WriteFile(path, []byte("null"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path, s.config, ""); err == nil {
		t.Fatal("null saved file silently became empty configuration")
	}
}

func secretConfigFixture(t *testing.T) Config {
	t.Helper()
	var c Config
	err := json.Unmarshal([]byte(`{
		"provider":{"url":"https://default.invalid","model":"main","api_key":"default-fixture-key","model_capabilities":{"main":{"reasoning_efforts":["high"],"image_input":true}}},
		"providers":{"named":{"url":"https://named.invalid","model":"worker","api_key":"named-fixture-key"}},
		"search_sources":{"fofa":{"api_key":"source-fixture-key","email":"fixture@example.invalid"}},
		"role_models":{"worker":{"provider":"named","model":"worker"},"stage_routes":[{"provider":"named","model":"worker"}]},
		"costs":{"currency":"USD","rates":[{"model":"main","currency":"USD","input_per_million":"0.123456789123456789","output_per_million":"1","source":"fixture","effective_at":"2026-11-11T00:00:00Z"}]},
		"mcp":[{"name":"mcp","env":{"TOKEN":"mcp-fixture-key","EMPTY":""},"headers":{"Authorization":"header-fixture-key","Empty":""}}],
		"local_tools":[{"name":"local","env":{"TOKEN":"local-fixture-key","EMPTY":""}}]
	}`), &c)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoadConfigRejectsInvalidSavedValuesWithoutChangingFile(t *testing.T) {
	for _, tc := range []struct{ name, data, message string }{
		{"unknown_root", `{"future":true}`, "unknown field"},
		{"unknown_provider", `{"provider":{"future":true}}`, "unknown field"},
		{"unknown_named_provider", `{"providers":{"named":{"future":true}}}`, "unknown field"},
		{"unknown_model_capability", `{"provider":{"model_capabilities":{"main":{"future":true}}}}`, "unknown field"},
		{"unknown_role", `{"role_models":{"worker":{"future":true}}}`, "unknown field"},
		{"unknown_route", `{"role_models":{"stage_routes":[{"future":true}]}}`, "unknown field"},
		{"unknown_rate", `{"costs":{"rates":[{"future":true}]}}`, "unknown field"},
		{"unknown_search_source", `{"search_sources":{"fofa":{"future":true}}}`, "unknown field"},
		{"unknown_mcp", `{"mcp":[{"future":true}]}`, "unknown field"},
		{"unknown_local_tool", `{"local_tools":[{"future":true}]}`, "unknown field"},
		{"wrong_known_type", `{"provider":{"image_input":"true"}}`, ""},
		{"null", `null`, "JSON object"},
		{"array", `[]`, ""},
		{"multiple_values", `{} {}`, "one JSON value"},
		{"trailing_junk", `{} trailing`, "one JSON value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			before := []byte(tc.data)
			if err := os.WriteFile(path, before, 0600); err != nil {
				t.Fatal(err)
			}
			result, err := LoadConfig(path, secretConfigFixture(t), "")
			if err == nil || !strings.Contains(err.Error(), tc.message) || result.IsFirstBoot || result.ConfigMissing {
				t.Fatalf("invalid saved configuration was accepted or treated as missing: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected saved configuration changed on disk")
			}
		})
	}
}

func TestLoadConfigSupportedRoundTripAndMissingBootstrap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	want := secretConfigFixture(t)
	missing, err := LoadConfig(path, want, "")
	if err != nil || !missing.IsFirstBoot || !missing.ConfigMissing || !reflect.DeepEqual(missing.Config, want) {
		t.Fatal("missing-file bootstrap changed")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing-file load created a configuration")
	}
	if err := saveConfig(path, want); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path, Config{}, "")
	if err != nil || loaded.IsFirstBoot || loaded.ConfigMissing || !reflect.DeepEqual(loaded.Config, want) {
		t.Fatalf("supported saved configuration changed: %v", err)
	}
}

func TestConfigurationRejectsUnresolvedStoredSecrets(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(next, old *Config)
	}{
		{"provider_rename", func(n, _ *Config) { n.Providers["renamed"] = n.Providers["named"]; delete(n.Providers, "named") }},
		{"default_missing_key", func(_, o *Config) { o.Provider.APIKey = "" }},
		{"named_missing_key", func(_, o *Config) { p := o.Providers["named"]; p.APIKey = ""; o.Providers["named"] = p }},
		{"source_rename", func(n, _ *Config) {
			n.SearchSources["renamed"] = n.SearchSources["fofa"]
			delete(n.SearchSources, "fofa")
		}},
		{"source_missing_email", func(_, o *Config) { s := o.SearchSources["fofa"]; s.Email = ""; o.SearchSources["fofa"] = s }},
		{"mcp_rename", func(n, _ *Config) { n.MCP[0].Name = "renamed" }},
		{"mcp_missing_env_key", func(n, _ *Config) { n.MCP[0].Env["NEW"] = hiddenSecret }},
		{"mcp_missing_header_key", func(n, _ *Config) { n.MCP[0].Headers["New"] = hiddenSecret }},
		{"local_rename", func(n, _ *Config) { n.LocalTools[0].Name = "renamed" }},
		{"local_missing_env_key", func(n, _ *Config) { n.LocalTools[0].Env["NEW"] = hiddenSecret }},
		{"unresolved_default", func(_, o *Config) { o.Provider.APIKey = hiddenSecret }},
		{"unresolved_named", func(_, o *Config) { p := o.Providers["named"]; p.APIKey = hiddenSecret; o.Providers["named"] = p }},
		{"unresolved_source_key", func(_, o *Config) { s := o.SearchSources["fofa"]; s.APIKey = hiddenSecret; o.SearchSources["fofa"] = s }},
		{"unresolved_source_email", func(_, o *Config) { s := o.SearchSources["fofa"]; s.Email = hiddenSecret; o.SearchSources["fofa"] = s }},
		{"unresolved_mcp_env", func(_, o *Config) { o.MCP[0].Env["TOKEN"] = hiddenSecret }},
		{"unresolved_mcp_header", func(_, o *Config) { o.MCP[0].Headers["Authorization"] = hiddenSecret }},
		{"unresolved_local_env", func(_, o *Config) { o.LocalTools[0].Env["TOKEN"] = hiddenSecret }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, live := fixture(t, nil)
			old := secretConfigFixture(t)
			old.Revision, old.Context = s.config.Revision, s.config.Context
			s.config = old
			next := redactConfig(s.config)
			tc.edit(&next, &s.config)
			baseline := cloneConfig(s.config)
			if err := saveConfig(s.options.ConfigPath, s.config); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(s.options.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			builds := 0
			s.options.BuildAgent = func(context.Context, Config) (*agent.Agent, error) {
				builds++
				return agent.New(live.Store(), nil, "")
			}
			response := request(t, s.Handler(), "PUT", "/api/config", next, 400)
			if !strings.Contains(response.Body.String(), "stored secret has no matching saved value") {
				t.Fatal("missing secret source did not produce a repairable error")
			}
			// Import/update/availability writers enter the same commit gate directly.
			s.mu.Lock()
			_, applyErr := s.applyConfig(context.Background(), cloneConfig(next))
			s.mu.Unlock()
			if applyErr == nil {
				t.Fatal("shared commit gate accepted an unresolved secret")
			}
			after, err := os.ReadFile(s.options.ConfigPath)
			if err != nil || !bytes.Equal(before, after) || s.a != live || builds != 0 || !reflect.DeepEqual(s.config, baseline) {
				t.Fatal("rejected secret changed the file, Agent, configuration, or revision")
			}
		})
	}
}

func TestConfigurationStoredSecretPreserveClearAndReplace(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		replace     bool
	}{{name: "preserve"}, {name: "clear", replace: true}, {name: "replace", value: "replacement-fixture-key", replace: true}} {
		t.Run(tc.name, func(t *testing.T) {
			s, live := fixture(t, nil)
			old := secretConfigFixture(t)
			old.Revision, old.Context = s.config.Revision, s.config.Context
			s.config = old
			next, want := redactConfig(old), cloneConfig(old)
			if tc.replace {
				for _, c := range []*Config{&next, &want} {
					c.Provider.APIKey = tc.value
					p := c.Providers["named"]
					p.APIKey = tc.value
					c.Providers["named"] = p
					source := c.SearchSources["fofa"]
					source.APIKey, source.Email = tc.value, tc.value
					c.SearchSources["fofa"] = source
					for _, fields := range []map[string]string{c.MCP[0].Env, c.MCP[0].Headers, c.LocalTools[0].Env} {
						for key := range fields {
							fields[key] = tc.value
						}
					}
				}
			}
			builds := 0
			var built Config
			s.options.BuildAgent = func(_ context.Context, c Config) (*agent.Agent, error) {
				builds++
				built = cloneConfig(c)
				return agent.New(live.Store(), nil, "")
			}
			request(t, s.Handler(), "PUT", "/api/config", next, 200)
			if builds != 1 || s.config.Revision == old.Revision || !reflect.DeepEqual(built, s.config) {
				t.Fatal("valid secret update did not build and commit exactly once")
			}
			want.Revision = s.config.Revision
			if !reflect.DeepEqual(s.config, want) {
				t.Fatal("valid secret update changed supported fields or empty map values")
			}
			loaded, err := LoadConfig(s.options.ConfigPath, Config{}, "")
			if err != nil || !reflect.DeepEqual(loaded.Config, want) {
				t.Fatal("valid secret update did not survive disk reload")
			}
		})
	}
}
