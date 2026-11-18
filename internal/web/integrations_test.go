package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trace/internal/agent"
	"trace/internal/localtool"
	"trace/internal/mcp"
)

func gitFixture(t *testing.T) (string, string) {
	t.Helper()
	remote := t.TempDir()
	mustGit(t, remote, "init", "-b", "main")
	mustGit(t, remote, "config", "user.name", "Trace Test")
	mustGit(t, remote, "config", "user.email", "trace@example.test")
	writeFixture(t, remote, "skills/one/SKILL.md", "---\nname: fixture-skill\ndescription: fixture\n---\nold body")
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "initial")
	url := "https://github.com/fixture/integration"
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "url."+filepath.ToSlash(remote)+".insteadOf")
	t.Setenv("GIT_CONFIG_VALUE_0", url)
	return remote, url
}
func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func writeFixture(t *testing.T, root, path, body string) {
	t.Helper()
	dest := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}
func integrationFixture(t *testing.T) *Server {
	t.Helper()
	s, _ := fixture(t, nil)
	s.options.BuildAgent = func(ctx context.Context, c Config) (a *agent.Agent, err error) {
		a, err = agent.New(s.store, nil, "")
		if err != nil {
			return
		}
		defer func() {
			if err != nil {
				_ = a.CloseMCP()
			}
		}()
		if err = a.ConfigureSkills(c.SkillPaths); err != nil {
			return
		}
		if err = a.AddLocalTools(c.LocalTools); err != nil {
			return
		}
		for _, cfg := range c.MCP {
			if _, err = a.AddMCP(ctx, cfg); err != nil {
				return
			}
		}
		return
	}
	return s
}
func importFixture(t *testing.T, s *Server, url string) Integration {
	t.Helper()
	response := request(t, s.Handler(), "POST", "/api/integrations/import", integrationRequest{Kind: "skill", URL: url}, 200)
	var result struct{ Integration Integration }
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result.Integration
}

func TestGitHubSkillImportUpdateAndFailureKeepsReadableOldFiles(t *testing.T) {
	remote, url := gitFixture(t)
	s := integrationFixture(t)
	entry := importFixture(t, s, url+"/blob/main/skills/one/SKILL.md")
	body, err := s.a.ReadSkill("fixture-skill")
	if err != nil || !strings.Contains(body, "old body") {
		t.Fatalf("skill not live: %s %v", body, err)
	}
	skills := s.skillsWithSources()
	if len(skills) == 0 || skills[0].IntegrationID != entry.ID || skills[0].GitHubURL != entry.GitHubURL {
		t.Fatalf("missing source: %+v", skills)
	}
	request(t, s.Handler(), "POST", "/api/integrations/import", integrationRequest{Kind: "skill", URL: url + "/tree/main/skills/one"}, 409)
	before, _ := os.ReadDir(filepath.Dir(entry.LocalPath))
	status := request(t, s.Handler(), "POST", "/api/integrations/update", integrationRequest{ID: entry.ID}, 200)
	if !strings.Contains(status.Body.String(), "up_to_date") {
		t.Fatal(status.Body.String())
	}
	after, _ := os.ReadDir(filepath.Dir(entry.LocalPath))
	if len(before) != len(after) {
		t.Fatal("no-op update created checkout")
	}
	writeFixture(t, remote, "skills/one/SKILL.md", "---\nname: fixture-skill\n---\nnew body")
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "new skill")
	status = request(t, s.Handler(), "POST", "/api/integrations/check", integrationRequest{ID: entry.ID}, 200)
	if !strings.Contains(status.Body.String(), "update_available") {
		t.Fatal(status.Body.String())
	}
	previousAgent := s.a
	build := s.options.BuildAgent
	s.options.BuildAgent = func(context.Context, Config) (*agent.Agent, error) {
		return nil, fmt.Errorf("fixture connection failed")
	}
	request(t, s.Handler(), "POST", "/api/integrations/update", integrationRequest{ID: entry.ID}, 409)
	body, err = s.a.ReadSkill("fixture-skill")
	if s.a != previousAgent || err != nil || !strings.Contains(body, "old body") {
		t.Fatalf("failed update changed live skill: %s %v", body, err)
	}
	if head := mustGit(t, entry.LocalPath, "rev-parse", "HEAD"); head != entry.Revision {
		t.Fatal("failed update moved old checkout")
	}
	s.options.BuildAgent = build
	request(t, s.Handler(), "POST", "/api/integrations/update", integrationRequest{ID: entry.ID}, 200)
	body, err = s.a.ReadSkill("fixture-skill")
	if err != nil || !strings.Contains(body, "new body") {
		t.Fatalf("updated skill not live: %s %v", body, err)
	}
	old, err := os.ReadFile(filepath.Join(entry.SkillPath, "SKILL.md"))
	if err != nil || !strings.Contains(string(old), "old body") {
		t.Fatal("previous checkout not retained")
	}
	if s.config.Integrations[0].LocalPath == entry.LocalPath {
		t.Fatal("update reused live directory")
	}
}

