package web

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"trace/internal/agent"
)

func withinPath(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	rel, err := filepath.Rel(a, b)
	return err == nil && rel == "."
}

func githubOrigin(raw string) string {
	if strings.HasPrefix(raw, "git@github.com:") {
		raw = "https://github.com/" + strings.TrimPrefix(raw, "git@github.com:")
	}
	if strings.HasPrefix(raw, "ssh://git@github.com/") {
		raw = "https://github.com/" + strings.TrimPrefix(raw, "ssh://git@github.com/")
	}
	loc, err := parseGitHub(raw)
	if err != nil {
		return ""
	}
	return loc.repository
}

func (s *Server) localSource(path string) (root, origin string) {
	if path == "" {
		return "", ""
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		path = filepath.Dir(path)
	}
	root, err := git(s.ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", ""
	}
	raw, err := git(s.ctx, path, "remote", "get-url", "origin")
	if err != nil {
		return root, ""
	}
	return filepath.Clean(root), githubOrigin(raw)
}

func sourceDirectory(local, dir, command string, args []string) string {
	if local != "" {
		return local
	}
	// A work directory or interpreter binary is execution metadata, not source.
	scriptDirectory := func(value string) string {
		switch strings.ToLower(filepath.Ext(value)) {
		case ".py", ".js", ".mjs", ".cjs", ".ts", ".sh", ".ps1", ".rb":
		default:
			return ""
		}
		if !filepath.IsAbs(value) {
			if dir == "" {
				return ""
			}
			value = filepath.Join(dir, value)
		}
		if info, err := os.Stat(value); err == nil && !info.IsDir() {
			return filepath.Dir(value)
		}
		return ""
	}
	if path := scriptDirectory(command); path != "" {
		return path
	}
	for _, value := range args {
		if value == "-m" || value == "-c" || value == "-e" || value == "--eval" {
			return ""
		}
		if strings.HasPrefix(value, "-") {
			continue
		}
		return scriptDirectory(value)
	}
	return ""
}

func (s *Server) configWithSources() Config {
	c := cloneConfig(s.config)
	type sourceInfo struct{ root, origin string }
	cache := map[string]sourceInfo{}
	resolve := func(local, dir, command string, args []string) (string, string) {
		path := sourceDirectory(local, dir, command, args)
		// Git membership does not turn an interpreter or unrelated working
		// directory into source provenance. Only upgrade an actual source path.
		if path == "" {
			return "", ""
		}
		info, ok := cache[path]
		if !ok {
			info.root, info.origin = s.localSource(path)
			cache[path] = info
		}
		if info.root != "" {
			path = info.root
		}
		return path, info.origin
	}
	for i := range c.MCP {
		cfg := &c.MCP[i]
		path, origin := resolve(cfg.LocalPath, cfg.Dir, cfg.Command, cfg.Args)
		cfg.LocalPath = path
		if cfg.GitHubURL == "" {
			cfg.GitHubURL = origin
		}
	}
	for i := range c.LocalTools {
		cfg := &c.LocalTools[i]
		path, origin := resolve(cfg.LocalPath, cfg.Dir, cfg.Command, cfg.Args)
		cfg.LocalPath = path
		if cfg.GitHubURL == "" {
			cfg.GitHubURL = origin
		}
	}
	return c
}

func (s *Server) skillsWithSources() []agent.Skill {
	skills := s.a.Skills()
	roots := map[string][2]string{}
	for i := range skills {
		skill := &skills[i]
		local := skill.LocalPath
		if local == "" {
			local = filepath.Dir(skill.Path)
		}
		found := false
		for _, entry := range s.config.Integrations {
			if withinPath(entry.LocalPath, skill.Path) || samePath(local, entry.LocalPath) || samePath(local, entry.SourcePath) {
				skill.GitHubURL, skill.LocalPath, skill.IntegrationID = entry.GitHubURL, entry.LocalPath, entry.ID
				found = true
				break
			}
		}
		if found {
			continue
		}
		source, ok := roots[local]
		if !ok {
			root, origin := s.localSource(local)
			source = [2]string{root, origin}
			roots[local] = source
		}
		if skill.GitHubURL == "" {
			skill.GitHubURL = source[1]
		}
		if skill.LocalPath == "" {
			skill.LocalPath = filepath.Dir(skill.Path)
		}
		if source[0] != "" && skill.GitHubURL != "" {
			skill.LocalPath = source[0]
		}
	}
	return skills
}

func (s *Server) findIntegration(in integrationRequest) (Integration, error) {
	if in.ID != "" {
		for _, entry := range s.config.Integrations {
			if entry.ID == in.ID {
				return entry, nil
			}
		}
		return Integration{}, fmt.Errorf("integration not found: %s", in.ID)
	}
	c := s.configWithSources()
	entry := Integration{Kind: in.Kind}
	switch in.Kind {
	case "mcp":
		for _, cfg := range c.MCP {
			if cfg.Name == in.Name {
				entry.GitHubURL, entry.LocalPath = cfg.GitHubURL, cfg.LocalPath
				entry.MCPNames = []string{cfg.Name}
				break
			}
		}
	case "local":
		for _, cfg := range c.LocalTools {
			if cfg.Name == in.Name {
				entry.GitHubURL, entry.LocalPath = cfg.GitHubURL, cfg.LocalPath
				entry.LocalNames = []string{cfg.Name}
				break
			}
		}
	case "skill":
		for _, skill := range s.skillsWithSources() {
			if skill.Name != in.Name {
				continue
			}
			if skill.IntegrationID != "" {
				return s.findIntegration(integrationRequest{ID: skill.IntegrationID})
			}
			entry.GitHubURL, entry.LocalPath = skill.GitHubURL, skill.LocalPath
			if withinPath(entry.LocalPath, skill.Path) {
				entry.SkillPath = skill.Source
				if !withinPath(entry.LocalPath, entry.SkillPath) {
					entry.SkillPath = filepath.Dir(skill.Path)
				}
				relative, _ := filepath.Rel(entry.LocalPath, entry.SkillPath)
				entry.Subpath = filepath.ToSlash(relative)
			}
			break
		}
	default:
		return entry, fmt.Errorf("provide integration id or kind (mcp, skill, local) and name")
	}
	if _, err := parseGitHub(entry.GitHubURL); err != nil {
		return entry, fmt.Errorf("item has no GitHub source: %s", in.Name)
	}
	root, _ := s.localSource(entry.LocalPath)
	if root == "" {
		return entry, fmt.Errorf("item has no local Git checkout; import its GitHub URL first")
	}
	entry.LocalPath = root
	for _, existing := range c.Integrations {
		if filepath.Clean(existing.LocalPath) == root {
			return existing, nil
		}
	}
	if entry.SkillPath == "" {
		for _, skill := range s.a.Skills() {
			if withinPath(root, skill.Path) {
				entry.SkillPath = root
				entry.Subpath = ""
				break
			}
		}
	}
	entry.SourcePath = root
	entry.ID = integrationID(entry.Kind, root)
	revision, err := git(s.ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return entry, err
	}
	entry.Revision = revision
	branch, err := git(s.ctx, root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || s.ctx.Err() != nil {
			return entry, err
		}
		entry.Ref = entry.Revision
	} else {
		entry.Ref = "refs/heads/" + branch
	}
	return entry, nil
}
