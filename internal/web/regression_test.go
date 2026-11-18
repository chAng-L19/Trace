package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"trace/internal/agent"
	"trace/internal/localtool"
)

func TestMessageRejectedBeforeAcceptance(t *testing.T) {
	for _, status := range []agent.Status{agent.StatusPaused, agent.StatusCancelled, agent.StatusRunning, agent.StatusWaitingTool, agent.StatusCreated} {
		t.Run(string(status), func(t *testing.T) {
			s, a := fixture(t, nil)
			snap, err := a.Create("message acceptance")
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.Store().Update(snap.Run.ID, func(snap *agent.Snapshot) error { snap.Run.Status = status; return nil })
			if err != nil {
				t.Fatal(err)
			}
			before, _ := a.Store().Load(snap.Run.ID)
			content := "must not vanish"
			if status == agent.StatusCreated {
				content = "  "
			}
			request(t, s.Handler(), "POST", "/api/runs/"+snap.Run.ID+"/message", map[string]string{"content": content}, 409)
			after, _ := a.Store().Load(snap.Run.ID)
			if after.Run.EventSeq != before.Run.EventSeq || len(after.Messages) != len(before.Messages) {
				t.Fatal("rejected input mutated transcript")
			}
			if status == agent.StatusPaused {
				request(t, s.Handler(), "POST", "/api/runs/"+snap.Run.ID+"/steer", map[string]string{"content": content}, 200)
				request(t, s.Handler(), "POST", "/api/runs/"+snap.Run.ID+"/resume", map[string]any{}, 200)
				s.wg.Wait()
				after, _ = a.Store().Load(snap.Run.ID)
				found := false
				for _, message := range after.Messages {
					found = found || message.Content == content
				}
				if !found || after.Run.Status != agent.StatusCompleted {
					t.Fatal("queued paused message was not delivered on resume")
				}
			}
		})
	}
}

func TestAsynchronousStartFailureIsVisible(t *testing.T) {
	s, a := fixture(t, providerFunc(func(context.Context, agent.ModelRequest) (agent.ModelResponse, error) {
		return agent.ModelResponse{}, fmt.Errorf("fixture execution failed")
	}))
	snap, err := a.Create("execution error remains visible")
	if err != nil {
		t.Fatal(err)
	}
	request(t, s.Handler(), "POST", "/api/runs/"+snap.Run.ID+"/message", map[string]string{"content": "retain error visibility"}, 200)
	s.wg.Wait()
	snap, err = a.Store().Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Error == "" || snap.Events[len(snap.Events)-1].Type != "web_operation_error" {
		t.Fatalf("async error hidden: %+v", snap.Run)
	}
}

