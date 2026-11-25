package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"trace/internal/localtool"
)

// Deterministic decisions exercise the real agent-loop/tool/Store/Judge path. They are
// mechanism acceptance, not a measurement of a live model's research quality.
type generalAcceptanceStep struct {
	id, tool string
	args     func(map[string]ToolResult) any
}

func runGeneralAcceptance(t *testing.T, goal, marker, proof, deliverable string, requirements []VerificationRequirement, steps []generalAcceptanceStep, manifests []localtool.Manifest) {
	t.Helper()
	provider := settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
		results := map[string]ToolResult{}
		for _, message := range request.Messages {
			if message.Role == "tool" {
				var result ToolResult
				if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
					return ModelResponse{}, err
				}
				results[message.ToolCallID] = result
			}
		}
		for _, step := range steps {
			if result, ok := results[step.id]; ok {
				if result.ExitCode != 0 || result.Error != "" {
					return ModelResponse{}, fmt.Errorf("%s failed: %+v", step.id, result)
				}
				continue
			}
			args, err := json.Marshal(step.args(results))
			return ModelResponse{ToolCalls: []ToolCall{{ID: step.id, Name: step.tool, Arguments: string(args)}}}, err
		}
		evidence := results[proof].EvidenceID
		raw, err := json.Marshal(map[string]any{"trace_result": Result{Status: "success", Summary: goal, EvidenceIDs: []string{evidence}, Answers: []Answer{{Value: marker, EvidenceIDs: []string{evidence}}}}})
		return ModelResponse{Content: fixtureReport(string(raw))}, err
	})
	a, store := newTestAgent(t, provider)
	a.SetRequireVerifiedCompletion(true)
	if err := a.AddLocalTools(manifests); err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, r := range requirements {
		ids = append(ids, r.ID)
	}
	// These cases validate execution and Judge acceptance within a Worker.
	s, err := a.CreateConfigured(goal, RunConfiguration{role: "worker", Requirements: requirements, RootContract: &RootContract{Version: 1, RequirementIDs: ids}, ToolSchema: ToolSchemaConfig{Mode: "deferred"}})
	if err != nil {
		t.Fatal(err)
	}
	s, err = a.Run(context.Background(), s.Run.ID)
	if err != nil || s.Run.Status != StatusCompleted || s.Result.Status != "success" || len(unmetRequirements(&s)) != 0 || len(rootContractGaps(&s)) != 0 {
		t.Fatalf("general acceptance failed: run=%+v result=%+v unmet=%+v err=%v", s.Run, s.Result, unmetRequirements(&s), err)
	}
	for _, requirement := range requirements {
		if requirement.Category == "cleanup" && s.Result.CleanupStatus != "completed" {
			t.Fatalf("cleanup contract passed without completed cleanup: %+v", s.Result)
		}
	}
	if len(s.Actions) != len(steps) {
		t.Fatalf("extra or missing actions: %d want %d", len(s.Actions), len(steps))
	}
	for _, action := range s.Actions {
		if action.Result == nil || action.Result.ExitCode != 0 || action.Status != "completed" {
			t.Fatalf("unsettled action: %+v", action)
		}
	}
	last := s.Actions[len(s.Actions)-1].Result
	if len(last.Artifacts) != 1 {
		t.Fatal("deliverable was not captured")
	}
	actual, err := os.ReadFile(deliverable)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.OpenArtifact(last.Artifacts[0].Hash)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := io.ReadAll(blob)
	blob.Close()
	if err != nil || string(actual) != string(captured) {
		t.Fatal("captured deliverable differs from actual file")
	}
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reopened.Load(s.Run.ID)
	if err != nil || saved.Result.Status != "success" || len(saved.Actions) != len(steps) || len(rootContractGaps(&saved)) != 0 {
		t.Fatalf("restart lost acceptance: %v", err)
	}
	t.Logf("mechanism-only requests=%d actions=%d deliverable_bytes=%d schema_mode=%s", len(s.Requests), len(s.Actions), len(captured), s.Run.ToolSchema.Mode)
}

func generalArgs(value any) func(map[string]ToolResult) any {
	return func(map[string]ToolResult) any { return value }
}
func generalCheck(id, source, requirement string) generalAcceptanceStep {
	return generalAcceptanceStep{id, "evidence_check", func(results map[string]ToolResult) any {
		return map[string]any{"requirement_id": requirement, "evidence_ids": []string{results[source].EvidenceID}}
	}}
}
func generalRequirement(id, tool, pointer string, expected any) VerificationRequirement {
	return VerificationRequirement{ID: id, Description: id, Category: "goal", ToolID: tool, Predicate: EvidencePredicate{Kind: "json_equals", Pointer: pointer, Expected: expected}}
}

