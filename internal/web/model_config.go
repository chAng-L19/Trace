package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"trace/internal/agent"
)

// ModelConfiguration keeps legacy "default" routes intact. An active named
// profile supplies only unspecified role defaults; persisted run bindings still
// resolve their original provider name and endpoint identity.
// Reference: CC Switch fc3eb797 provider.rs and services/provider/mod.rs separate
// stable provider records from the selected current-provider pointer.
func (c Config) ModelConfiguration() (agent.Provider, map[string]*agent.OpenAIProvider, agent.RoleModels, error) {
	convert := func(p ProviderConfig) *agent.OpenAIProvider {
		return &agent.OpenAIProvider{BaseURL: p.URL, APIKey: p.APIKey, Model: p.Model, ImageInput: p.ImageInput, ReasoningEfforts: p.ReasoningEfforts, ModelCapabilities: p.ModelCapabilities}
	}
	var provider agent.Provider = agent.MockProvider{}
	if c.Provider.URL != "" {
		provider = convert(c.Provider)
	}
	providers := make(map[string]*agent.OpenAIProvider, len(c.Providers))
	for name, p := range c.Providers {
		providers[name] = convert(p)
	}
	roles := c.RoleModels
	if c.ActiveProvider != "" && c.ActiveProvider != "default" {
		active, exists := providers[c.ActiveProvider]
		if !exists {
			return nil, nil, agent.RoleModels{}, fmt.Errorf("active model provider %q is unavailable", c.ActiveProvider)
		}
		if roles.Orchestrator == nil {
			roles.Orchestrator = &agent.ModelSelection{Provider: c.ActiveProvider, Model: active.Model}
		}
		if roles.Worker == nil {
			roles.Worker = &agent.ModelSelection{Provider: c.ActiveProvider, Model: active.Model}
		}
		// Recon continues to inherit Worker when no explicit recon role exists.
	}
	return provider, providers, roles, nil
}

// An unchanged submission retains the existing explicit full-refresh behavior.
// Clearing only these fields makes this an allowlist: every other current or
// future Config field must still compare equal to avoid a full rebuild.
func modelConfigurationOnly(old, next Config) bool {
	old.Revision, next.Revision = "", ""
	before, oldErr := json.Marshal(old)
	after, nextErr := json.Marshal(next)
	if oldErr != nil || nextErr != nil || bytes.Equal(before, after) {
		return false
	}
	old.Provider, next.Provider = ProviderConfig{}, ProviderConfig{}
	old.Providers, next.Providers = nil, nil
	old.RoleModels, next.RoleModels = agent.RoleModels{}, agent.RoleModels{}
	old.ActiveProvider, next.ActiveProvider = "", ""
	before, oldErr = json.Marshal(old)
	after, nextErr = json.Marshal(next)
	return oldErr == nil && nextErr == nil && bytes.Equal(before, after)
}

// GET /config enriches source links. Accept that exact read-only projection,
// but retain the raw non-model config instead of persisting discovered metadata.
// A user-edited source path or any other non-model field still needs a rebuild.
// Caller holds mu; source discovery performs local reads only.
func (s *Server) modelConfigurationCandidate(next Config) (Config, bool) {
	if modelConfigurationOnly(s.config, next) {
		return next, true
	}
	if !modelConfigurationOnly(s.configWithSources(), next) {
		return Config{}, false
	}
	result := s.config
	result.Provider, result.Providers = next.Provider, next.Providers
	result.RoleModels, result.ActiveProvider = next.RoleModels, next.ActiveProvider
	return result, true
}

// Caller holds mu throughout; no remote I/O, runtime shutdown or lock release.
func (s *Server) applyModelConfiguration(ctx context.Context, next Config) (map[string]any, error) {
	provider, providers, roles, err := next.ModelConfiguration()
	if err != nil {
		return nil, err
	}
	candidate, err := s.a.WithModelConfiguration(provider, providers, roles)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	next.Revision, err = newConfigRevision()
	if err != nil {
		return nil, err
	}
	if err = saveConfig(s.options.ConfigPath, next); err != nil {
		return nil, err
	}
	s.a, s.config = candidate, next
	return map[string]any{"saved": true, "model_only": true, "config": redactConfig(next)}, nil
}