func TestGitHubUpdatePreservesDirtyAndDivergedCheckouts(t *testing.T) {
	for _, mode := range []string{"dirty", "diverged"} {
		t.Run(mode, func(t *testing.T) {
			remote, url := gitFixture(t)
			s := integrationFixture(t)
			entry := importFixture(t, s, url)
			writeFixture(t, entry.LocalPath, "local-notes.txt", "preserve this")
			if mode == "diverged" {
				mustGit(t, entry.LocalPath, "-c", "user.name=Trace", "-c", "user.email=trace@example.test", "add", ".")
				mustGit(t, entry.LocalPath, "-c", "user.name=Trace", "-c", "user.email=trace@example.test", "commit", "-m", "local patch")
			}
			writeFixture(t, remote, "upstream.txt", "new")
			mustGit(t, remote, "add", ".")
			mustGit(t, remote, "commit", "-m", "upstream")
			request(t, s.Handler(), "POST", "/api/integrations/update", integrationRequest{ID: entry.ID}, 409)
			b, err := os.ReadFile(filepath.Join(entry.LocalPath, "local-notes.txt"))
			if err != nil || string(b) != "preserve this" {
				t.Fatal("local change was lost")
			}
			if s.config.Integrations[0].LocalPath != entry.LocalPath {
				t.Fatal("rejected update changed config")
			}
		})
	}
}

func TestGitHubBranchNamesAndPinnedTags(t *testing.T) {
	remote, url := gitFixture(t)
	mustGit(t, remote, "checkout", "-b", "feature/skills")
	writeFixture(t, remote, "skills/one/SKILL.md", "---\nname: branch-skill\n---\nbranch")
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "branch")
	mustGit(t, remote, "tag", "v1")
	mustGit(t, remote, "checkout", "main")
	s := integrationFixture(t)
	entry := importFixture(t, s, url+"/tree/feature/skills/skills/one")
	if entry.Ref != "refs/heads/feature/skills" {
		t.Fatalf("wrong ref: %+v", entry)
	}
	if _, err := s.a.ReadSkill("branch-skill"); err != nil {
		t.Fatal(err)
	}
	second := integrationFixture(t)
	tag := importFixture(t, second, url+"/tree/v1/skills/one")
	status := request(t, second.Handler(), "POST", "/api/integrations/update", integrationRequest{ID: tag.ID}, 200)
	if !strings.Contains(status.Body.String(), `"status":"pinned"`) {
		t.Fatal(status.Body.String())
	}
}

func TestGitHubImportRequiresEntryAndPreservesConfig(t *testing.T) {
	_, url := gitFixture(t)
	s := integrationFixture(t)
	old := s.a
	for _, in := range []integrationRequest{{Kind: "skill", URL: url + "/tree/main/missing"}, {Kind: "mcp", URL: url}, {Kind: "skill", URL: "https://example.com/user/repo"}} {
		want := 409
		if strings.Contains(in.URL, "example.com") {
			want = 400
		}
		request(t, s.Handler(), "POST", "/api/integrations/import", in, want)
		if s.a != old || len(s.config.Integrations) != 0 {
			t.Fatal("failed import replaced runtime")
		}
	}
}

