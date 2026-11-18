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