func TestGeneralAgentCrossTaskAcceptance(t *testing.T) {
	for _, mode := range []string{"research_report", "code_fix_review", "file_analysis"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.txt")
			output := filepath.Join(dir, "result.txt")
			marker := "G7-" + mode
			input := "source-evidence: 21 + 21 = 42"
			content := marker + " supported result: 42"
			steps := []generalAcceptanceStep{}
			requirements := []VerificationRequirement{generalRequirement("source", "read", "/content", input)}
			if mode == "code_fix_review" {
				input = "assert 20 + 21 == 42\nprint('G7-code_fix_review')\n"
				source = filepath.Join(dir, "check.py")
				output = source
				requirements[0].Predicate.Expected = input
			}
			if mode == "file_analysis" {
				input = "[13,17,12]"
				requirements[0].Predicate.Expected = input
			}
			if err := os.WriteFile(source, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			steps = append(steps, generalAcceptanceStep{"source", "read", generalArgs(FileRequest{Path: source})}, generalCheck("source-check", "source", "source"))
			if mode == "code_fix_review" {
				content = "assert 21 + 21 == 42\nprint('G7-code_fix_review')\n"
				steps = append(steps, generalAcceptanceStep{"fix", "edit", generalArgs(FileRequest{Path: source, OldText: "20 + 21", NewText: "21 + 21", ExpectedHash: promptHash(input)})}, generalAcceptanceStep{"execute", "exec", generalArgs(map[string]any{"command": "python3 '" + source + "'", "timeout_seconds": 10})})
				requirements = append(requirements, VerificationRequirement{ID: "executed", Description: "actual patched code assertion", Category: "goal", ToolID: "exec", Predicate: EvidencePredicate{Kind: "equals", Value: marker + "\n"}})
				steps = append(steps, generalCheck("execution-check", "execute", "executed"))
			} else if mode == "file_analysis" {
				content = "G7-file_analysis sum=42"
				command := fmt.Sprintf("python3 -c \"import json,pathlib; values=json.loads(pathlib.Path('%s').read_text()); pathlib.Path('%s').write_text('G7-file_analysis sum='+str(sum(values)))\"", source, output)
				steps = append(steps, generalAcceptanceStep{"analyze", "exec", generalArgs(map[string]any{"command": command, "timeout_seconds": 10})})
			} else {
				steps = append(steps, generalAcceptanceStep{"report", "write", generalArgs(FileRequest{Path: output, Content: content})})
			}
			requirements = append(requirements, generalRequirement("deliverable", "read", "/content", content))
			steps = append(steps, generalAcceptanceStep{"review", "read", generalArgs(FileRequest{Path: output})}, generalCheck("deliverable-check", "review", "deliverable"), generalAcceptanceStep{"register", "artifact_register", generalArgs(ArtifactRegistration{Path: output, Name: mode, Purpose: "verified content deliverable"})})
			runGeneralAcceptance(t, mode, marker, "review", output, requirements, steps, nil)
		})
	}
}

func TestGeneralAgentHeadedBrowserAcceptance(t *testing.T) {
	if os.Getenv("TRACE_CLOAKBROWSER_TEST") != "1" {
		t.Skip("server-only explicit headed browser acceptance")
	}
	manifests, _ := localtool.Presets(context.Background())
	var browser localtool.Manifest
	for _, m := range manifests {
		if m.Name == "cloakbrowser" {
			browser = m
		}
	}
	if browser.Name == "" {
		t.Fatal("fixed CloakBrowser environment missing")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<button onclick="document.querySelector('#result').textContent='G7-browser'">Apply</button><p id="result">before</p>`)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "browser.png")
	code := `import psutil
assert any('--remote-debugging-pipe' in p.cmdline() and not any(a.startswith('--headless') for a in p.cmdline()) for p in psutil.Process().children(recursive=True)), 'headed browser missing'
page.goto(args['url'])
page.get_by_role('button',name='Apply').click()
assert page.locator('#result').inner_text() == 'G7-browser'
page.screenshot(path=args['path'])
result={'text':page.locator('#result').inner_text()}`
	requirements := []VerificationRequirement{generalRequirement("effect", browser.StableID(), "/result/text", "G7-browser"), generalRequirement("cleanup", browser.StableID(), "/cleanup", "completed")}
	requirements[1].Category = "cleanup"
	browseArgs := map[string]any{"code": code, "args": map[string]string{"url": server.URL, "path": path}}
	raw, err := json.Marshal(browseArgs)
	if err != nil {
		t.Fatal(err)
	}
	requirements[1].ArgumentsHash = promptHash(string(raw))
	steps := []generalAcceptanceStep{{"select", "tool_select", generalArgs(map[string]any{"ids": []string{browser.StableID()}})}, {"browse", browser.StableID(), generalArgs(browseArgs)}, generalCheck("effect-check", "browse", "effect"), generalCheck("cleanup-check", "browse", "cleanup"), {"register", "artifact_register", generalArgs(ArtifactRegistration{Path: path, Name: "browser effect", Purpose: "actual headed screenshot"})}}
	runGeneralAcceptance(t, "browser interaction and observed effect", "G7-browser", "browse", path, requirements, steps, []localtool.Manifest{browser})
}
