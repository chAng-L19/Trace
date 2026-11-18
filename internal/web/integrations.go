package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"trace/internal/agent"
	"trace/internal/mcp"
)

type integrationRequest struct {
	ID       string      `json:"id"`
	Kind     string      `json:"kind"`
	Name     string      `json:"name"`
	URL      string      `json:"url"`
	MCP      *mcp.Config `json:"mcp,omitempty"`
	Disabled bool        `json:"disabled,omitempty"`
}

func (s *Server) registerIntegrationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/integrations", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		writeJSON(w, 200, s.config.Integrations)
	})
	mux.HandleFunc("POST /api/integrations/{op}", s.integrationOperation)
}

func (s *Server) integrationOperation(w http.ResponseWriter, r *http.Request) {
	var in integrationRequest
	if !decode(w, r, &in) {
		return
	}
	op := r.PathValue("op")
	if op != "import" && op != "check" && op != "update" {
		writeError(w, 404, fmt.Errorf("unknown integration operation"))
		return
	}
	s.mu.Lock()
	if op == "check" {
		if s.closed || s.configuring {
			s.mu.Unlock()
			writeError(w, 409, fmt.Errorf("another integration operation is in progress or server is stopping"))
			return
		}
	} else if err := s.configurationReady(); err != nil {
		s.mu.Unlock()
		writeError(w, 409, err)
		return
	}
	if op != "check" && !s.checkConfigRevision(w, r) {
		s.mu.Unlock()
		return
	}
	next := cloneConfig(s.config)
	existingSkills := s.a.Skills()
	var entry Integration
	var err error
	if op != "import" {
		entry, err = s.findIntegration(in)
	}
	if err != nil {
		s.mu.Unlock()
		writeError(w, 400, err)
		return
	}
	s.configuring = true
	s.wg.Add(1)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.configuring = false; s.mu.Unlock(); s.wg.Done() }()
	// Git I/O runs outside mu so config, skills, run events, and cancellation remain responsive.
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	var result map[string]any
	if op == "import" {
		if in.Kind != "mcp" && in.Kind != "skill" {
			writeError(w, 400, fmt.Errorf("import kind must be mcp or skill"))
			return
		}
		if _, err = parseGitHub(in.URL); err != nil {
			writeError(w, 400, err)
			return
		}
		var dir string
		dir, err = s.checkoutDirectory()
		if err != nil {
			respond(w, nil, err)
			return
		}
		keep := false
		defer func() {
			if !keep {
				_ = os.RemoveAll(dir)
			}
		}()
		entry, err = cloneIntegration(ctx, dir, in.Kind, in.URL)
		if err == nil {
			for _, existing := range next.Integrations {
				if existing.ID == entry.ID {
					err = fmt.Errorf("project is already imported; use check or update")
					break
				}
			}
		}
		if err == nil && entry.Kind == "skill" {
			err = validateSkillNames(entry.SkillPath, existingSkills, "")
		}
		if err == nil {
			err = registerImport(&next, &entry, in)
		}
		if err != nil {
			writeError(w, 409, err)
			return
		}
		next.Integrations = append(next.Integrations, entry)
		s.mu.Lock()
		if s.closed {
			err = fmt.Errorf("server is stopping")
		} else {
			result, err = s.applyConfig(ctx, next)
		}
		s.configRevisionHeader(w)
		s.mu.Unlock()
		if err == nil {
			keep = true
			result["integration"] = entry
			result["status"] = "imported"
		}
	} else {
		status, e := checkIntegration(ctx, entry)
		if e != nil {
			respond(w, nil, e)
			return
		}
		if op == "check" || !status.UpdateAvailable && !status.Dirty {
			writeJSON(w, 200, status)
			return
		}
		if status.Status != "update_available" {
			writeError(w, 409, fmt.Errorf("project has %s; original checkout and configuration were retained", status.Status))
			return
		}
		var dir string
		dir, err = s.checkoutDirectory()
		if err != nil {
			respond(w, nil, err)
			return
		}
		keep := false
		defer func() {
			if !keep {
				_ = os.RemoveAll(dir)
			}
		}()
		entry.Revision = status.CurrentRevision
		updated, e := prepareUpdate(ctx, entry, dir, status.LatestRevision)
		if e != nil {
			respond(w, nil, e)
			return
		}
		remapIntegration(&next, entry, updated)
		if updated.SkillPath != "" {
			err = validateSkills(updated.SkillPath)
			if err == nil {
				err = validateSkillNames(updated.SkillPath, existingSkills, entry.LocalPath)
			}
		}
		if err != nil {
			writeError(w, 409, err)
			return
		}
		s.mu.Lock()
		if s.closed {
			err = fmt.Errorf("server is stopping")
		} else {
			result, err = s.applyConfig(ctx, next)
		}
		s.configRevisionHeader(w)
		s.mu.Unlock()
		if err == nil {
			keep = true
			result["integration"] = updated
			result["status"] = "updated"
			result["previous_path"] = entry.LocalPath
		}
	}
	respond(w, result, err)
}

