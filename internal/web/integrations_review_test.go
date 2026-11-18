package web

import (
	"path/filepath"
	"strings"
	"testing"

	"trace/internal/mcp"
)

func TestIntegrationRemapPreservesSiblingProjects(t *testing.T) {
	root := t.TempDir()
	old := Integration{ID: "project", LocalPath: filepath.Join(root, "repo")}
	next := old
	next.LocalPath = filepath.Join(root, "updated")
	sibling := filepath.Join(root, "repo-extra", "skills")
	c := Config{SkillPaths: []string{filepath.Join(old.LocalPath, "skills"), sibling}, MCP: []mcp.Config{{Name: "other", Dir: filepath.Join(root, "repo-extra")}}}
	remapIntegration(&c, old, next)
	if c.SkillPaths[1] != sibling || c.MCP[0].Dir != filepath.Join(root, "repo-extra") {
		t.Fatalf("unrelated sibling source changed: %+v", c)
	}
}

func TestIntegrationRemapPathArgumentsAndEnvironment(t *testing.T) {
	old, next := `C:\projects\repo`, `C:\managed\checkout`
	for _, item := range []struct{ value, want string }{
		{old + `\server.js`, next + `\server.js`},
		{`--project=` + old + `\src`, `--project=` + next + `\src`},
		{`C:\other;` + old + `\lib;` + old + `-extra`, `C:\other;` + next + `\lib;` + old + `-extra`},
		{`TOKENprefix` + old, `TOKENprefix` + old},
		{old + `-extra`, old + `-extra`},
	} {
		if got := replaceSourcePath(item.value, old, next); got != item.want {
			t.Fatalf("remap %q got %q want %q", item.value, got, item.want)
		}
	}
}

func TestExistingSkillParentDiscoveryRootUpdate(t *testing.T) {
	remote, url := gitFixture(t)
	writeFixture(t, remote, "skills/one/SKILL.md", "---\nname: fixture-skill\ngithub_url: "+url+"\n---\nold body")
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "source metadata")
	root := t.TempDir()
	checkout := filepath.Join(root, "project")
	mustGit(t, "", "clone", "--", url, checkout)
	s := integrationFixture(t)
	s.config.SkillPaths = []string{root}
	if err := s.a.ConfigureSkills(s.config.SkillPaths); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, remote, "skills/one/SKILL.md", "---\nname: fixture-skill\n---\nupdated parent-root skill")
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "new parent-root skill")
	request(t, s.Handler(), "POST", "/api/integrations/update", integrationRequest{Kind: "skill", Name: "fixture-skill"}, 200)
	body, err := s.a.ReadSkill("fixture-skill")
	if err != nil || !strings.Contains(body, "updated parent-root skill") {
		t.Fatalf("update succeeded but live skill remained old: %q %v config=%+v", body, err, s.config)
	}
}
