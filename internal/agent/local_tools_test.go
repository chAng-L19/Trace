package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trace/internal/localtool"
	"trace/internal/worker"
)

func TestAgentLocalFixture(t *testing.T) {
	mode := os.Getenv("TRACE_AGENT_LOCAL_FIXTURE")
	if mode == "" {
		return
	}
	if marker := os.Getenv("TRACE_AGENT_LOCAL_MARKER"); marker != "" {
		_ = os.WriteFile(marker, []byte("started"), 0600)
	}
	if mode == "hang" {
		time.Sleep(time.Hour)
		os.Exit(2)
	}
	if mode == "fail" {
		_, _ = os.Stderr.WriteString("tool failure")
		os.Exit(7)
	}
	data, _ := io.ReadAll(os.Stdin)
	_, _ = os.Stdout.Write(data)
	os.Exit(0)
}

func localManifest(t *testing.T, mode string) localtool.Manifest {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return localtool.Manifest{Name: "fixture", Command: binary, Args: []string{"-test.run=^TestAgentLocalFixture$"}, Mode: localtool.ModeStdio, Env: map[string]string{"TRACE_AGENT_LOCAL_FIXTURE": mode}, InputSchema: json.RawMessage(`{"type":"object","properties":{"message":{"type":"string"}}}`)}
}

func TestLocalToolPersistenceAndFailure(t *testing.T) {
	for _, mode := range []string{"echo", "fail"} {
		t.Run(mode, func(t *testing.T) {
			manifest := localManifest(t, mode)
			a, store := newTestAgent(t, manualProvider(ToolCall{ID: "local", Name: manifest.StableID(), Arguments: `{"message":"hello local"}`}))
			if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
				t.Fatal(err)
			}
			id := createTestRun(t, a)
			snap, err := a.Run(context.Background(), id)
			if err != nil || snap.Run.Status != StatusCompleted || len(snap.Actions) != 1 || len(snap.Evidence) != 1 {
				t.Fatalf("run: %+v %v", snap, err)
			}
			action := snap.Actions[0]
			if action.Adapter != "local" || action.LocalName != "fixture" || action.Result.WorkerPID == 0 || action.Result.EvidenceID != snap.Evidence[0].ID {
				t.Fatalf("action: %+v", action)
			}
			ledger, err := store.EvidenceLedger(id)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "echo" && (ledger[0].Verification != "verified" || !strings.Contains(action.Result.Stdout, "hello local")) {
				t.Fatalf("success: %+v", ledger)
			}
			if mode == "fail" && (ledger[0].Verification != "unverified" || action.Result.ExitCode != 7 || action.Result.Stderr != "tool failure") {
				t.Fatalf("failure: %+v", action.Result)
			}
			if len(snap.Requests) != 2 || len(snap.Requests[0].Tools) != 2+len(builtinDefinitions()) {
				t.Fatal("model registry or tool continuation missing")
			}
			last := lastConversationMessage(snap.Requests[1])
			if last.Role != "tool" || last.ToolCallID != action.CallID || last.Content != snap.Evidence[0].Content {
				t.Fatal("tool continuation missing")
			}
			encoded, _ := json.Marshal(snap.Requests[0].Tools)
			if strings.Contains(string(encoded), "TRACE_AGENT_LOCAL_FIXTURE") || strings.Contains(string(encoded), binaryName(manifest.Command)) {
				t.Fatal("process config exposed in schema")
			}
		})
	}
}

func binaryName(path string) string { return filepath.Base(path) }