func TestMCPGitHubImportLoadsManifestAndLiveTool(t *testing.T) {
	remote, url := gitFixture(t)
	mcpServer := newMCPFixture(t)
	manifest := fmt.Sprintf(`{"mcpServers":{"fixture":{"url":%q,"description":"from repository","env":{"TOKEN":"secret"}}}}`, mcpServer.URL)
	writeFixture(t, remote, ".mcp.json", manifest)
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "mcp manifest")
	s := integrationFixture(t)
	response := request(t, s.Handler(), "POST", "/api/integrations/import", integrationRequest{Kind: "mcp", URL: url}, 200)
	if strings.Contains(response.Body.String(), `"TOKEN":"secret"`) {
		t.Fatal("import exposed secret")
	}
	if len(s.config.MCP) != 1 || s.config.MCP[0].GitHubURL != url {
		t.Fatalf("manifest not configured: %+v", s.config.MCP)
	}
	if _, ok := s.a.Registry().Lookup(mcp.ToolID("fixture", "echo")); !ok {
		t.Fatal("MCP tool not registered")
	}
	request(t, s.Handler(), "POST", "/api/integrations/check", integrationRequest{Kind: "mcp", Name: "fixture"}, 200)
}

func TestIntegrationBusyPreservesReadAccess(t *testing.T) {
	s := integrationFixture(t)
	s.configuring = true
	request(t, s.Handler(), http.MethodGet, "/api/config", nil, 200)
	request(t, s.Handler(), http.MethodGet, "/api/skills", nil, 200)
	request(t, s.Handler(), http.MethodPut, "/api/config", s.config, 409)
	request(t, s.Handler(), http.MethodPost, "/api/integrations/update", integrationRequest{ID: "missing"}, 409)
	s.configuring = false
}

func newMCPFixture(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if rpc.Method == "notifications/initialized" {
			w.WriteHeader(202)
			return
		}
		var result any
		switch rpc.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "echo", "inputSchema": map[string]any{"type": "object"}}}}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	t.Cleanup(server.Close)
	return server
}

func TestExistingLocalProjectAndWrapperShareUpdatedSource(t *testing.T) {
	remote, url := gitFixture(t)
	existing := filepath.Join(t.TempDir(), "existing")
	mustGit(t, "", "clone", "--", url, existing)
	s := integrationFixture(t)
	wrapper := t.TempDir()
	writeFixture(t, wrapper, "SKILL.md", "---\nname: wrapper\ngithub_url: "+url+"\nlocal_path: "+filepath.ToSlash(existing)+"\n---\nwrapper body")
	s.config.SkillPaths = []string{wrapper}
	s.config.LocalTools = []localtool.Manifest{{Name: "project-cli", Command: "python", Args: []string{"-m", "fixture"}, GitHubURL: url, LocalPath: existing, Env: map[string]string{"PYTHONPATH": existing}}}
	a, err := s.options.BuildAgent(context.Background(), s.config)
	if err != nil {
		t.Fatal(err)
	}
	s.a = a
	writeFixture(t, remote, "version.txt", "updated")
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "new version")
	request(t, s.Handler(), "POST", "/api/integrations/update", integrationRequest{Kind: "skill", Name: "wrapper"}, 200)
	next := s.config.LocalTools[0]
	if samePath(next.LocalPath, existing) || next.Env["PYTHONPATH"] != next.LocalPath {
		t.Fatalf("CLI still uses old checkout: %+v", next)
	}
	if b, err := os.ReadFile(filepath.Join(next.LocalPath, "version.txt")); err != nil || string(b) != "updated" {
		t.Fatalf("missing updated CLI: %v", err)
	}
	var actual agent.Skill
	for _, skill := range s.skillsWithSources() {
		if skill.Name == "wrapper" {
			actual = skill
		}
	}
	if actual.IntegrationID == "" || !samePath(actual.LocalPath, next.LocalPath) {
		t.Fatalf("wrapper source not linked: %+v", actual)
	}
	if body, err := s.a.ReadSkill("wrapper"); err != nil || !strings.Contains(body, "wrapper body") {
		t.Fatalf("wrapper lost: %v", err)
	}
	request(t, s.Handler(), "POST", "/api/integrations/check", integrationRequest{Kind: "local", Name: "project-cli"}, 200)
}

func TestGitHubFailedConfigSaveKeepsOldRuntime(t *testing.T) {
	remote, url := gitFixture(t)
	s := integrationFixture(t)
	entry := importFixture(t, s, url)
	writeFixture(t, remote, "skills/one/SKILL.md", "---\nname: fixture-skill\n---\nnew body")
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "update")
	// A directory at the config filename makes the final atomic rename fail.
	s.options.ConfigPath = filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(s.options.ConfigPath, 0700); err != nil {
		t.Fatal(err)
	}
	old := s.a
	request(t, s.Handler(), "POST", "/api/integrations/update", integrationRequest{ID: entry.ID}, 409)
	body, err := s.a.ReadSkill("fixture-skill")
	if s.a != old || err != nil || !strings.Contains(body, "old body") {
		t.Fatalf("save failure damaged old runtime: %v %s", err, body)
	}
}

