package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trace/internal/agent"
	"trace/internal/mcp"
)

func TestConfigurationRebuildCancellation(t *testing.T) {
	for _, mode := range []string{"request", "close"} {
		t.Run(mode, func(t *testing.T) {
			s := integrationFixture(t)
			previous := s.a
			entered := make(chan struct{})
			s.options.BuildAgent = func(ctx context.Context, c Config) (*agent.Agent, error) {
				close(entered)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body, _ := json.Marshal(s.config)
			req := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("If-Match", `"`+s.config.Revision+`"`)
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { s.Handler().ServeHTTP(response, req); close(done) }()
			<-entered
			closed := make(chan struct{})
			if mode == "request" {
				cancel()
				close(closed)
			} else {
				go func() { _ = s.Close(); close(closed) }()
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				// Ensure a failing test never leaves the deliberately stalled builder alive.
				s.cancel()
				<-done
				t.Error("configuration rebuild ignored " + mode + " cancellation")
			}
			<-closed
			if s.a != previous || response.Code != 409 {
				t.Fatalf("cancelled rebuild changed live Agent or succeeded: status=%d", response.Code)
			}
		})
	}
}

func TestIntegrationStdioProcessFixture(t *testing.T) {
	if os.Getenv("TRACE_INTEGRATION_STDIO_FIXTURE") != "1" {
		return
	}
	version, err := os.ReadFile("version.txt")
	if err != nil {
		os.Exit(2)
	}
	if dependency := os.Getenv("TRACE_INTEGRATION_DEPENDENCY"); dependency != "" {
		if _, err := os.Stat(dependency); err != nil {
			os.Exit(4)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &rpc) != nil {
			os.Exit(3)
		}
		if len(rpc.ID) == 0 {
			continue
		}
		var result any
		switch rpc.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "round3", "version": string(version)}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{"name": "echo", "description": string(version), "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": string(version)}}}
		default:
			result = map[string]any{}
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}
	os.Exit(0)
}

func TestGitHubMCPUpdateStartsNewSourceProcess(t *testing.T) {
	remote, url := gitFixture(t)
	writeFixture(t, remote, "version.txt", "old process")
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "old process")
	s := integrationFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, s.Handler(), "POST", "/api/integrations/import", integrationRequest{Kind: "mcp", URL: url, MCP: &mcp.Config{Name: "process", Command: executable, Args: []string{"-test.run=^TestIntegrationStdioProcessFixture$"}, Env: map[string]string{"TRACE_INTEGRATION_STDIO_FIXTURE": "1"}}}, 200)
	var result struct{ Integration Integration }
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	assertLive := func(want string) {
		t.Helper()
		tools := request(t, s.Handler(), "GET", "/api/tools", nil, 200).Body.String()
		if !strings.Contains(tools, want) {
			t.Fatalf("live process did not read %q: %s", want, tools)
		}
		result, err := s.a.Registry().Call(context.Background(), mcp.ToolID("process", "echo"), json.RawMessage(`{}`))
		if err != nil || result.Text() != want {
			t.Fatalf("live call = %q, %v; want %q", result.Text(), err, want)
		}
	}
	assertLive("old process")
	writeFixture(t, remote, "version.txt", "updated process")
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "updated process")
	request(t, s.Handler(), "POST", "/api/integrations/update", integrationRequest{ID: result.Integration.ID}, 200)
	assertLive("updated process")
	if samePath(s.config.MCP[0].Dir, result.Integration.LocalPath) {
		t.Fatal("MCP source directory did not move")
	}
	if old, err := os.ReadFile(filepath.Join(result.Integration.LocalPath, "version.txt")); err != nil || string(old) != "old process" {
		t.Fatal("old source was not retained")
	}
}

func TestGitHubMCPUpdateMissingIgnoredDependencyKeepsOldProcess(t *testing.T) {
	remote, url := gitFixture(t)
	writeFixture(t, remote, "version.txt", "working old process")
	writeFixture(t, remote, ".gitignore", "node_modules/\n")
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "dependency fixture")
	s := integrationFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	request(t, s.Handler(), "POST", "/api/integrations/import", integrationRequest{Kind: "mcp", URL: url, Disabled: true, MCP: &mcp.Config{Name: "dependency", Command: executable, Args: []string{"-test.run=^TestIntegrationStdioProcessFixture$"}, Env: map[string]string{"TRACE_INTEGRATION_STDIO_FIXTURE": "1", "TRACE_INTEGRATION_DEPENDENCY": "node_modules/installed.txt"}}}, 200)
	entry := s.config.Integrations[0]
	writeFixture(t, entry.LocalPath, "node_modules/installed.txt", "installed")
	next := cloneConfig(s.config)
	next.MCP[0].Disabled = false
	request(t, s.Handler(), "PUT", "/api/config", next, 200)
	previous := s.a
	writeFixture(t, remote, "version.txt", "new requires dependency")
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "new version")
	request(t, s.Handler(), "POST", "/api/integrations/update", integrationRequest{ID: entry.ID}, 409)
	if s.a != previous || s.config.Integrations[0].LocalPath != entry.LocalPath {
		t.Fatal("dependency failure replaced old runtime")
	}
	tools := request(t, s.Handler(), "GET", "/api/tools", nil, 200).Body.String()
	if !strings.Contains(tools, "working old process") {
		t.Fatal("working old process lost")
	}
	result, err := s.a.Registry().Call(context.Background(), mcp.ToolID("dependency", "echo"), json.RawMessage(`{}`))
	if err != nil || result.Text() != "working old process" {
		t.Fatalf("old process unusable: %q %v", result.Text(), err)
	}
	if _, err := os.Stat(filepath.Join(entry.LocalPath, "node_modules/installed.txt")); err != nil {
		t.Fatal("old dependency removed")
	}
}

func TestGitHubSkillUpdateRejectsNewNameCollision(t *testing.T) {
	remote, url := gitFixture(t)
	s := integrationFixture(t)
	existing := t.TempDir()
	writeFixture(t, existing, "SKILL.md", "---\nname: other-skill\n---\nkeep other skill")
	s.config.SkillPaths = []string{existing}
	if err := s.a.ConfigureSkills(s.config.SkillPaths); err != nil {
		t.Fatal(err)
	}
	entry := importFixture(t, s, url)
	previous := s.a
	writeFixture(t, remote, "skills/one/SKILL.md", "---\nname: other-skill\n---\nupstream renamed skill")
	mustGit(t, remote, "add", ".")
	mustGit(t, remote, "commit", "-m", "collision")
	response := request(t, s.Handler(), "POST", "/api/integrations/update", integrationRequest{ID: entry.ID}, 409)
	if !strings.Contains(response.Body.String(), "skill name already exists") {
		t.Fatal(response.Body.String())
	}
	if s.a != previous {
		t.Fatal("collision replaced Agent")
	}
	for name, want := range map[string]string{"fixture-skill": "old body", "other-skill": "keep other skill"} {
		body, err := s.a.ReadSkill(name)
		if err != nil || !strings.Contains(body, want) {
			t.Fatalf("skill %q changed: %q %v", name, body, err)
		}
	}
}
