package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"trace/internal/localtool"
	"trace/internal/mcp"
)

func TestRunResponsibilityRegisteredEntrypointsDenyBeforeInvocation(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, _ := responsibilityAgent(t, backend, MockProvider{})
			server, calls, _ := integrationServer(t, "success")
			addIntegrationMCP(t, a, mcp.Config{Name: "fixture", URL: server.URL})
			marker := filepath.Join(t.TempDir(), "local-started")
			manifest := localManifest(t, "echo")
			manifest.Name = "browser-fixture"
			manifest.Env["TRACE_AGENT_LOCAL_MARKER"] = marker
			if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
				t.Fatal(err)
			}
			remoteID := mcp.ToolID("fixture", "echo")
			for _, test := range []struct {
				name, tool string
				run        func(string, ToolCall) (Snapshot, error)
			}{
				{"manual-exec", "plan", func(id string, c ToolCall) (Snapshot, error) {
					return a.ExecuteTool(context.Background(), id, c.ID, []string{manifest.Command})
				}},
				{"process", "exec", func(id string, c ToolCall) (Snapshot, error) {
					return a.executeProcess(context.Background(), id, c.ID, []string{manifest.Command}, "")
				}},
				{"builtin", "write", func(id string, c ToolCall) (Snapshot, error) { return a.executeBuiltin(context.Background(), id, c) }},
				{"registered-local", manifest.StableID(), func(id string, c ToolCall) (Snapshot, error) {
					return a.ExecuteRegistered(context.Background(), id, c.ID, c.Name)
				}},
				{"registered-local-locked", manifest.StableID(), func(id string, c ToolCall) (Snapshot, error) {
					return a.executeRegisteredLocked(context.Background(), id, c.ID, c.Name)
				}},
				{"local", manifest.StableID(), func(id string, c ToolCall) (Snapshot, error) {
					return a.executeLocal(context.Background(), id, c, &manifest, []string{manifest.Command})
				}},
				{"registered-mcp", remoteID, func(id string, c ToolCall) (Snapshot, error) {
					return a.ExecuteRegistered(context.Background(), id, c.ID, c.Name)
				}},
				{"mcp", remoteID, func(id string, c ToolCall) (Snapshot, error) {
					return a.ExecuteMCP(context.Background(), id, c.ID, c.Name)
				}},
				{"mcp-locked", remoteID, func(id string, c ToolCall) (Snapshot, error) {
					return a.executeRegisteredLocked(context.Background(), id, c.ID, c.Name)
				}},
				{"mcp-action", remoteID, func(id string, c ToolCall) (Snapshot, error) { return a.executeMCP(context.Background(), id, c) }},
				{"unavailable", "future-adapter", func(id string, c ToolCall) (Snapshot, error) { return a.recordUnavailableTool(id, c.ID, c.Name) }},
			} {
				t.Run(test.name, func(t *testing.T) {
					root, err := a.Create("coordinate only")
					if err != nil {
						t.Fatal(err)
					}
					call := ToolCall{ID: "blocked", Name: test.tool, Arguments: "not-json"}
					responsibilityPending(t, a, root.Run.ID, call)
					s, err := test.run(root.Run.ID, call)
					responsibilityDenied(t, s, err)
					if calls.Load() != 0 {
						t.Fatal("coordinator reached MCP server")
					}
					if _, err := os.Stat(marker); !os.IsNotExist(err) {
						t.Fatal("coordinator launched local adapter")
					}
				})
			}
			// Manual execution continues through the real model loop. Supply its
			// final report so this positive control does not enter delivery retries.
			a.provider = &scriptedProvider{responses: []ModelResponse{{Content: "MCP observation complete."}}}
			worker, err := a.CreateConfigured("execute MCP", RunConfiguration{role: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			call := ToolCall{ID: "allowed", Name: remoteID, Arguments: "{}"}
			responsibilityPending(t, a, worker.Run.ID, call)
			s, err := a.ExecuteMCP(context.Background(), worker.Run.ID, call.ID, remoteID)
			if err != nil || calls.Load() != 1 || len(s.Actions) != 1 || s.Actions[0].Result == nil || s.Actions[0].Result.ExitCode != 0 {
				t.Fatalf("worker MCP did not execute once: %v", err)
			}
		})
	}
}

