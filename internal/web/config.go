package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"trace/internal/agent"
	"trace/internal/localtool"
	"trace/internal/mcp"
)

type ProviderConfig struct {
	ModelCapabilities map[string]agent.ModelCapabilities `json:"model_capabilities,omitempty"`
	ReasoningEfforts  []string                           `json:"reasoning_efforts,omitempty"`
	ImageInput        bool                               `json:"image_input"`
	URL               string                             `json:"url"`
	Model             string                             `json:"model"`
	APIKey            string                             `json:"api_key,omitempty"`
}

// Config belongs to the service, never to a run snapshot.
type Config struct {
	Revision       string                              `json:"revision,omitempty"`
	SearchSources  map[string]agent.SearchSourceConfig `json:"search_sources,omitempty"`
	Providers      map[string]ProviderConfig           `json:"providers,omitempty"`
	ActiveProvider string                              `json:"active_provider,omitempty"`
	RoleModels     agent.RoleModels                    `json:"role_models,omitempty"`
	Costs          agent.CostConfig                    `json:"costs,omitempty"`
	Integrations   []Integration                       `json:"integrations,omitempty"`
	Provider       ProviderConfig                      `json:"provider"`
	MCP            []mcp.Config                        `json:"mcp"`
	LocalTools     []localtool.Manifest                `json:"local_tools"`
	SkillPaths     []string                            `json:"skill_paths"`
	DisabledSkills []string                            `json:"disabled_skills,omitempty"`
	Context        agent.ContextConfig                 `json:"context"`
	PromptPath     string                              `json:"prompt_path,omitempty"`
}

const hiddenSecret = "[stored secret]"

// ConfigLoadResult identifies saved configuration versus missing-file bootstrap.
type ConfigLoadResult struct {
	Config        Config
	IsFirstBoot   bool
	ConfigMissing bool
}

// LoadConfig uses saved configuration when present. Missing files retain the
// environment-only CLI/first-boot contract, including when runs already exist.
// stateRoot is retained for source compatibility; JSON filenames do not prove
// that service configuration previously existed.
// Reject unknown fields rather than silently dropping them on the next save.
func LoadConfig(path string, fallback Config, stateRoot string) (ConfigLoadResult, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ConfigLoadResult{Config: fallback, IsFirstBoot: true, ConfigMissing: true}, nil
	}
	if err != nil {
		return ConfigLoadResult{}, err
	}
	var c *Config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return ConfigLoadResult{}, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return ConfigLoadResult{}, errors.New("service configuration must contain one JSON value")
	}
	if c == nil {
		return ConfigLoadResult{}, errors.New("service configuration must be a JSON object")
	}
	return ConfigLoadResult{Config: *c}, nil
}

func redactConfig(c Config) Config {
	b, _ := json.Marshal(c)
	var copy Config
	_ = json.Unmarshal(b, &copy)
	for name, source := range copy.SearchSources {
		if source.APIKey != "" {
			source.APIKey = hiddenSecret
		}
		if source.Email != "" {
			source.Email = hiddenSecret
		}
		copy.SearchSources[name] = source
	}
	if copy.Provider.APIKey != "" {
		copy.Provider.APIKey = hiddenSecret
	}
	for name, p := range copy.Providers {
		if p.APIKey != "" {
			p.APIKey = hiddenSecret
			copy.Providers[name] = p
		}
	}
	for i := range copy.MCP {
		for k := range copy.MCP[i].Env {
			copy.MCP[i].Env[k] = hiddenSecret
		}
		for k := range copy.MCP[i].Headers {
			copy.MCP[i].Headers[k] = hiddenSecret
		}
	}
	for i := range copy.LocalTools {
		for k := range copy.LocalTools[i].Env {
			copy.LocalTools[i].Env[k] = hiddenSecret
		}
	}
	return copy
}

func restoreSecrets(next *Config, old Config) error {
	if _, err := agent.IndexLocalManifests(next.LocalTools); err != nil {
		return err
	}
	oldLocal, err := agent.IndexLocalManifests(old.LocalTools)
	if err != nil {
		return err
	}
	restore := func(value *string, previous string, exists bool) error {
		if *value != hiddenSecret {
			return nil
		}
		if !exists || previous == hiddenSecret {
			return errors.New("stored secret has no matching saved value; re-enter it or leave blank to clear")
		}
		*value = previous
		return nil
	}
	for name, source := range next.SearchSources {
		previous := old.SearchSources[name]
		if err := restore(&source.APIKey, previous.APIKey, previous.APIKey != ""); err != nil {
			return err
		}
		if err := restore(&source.Email, previous.Email, previous.Email != ""); err != nil {
			return err
		}
		next.SearchSources[name] = source
	}
	for name, p := range next.Providers {
		previous := old.Providers[name]
		if err := restore(&p.APIKey, previous.APIKey, previous.APIKey != ""); err != nil {
			return err
		}
		next.Providers[name] = p
	}
	if err := restore(&next.Provider.APIKey, old.Provider.APIKey, old.Provider.APIKey != ""); err != nil {
		return err
	}
	for i := range next.MCP {
		var previous mcp.Config
		for _, prev := range old.MCP {
			if prev.Name == next.MCP[i].Name {
				previous = prev
				break
			}
		}
		for k, v := range next.MCP[i].Env {
			prior, exists := previous.Env[k]
			if err := restore(&v, prior, exists); err != nil {
				return err
			}
			next.MCP[i].Env[k] = v
		}
		for k, v := range next.MCP[i].Headers {
			prior, exists := previous.Headers[k]
			if err := restore(&v, prior, exists); err != nil {
				return err
			}
			next.MCP[i].Headers[k] = v
		}
	}
	for i := range next.LocalTools {
		previous := oldLocal[next.LocalTools[i].Name]
		for k, v := range next.LocalTools[i].Env {
			prior, exists := previous.Env[k]
			if err := restore(&v, prior, exists); err != nil {
				return err
			}
			next.LocalTools[i].Env[k] = v
		}
	}
	return nil
}

