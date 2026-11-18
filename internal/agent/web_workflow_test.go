package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"trace/internal/localtool"
)

func TestWebWorkflowSkillDiscoverySelectionAndArtifact(t *testing.T) {
	if os.Getenv("TRACE_WEB_ANALYSIS_DIR") == "" {
		t.Skip("set TRACE_WEB_ANALYSIS_DIR to the separately built web package")
	}
	manifests, status := localtool.Presets(context.Background())
	var capture localtool.Manifest
	for _, manifest := range manifests {
		if manifest.Name == "inspect_web_network_capture" {
			capture = manifest
		}
	}
	if capture.Name == "" {
		t.Fatalf("web capability missing: %+v", status)
	}
	path := filepath.Join(t.TempDir(), "capture.har")
	har := `{"log":{"version":"1.2","creator":{"name":"trace-fixture","version":"1"},"entries":[]}}`
	if err := os.WriteFile(path, []byte(har), 0600); err != nil {
		t.Fatal(err)
	}
	selection, _ := json.Marshal(map[string]any{"ids": []string{capture.StableID()}})
	input, _ := json.Marshal(map[string]string{"capture_path": path, "format": "har"})
	p := &scriptedProvider{responses: []ModelResponse{
		{ToolCalls: []ToolCall{{ID: "skill", Name: "read_skill", Arguments: `{"name":"web-analysis"}`}}},
		{ToolCalls: []ToolCall{{ID: "search", Name: "tool_search", Arguments: `{"query":"inspect_web_network_capture"}`}}},
		{ToolCalls: []ToolCall{{ID: "select", Name: "tool_select", Arguments: string(selection)}}},
		{ToolCalls: []ToolCall{{ID: "capture", Name: capture.StableID(), Arguments: string(input)}}},
		{Content: "empty capture inspected; no network activity established"},
	}}
	a, _ := newTestAgent(t, p)
	if err := a.AddLocalTools([]localtool.Manifest{capture}); err != nil {
		t.Fatal(err)
	}
	if err := a.ConfigureSkills([]string{filepath.Join("..", "..", ".trace", "skills", "web-analysis")}); err != nil {
		t.Fatal(err)
	}
	id := createTestRun(t, a)
	if _, err := a.SelectTools(id, []string{}, true); err != nil {
		t.Fatal(err)
	}
	s, err := a.Run(context.Background(), id)
	if err != nil || len(s.Actions) != 4 || s.Run.Status != StatusCompleted {
		t.Fatalf("workflow: %v %+v", err, s.Run)
	}
	for _, action := range s.Actions {
		if action.Result == nil || action.Result.ExitCode != 0 {
			t.Fatalf("workflow action failed: %+v", action)
		}
	}
	for i, req := range s.Requests {
		selected := false
		for _, tool := range req.Tools {
			if tool.Function.Name == capture.StableID() {
				selected = true
				if tool.Function.Parameters == nil {
					t.Fatal("selected schema missing")
				}
			}
		}
		if selected != (i >= 3) {
			t.Fatalf("schema exposed at wrong request %d", i)
		}
	}
	if !strings.Contains(s.Actions[0].Result.Stdout, "capture_sha256") || len(s.Actions[0].Result.Artifacts) == 0 {
		t.Fatal("skill body or raw revision identity missing")
	}
	actual := s.Actions[3]
	output, ok := verifiedOutput(&s, actual.Result.EvidenceID)
	if !ok || !strings.Contains(output, `"total_records":0`) || actual.Adapter != "local" || actual.LocalName != capture.Name {
		t.Fatalf("real capture identity/result absent: %+v %s", actual, output)
	}
}

func TestWebWorkflowSkillMissingDependencyKeepsDirectoryHonest(t *testing.T) {
	a, _ := newTestAgent(t, successProvider{})
	if err := a.ConfigureSkills([]string{filepath.Join("..", "..", ".trace", "skills", "web-analysis")}); err != nil {
		t.Fatal(err)
	}
	doc, err := a.readModelSkillDocument("web-analysis", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(doc.Body, "missing dependency") || !strings.Contains(doc.Body, "partial/unknown") {
		t.Fatal("workflow omitted missing/partial boundary")
	}
	result, err := a.SearchTools("inspect_web_network_capture", 0, 10)
	if err != nil || result.Total != 0 {
		t.Fatalf("skill invented unavailable capability: %+v %v", result, err)
	}
}
