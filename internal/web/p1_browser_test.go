package web

// Server-only, real Store/API and fixed headed browser. Transport fixtures
// validate configuration and presentation; they make no model quality claim.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trace/internal/agent"
	"trace/internal/localtool"
	"trace/internal/mcp"
)

func TestP1BrowserAcceptance(t *testing.T) {
	if os.Getenv("TRACE_P1_BROWSER") != "1" {
		t.Skip("server-only fixed headed CloakBrowser acceptance")
	}
	if os.Getenv("DISPLAY") == "" || os.Getenv("CLOAKBROWSER_BINARY_PATH") == "" || os.Getenv("TRACE_CLOAKBROWSER_NODE_MODULE") == "" {
		t.Fatal("explicit fixed SDK, binary and headed DISPLAY required")
	}
	store, err := agent.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	upstream := newMCPFixture(t)
	skills, err := filepath.Abs(filepath.Join("..", "..", ".trace", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := localtool.Manifest{Name: "p1-local-browser", Command: "python3", Mode: localtool.ModeStdio, Description: "本地浏览器接口 / Local browser interface", InputSchema: json.RawMessage(`{"type":"object"}`)}
	config := Config{
		Provider: ProviderConfig{URL: "https://fixture.invalid/v1", Model: "main", APIKey: "fixture-secret", ReasoningEfforts: []string{"high"}, ModelCapabilities: map[string]agent.ModelCapabilities{"old": {ReasoningEfforts: []string{"high"}}}},
		Context:  agent.DefaultContextConfig(), SkillPaths: []string{skills},
		MCP: []mcp.Config{{Name: "p1-mcp", URL: upstream.URL}}, LocalTools: []localtool.Manifest{manifest},
	}
	build := func(ctx context.Context, c Config) (*agent.Agent, error) {
		makeProvider := func(p ProviderConfig) *agent.OpenAIProvider {
			return &agent.OpenAIProvider{BaseURL: p.URL, Model: p.Model, APIKey: p.APIKey, ImageInput: p.ImageInput, ReasoningEfforts: p.ReasoningEfforts, ModelCapabilities: p.ModelCapabilities}
		}
		a, err := agent.New(store, makeProvider(c.Provider), "")
		if err != nil {
			return nil, err
		}
		providers := map[string]*agent.OpenAIProvider{}
		for name, p := range c.Providers {
			providers[name] = makeProvider(p)
		}
		if err = a.ConfigureModels(providers, c.RoleModels); err == nil {
			err = a.ConfigureCosts(c.Costs)
		}
		if err == nil {
			err = a.ConfigureContext(c.Context)
		}
		if err == nil {
			err = a.ConfigureSkills(c.SkillPaths, c.DisabledSkills...)
		}
		if err == nil {
			err = a.AddLocalTools(c.LocalTools)
		}
		if err == nil {
			for _, cfg := range c.MCP {
				if _, err = a.AddMCP(ctx, cfg); err != nil {
					break
				}
			}
		}
		if err != nil {
			a.CloseMCP()
			return nil, err
		}
		return a, nil
	}
	a, err := build(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(a, Options{ConfigPath: filepath.Join(t.TempDir(), "config.json"), InitialConfig: config, BuildAgent: build})
	if err != nil {
		a.CloseMCP()
		t.Fatal(err)
	}
	defer app.Close()
	run, err := a.Create("P1 固定来源与交付验收")
	if err != nil {
		t.Fatal(err)
	}
	other, err := a.Create("P1 另一运行的晚响应负控")
	if err != nil {
		t.Fatal(err)
	}
	_, message, _, err := a.QueueMessageOperation(run.Run.ID, "steer", "p1-input", "P1 receipt input")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(run.Run.ID, func(s *agent.Snapshot) error {
		now := time.Now().UTC()
		s.Mailbox[0].ConsumedAt, s.Mailbox[0].MessageID = now, "p1-history-input"
		input := agent.Message{ID: "p1-history-input", Role: "user", Content: message.Content, CreatedAt: now}
		s.Messages = append(s.Messages, input)
		s.Requests = append(s.Requests, agent.ModelRequest{ID: "p1-summary", Purpose: "context_summary", Messages: []agent.Message{input}, CreatedAt: now})
		s.Responses = append(s.Responses, agent.ResponseRecord{RequestID: "p1-summary", Response: agent.ModelResponse{Content: "archived fixture summary"}})
		call := agent.ToolCall{ID: "p1-long", Name: "exec", Arguments: `{"command":"fixture read"}`}
		result := agent.ToolResult{Stdout: "P1_LONG_TOOL_OUTPUT\n" + strings.Repeat("long fixed output line\n", 5000)}
		resultJSON, err := json.Marshal(result)
		if err != nil {
			return err
		}
		s.Requests = append(s.Requests, agent.ModelRequest{ID: "p1-request", Messages: []agent.Message{input}, CreatedAt: now})
		s.Responses = append(s.Responses, agent.ResponseRecord{RequestID: "p1-request", Response: agent.ModelResponse{ToolCalls: []agent.ToolCall{call}}})
		s.Messages = append(s.Messages, agent.Message{ID: "p1-assistant", RequestID: "p1-request", Role: "assistant", ToolCalls: []agent.ToolCall{call}, CreatedAt: now}, agent.Message{ID: "p1-tool", RequestID: "p1-request", Role: "tool", ToolCallID: call.ID, Content: string(resultJSON), CreatedAt: now})
		s.Actions = append(s.Actions, agent.Action{Name: call.Name, RequestID: "p1-request", CallID: call.ID, Status: "completed", Arguments: call.Arguments, Result: &result})
		content := "P1 original source"
		digest := sha256.Sum256([]byte(content))
		s.Run.EventSeq++
		s.Events = append(s.Events, agent.Event{Sequence: s.Run.EventSeq, Type: "tool_result", Data: map[string]string{"call_id": "p1-observation", "evidence_id": "ev-p1"}, CreatedAt: now})
		s.Evidence = append(s.Evidence, agent.Evidence{ID: "ev-p1", CallID: "p1-observation", Type: "manual_observation", Content: content, ContentHash: hex.EncodeToString(digest[:]), SourceEventSequence: s.Run.EventSeq})
		s.Messages = append(s.Messages, agent.Message{Role: "tool", ToolCallID: "p1-observation", Content: content})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	chain, err := store.CreateEvidenceChain(run.Run.ID, agent.EvidenceChain{Name: "Meridian 2.4 · 固定条件 · 历史观察 · unverified", Summary: "原始手动来源，只验证固定引用展示，不作为执行证明。", Steps: []agent.EvidenceChainStep{{RunID: run.Run.ID, EvidenceID: "ev-p1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if chain.Verification != "unverified" || chain.SourceStatus != "unverified" {
		t.Fatalf("fixture must begin with an intact manual source, not invalid: %s/%s", chain.Verification, chain.SourceStatus)
	}
	mux := http.NewServeMux()
	mux.Handle("/", app.Handler())
	mux.HandleFunc("POST /_p1/change-source", func(w http.ResponseWriter, r *http.Request) {
		_, err := store.Update(run.Run.ID, func(s *agent.Snapshot) error {
			s.Evidence[0].Content, s.Evidence[0].ContentHash = "P1 changed source", "p1-changed-hash"
			return nil
		})
		respond(w, map[string]bool{"changed": err == nil}, err)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	out := os.Getenv("TRACE_P1_REPORT_DIR")
	if out == "" {
		out = t.TempDir()
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"base": server.URL, "run": run.Run.ID, "otherRun": other.Run.ID, "output": out, "chain": map[string]string{"run": run.Run.ID, "id": chain.Chain.ID, "hash": chain.Chain.ContentHash}, "toolMarker": "P1_LONG_TOOL_OUTPUT", "messageID": message.ID, "mcpTool": mcp.ToolID("p1-mcp", "echo"), "localTool": manifest.StableID()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	node := testNodeBinary()
	cmd := exec.CommandContext(ctx, node, "p1_browser.mjs")
	cmd.Env = append(os.Environ(), "TRACE_P1_FIXTURE="+string(data))
	output, err := cmd.CombinedOutput()
	t.Logf("P1_BROWSER %s", output)
	if err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	secret := app.config.Provider.APIKey
	app.mu.Unlock()
	if secret != "fixture-secret" {
		t.Fatal("masked ordinary-form save changed stored credential")
	}
}
