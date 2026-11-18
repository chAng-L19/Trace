package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerContextActualSemanticSummaryAndResume(t *testing.T) {
	a, store, id := contextFixture(t, "source-proof")
	child := contextChild(t, a, store, id)
	a.registry = &ToolRegistry{tools: map[string]RegisteredTool{}}
	// The fixed planner/verification context must fit before history can be
	// summarized. Derive the fixture window from the actual mandatory request
	// prefix, not a prompt-version-specific constant. Tools were cleared above.
	// Eight old answers exceed this 4096-token history/summary allowance.
	inputLimit := EstimateContextTokens(child.Requests[0].Messages, nil) + 4096
	// Allow several summary batches and a resumed model response.
	limit := 10
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
	if _, err := store.Update(child.Run.ID, func(s *Snapshot) error {
		s.Run.Context = ContextConfig{WindowTokens: inputLimit + 500, OutputReserve: 500, CompactionMode: "semantic"}
		s.Messages = []Message{{Role: "user", Content: s.Run.Goal}}
		for i := 0; i < 8; i++ {
			s.Messages = append(s.Messages, Message{Role: "assistant", Content: strings.Repeat("old answer ", 300)})
		}
		s.Messages = append(s.Messages, Message{Role: "user", Content: "latest-constraint"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.QueueMessage(child.Run.ID, "follow_up", "resume-long-context"); err != nil {
		t.Fatal(err)
	}
	result, err := a.Run(context.Background(), child.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Compactions) == 0 {
		t.Fatalf("fixture did not compact: status=%s reason=%s requests=%d", result.Run.Status, result.Run.PauseReason, len(result.Requests))
	}
	last := result.Requests[len(result.Requests)-1]
	// A budget can stop immediately after another summary. Inspect the latest
	// actual agent request: summary requests intentionally contain history only.
	for i := len(result.Requests) - 1; i >= 0; i-- {
		if result.Requests[i].Purpose != "context_summary" {
			last = result.Requests[i]
			break
		}
	}
	if last.CompactionID == "" {
		t.Fatal("fixture never resumed an agent request after compaction")
	}
	c := contextFromRequest(t, last)
	if c.Evidence[0].Status != "source_verified" || !strings.Contains(c.Evidence[0].Output, "source-proof") {
		t.Fatalf("source lost after actual summary: %+v", c)
	}
	if last.ContextTokens > inputLimit {
		t.Fatalf("budget exceeded: %d", last.ContextTokens)
	}
	for _, req := range result.Requests {
		if strings.Contains(req.Messages[0].Content, "chosen-asset-value") {
			t.Fatal("source promoted to system")
		}
	}
}

func TestWorkerContextCompletedDependencyChildEvidence(t *testing.T) {
	for _, claim := range []bool{false, true} {
		t.Run(map[bool]string{false: "no_result_claim", true: "selected_result_evidence"}[claim], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "dependency.txt")
			if err := os.WriteFile(path, []byte("dependency-source-proof"), 0600); err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(t.TempDir(), "unrelated.txt")
			if err := os.WriteFile(other, []byte("unrelated-source-proof"), 0600); err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(FileRequest{Path: path})
			otherArgs, _ := json.Marshal(FileRequest{Path: other})
			calls := 0
			provider := settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
				calls++
				if calls > 3 {
					return ModelResponse{}, fmt.Errorf("dependency fixture repeated completion")
				}
				if calls == 1 {
					return ModelResponse{ToolCalls: []ToolCall{{ID: "dep-read", Name: "read", Arguments: string(args)}, {ID: "unrelated-read", Name: "read", Arguments: string(otherArgs)}}}, nil
				}
				if !claim {
					return ModelResponse{Content: fixtureReport("inspection completed")}, nil
				}
				for _, message := range request.Messages {
					if message.Role != "tool" || message.ToolCallID != "dep-read" {
						continue
					}
					var proof ToolResult
					if err := json.Unmarshal([]byte(message.Content), &proof); err != nil {
						return ModelResponse{}, err
					}
					if proof.EvidenceID == "" || proof.ExitCode != 0 || proof.Error != "" {
						return ModelResponse{}, fmt.Errorf("dependency read lacks successful evidence: %+v", proof)
					}
					body, err := json.Marshal(map[string]Result{"trace_result": {Status: "success", Summary: "selected dependency observation", EvidenceIDs: []string{proof.EvidenceID}}})
					return ModelResponse{Content: fixtureReport(string(body))}, err
				}
				return ModelResponse{}, fmt.Errorf("dependency tool result missing")
			})
			a, store := newTestAgent(t, provider)
			id := createTestRun(t, a)
			if _, err := a.SetPlan(id, []PlanTask{{ID: "dep", Goal: "read dependency"}, {ID: "work", Goal: "use dependency", Dependencies: []string{"dep"}}}); err != nil {
				t.Fatal(err)
			}
			w, err := a.DispatchTask(context.Background(), id, "dep")
			if err != nil {
				t.Fatal(err)
			}
			parent, err := waitAllTestWorkers(a, context.Background(), id, 5)
			if err != nil || parent.Plan.Tasks[0].Status != "completed" {
				t.Fatalf("dependency not completed: %v %+v", err, parent.Plan)
			}
			source, err := store.Load(w.RunID)
			if err != nil || len(source.Evidence) != 2 {
				t.Fatal("fixture lost independent source ledger", err)
			}
			for _, evidence := range source.Evidence {
				if evidence.Verification != "verified" {
					t.Fatal("source fixture is not verified")
				}
			}
			a.provider = successProvider{}
			child := contextChild(t, a, store, id)
			c := contextFromRequest(t, child.Requests[0])
			if len(c.Dependencies) != 1 || c.Dependencies[0].Status != "available" || len(child.Evidence) != 0 || len(child.Actions) != 0 {
				t.Fatalf("dependency identity or child ledger changed: %+v", c)
			}
			if !claim {
				if len(source.Result.EvidenceIDs) != 0 || len(c.Evidence) != 0 {
					t.Fatal("unclaimed source history copied as dependency result")
				}
				return
			}
			if len(source.Result.EvidenceIDs) != 1 || len(c.Evidence) != 1 || c.Evidence[0].ID != source.Result.EvidenceIDs[0] || c.Evidence[0].SourceRunID != w.RunID || c.Evidence[0].Status != "source_verified" || !strings.Contains(c.Evidence[0].Output, "dependency-source-proof") || strings.Contains(c.Evidence[0].Output, "unrelated-source-proof") {
				t.Fatalf("wrong selected dependency source: %+v", c)
			}
			if _, err := store.Update(w.RunID, func(s *Snapshot) error { s.Run.ParentTaskID = "different-task"; return nil }); err != nil {
				t.Fatal(err)
			}
			changed := contextFromRequest(t, ModelRequest{Messages: []Message{{Role: "user", Content: a.workerContextProjection(&child)}}})
			if changed.Dependencies[0].Status != "unavailable" || changed.Evidence[0].Status != "unavailable" || changed.Evidence[0].Output != "" {
				t.Fatalf("changed ancestry trusted: %+v", changed)
			}
		})
	}
}