func TestRunResponsibilitySourceSearchNeverReservesOrRequests(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, "{}") }))
	defer server.Close()
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, _ := responsibilityAgent(t, backend, MockProvider{})
			if err := a.ConfigureSearchSources(map[string]SearchSourceConfig{"fofa": {Enabled: true, APIKey: "fixture", BaseURL: server.URL}}); err != nil {
				t.Fatal(err)
			}
			root, err := a.Create("delegate discovery")
			if err != nil {
				t.Fatal(err)
			}
			args := string(mustResponsibilityJSON(t, map[string]string{"source": "fofa", "query": "fixture"}))
			call := ToolCall{ID: "search", Name: "source_search", Arguments: args}
			responsibilityPending(t, a, root.Run.ID, call)
			s, err := a.executePendingTool(context.Background(), root.Run.ID, call)
			responsibilityDenied(t, s, err)
			if calls.Load() != 0 || len(s.SearchUsage) != 0 {
				t.Fatal("denied search consumed upstream requests/quota")
			}
		})
	}
}

func TestRunResponsibilityPauseReopenResumePreservesRoleAndPrompt(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "must-not-exist")
			args := string(mustResponsibilityJSON(t, map[string]string{"path": path, "content": "forbidden"}))
			call := ToolCall{ID: "resume-write", Name: "write", Arguments: args}
			a, store := responsibilityAgent(t, backend, &scriptedProvider{responses: []ModelResponse{{ToolCalls: []ToolCall{call}}}})
			root, err := a.Create("delegate execution after resume")
			if err != nil {
				t.Fatal(err)
			}
			ctx := piMigrationContext(t)
			// Archive a real provider response at the pre-dispatch boundary;
			// do not synthesize a request or grant the root an execution schema.
			retries := 0
			request, err := a.preparePiConversation(ctx, root.Run.ID, &retries)
			if err != nil {
				t.Fatal(err)
			}
			response, callErr := a.complete(ctx, root.Run.ID, request)
			waiting, _, err := a.settlePiResponse(ctx, root.Run.ID, request, response, callErr, &retries)
			if err != nil || waiting.Run.Status != StatusWaitingTool || len(waiting.Responses) != 1 || len(pendingToolCalls(&waiting)) != 1 {
				t.Fatalf("provider did not leave a durable pending call: run=%+v err=%v", waiting.Run, err)
			}
			paused, err := a.Pause(root.Run.ID)
			if err != nil || paused.Run.Status != StatusPaused {
				t.Fatalf("pending call did not pause: %+v %v", paused.Run, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			saved, err := reopened.Load(root.Run.ID)
			if err != nil || saved.Run.Status != StatusPaused || len(saved.Responses) != 1 || saved.Responses[0].RequestID != request.ID || pendingToolCalls(&saved)[call.ID].Arguments != args {
				t.Fatalf("reopen lost the paused provider boundary: %+v %v", saved.Run, err)
			}
			provider := &scriptedProvider{responses: []ModelResponse{{Content: "The saved call was declined; execution requires a Worker."}}}
			restarted, err := New(reopened, provider, "")
			if err != nil {
				t.Fatal(err)
			}
			restarted.prompt, restarted.promptID = "deployment prompt changed", promptHash("deployment prompt changed")
			// Operator resume restores an outstanding paused call for explicit
			// resolution. It deliberately does not execute or request a model turn.
			s, err := restarted.Run(ctx, root.Run.ID)
			if err != nil || s.Run.Status != StatusWaitingTool || len(s.Requests) != 1 || len(s.Responses) != 1 || len(provider.requests) != 0 || len(s.Actions) != 0 || len(s.Evidence) != 0 || len(pendingToolCalls(&s)) != 1 {
				t.Fatalf("resume crossed the pending boundary: %+v %v", s.Run, err)
			}
			s, err = restarted.ExecuteRegistered(ctx, root.Run.ID, call.ID, call.Name)
			responsibilityDenied(t, s, err)
			if len(provider.requests) != 0 {
				t.Fatal("denied manual resolution requested a model turn")
			}
			s, err = restarted.Run(ctx, root.Run.ID)
			if err != nil || s.Run.Status != StatusCompleted || s.Run.Responsibility != ResponsibilityCoordinator || s.Prompt != root.Prompt || s.Run.PromptHash != root.Run.PromptHash || len(s.Requests) != 2 || len(s.Responses) != 2 || len(provider.requests) != 1 {
				t.Fatalf("resume changed identity/role/prompt or failed to continue: %+v %v", s.Run, err)
			}
			responsibilityDenied(t, s, nil)
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatal("resume executed forbidden pending write")
			}
			for _, req := range s.Requests {
				if responsibilityHasTool(req, "write") {
					t.Fatal("paused or resumed request exposed execution schema")
				}
			}
		})
	}
}