func cloneConfig(c Config) Config {
	data, _ := json.Marshal(c)
	var out Config
	_ = json.Unmarshal(data, &out)
	return out
}

func validateSkills(path string) error {
	skills, err := agent.DiscoverSkills([]string{path})
	if err != nil {
		return err
	}
	if len(skills) == 0 {
		return fmt.Errorf("no SKILL.md found in %s", path)
	}
	return nil
}

func validateSkillNames(root string, existingSkills []agent.Skill, replacedRoot string) error {
	incoming, err := agent.DiscoverSkills([]string{root})
	if err != nil {
		return err
	}
	for _, skill := range incoming {
		for _, existing := range existingSkills {
			if skill.Name == existing.Name && !withinPath(replacedRoot, existing.Path) {
				return fmt.Errorf("skill name already exists: %s (%s)", skill.Name, existing.Path)
			}
		}
	}
	return nil
}

func registerImport(c *Config, entry *Integration, in integrationRequest) error {
	if entry.Kind == "skill" {
		if err := validateSkills(entry.SkillPath); err != nil {
			return err
		}
		c.SkillPaths = append(c.SkillPaths, entry.SkillPath)
		return nil
	}
	root := filepath.Join(entry.LocalPath, filepath.FromSlash(entry.Subpath))
	configs := []mcp.Config{}
	if in.MCP != nil {
		configs = append(configs, *in.MCP)
	} else {
		var err error
		configs, err = readMCPManifest(root)
		if err != nil {
			return err
		}
	}
	seen := map[string]bool{}
	for _, cfg := range c.MCP {
		seen[cfg.Name] = true
	}
	for i := range configs {
		cfg := &configs[i]
		if in.Disabled {
			cfg.Disabled = true
		}
		if in.Name != "" && len(configs) == 1 {
			cfg.Name = in.Name
		}
		if strings.TrimSpace(cfg.Name) == "" {
			return fmt.Errorf("MCP service name is required")
		}
		if seen[cfg.Name] {
			return fmt.Errorf("MCP service already exists: %s", cfg.Name)
		}
		if (cfg.Command == "") == (cfg.URL == "") {
			return fmt.Errorf("MCP %s requires exactly one command or URL", cfg.Name)
		}
		seen[cfg.Name] = true
		expand := func(value string) string { return strings.ReplaceAll(value, "${REPO_DIR}", root) }
		cfg.Command = expand(cfg.Command)
		cfg.URL = expand(cfg.URL)
		for j := range cfg.Args {
			cfg.Args[j] = expand(cfg.Args[j])
		}
		for k, v := range cfg.Env {
			cfg.Env[k] = expand(v)
		}
		cfg.Dir = expand(cfg.Dir)
		if cfg.Dir == "" {
			cfg.Dir = root
		} else if !filepath.IsAbs(cfg.Dir) {
			cfg.Dir = filepath.Join(root, cfg.Dir)
		}
		cfg.GitHubURL, cfg.LocalPath = entry.GitHubURL, entry.LocalPath
		entry.MCPNames = append(entry.MCPNames, cfg.Name)
		c.MCP = append(c.MCP, *cfg)
	}
	return nil
}

