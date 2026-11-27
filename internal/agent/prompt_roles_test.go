package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"trace/prompts"
)

func TestRolePromptBundlesAndLegacyOverride(t *testing.T) {
	embedded, err := loadRuntimePrompts("")
	if err != nil {
		t.Fatal(err)
	}
	disk, err := loadRuntimePrompts(filepath.Join("..", "..", "prompts", "agent.md"))
	if err != nil || disk != embedded {
		t.Fatalf("disk/embedded mismatch: %v", err)
	}
	for name, text := range map[string]string{"coordinator": disk.coordinator, "worker": disk.worker, "recon": disk.recon} {
		for _, rule := range []string{"visible hints guide research", "Uncertain stays unknown", "source_digest", "JSON repair", "immutable acceptance", "concurrency ceiling, not a dispatch target"} {
			if !strings.Contains(text, rule) {
				t.Errorf("%s lost common rule %q", name, rule)
			}
		}
		if strings.Count(text, prompts.Common) != 1 {
			t.Errorf("%s repeats common rules", name)
		}
	}
	if strings.Contains(disk.coordinator, "For webpage interaction") || strings.Contains(disk.worker, "From the first turn, the root Run only") || strings.Contains(disk.worker, "## Recon role") {
		t.Fatal("role-specific instructions leaked into unrelated role")
	}
	if !strings.Contains(disk.worker, "For webpage interaction") || !strings.HasPrefix(disk.recon, disk.worker) || !strings.Contains(disk.recon, "information-collection Worker") {
		t.Fatal("worker/recon execution instructions missing")
	}
	path := filepath.Join(t.TempDir(), "custom.md")
	const original = " Custom {goal} instructions\r\nPreserve exact bytes.\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	legacy, err := loadRuntimePrompts(path)
	if err != nil || legacy != (runtimePrompts{original, original, original}) {
		t.Fatalf("legacy override changed: %v", err)
	}
	for _, name := range []string{"coordinator", "worker", "recon"} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), name+".md"), []byte("ignored sibling"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	withSiblings, err := loadRuntimePrompts(path)
	if err != nil || withSiblings != legacy {
		t.Fatal("unmarked custom prompt read role siblings")
	}
}

func TestRolePromptBundleRejectsPartialConfiguration(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "agent.md")
	if _, err := loadRuntimePrompts(path); err == nil {
		t.Fatal("missing common prompt accepted")
	}
	if err := os.WriteFile(path, []byte(" \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRuntimePrompts(path); err == nil {
		t.Fatal("empty common prompt accepted")
	}
	if err := os.WriteFile(path, []byte(prompts.RoleMarker+"\ncommon {literal}"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"coordinator", "worker", "recon"} {
		rolePath := filepath.Join(root, name+".md")
		if _, err := loadRuntimePrompts(path); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("missing %s prompt accepted: %v", name, err)
		}
		if err := os.WriteFile(rolePath, []byte(" \n"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadRuntimePrompts(path); err == nil || !strings.Contains(err.Error(), name) {
			t.Fatalf("empty %s prompt accepted: %v", name, err)
		}
		if err := os.WriteFile(rolePath, []byte("custom "+name+" {literal}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	roles, err := loadRuntimePrompts(path)
	if err != nil || !strings.Contains(roles.recon, "custom worker {literal}\ncustom recon {literal}") {
		t.Fatalf("literal custom role composition failed: %v", err)
	}
}

func TestRolePromptsDispatchPersistAndResume(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := responsibilityAgent(t, backend, successProvider{})
			limit := 1
			a.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
			parent, err := a.Create("inspect supplied documents")
			if err != nil {
				t.Fatal(err)
			}
			rootRequest, err := a.preparePiRequest(context.Background(), parent.Run.ID)
			if err != nil || !strings.HasPrefix(rootRequest.Messages[0].Content, DefaultPrompt) {
				t.Fatalf("coordinator prompt missing: %v", err)
			}
			if _, err := a.SetPlan(parent.Run.ID, []PlanTask{{ID: "work", Goal: "analyze supplied document"}, {ID: "collect", Goal: "collect supplied inventory", ExecutionRole: "recon"}}); err != nil {
				t.Fatal(err)
			}
			for _, taskID := range []string{"work", "collect"} {
				worker, err := a.DispatchTask(context.Background(), parent.Run.ID, taskID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := waitAllTestWorkers(a, context.Background(), parent.Run.ID, 5); err != nil {
					t.Fatal(err)
				}
				child, err := store.Load(worker.RunID)
				if err != nil || len(child.Requests) == 0 {
					t.Fatalf("missing child request: %v", err)
				}
				expected := a.workerPrompt
				if taskID == "collect" {
					expected = a.reconPrompt
				}
				if child.Prompt != expected || child.Run.PromptHash != promptHash(expected) || child.Run.PromptBytes != len(expected) || child.Requests[0].PromptHash != child.Run.PromptHash || !strings.HasPrefix(child.Requests[0].Messages[0].Content, expected) {
					t.Fatalf("%s prompt identity/request mismatch", taskID)
				}
				// A new Agent and changed on-disk prompt must not replace saved role text.
				path := filepath.Join(t.TempDir(), "new.md")
				if err := os.WriteFile(path, []byte("new custom root prompt"), 0600); err != nil {
					t.Fatal(err)
				}
				restarted, err := New(store, successProvider{}, path)
				if err != nil {
					t.Fatal(err)
				}
				resumeLimit := 3
				restarted.ConfigureBudget(BudgetOverride{MaxModelRequests: &resumeLimit})
				if _, err := restarted.QueueMessage(child.Run.ID, "follow_up", "continue using saved sources"); err != nil {
					t.Fatal(err)
				}
				resumed, err := restarted.Run(context.Background(), child.Run.ID)
				if err != nil || len(resumed.Requests) <= len(child.Requests) {
					t.Fatalf("%s did not resume: %v", taskID, err)
				}
				request := resumed.Requests[len(resumed.Requests)-1]
				if !strings.HasPrefix(request.Messages[0].Content, expected) || request.PromptHash != child.Run.PromptHash || resumed.Prompt != expected {
					t.Fatalf("%s resumed with a different prompt", taskID)
				}
				// Independent branches preserve historical prompt bytes but acquire root admission.
				branch, err := restarted.Branch(resumed.Run.ID, resumed.Messages[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				branchRequest, err := restarted.preparePiRequest(context.Background(), branch.Run.ID)
				if err != nil || branch.Prompt != expected || branch.Run.Responsibility != ResponsibilityCoordinator || responsibilityHasTool(branchRequest, "exec") || !strings.Contains(requestRuntimeObservations(branch.Run.ID, branchRequest), "from the first turn") || !strings.Contains(branchRequest.Messages[0].Content, "execution instructions apply only to Worker runs") {
					t.Fatalf("%s branch lost saved prompt/root boundary: %v", taskID, err)
				}
			}
		})
	}
}