func TestLocalToolExplicitValidationDoesNotMutate(t *testing.T) {
	manifest := localManifest(t, "echo")
	manifest.Args = append(manifest.Args, "{{required}}")
	a, store := newTestAgent(t, manualProvider(ToolCall{ID: "pending", Name: manifest.StableID(), Arguments: `{}`}))
	id := createTestRun(t, a)
	waitTestRun(t, a, id)
	if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
		t.Fatal(err)
	}
	before, _ := store.Load(id)
	x, _ := json.Marshal(before)
	for _, toolID := range []string{"unknown", manifest.StableID()} {
		if _, err := a.ExecuteRegistered(context.Background(), id, "pending", toolID); err == nil {
			t.Fatal("invalid invocation accepted")
		}
		after, _ := store.Load(id)
		y, _ := json.Marshal(after)
		if !bytes.Equal(x, y) {
			t.Fatal("invalid invocation mutated pending state")
		}
	}
}

func TestLocalToolTimeoutAndCancel(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			manifest := localManifest(t, "hang")
			marker := filepath.Join(t.TempDir(), "started")
			manifest.Env["TRACE_AGENT_LOCAL_MARKER"] = marker
			if mode == "timeout" {
				manifest.TimeoutSeconds = 1
			}
			p := manualProvider(ToolCall{ID: "pending", Name: manifest.StableID(), Arguments: `{}`})
			a, _ := newTestAgent(t, p)
			if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
				t.Fatal(err)
			}
			id := createTestRun(t, a)
			done := make(chan Snapshot, 1)
			go func() { snap, _ := a.Run(context.Background(), id); done <- snap }()
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("worker did not start")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if mode == "cancel" {
				if _, err := a.Cancel(id); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case snap := <-done:
				if len(snap.Actions) != 1 {
					t.Fatalf("actions: %+v", snap)
				}
				result := snap.Actions[0].Result
				want := worker.StatusTimedOut
				if mode == "cancel" {
					want = worker.StatusCancelled
				}
				if result == nil || result.WorkerStatus != want || !result.WorkerOutcomeUncertain || result.Error == "" {
					t.Fatalf("result: %+v", result)
				}
				if snap.Actions[0].Status != "uncertain" || !result.OutcomeUncertain || len(snap.Evidence) != 1 || snap.Evidence[0].Verification != "unverified" || len(pendingToolCalls(&snap)) != 0 {
					t.Fatal("uncertain worker result was not returned as an unverified observation")
				}
				raw, err := json.Marshal(result)
				if err != nil || snap.Evidence[0].Content != string(raw) {
					t.Fatal("original interrupted output or error changed in the observation")
				}
				if mode == "timeout" {
					if snap.Run.Status != StatusCompleted || len(p.requests) != 2 {
						t.Fatal("timeout did not automatically return to the model")
					}
					observed := false
					for _, message := range p.requests[1].Messages {
						if message.Role == "tool" && message.ToolCallID == "pending" && message.Content == string(raw) {
							observed = true
						}
					}
					if !observed {
						t.Fatal("model did not receive the original uncertain result")
					}
				} else if snap.Run.Status != StatusCancelled || len(p.requests) != 1 {
					t.Fatal("explicit cancellation did not stop model continuation")
				}
				if _, err := a.ExecuteRegistered(context.Background(), id, "pending", manifest.StableID()); err == nil {
					t.Fatal("settled unknown call was admitted for replay")
				}
				persisted, err := a.store.Load(id)
				if err != nil || len(persisted.Actions) != 1 || persisted.Actions[0].Status != "uncertain" || persisted.Actions[0].Result == nil {
					t.Fatal("unknown action was replayed or lost its durable result", err)
				}
				persistedRaw, _ := json.Marshal(persisted.Actions[0].Result)
				if string(persistedRaw) != string(raw) {
					t.Fatal("replay rejection changed the original result")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("worker not cancelled")
			}
		})
	}
}