func readMCPManifest(root string) ([]mcp.Config, error) {
	for _, name := range []string{".mcp.json", "mcp.json", "trace-mcp.json"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var wrapped struct {
			MCPServers map[string]mcp.Config `json:"mcpServers"`
			MCP        []mcp.Config          `json:"mcp"`
		}
		if err = json.Unmarshal(data, &wrapped); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		if len(wrapped.MCP) > 0 {
			return wrapped.MCP, nil
		}
		var names []string
		for key := range wrapped.MCPServers {
			names = append(names, key)
		}
		sort.Strings(names)
		var configs []mcp.Config
		for _, key := range names {
			cfg := wrapped.MCPServers[key]
			cfg.Name = key
			configs = append(configs, cfg)
		}
		if len(configs) > 0 {
			return configs, nil
		}
		return nil, fmt.Errorf("%s needs mcpServers or mcp entries", name)
	}
	return nil, fmt.Errorf("project has no .mcp.json, mcp.json, or trace-mcp.json; provide an explicit MCP command/URL configuration")
}

func remapIntegration(c *Config, old, next Integration) {
	// Rewrite only source paths; credentials and unrelated configuration are untouched.
	replace := func(value string) string {
		value = replaceSourcePath(value, old.LocalPath, next.LocalPath)
		return replaceSourcePath(value, filepath.ToSlash(old.LocalPath), filepath.ToSlash(next.LocalPath))
	}
	skillRootReplaced := false
	for i := range c.SkillPaths {
		if withinPath(old.LocalPath, c.SkillPaths[i]) {
			skillRootReplaced = true
		}
		c.SkillPaths[i] = replace(c.SkillPaths[i])
	}
	if next.SkillPath != "" && !skillRootReplaced {
		// A parent/default discovery root still contains the old checkout. Prefer the updated skill.
		c.SkillPaths = append([]string{next.SkillPath}, c.SkillPaths...)
	}
	for i := range c.MCP {
		cfg := &c.MCP[i]
		cfg.LocalPath = replace(cfg.LocalPath)
		cfg.Dir = replace(cfg.Dir)
		cfg.Command = replace(cfg.Command)
		for j := range cfg.Args {
			cfg.Args[j] = replace(cfg.Args[j])
		}
		for k, v := range cfg.Env {
			cfg.Env[k] = replace(v)
		}
	}
	for i := range c.LocalTools {
		cfg := &c.LocalTools[i]
		cfg.LocalPath = replace(cfg.LocalPath)
		cfg.Dir = replace(cfg.Dir)
		cfg.Command = replace(cfg.Command)
		for j := range cfg.Args {
			cfg.Args[j] = replace(cfg.Args[j])
		}
		for k, v := range cfg.Env {
			cfg.Env[k] = replace(v)
		}
	}
	found := false
	for i := range c.Integrations {
		if c.Integrations[i].ID == old.ID {
			c.Integrations[i] = next
			found = true
		}
	}
	if !found {
		c.Integrations = append(c.Integrations, next)
	}
}

func replaceSourcePath(value, old, next string) string {
	if old == "" {
		return value
	}
	var out strings.Builder
	for cursor := 0; cursor < len(value); {
		at := strings.Index(value[cursor:], old)
		if at < 0 {
			out.WriteString(value[cursor:])
			break
		}
		at += cursor
		end := at + len(old)
		left := at == 0 || strings.ContainsRune("=;:\"' \t\n", rune(value[at-1]))
		right := end == len(value) || strings.ContainsRune("/\\;:\"' \t\n", rune(value[end]))
		out.WriteString(value[cursor:at])
		if left && right {
			out.WriteString(next)
		} else {
			out.WriteString(old)
		}
		cursor = end
	}
	return out.String()
}
