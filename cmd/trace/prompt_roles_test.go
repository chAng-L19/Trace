package main

import (
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
	"trace/prompts"
)

func TestRuntimeAgentUsesConfiguredPromptsForNewWorkers(t *testing.T) {
	for _, mode := range []string{"roles", "single-file"} {
		t.Run(mode, func(t *testing.T) {
			isolateServiceConfigEnv(t)
			t.Setenv("TRACE_MAX_ROUNDS", "1")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			systems := make(chan string, 8)
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []agent.Message `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if len(request.Messages) == 0 {
					t.Error("missing request messages")
					w.WriteHeader(400)
					return
				}
				systems <- request.Messages[0].Content
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "fixture complete"}, "finish_reason": "stop"}}})
			}))
			defer provider.Close()
			dir := t.TempDir()
			promptPath := filepath.Join(dir, "agent.md")
			common := "CUSTOM_COMMON\n"
			if mode == "roles" {
				common = prompts.RoleMarker + "\n" + common
			}
			if err := os.WriteFile(promptPath, []byte(common), 0600); err != nil {
				t.Fatal(err)
			}
			for _, role := range []string{"coordinator", "worker", "recon"} {
				if err := os.WriteFile(filepath.Join(dir, role+".md"), []byte("CUSTOM_"+strings.ToUpper(role)), 0600); err != nil {
					t.Fatal(err)
				}
			}
			config := map[string]any{"prompt_path": promptPath, "provider": map[string]string{"url": provider.URL, "model": "prompt-fixture"}}
			raw, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(dir, "config.json")
			if err := os.WriteFile(configPath, raw, 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TRACE_CONFIG_FILE", configPath)
			store, err := agent.NewStore(os.Getenv("TRACE_DATA_DIR"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			creator, err := newAgent(store)
			if err != nil {
				t.Fatal(err)
			}
			parent, err := creator.Create("analyze supplied inputs")
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(parent.Prompt, common) {
				t.Fatal("root lost custom common prompt")
			}
			runner, err := configuredRuntimeAgent(ctx, store)
			if err != nil {
				t.Fatal(err)
			}
			defer runner.CloseMCP()
			defer func() {
				stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := runner.StopWorkers(stop); err != nil {
					t.Error(err)
				}
			}()
			if _, err := runner.SetPlan(parent.Run.ID, []agent.PlanTask{{ID: "work", Goal: "analyze document"}, {ID: "collect", Goal: "collect supplied facts", ExecutionRole: "recon"}}); err != nil {
				t.Fatal(err)
			}
			for _, task := range []string{"work", "collect"} {
				worker, err := runner.DispatchTask(ctx, parent.Run.ID, task)
				if err != nil {
					t.Fatal(err)
				}
				var system string
				select {
				case system = <-systems:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				child, err := store.Load(worker.RunID)
				if err != nil {
					t.Fatal(err)
				}
				want := common
				if mode == "roles" {
					want += "\nCUSTOM_WORKER"
					if task == "collect" {
						want += "\nCUSTOM_RECON"
					}
				}
				if child.Prompt != want || !strings.HasPrefix(system, want) || strings.Contains(system, "CUSTOM_COORDINATOR") {
					t.Fatalf("%s/%s lost configured role text", mode, task)
				}
			}
			missing := promptPath
			if mode == "roles" {
				missing = filepath.Join(dir, "worker.md")
			}
			if err := os.Remove(missing); err != nil {
				t.Fatal(err)
			}
			restarted, err := configuredRuntimeAgent(ctx, store)
			if err != nil {
				t.Fatalf("missing template blocked history recovery: %v", err)
			}
			defer restarted.CloseMCP()
			if _, err := restarted.Create("new work requires configured prompt"); err == nil {
				t.Fatal("missing configured template silently used defaults")
			}
			if _, err := restarted.UpdatePlan(parent.Run.ID, []agent.PlanTask{{ID: "missing", Goal: "new child requires configured prompt"}}, "append"); err != nil {
				t.Fatal(err)
			}
			if _, err := restarted.DispatchTask(ctx, parent.Run.ID, "missing"); err == nil || !strings.Contains(err.Error(), "runtime prompt") {
				t.Fatalf("missing template did not stop dispatch: %v", err)
			}
			aborted, err := store.Load(parent.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			missingTask := aborted.Plan.Tasks[len(aborted.Plan.Tasks)-1]
			if len(aborted.Workers) != 2 || missingTask.Status != "failed" || missingTask.WorkerID != "" || missingTask.ChildRunID != "" || missingTask.DispatchPhase != "" {
				t.Fatal("failed prompt loading retained a new child or task owner")
			}
			resumed, err := restarted.Run(ctx, parent.Run.ID)
			if err != nil || resumed.Prompt != parent.Prompt || resumed.Run.PromptHash != parent.Run.PromptHash {
				t.Fatalf("history prompt changed after file removal: %v", err)
			}
		})
	}
}
