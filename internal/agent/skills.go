package agent

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

// A skill is an on-demand instruction, not an unbounded file-read channel.
const maxSkillBytes = 64 << 10

type SkillRead struct {
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	Source   string      `json:"source"`
	SHA256   string      `json:"sha256"`
	Bytes    int         `json:"bytes"`
	Body     string      `json:"body"`
	Artifact ArtifactRef `json:"artifact"`
}

type Skill struct {
	Disabled      bool   `json:"disabled"`
	GitHubURL     string `json:"github_url,omitempty"`
	LocalPath     string `json:"local_path,omitempty"`
	IntegrationID string `json:"integration_id,omitempty"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Path          string `json:"path"`
	Source        string `json:"source"`
}

// DiscoverSkills reads only frontmatter; bodies are loaded explicitly by ReadSkill.
// Roots are ordered from highest to lowest priority; the first name wins.
func DiscoverSkills(roots []string) ([]Skill, error) {
	byName := map[string]Skill{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || entry.Name() != "SKILL.md" {
				return nil
			}
			f, err := openSkillFile(path)
			if err != nil {
				return err
			}
			defer f.Close()
			info, err := f.Stat()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("skill must be a regular file: %s", path)
			}
			abs, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			skill := Skill{Name: filepath.Base(filepath.Dir(path)), Path: abs}
			skill.Source, err = filepath.Abs(root)
			if err != nil {
				return err
			}
			scanner := bufio.NewScanner(io.LimitReader(f, maxSkillBytes+1))
			if scanner.Scan() && strings.TrimSpace(scanner.Text()) == "---" {
				continuation := false
				closed := false
				for scanner.Scan() {
					line := scanner.Text()
					trimmed := strings.TrimSpace(line)
					if trimmed == "---" {
						closed = true
						break
					}
					if continuation && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
						skill.Description += " " + trimmed
						continue
					}
					continuation = false
					key, value, ok := strings.Cut(trimmed, ":")
					if !ok {
						continue
					}
					value = strings.Trim(strings.TrimSpace(value), "\"'")
					switch key {
					case "github_url":
						skill.GitHubURL = value
					case "local_path":
						skill.LocalPath = value
					case "name":
						if value != "" {
							skill.Name = value
						}
					case "description":
						if value == ">" || value == "|" || value == ">-" || value == "|-" {
							continuation = true
						} else {
							skill.Description = value
						}
					}
				}
				if !closed {
					return fmt.Errorf("skill frontmatter is incomplete or exceeds %d bytes: %s", maxSkillBytes, path)
				}
			}
			if err := scanner.Err(); err != nil {
				return err
			}
			skill.Description = strings.TrimSpace(skill.Description)
			if _, exists := byName[skill.Name]; exists {
				return nil
			}
			byName[skill.Name] = skill
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("discover skills %s: %w", root, err)
		}
	}
	out := make([]Skill, 0, len(byName))
	for _, skill := range byName {
		out = append(out, skill)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
func (a *Agent) ConfigureSkills(roots []string, disabledNames ...string) error {
	// Explicit paths override project skills, which override user skills.
	roots = append([]string(nil), roots...)
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(cwd, ".trace", "skills"), filepath.Join(home, ".trace", "skills")} {
		if _, err := os.Stat(path); err == nil {
			roots = append(roots, path)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	skills, err := DiscoverSkills(roots)
	if err != nil {
		return err
	}
	// Resolve the winning source first. Disabling a name must not expose a
	// lower-priority copy from another explicit, project or user root.
	disabled := make(map[string]bool, len(disabledNames))
	for _, name := range disabledNames {
		disabled[name] = true
	}
	for i := range skills {
		skills[i].Disabled = disabled[skills[i].Name]
	}
	a.skills = skills
	return nil
}
func (a *Agent) Skills() []Skill { return append([]Skill(nil), a.skills...) }
func (a *Agent) ModelSkills() []Skill {
	skills := make([]Skill, 0, len(a.skills))
	for _, skill := range a.skills {
		if !skill.Disabled {
			skills = append(skills, skill)
		}
	}
	return skills
}

// ReadSkill is the management read path, including disabled entries.
func (a *Agent) ReadSkill(name string) (string, error) {
	for _, skill := range a.skills {
		if skill.Name == name {
			return readSkillBody(skill.Path)
		}
	}
	return "", fmt.Errorf("skill not found: %s", name)
}

func readSkillBody(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("skill must be a regular file")
	}
	if info.Size() > maxSkillBytes {
		return "", fmt.Errorf("skill exceeds %d byte limit", maxSkillBytes)
	}
	f, err := openSkillFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !before.Mode().IsRegular() || !os.SameFile(info, before) {
		return "", fmt.Errorf("skill changed while opening; rediscover and retry")
	}
	body, err := io.ReadAll(io.LimitReader(f, maxSkillBytes+1))
	if err != nil {
		return "", err
	}
	if len(body) > maxSkillBytes {
		return "", fmt.Errorf("skill exceeds %d byte limit", maxSkillBytes)
	}
	after, err := f.Stat()
	if err != nil {
		return "", err
	}
	current, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !os.SameFile(before, current) || before.Size() != int64(len(body)) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || !after.ModTime().Equal(current.ModTime()) {
		return "", fmt.Errorf("skill changed during read; retry with its current identity")
	}
	if !utf8.Valid(body) {
		return "", fmt.Errorf("skill is not valid UTF-8")
	}
	return string(body), nil
}

// The tool ledger and CAS retain the exact loaded revision, while a subsequent
// read still observes the current file. expectedSHA256 makes a pinned read explicit.
func (a *Agent) readModelSkillDocument(name, expectedSHA256 string) (SkillRead, error) {
	if expectedSHA256 != "" {
		decoded, err := hex.DecodeString(expectedSHA256)
		if err != nil || len(decoded) != sha256.Size || strings.ToLower(expectedSHA256) != expectedSHA256 {
			return SkillRead{}, fmt.Errorf("expected_sha256 must be 64 lowercase hexadecimal characters")
		}
	}
	for _, skill := range a.skills {
		if skill.Name != name {
			continue
		}
		if skill.Disabled {
			return SkillRead{}, fmt.Errorf("skill is disabled for model use: %s", name)
		}
		body, err := readSkillBody(skill.Path)
		if err != nil {
			return SkillRead{}, err
		}
		digest := sha256.Sum256([]byte(body))
		hash := hex.EncodeToString(digest[:])
		if expectedSHA256 != "" && expectedSHA256 != hash {
			return SkillRead{}, fmt.Errorf("skill revision mismatch: expected %s, current %s", expectedSHA256, hash)
		}
		ref, err := a.store.PutArtifact("skill", []byte(body))
		if err != nil {
			return SkillRead{}, err
		}
		return SkillRead{Name: name, Path: skill.Path, Source: skill.Source, SHA256: hash, Bytes: len(body), Body: body, Artifact: ref}, nil
	}
	return SkillRead{}, fmt.Errorf("skill not found: %s", name)
}
func (a *Agent) ReadModelSkill(name string) (string, error) {
	for _, skill := range a.skills {
		if skill.Name == name && skill.Disabled {
			return "", fmt.Errorf("skill is disabled for model use: %s", name)
		}
	}
	return a.ReadSkill(name)
}
func (a *Agent) skillCatalog() string {
	skills := a.ModelSkills()
	if len(skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nAvailable skills (metadata only; use read_skill to load instructions when needed):\n")
	for _, skill := range skills {
		fmt.Fprintf(&b, "- %s: %s (location: %s)\n", skill.Name, skill.Description, skill.Path)
	}
	return b.String()
}
