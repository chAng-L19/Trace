package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestContextRecentTargetKeepsToolBatchesAndHeadroom(t *testing.T) {
	s := Snapshot{Run: Run{ID: "recent", Context: ContextConfig{WindowTokens: 13000, OutputReserve: 500, KeepRecentTokens: 1800}}, Prompt: strings.Repeat("p", 7000)}
	s.Messages = []Message{{Role: "user", Content: "objective: preserve original proof"}}
	for i := 0; i < 8; i++ {
		call := fmt.Sprintf("probe-%d", i)
		s.Messages = append(s.Messages,
			Message{Role: "assistant", ToolCalls: []ToolCall{{ID: call, Name: "read", Arguments: `{"path":"proof.txt"}`}}},
			Message{Role: "tool", ToolCallID: call, Content: strings.Repeat("observation ", 65)})
	}
	s.Messages = append(s.Messages, Message{Role: "user", Content: "latest constraint: preserve exact file paths"})
	assignMessageIDs(&s)
	original := messageHash(s.Messages)
	_, _, _, err := projectContext(&s, nil, "")
	plan, ok := err.(*summaryNeeded)
	if !ok {
		t.Fatalf("expected bounded semantic summary, got %v", err)
	}
	cut := len(plan.record.SourceMessageIDs)
	if cut <= 2 || cut >= len(s.Messages) || s.Messages[cut].Role == "tool" {
		t.Fatalf("invalid recent-history boundary: %d", cut)
	}
	if EstimateContextTokens(s.Messages[cut:], nil) > 1800 {
		t.Fatalf("recent target ignored: cut=%d tokens=%d", cut, EstimateContextTokens(s.Messages[cut:], nil))
	}
	if EstimateContextTokens(plan.messages, nil) > 12500 {
		t.Fatal("summary request escaped input budget")
	}
	plan.record.ID, plan.record.RequestID = "summary", "summary-request"
	s.Compactions = append(s.Compactions, plan.record)
	if handled, err := finishCompaction(&s, ModelRequest{ID: "summary-request"}, ModelResponse{Content: "Historical observations retained with their message IDs.", StopReason: "stop"}, nil); !handled || err != nil {
		t.Fatalf("save summary: handled=%t err=%v", handled, err)
	}
	projected, estimate, cid, err := projectContext(&s, nil, "")
	if err != nil || cid != "summary" || estimate > 12500 || messageHash(s.Messages) != original {
		t.Fatalf("archive/projection changed: id=%s estimate=%d err=%v", cid, estimate, err)
	}
	calls := map[string]bool{}
	for _, m := range projected {
		for _, call := range m.ToolCalls {
			calls[call.ID] = true
		}
		if m.Role == "tool" && !calls[m.ToolCallID] {
			t.Fatal("tool result cut away from its assistant batch")
		}
	}
	if projected[len(projected)-1].Content != "latest constraint: preserve exact file paths" {
		t.Fatal("latest user instruction lost")
	}
	s.Messages = append(s.Messages, Message{Role: "assistant", Content: strings.Repeat("new observation ", 40)})
	if _, _, id, err := projectContext(&s, nil, ""); err != nil || id != "summary" {
		t.Fatalf("small continuation immediately recompacted: %s %v", id, err)
	}
}

func TestContextRecentConfigurationAndSmallWindow(t *testing.T) {
	c, err := normalizeContext(ContextConfig{WindowTokens: 7000, OutputReserve: 500})
	if err != nil || c.KeepRecentTokens != 20000 || contextRecentBudget(c) != 3250 {
		t.Fatalf("small-window default: %+v %v", c, err)
	}
	if _, err := normalizeContext(ContextConfig{KeepRecentTokens: -1}); err == nil {
		t.Fatal("negative recent-history budget accepted")
	}
	t.Setenv("TRACE_KEEP_RECENT_TOKENS", "1234")
	c, err = ContextFromEnv()
	if err != nil || c.KeepRecentTokens != 1234 {
		t.Fatalf("explicit recent-history budget ignored: %+v %v", c, err)
	}
	t.Setenv("TRACE_KEEP_RECENT_TOKENS", "invalid")
	if _, err := ContextFromEnv(); err == nil {
		t.Fatal("invalid recent-history budget accepted")
	}
}

func TestContextRecentTargetDoesNotSplitOversizedLatestBatch(t *testing.T) {
	s := Snapshot{Run: Run{ID: "large-batch", Context: ContextConfig{WindowTokens: 11000, OutputReserve: 500, KeepRecentTokens: 1000}}, Prompt: strings.Repeat("p", 3500),
		Messages: []Message{{Role: "user", Content: "retain complete observations"}, {Role: "assistant", Content: strings.Repeat("old history ", 450)},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "latest", Name: "read", Arguments: "{}"}}}, {Role: "tool", ToolCallID: "latest", Content: strings.Repeat("large latest observation ", 100)}}}
	_, _, _, err := projectContext(&s, nil, "")
	plan, ok := err.(*summaryNeeded)
	if !ok || len(plan.record.SourceMessageIDs) != 2 {
		t.Fatalf("recent target split or summarized latest tool batch: %v %+v", err, plan)
	}
	if EstimateContextTokens(s.Messages[2:], nil) <= 1000 {
		t.Fatal("fixture did not exceed the soft recent-history target")
	}
	if strings.Contains(plan.messages[1].Content, "large latest observation") {
		t.Fatal("latest batch moved into historical summary")
	}
}

func TestContextProjectionArtifactsComeFromExactResult(t *testing.T) {
	result := ToolResult{Stdout: strings.Repeat("large output ", 600), Artifacts: []ArtifactRef{{Hash: "own-artifact", Kind: "stdout", Bytes: 7000}}}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	s := Snapshot{Run: Run{ID: "artifact-context", Context: ContextConfig{WindowTokens: 5000, OutputReserve: 500, CompactionMode: "projection"}}, Prompt: "system",
		Messages: []Message{{Role: "user", Content: "inspect the original output"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "reused", Name: "read", Arguments: "{}"}}}, {Role: "tool", ToolCallID: "reused", Content: string(raw)}, {Role: "user", Content: "latest instruction"}},
		Actions:  []Action{{RequestID: "other-request", CallID: "reused", Result: &ToolResult{Artifacts: []ArtifactRef{{Hash: "unrelated-artifact", Kind: "stdout", Bytes: 9}}}}}}
	assignMessageIDs(&s)
	original := messageHash(s.Messages)
	messages, _, _, err := projectContext(&s, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	content := ""
	for _, m := range messages {
		if m.Role == "tool" {
			content = m.Content
		}
	}
	if !strings.Contains(content, "artifact:own-artifact") || strings.Contains(content, "unrelated-artifact") || messageHash(s.Messages) != original {
		t.Fatal("projection lost exact artifact references or mixed a reused call ID")
	}
}