func TestRunAdmissionFailureDoesNotMutateOtherOwner(t *testing.T) {
	for _, operation := range []string{"resume", "message"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			store, err := agent.NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			a, err := agent.New(store, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			s, err := New(a, Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			snap, err := a.Create("owned by the CLI")
			if err != nil {
				t.Fatal(err)
			}
			release, err := store.AcquireRunLock(snap.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			path := filepath.Join(root, snap.Run.ID+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			request(t, s.Handler(), "POST", "/api/runs/"+snap.Run.ID+"/"+operation, map[string]string{"content": "rejected competing request"}, 200)
			s.wg.Wait()
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("lock rejection mutated another owner's snapshot")
			}
			observed, err := store.Load(snap.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if observed.Run.EventSeq != snap.Run.EventSeq {
				t.Fatal("lock rejection appended an event")
			}
			s.mu.Lock()
			active := s.active[snap.Run.ID]
			s.mu.Unlock()
			if active {
				t.Fatal("rejected request retained the Web active slot")
			}
		})
	}
}

func TestToolSelectionAPI(t *testing.T) {
	s, a := fixture(t, nil)
	manifest := localtool.Manifest{Name: "web-select", Command: "fixture-command", InputSchema: json.RawMessage(`{"type":"object"}`)}
	if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
		t.Fatal(err)
	}
	snap, err := a.Create("select schemas")
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/runs/" + snap.Run.ID + "/tools"
	request(t, s.Handler(), "PUT", path, map[string]any{"ids": []string{manifest.StableID()}, "replace": true}, 200)
	snap, err = a.Store().Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.ToolSelection == nil || len(snap.Run.ToolSelection.IDs) != 1 || snap.Run.ToolSelection.IDs[0] != manifest.StableID() {
		t.Fatal("tool selection not persisted")
	}
	request(t, s.Handler(), "PUT", path, map[string]any{"ids": []string{"exec"}, "replace": true}, 409)
	request(t, s.Handler(), "PUT", path, map[string]any{"ids": []string{"missing"}, "replace": true}, 409)
	after, _ := a.Store().Load(snap.Run.ID)
	if after.Run.ToolSelection.IDs[0] != manifest.StableID() {
		t.Fatal("failed selection was not atomic")
	}
	request(t, s.Handler(), "PUT", path, map[string]any{"ids": []string{}, "replace": true}, 200)
	after, _ = a.Store().Load(snap.Run.ID)
	if after.Run.ToolSelection == nil || len(after.Run.ToolSelection.IDs) != 0 {
		t.Fatal("clear did not persist explicit empty selection")
	}
}

func TestFailedWorkerRetryAPI(t *testing.T) {
	calls := 0
	s, a := fixture(t, providerFunc(func(_ context.Context, _ agent.ModelRequest) (agent.ModelResponse, error) {
		calls++
		if calls == 1 {
			return agent.ModelResponse{}, fmt.Errorf("provider fixture unavailable")
		}
		return agent.ModelResponse{Content: "retry completed"}, nil
	}))
	snap, err := a.Create("worker retry")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.SetPlan(snap.Run.ID, []agent.PlanTask{{ID: "task", Goal: "request model"}})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/runs/" + snap.Run.ID
	request(t, s.Handler(), "POST", path+"/tasks/task/dispatch", map[string]any{}, 200)
	request(t, s.Handler(), "POST", path+"/workers/wait", map[string]int{"seconds": 2}, 200)
	request(t, s.Handler(), "POST", path+"/tasks/task/retry", map[string]any{}, 200)
	// An unconsumed failed-attempt message now intentionally wakes worker_wait.
	// Mirror the parent model loop's delivery before waiting on the next attempt.
	if err := a.DeliverWorkerMessages(snap.Run.ID); err != nil {
		t.Fatal(err)
	}
	request(t, s.Handler(), "POST", path+"/tasks/task/dispatch", map[string]any{}, 200)
	request(t, s.Handler(), "POST", path+"/workers/wait", map[string]int{"seconds": 2}, 200)
	snap, err = a.Store().Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Plan.Tasks[0].Status != "completed" || len(snap.Workers) != 2 || snap.Workers[0].Status != "superseded" {
		t.Fatalf("retry provenance lost: %+v", snap.Plan)
	}
}

func TestProviderConfigRoundTrip(t *testing.T) {
	s, _ := fixture(t, nil)
	want := Config{Provider: ProviderConfig{URL: "http://127.0.0.1:9876/v1", Model: "web-fixture", APIKey: "private-fixture"}, Context: agent.DefaultContextConfig()}
	request(t, s.Handler(), "PUT", "/api/config", want, 200)
	w := request(t, s.Handler(), "GET", "/api/config", nil, 200)
	var got Config
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Provider.URL != want.Provider.URL || got.Provider.Model != want.Provider.Model || got.Provider.APIKey != hiddenSecret {
		t.Fatalf("config roundtrip changed fields: %+v", got.Provider)
	}
	request(t, s.Handler(), "PUT", "/api/config", got, 200)
	persisted, err := LoadConfig(s.options.ConfigPath, Config{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted.Config.Provider, want.Provider) {
		t.Fatal("stored-secret roundtrip changed provider")
	}
}

func TestCASOutputProcess(t *testing.T) {
	if os.Getenv("TRACE_WEB_CAS_FIXTURE") != "1" {
		return
	}
	fmt.Print(strings.Repeat("abcde", 5000) + "web-proof")
	os.Exit(0)
}

func TestCASLossConsistentDetailAndExports(t *testing.T) {
	root := t.TempDir()
	store, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	manifest := localtool.Manifest{Name: "web-cas", Command: binary, Args: []string{"-test.run=^TestCASOutputProcess$"}, Env: map[string]string{"TRACE_WEB_CAS_FIXTURE": "1"}, InputSchema: json.RawMessage(`{"type":"object"}`)}
	p := providerFunc(func(_ context.Context, r agent.ModelRequest) (agent.ModelResponse, error) {
		// Consume this fixture's actual result, not a request-only ledger tail.
		var last agent.Message
		for _, message := range r.Messages {
			if message.Role == "tool" && message.ToolCallID == "large" {
				last = message
			}
		}
		if last.Role != "tool" {
			return agent.ModelResponse{ToolCalls: []agent.ToolCall{{ID: "large", Name: manifest.StableID(), Arguments: `{}`}}}, nil
		}
		var result agent.ToolResult
		if err := json.Unmarshal([]byte(last.Content), &result); err != nil {
			return agent.ModelResponse{}, err
		}
		body, _ := json.Marshal(map[string]any{"trace_result": map[string]any{"status": "success", "evidence_ids": []string{result.EvidenceID}}})
		return agent.ModelResponse{Content: string(body)}, nil
	})
	a, err := agent.New(store, p, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
		t.Fatal(err)
	}
	s, err := New(a, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	run, err := a.Create("large proof")
	if err != nil {
		t.Fatal(err)
	}
	done, err := a.Run(context.Background(), run.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Result.Status != "success" || len(done.Actions[0].Result.Artifacts) != 1 {
		t.Fatalf("baseline failed: %+v", done.Result)
	}
	ref := done.Actions[0].Result.Artifacts[0]
	if err = os.Remove(filepath.Join(root, "artifacts", ref.Hash)); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "/export?format=json"} {
		w := request(t, s.Handler(), "GET", "/api/runs/"+run.Run.ID+suffix, nil, 200)
		var detail agent.Snapshot
		if err = json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
			t.Fatal(err)
		}
		if detail.Result.Status == "success" || detail.Evidence[0].Verification == "verified" {
			t.Fatalf("stale success in %s", suffix)
		}
	}
	w := request(t, s.Handler(), "GET", "/api/runs/"+run.Run.ID+"/export?format=evidence", nil, 200)
	var evidence []agent.Evidence
	if err = json.Unmarshal(w.Body.Bytes(), &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence[0].Verification == "verified" {
		t.Fatal("stale evidence export")
	}
	w = request(t, s.Handler(), "GET", "/api/runs/"+run.Run.ID+"/export?format=report", nil, 200)
	if !strings.Contains(w.Body.String(), `"business_status": "incomplete"`) || strings.Contains(w.Body.String(), `"verification": "verified"`) || !strings.Contains(w.Body.String(), "translation_missing") {
		t.Fatal("report disagrees with revalidated snapshot")
	}
}