func TestLocalToolDisabledRegistrationAndDispatch(t *testing.T) {
	manifest := localManifest(t, "echo")
	call := ToolCall{ID: "stale", Name: manifest.StableID(), Arguments: `{}`}
	manifest.Disabled = true
	a, store := newTestAgent(t, manualProvider(call))
	if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Registry().Lookup(call.Name); ok {
		t.Fatal("disabled tool registered")
	}
	id := createTestRun(t, a)
	waitTestRun(t, a, id)
	ctx := context.Background()
	if _, err := a.ExecuteRegistered(ctx, id, call.ID, call.Name); err == nil {
		t.Fatal("explicit dispatch accepted disabled tool")
	}
	snap, err := a.executePendingTool(ctx, id, call)
	if err != nil {
		t.Fatal(err)
	}
	var result ToolResult
	last := snap.Messages[len(snap.Messages)-1]
	if len(snap.Actions) != 0 || last.ToolCallID != call.ID || json.Unmarshal([]byte(last.Content), &result) != nil || result.WorkerStatus != worker.StatusNotStarted {
		t.Fatal("stale call did not produce a not-started observation without an action")
	}

	manifest.Disabled = false
	rebuilt, err := New(store, MockProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := rebuilt.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
		t.Fatal(err)
	}
	if tool, ok := rebuilt.Registry().Lookup(call.Name); !ok || tool.Local == nil || tool.Local.Name != manifest.Name {
		t.Fatal("re-enabled tool missing at original stable ID")
	}
}

func TestLocalToolDuplicateRegistrationAtomic(t *testing.T) {
	r := NewToolRegistry()
	m := localManifest(t, "echo")
	if err := r.AddLocal([]localtool.Manifest{m, m}); err == nil {
		t.Fatal("duplicate accepted")
	}
	if len(r.Definitions()) != 1+len(builtinDefinitions()) {
		t.Fatal("partial registry mutation")
	}
	if err := r.AddLocal([]localtool.Manifest{m}); err != nil {
		t.Fatal(err)
	}
	if err := r.AddLocal([]localtool.Manifest{m}); err == nil {
		t.Fatal("existing identity collision accepted")
	}
}

