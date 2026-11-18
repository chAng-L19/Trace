package web

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"trace/internal/worker"
)

// Integration is service configuration, separate from run snapshots and evidence.
type Integration struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	GitHubURL  string   `json:"github_url"`
	LocalPath  string   `json:"local_path"`
	SourcePath string   `json:"source_path,omitempty"`
	Revision   string   `json:"revision"`
	Ref        string   `json:"ref,omitempty"`
	Subpath    string   `json:"subpath,omitempty"`
	SkillPath  string   `json:"skill_path,omitempty"`
	MCPNames   []string `json:"mcp_names,omitempty"`
	LocalNames []string `json:"local_names,omitempty"`
}

type githubLocation struct{ repository, tail, mode string }

func parseGitHub(raw string) (githubLocation, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return githubLocation{}, fmt.Errorf("use an HTTPS github.com repository, tree, or SKILL.md link")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return githubLocation{}, fmt.Errorf("GitHub owner and repository are required")
	}
	for _, p := range parts {
		if p == ".." || p == "." || strings.Contains(p, "\\") {
			return githubLocation{}, fmt.Errorf("invalid GitHub path")
		}
	}
	loc := githubLocation{repository: "https://github.com/" + parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")}
	if len(parts) > 2 {
		if len(parts) < 4 || (parts[2] != "tree" && parts[2] != "blob") {
			return loc, fmt.Errorf("use a GitHub repository, tree, or SKILL.md link")
		}
		loc.mode, loc.tail = parts[2], strings.Join(parts[3:], "/")
	}
	return loc, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	worker.ConfigureCommand(cmd)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func integrationID(kind, source string) string {
	return fmt.Sprintf("%s-%x", kind, sha256.Sum256([]byte(source)))[:len(kind)+17]
}

func resolveCheckout(ctx context.Context, dir string, loc githubLocation) (ref, subpath string, err error) {
	if loc.tail == "" {
		branch, e := git(ctx, dir, "symbolic-ref", "--short", "HEAD")
		return "refs/heads/" + branch, "", e
	}
	parts := strings.Split(loc.tail, "/")
	for n := len(parts); n > 0; n-- {
		candidate := strings.Join(parts[:n], "/")
		for _, prefix := range []string{"refs/remotes/origin/", "refs/tags/"} {
			revision, e := git(ctx, dir, "rev-parse", "--verify", prefix+candidate+"^{commit}")
			if e != nil {
				continue
			}
			if _, e = git(ctx, dir, "checkout", "--detach", revision); e != nil {
				return "", "", e
			}
			ref = prefix + candidate
			if prefix == "refs/remotes/origin/" {
				ref = "refs/heads/" + candidate
			}
			return ref, strings.Join(parts[n:], "/"), nil
		}
	}
	// GitHub also accepts pinned full commit URLs.
	if len(parts[0]) == 40 {
		if _, e := git(ctx, dir, "checkout", "--detach", parts[0]); e == nil {
			return parts[0], strings.Join(parts[1:], "/"), nil
		}
	}
	return "", "", fmt.Errorf("GitHub branch or tag was not found: %s", loc.tail)
}

func (s *Server) checkoutDirectory() (string, error) {
	if s.options.ConfigPath == "" {
		return "", fmt.Errorf("service config path is not configured")
	}
	// Keep repositories below the service directory, never beside Store snapshots.
	root, err := filepath.Abs(filepath.Join(filepath.Dir(s.options.ConfigPath), "integrations"))
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	return os.MkdirTemp(root, "checkout-")
}

func cloneIntegration(ctx context.Context, dir, kind, raw string) (Integration, error) {
	loc, err := parseGitHub(raw)
	if err != nil {
		return Integration{}, err
	}
	if _, err = git(ctx, "", "clone", "--filter=blob:none", "--", loc.repository, dir); err != nil {
		return Integration{}, err
	}
	ref, subpath, err := resolveCheckout(ctx, dir, loc)
	if err != nil {
		return Integration{}, err
	}
	root := filepath.Join(dir, filepath.FromSlash(subpath))
	info, err := os.Stat(root)
	if err != nil {
		return Integration{}, err
	}
	if loc.mode == "blob" {
		if info.IsDir() || info.Name() != "SKILL.md" || kind != "skill" {
			return Integration{}, fmt.Errorf("blob imports must point to a SKILL.md file")
		}
		subpath = filepath.ToSlash(filepath.Dir(subpath))
		if subpath == "." {
			subpath = ""
		}
	} else if !info.IsDir() {
		return Integration{}, fmt.Errorf("GitHub tree must point to a directory")
	}
	revision, err := git(ctx, dir, "rev-parse", "HEAD")
	entry := Integration{ID: integrationID(kind, strings.ToLower(loc.repository)+"/"+ref+"/"+subpath), Kind: kind, GitHubURL: strings.TrimSpace(raw), LocalPath: dir, Revision: revision, Ref: ref, Subpath: subpath}
	if kind == "skill" {
		entry.SkillPath = filepath.Join(dir, filepath.FromSlash(subpath))
	}
	return entry, err
}

type integrationStatus struct {
	Integration     Integration `json:"integration"`
	Status          string      `json:"status"`
	CurrentRevision string      `json:"current_revision"`
	LatestRevision  string      `json:"latest_revision"`
	Dirty           bool        `json:"dirty"`
	UpdateAvailable bool        `json:"update_available"`
}

func checkIntegration(ctx context.Context, entry Integration) (integrationStatus, error) {
	result := integrationStatus{Integration: entry, Status: "up_to_date"}
	current, err := git(ctx, entry.LocalPath, "rev-parse", "HEAD")
	if err != nil {
		return result, err
	}
	result.CurrentRevision = current
	status, err := git(ctx, entry.LocalPath, "status", "--porcelain")
	if err != nil {
		return result, err
	}
	result.Dirty = status != ""
	if !strings.HasPrefix(entry.Ref, "refs/heads/") {
		result.LatestRevision = current
		result.Status = "pinned"
		if result.Dirty {
			result.Status = "local_changes"
		}
		return result, nil
	}
	loc, err := parseGitHub(entry.GitHubURL)
	if err != nil {
		return result, err
	}
	// Fetch the declared GitHub source, not a possibly changed origin URL.
	if _, err = git(ctx, entry.LocalPath, "fetch", "--no-tags", loc.repository, entry.Ref); err != nil {
		return result, err
	}
	result.LatestRevision, err = git(ctx, entry.LocalPath, "rev-parse", "FETCH_HEAD")
	if err != nil {
		return result, err
	}
	if current != result.LatestRevision {
		result.UpdateAvailable = true
		result.Status = "update_available"
		if _, err = git(ctx, entry.LocalPath, "merge-base", "--is-ancestor", current, result.LatestRevision); err != nil {
			result.Status = "diverged"
		}
	}
	if result.Dirty {
		result.Status = "local_changes"
	}
	return result, nil
}

func prepareUpdate(ctx context.Context, entry Integration, nextPath, revision string) (Integration, error) {
	// A separate checkout keeps the previous files and live Agent intact on failure.
	if _, err := git(ctx, "", "clone", "--no-hardlinks", "--", entry.LocalPath, nextPath); err != nil {
		return entry, err
	}
	if _, err := git(ctx, nextPath, "checkout", "--detach", entry.Revision); err != nil {
		return entry, err
	}
	loc, err := parseGitHub(entry.GitHubURL)
	if err != nil {
		return entry, err
	}
	if _, err = git(ctx, nextPath, "remote", "set-url", "origin", loc.repository); err != nil {
		return entry, err
	}
	if _, err = git(ctx, nextPath, "fetch", "--no-tags", "origin", entry.Ref); err != nil {
		return entry, err
	}
	if _, err = git(ctx, nextPath, "merge", "--ff-only", revision); err != nil {
		return entry, err
	}
	entry.LocalPath, entry.Revision = nextPath, revision
	if entry.SkillPath != "" {
		entry.SkillPath = filepath.Join(nextPath, filepath.FromSlash(entry.Subpath))
	}
	return entry, nil
}
