package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Role tests deliberately use production Create/Configured, never createTestRun.
func responsibilityAgent(t *testing.T, backend string, p Provider) (*Agent, *Store) {
	t.Helper()
	var store *Store
	var err error
	if backend == "sqlite" {
		store = sqliteTestStore(t)
	} else {
		store, err = NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
	}
	a, err := New(store, p, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.StopWorkers(ctx); err != nil {
			t.Error(err)
		}
		if err := a.CloseMCP(); err != nil {
			t.Error(err)
		}
	})
	return a, store
}

func responsibilityPending(t *testing.T, a *Agent, id string, call ToolCall) Snapshot {
	t.Helper()
	s, err := a.store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Run.RequestSeq++
		requestID := fmt.Sprintf("%s-%04d", id, s.Run.RequestSeq)
		s.Requests = append(s.Requests, ModelRequest{ID: requestID, Tools: a.registry.Definitions(), CreatedAt: time.Now().UTC()})
		s.Messages = append(s.Messages, Message{Role: "assistant", RequestID: requestID, ToolCalls: []ToolCall{call}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func responsibilityHasTool(request ModelRequest, name string) bool {
	for _, d := range request.Tools {
		if d.Function.Name == name {
			return true
		}
	}
	return false
}

func responsibilityDenied(t *testing.T, s Snapshot, err error) {
	t.Helper()
	if err != nil || len(s.Actions) != 0 || len(s.Evidence) != 1 || len(pendingToolCalls(&s)) != 0 {
		t.Fatalf("denied call crossed action boundary: actions=%d evidence=%d pending=%d err=%v", len(s.Actions), len(s.Evidence), len(pendingToolCalls(&s)), err)
	}
	e := s.Evidence[0]
	var result ToolResult
	if err := json.Unmarshal([]byte(e.Content), &result); err != nil {
		t.Fatal(err)
	}
	if e.Type != "runtime_observation" || verifyEvidence(&s, &e) != "unverified" || result.EvidenceID != "" || result.WorkerStatus != "not_started" || result.OutcomeUncertain || result.Diagnostic == nil || result.Diagnostic.Code != "run_role_restricted" {
		t.Fatalf("denial fabricated execution evidence: evidence=%+v result=%+v", e, result)
	}
	for _, event := range s.Events {
		if event.Type == "action_started" {
			t.Fatal("denial created an intent")
		}
	}
}

func TestRunResponsibilityInitialSchemaJSONSQLite(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, mode := range []string{"auto", "deferred", "narrow"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				a, store := responsibilityAgent(t, backend, MockProvider{})
				external := execToolDefinition()
				external.Function.Name = "role-external"
				a.registry.tools[external.Function.Name] = RegisteredTool{Definition: external, Adapter: "mcp", ServerName: "fixture"}
				config := RunConfiguration{ToolSchema: ToolSchemaConfig{Mode: mode}}
				if mode == "narrow" {
					config.ToolSchema.IDs = []string{external.Function.Name}
				}
				root, err := a.CreateConfigured("delegate a simple execution task", config)
				if err != nil {
					t.Fatal(err)
				}
				if root.Run.Responsibility != ResponsibilityCoordinator || len(root.Workers) != 0 || len(root.Plan.Tasks) != 0 {
					t.Fatal("new root was not a coordinator before first dispatch")
				}
				req, err := a.preparePiRequest(context.Background(), root.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, name := range []string{"plan", "worker_dispatch", "worker_message", "artifact_read", "session_read", "finding_retest", "evidence_check"} {
					if !responsibilityHasTool(req, name) {
						t.Fatalf("coordinator missing %s", name)
					}
				}
				for _, name := range []string{"exec", "read", "write", "edit", "artifact_register", "source_search", "read_skill", "task_claim", "task_complete", external.Function.Name} {
					if responsibilityHasTool(req, name) {
						t.Fatalf("coordinator exposed execution tool %s", name)
					}
				}
				if !strings.Contains(requestRuntimeObservations(root.Run.ID, req), "from the first turn") {
					t.Fatal("first request lacks coordinator role")
				}
				if _, err := a.SelectTools(root.Run.ID, []string{external.Function.Name}, true); err != nil {
					t.Fatal(err)
				}
				req, err = a.preparePiRequest(context.Background(), root.Run.ID)
				if err != nil || responsibilityHasTool(req, external.Function.Name) {
					t.Fatalf("selection bypassed role filter: %v", err)
				}
				worker, err := a.CreateConfigured("execute and subdelegate", RunConfiguration{role: "worker", inheritedRun: &root.Run})
				if err != nil {
					t.Fatal(err)
				}
				req, err = a.preparePiRequest(context.Background(), worker.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if worker.Run.Responsibility != ResponsibilityWorker {
					t.Fatal("worker inherited coordinator responsibility")
				}
				for _, name := range []string{"exec", "read", "write", "source_search", "task_claim", "task_complete", "worker_dispatch"} {
					if !responsibilityHasTool(req, name) {
						t.Fatalf("worker missing %s", name)
					}
				}
				saved, err := store.Load(root.Run.ID)
				if err != nil || saved.Prompt != root.Prompt || saved.Run.PromptHash != root.Run.PromptHash || saved.Run.Responsibility != ResponsibilityCoordinator {
					t.Fatal("projection rewrote persisted role/prompt")
				}
			})
		}
	}
}

func TestRunResponsibilityForgedBuiltinCallsHaveNoSideEffects(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, name := range []string{"exec", "read", "write", "edit", "artifact_register", "read_skill", "source_search", "task_claim", "task_complete"} {
			t.Run(backend+"/"+name, func(t *testing.T) {
				a, _ := responsibilityAgent(t, backend, MockProvider{})
				path := filepath.Join(t.TempDir(), "unchanged.txt")
				if err := os.WriteFile(path, []byte("original-private-content"), 0600); err != nil {
					t.Fatal(err)
				}
				root, err := a.Create("delegate all execution")
				if err != nil {
					t.Fatal(err)
				}
				args, _ := json.Marshal(map[string]any{"path": path, "content": "changed", "old_text": "original-private-content", "new_text": "changed", "command": "exit 91", "source": "fofa", "query": "fixture", "task_id": "work", "worker_id": "forged", "lease_seconds": 30, "status": "completed", "name": "fixture"})
				call := ToolCall{ID: "forged", Name: name, Arguments: string(args)}
				responsibilityPending(t, a, root.Run.ID, call)
				s, err := a.executePendingTool(context.Background(), root.Run.ID, call)
				responsibilityDenied(t, s, err)
				content, err := os.ReadFile(path)
				if err != nil || string(content) != "original-private-content" {
					t.Fatal("denied call touched file")
				}
				if strings.Contains(s.Evidence[0].Content, "original-private-content") {
					t.Fatal("denied read disclosed file")
				}
				if len(s.Plan.Tasks) != 0 || len(s.Workers) != 0 || len(s.SearchUsage) != 0 {
					t.Fatal("denial changed work/search ledgers")
				}
			})
		}
	}
}

func TestRunResponsibilityAdapterCannotImpersonateCoordinatorBuiltin(t *testing.T) {
	for _, adapter := range []string{"exec", "local", "mcp", "unregistered"} {
		t.Run(adapter, func(t *testing.T) {
			a, _ := responsibilityAgent(t, "json", MockProvider{})
			root, err := a.Create("coordinate")
			if err != nil {
				t.Fatal(err)
			}
			call := ToolCall{ID: "spoof", Name: "plan", Arguments: "{}"}
			responsibilityPending(t, a, root.Run.ID, call)
			ran := false
			s, err := a.executeAction(context.Background(), root.Run.ID, call.ID, adapter, nil, func() ToolResult { ran = true; return ToolResult{} })
			responsibilityDenied(t, s, err)
			if ran {
				t.Fatal("adapter callback ran")
			}
		})
	}
}

func TestRunResponsibilityHistoricalIntentPrecedesDenial(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, savedResult := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/saved=%t", backend, savedResult), func(t *testing.T) {
				a, store := responsibilityAgent(t, backend, MockProvider{})
				root, err := a.Create("retain historical execution")
				if err != nil {
					t.Fatal(err)
				}
				call := ToolCall{ID: "historical", Name: "exec", Arguments: "{}"}
				responsibilityPending(t, a, root.Run.ID, call)
				before, err := store.Update(root.Run.ID, func(s *Snapshot) error {
					action := Action{RequestID: currentRequestID(s), CallID: call.ID, Name: call.Name, Adapter: "exec", Arguments: call.Arguments, Status: "started", ResourceCleanupError: "cleanup remains unknown"}
					if savedResult {
						action.Status = "uncertain"
						action.Result = &ToolResult{Stdout: "historical receipt", ExitCode: -1, OutcomeUncertain: true, WorkerCleanupError: "not reconciled"}
					}
					s.Actions = append(s.Actions, action)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				s, err := a.ExecuteTool(context.Background(), root.Run.ID, call.ID, nil)
				if err != nil || len(s.Actions) != 1 || len(s.Evidence) != 1 || s.Evidence[0].Type != "runtime_observation" || toolCleanupError(s) == nil {
					t.Fatalf("historical observation lost: %v", err)
				}
				expected := before.Actions[0]
				if !savedResult {
					expected.Status = "uncertain"
				}
				if !reflect.DeepEqual(s.Actions[0], expected) {
					t.Fatal("historical action/result/cleanup was rewritten")
				}
				var result ToolResult
				if err := json.Unmarshal([]byte(s.Evidence[0].Content), &result); err != nil {
					t.Fatal(err)
				}
				if !result.OutcomeUncertain || result.Diagnostic != nil && result.Diagnostic.Code == "run_role_restricted" {
					t.Fatal("role denial erased historical uncertainty")
				}
				if savedResult && s.Evidence[0].Content != string(mustResponsibilityJSON(t, expected.Result)) {
					t.Fatal("saved result was replaced")
				}
			})
		}
	}
}

func mustResponsibilityJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