func TestLocalToolDisabledIdentityIsReserved(t *testing.T) {
	dormant := localtool.Manifest{Name: "fixture", Disabled: true}
	active := localManifest(t, "echo")
	r := NewToolRegistry()
	for _, batch := range [][]localtool.Manifest{{active, dormant}, {dormant, active}, {dormant, dormant}, {{Name: "  ", Disabled: true}}} {
		if err := r.AddLocal(batch); err == nil {
			t.Fatal("invalid manifest identities accepted")
		}
	}
	if err := r.AddLocal([]localtool.Manifest{dormant}); err != nil {
		t.Fatalf("disabled identity should not require a command: %v", err)
	}
	clone, _, err := r.Clone(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, registry := range []*ToolRegistry{r, clone} {
		if _, ok := registry.Lookup(dormant.StableID()); ok {
			t.Fatal("reserved identity became callable")
		}
		if err := registry.AddLocal([]localtool.Manifest{active}); err == nil {
			t.Fatal("reserved identity could be registered a second time")
		}
	}
}

func TestLocalToolDisabledSelectionSaveResumeAndReenable(t *testing.T) {
	ctx := context.Background()
	m := localManifest(t, "echo")
	other := m
	other.Name = "other"
	p := manualProvider(ToolCall{ID: "find", Name: "tool_search", Arguments: `{}`})
	a, store := newTestAgent(t, p)
	if err := a.AddLocalTools([]localtool.Manifest{m, other}); err != nil {
		t.Fatal(err)
	}
	id := createTestRun(t, a)
	if _, err := a.SelectTools(id, []string{m.StableID()}, true); err != nil {
		t.Fatal(err)
	}
	one := 1
	a.ConfigureBudget(BudgetOverride{MaxModelRequests: &one})
	paused, err := a.Run(ctx, id)
	if err != nil || paused.Run.Status != StatusPaused || len(paused.Requests) != 1 {
		t.Fatalf("expected a paused run before rebuild: %v, %s", err, paused.Run.Status)
	}
	rebuild := func(disabled bool) *Agent {
		t.Helper()
		m.Disabled = disabled
		ag, err := New(store, successProvider{}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := ag.AddLocalTools([]localtool.Manifest{m, other}); err != nil {
			t.Fatal(err)
		}
		return ag
	}
	off := rebuild(true)
	ids := []string{m.StableID(), other.StableID()}
	if _, err := off.SelectTools(id, ids, true); err != nil {
		t.Fatal(err)
	}
	fresh := createTestRun(t, off)
	for _, bad := range []struct{ run, tool string }{{fresh, m.StableID()}, {id, "unknown"}, {id, "exec"}} {
		if _, err := off.SelectTools(bad.run, []string{bad.tool}, true); err == nil {
			t.Fatal("accepted an unavailable new selection", bad)
		}
	}
	two := 2
	off.ConfigureBudget(BudgetOverride{MaxModelRequests: &two})
	s, err := off.Run(ctx, id)
	if err != nil || s.Run.Status != StatusCompleted || len(s.Requests) != 2 || s.Run.ToolSelection == nil || !hasString(s.Run.ToolSelection.IDs, m.StableID()) {
		t.Fatalf("resume lost saved selection or failed to reach provider: %v, %s", err, s.Run.Status)
	}
	hasSchema := func(defs []ToolDefinition, id string) bool {
		for _, d := range defs {
			if d.Function.Name == id {
				return true
			}
		}
		return false
	}
	if hasSchema(s.Requests[1].Tools, m.StableID()) || !hasSchema(s.Requests[1].Tools, other.StableID()) {
		t.Fatal("resume did not project only the available selected schema")
	}
	on := rebuild(false)
	defs, _, err := on.modelTools(&s, "")
	if err != nil || !hasSchema(defs, m.StableID()) {
		t.Fatal("re-enabled schema not restored from the saved selection", err)
	}
	if _, err := off.SelectTools(id, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := off.SelectTools(id, ids, true); err == nil {
		t.Fatal("cleared disabled ID could be added again")
	}
}

func TestLocalToolRealExamples(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "local-tools.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifests []localtool.Manifest
	if err := json.Unmarshal(data, &manifests); err != nil {
		t.Fatal(err)
	}
	projectDir, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "space & literal.txt")
	content := []byte("TRACE_TOOL_EXAMPLE\n")
	if err := os.WriteFile(input, content, 0600); err != nil {
		t.Fatal(err)
	}
	for _, manifest := range manifests {
		t.Run(manifest.Name, func(t *testing.T) {
			if _, err := exec.LookPath(manifest.Command); err != nil {
				t.Skipf("optional example dependency %s: %v", manifest.Command, err)
			}
			manifest.Dir = projectDir
			arguments, _ := json.Marshal(map[string]string{"path": input, "pattern": "TRACE_TOOL_EXAMPLE"})
			a, store := newTestAgent(t, manualProvider(ToolCall{ID: "example", Name: manifest.StableID(), Arguments: string(arguments)}))
			if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
				t.Fatal(err)
			}
			id := createTestRun(t, a)
			snap, err := a.Run(context.Background(), id)
			if err != nil || snap.Run.Status != StatusCompleted || len(snap.Actions) != 1 || snap.Actions[0].Result.ExitCode != 0 {
				t.Fatalf("example: %+v %v", snap, err)
			}
			output := snap.Actions[0].Result.Stdout
			if manifest.Name == "search_text" && !strings.Contains(output, `"type":"match"`) {
				t.Fatalf("rg: %s", output)
			}
			if manifest.Name == "file_metadata" {
				var metadata struct {
					Bytes  int
					SHA256 string
				}
				if json.Unmarshal([]byte(output), &metadata) != nil || metadata.Bytes != len(content) || metadata.SHA256 != fmt.Sprintf("%x", sha256.Sum256(content)) {
					t.Fatalf("python: %s", output)
				}
			}
			ledger, err := store.EvidenceLedger(id)
			if err != nil || len(ledger) != 1 || ledger[0].Verification != "verified" {
				t.Fatalf("evidence: %+v %v", ledger, err)
			}
		})
	}
}