func saveConfig(path string, c Config) error {
	if path == "" {
		return errors.New("service config path is not configured")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".web-config-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configRevisionHeader(w)
	writeJSON(w, 200, redactConfig(s.configWithSources()))
}

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	var next *Config
	if !decode(w, r, &next) {
		return
	}
	if next == nil {
		writeError(w, 400, errors.New("service configuration must be a JSON object"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.configurationWritable(); err != nil {
		writeError(w, 409, err)
		return
	}
	if !s.checkConfigRevision(w, r) {
		return
	}
	if err := restoreSecrets(next, s.config); err != nil {
		writeError(w, 400, err)
		return
	}
	if modelNext, modelOnly := s.modelConfigurationCandidate(*next); modelOnly {
		result, err := s.applyModelConfiguration(r.Context(), modelNext)
		s.configRevisionHeader(w)
		respond(w, result, err)
		return
	}
	if err := s.configurationReady(); err != nil {
		writeError(w, 409, err)
		return
	}
	s.configuring = true
	s.wg.Add(1)
	defer func() { s.configuring = false; s.wg.Done() }()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	result, err := s.applyConfig(ctx, *next)
	s.configRevisionHeader(w)
	respond(w, result, err)
}

// Caller holds mu. Model-only writes retain the current runtime resources.
func (s *Server) configurationWritable() error {
	if s.closed {
		return errors.New("server is stopping")
	}
	if s.configuring {
		return errors.New("another integration operation is in progress")
	}
	return nil
}

// Caller holds mu. Non-model changes and imports retain the full busy boundary.
func (s *Server) configurationReady() error {
	if err := s.configurationWritable(); err != nil {
		return err
	}
	if len(s.active) > 0 || s.a.HasActiveWorkers() {
		return errors.New("pause active runs and wait for workers before changing configuration")
	}
	runs, err := s.store.List()
	if err != nil {
		return err
	}
	for _, run := range runs {
		if run.Status == agent.StatusRunning || run.Status == agent.StatusWaitingTool {
			return errors.New("pause active runs and workers before changing configuration")
		}
	}
	if s.options.BuildAgent == nil {
		return errors.New("configuration rebuild is not configured")
	}
	return nil
}

// Caller holds mu. A failed build or save never replaces the live Agent.
func (s *Server) applyConfig(ctx context.Context, next Config) (map[string]any, error) {
	if err := restoreSecrets(&next, s.config); err != nil {
		return nil, err
	}
	revision, err := newConfigRevision()
	if err != nil {
		return nil, err
	}
	next.Revision = revision
	// configuring excludes another rebuild/run while readers retain the previous Agent.
	// Release mu so Close can cancel a stalled external MCP initialization.
	s.mu.Unlock()
	a, err := s.options.BuildAgent(ctx, next)
	s.mu.Lock()
	if err != nil {
		var connection *mcp.ConnectionError
		if errors.As(err, &connection) {
			if s.mcpProbeSequence != nil {
				s.mcpProbeSequence[connection.Name]++
			}
			if s.mcpAttempts == nil {
				s.mcpAttempts = map[string]mcp.Status{}
			}
			s.mcpAttempts[connection.Name] = connection.Status
			detail := connection.Status.Detail
			for _, cleanup := range connection.Status.Cleanup {
				detail += "；连接清理失败：" + cleanup.Detail
			}
			return nil, errors.New(detail + "；原配置与 Agent 已保留")
		}
		return nil, err
	}
	discardCandidate := func(primary error) error {
		if cleanupErr := a.CloseMCP(); cleanupErr != nil {
			return errors.Join(primary, errors.New("候选连接清理失败："+mcp.Failure("cleanup", cleanupErr).Detail))
		}
		return primary
	}
	if s.closed || ctx.Err() != nil {
		if ctx.Err() != nil {
			return nil, discardCandidate(ctx.Err())
		}
		return nil, discardCandidate(errors.New("server is stopping"))
	}
	if a.Store() != s.store {
		return nil, discardCandidate(errors.New("configuration rebuild must retain the shared store"))
	}
	if err = saveConfig(s.options.ConfigPath, next); err != nil {
		return nil, discardCandidate(err)
	}
	old := s.a
	s.a, s.config = a, next
	s.mcpAttempts = nil
	result := map[string]any{"saved": true, "config": redactConfig(next)}
	if err = old.CloseMCP(); err != nil {
		result["cleanup_error"] = mcp.Failure("cleanup", err).Detail
	}
	return result, nil
}