func TestRunResponsibilityLegacyMissingFieldKeepsExecutionAndPrompt(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "legacy.txt")
			if err := os.WriteFile(path, []byte("legacy-observation"), 0600); err != nil {
				t.Fatal(err)
			}
			args := string(mustResponsibilityJSON(t, map[string]string{"path": path}))
			a, store := responsibilityAgent(t, backend, manualProvider(ToolCall{ID: "legacy-read", Name: "read", Arguments: args}))
			const savedPrompt = "Historical prompt: execute simple work directly."
			legacy, err := store.Create("read the legacy fixture", savedPrompt, promptHash(savedPrompt))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(mustResponsibilityJSON(t, legacy.Run)), "responsibility") {
				t.Fatal("fixture unexpectedly has a role field")
			}
			req, err := a.preparePiRequest(context.Background(), legacy.Run.ID)
			if err != nil || !responsibilityHasTool(req, "read") {
				t.Fatalf("legacy schema lost read: %v", err)
			}
			if strings.Contains(requestRuntimeObservations(legacy.Run.ID, req), "from the first turn") {
				t.Fatal("legacy request acquired coordinator prompt")
			}
			s, err := a.Run(piMigrationContext(t), legacy.Run.ID)
			if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 1 || s.Actions[0].Result == nil || !strings.Contains(s.Actions[0].Result.Stdout, "legacy-observation") {
				t.Fatalf("legacy execution regressed: %+v %v", s.Run, err)
			}
			if s.Run.Responsibility != "" || s.Prompt != savedPrompt || s.Run.PromptHash != legacy.Run.PromptHash {
				t.Fatal("legacy load/resume backfilled role or prompt")
			}
		})
	}
}

func TestRunResponsibilityBranchesKeepCoordinatorAdmission(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, role := range []RunResponsibility{"", ResponsibilityCoordinator, ResponsibilityWorker} {
			t.Run(backend+"/"+string(role), func(t *testing.T) {
				a, store := responsibilityAgent(t, backend, MockProvider{})
				source, err := a.Create("branch fixture")
				if err != nil {
					t.Fatal(err)
				}
				source, err = store.Update(source.Run.ID, func(s *Snapshot) error {
					s.Run.Responsibility = role
					s.Messages = append(s.Messages, Message{Role: "user", Content: "branch boundary"})
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				branch, err := a.Branch(source.Run.ID, source.Messages[len(source.Messages)-1].ID)
				if err != nil {
					t.Fatal(err)
				}
				want := ResponsibilityCoordinator
				if role == "" {
					want = ""
				}
				req, err := a.preparePiRequest(context.Background(), branch.Run.ID)
				if err != nil || branch.Run.Responsibility != want || responsibilityHasTool(req, "write") != (role == "") || branch.Prompt != source.Prompt {
					t.Fatalf("branch bypassed responsibility: %+v %v", branch.Run, err)
				}
			})
		}
	}
}

// A scripted provider still traverses the actual pi loop. Ordinary text keeps
// the existing completion policy; role admission does not add a Judge gate.
func TestRunResponsibilityPlainTextDoesNotRequireWorker(t *testing.T) {
	a, _ := responsibilityAgent(t, "json", &scriptedProvider{responses: []ModelResponse{{Content: "Hello."}}})
	root, err := a.Create("hello")
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.Run(piMigrationContext(t), root.Run.ID)
	if err != nil || s.Run.Status != StatusCompleted || len(s.Requests) != 1 || len(s.Workers) != 0 || len(s.Actions) != 0 {
		t.Fatalf("plain text acquired an orchestration completion loop: %+v %v", s.Run, err)
	}
}