func TestIntegrationOperationGatesAllWorkerLaunches(t *testing.T) {
	s := integrationFixture(t)
	snap, err := s.a.Create("busy fixture")
	if err != nil {
		t.Fatal(err)
	}
	s.configuring = true
	for _, path := range []string{"/api/runs/" + snap.Run.ID + "/resume", "/api/runs/" + snap.Run.ID + "/plan/dispatch", "/api/runs/" + snap.Run.ID + "/tasks/task/dispatch"} {
		response := request(t, s.Handler(), "POST", path, map[string]any{}, 409)
		if !strings.Contains(response.Body.String(), "integration operation") {
			t.Fatalf("busy gate missing: %s", response.Body.String())
		}
	}
	s.configuring = false
}

func TestGitCommandCancellation(t *testing.T) {
	remote, _ := gitFixture(t)
	// Git aliases run a child shell. Reuse worker's existing process-tree cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := git(ctx, remote, "-c", "alias.waitfixture=!sleep 20", "waitfixture")
	if err == nil || ctx.Err() == nil {
		t.Fatalf("git cancellation not observed: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("git descendants kept command alive: %v", elapsed)
	}
}

func TestSkillNameCollisionRejectsInvisibleImport(t *testing.T) {
	_, url := gitFixture(t)
	s := integrationFixture(t)
	existing := t.TempDir()
	writeFixture(t, existing, "SKILL.md", "---\nname: fixture-skill\n---\nkeep existing")
	s.config.SkillPaths = []string{existing}
	if err := s.a.ConfigureSkills(s.config.SkillPaths); err != nil {
		t.Fatal(err)
	}
	response := request(t, s.Handler(), "POST", "/api/integrations/import", integrationRequest{Kind: "skill", URL: url}, 409)
	if !strings.Contains(response.Body.String(), "skill name already exists") {
		t.Fatal(response.Body.String())
	}
	body, err := s.a.ReadSkill("fixture-skill")
	if err != nil || !strings.Contains(body, "keep existing") {
		t.Fatal("collision replaced existing skill")
	}
}
func TestGitHubMCPDisabledImportThenEnableDiscoversTools(t *testing.T) {
	_, url := gitFixture(t)
	s := integrationFixture(t)
	server := newMCPFixture(t)
	response := request(t, s.Handler(), "POST", "/api/integrations/import", integrationRequest{Kind: "mcp", URL: url, MCP: &mcp.Config{Name: "pending", Command: "not-installed-yet", Disabled: true}}, 200)
	if !strings.Contains(response.Body.String(), `"disabled":true`) {
		t.Fatal("disabled state not reported")
	}
	if _, ok := s.a.Registry().Lookup(mcp.ToolID("pending", "echo")); ok {
		t.Fatal("disabled MCP registered tools")
	}
	if _, err := os.Stat(s.config.Integrations[0].LocalPath); err != nil {
		t.Fatal("disabled import lost checkout")
	}
	next := cloneConfig(s.config)
	next.MCP[0].Disabled = false
	next.MCP[0].Command = ""
	next.MCP[0].URL = server.URL
	request(t, s.Handler(), "PUT", "/api/config", next, 200)
	if _, ok := s.a.Registry().Lookup(mcp.ToolID("pending", "echo")); !ok {
		t.Fatal("enabled MCP did not discover tools")
	}
}

func TestGitHubMCPManifestCanImportDisabled(t *testing.T) {
	remote, url := gitFixture(t)
	s := integrationFixture(t)
	writeFixture(t, remote, ".mcp.json", `{"mcpServers":{"pending":{"command":"not-installed-yet"}}}`)
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "manifest")
	request(t, s.Handler(), "POST", "/api/integrations/import", integrationRequest{Kind: "mcp", URL: url, Disabled: true}, 200)
	if len(s.config.MCP) != 1 || !s.config.MCP[0].Disabled {
		t.Fatal("manifest was started despite disabled import")
	}
}
