package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"trace/internal/localtool"
	"trace/internal/mcp"
)

func TestPlannerCleanupFailureFixture(t *testing.T) {
	if os.Getenv("TRACE_CLEANUP_FAILURE_FIXTURE") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var rpc integrationRPC
		if json.Unmarshal(scanner.Bytes(), &rpc) != nil {
			os.Exit(2)
		}
		if rpc.ID == 0 {
			continue
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": integrationReply(rpc, false)})
	}
	os.Exit(7)
}

func TestPlannerChildCleanupFailure(t *testing.T) {
	binary, _ := os.Executable()
	a, store := newTestAgent(t, successProvider{})
	_, err := a.AddMCP(context.Background(), mcp.Config{Name: "bad-cleanup", Command: binary, Args: []string{"-test.run=^TestPlannerCleanupFailureFixture$"}, Env: map[string]string{"TRACE_CLEANUP_FAILURE_FIXTURE": "1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.CloseMCP()
	id := createTestRun(t, a)
	a.SetPlan(id, []PlanTask{{ID: "one", Goal: "inspect"}, {ID: "two", Goal: "dependent", Dependencies: []string{"one"}}})
	if _, err = a.DispatchTask(context.Background(), id, "one"); err != nil {
		t.Fatal(err)
	}
	s, err := waitAllTestWorkers(a, context.Background(), id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if s.Workers[0].CleanupStatus != "uncertain" || s.Plan.Tasks[0].Status != "uncertain" || len(s.Plan.Frontier) != 0 {
		t.Fatalf("cleanup failure promoted: %+v %+v", s.Workers, s.Plan)
	}
	if err = a.StopWorkers(context.Background()); err == nil {
		t.Fatal("stop hid child cleanup failure")
	}
	child, err := store.Load(s.Workers[0].RunID)
	if err != nil || child.Run.Status != StatusCompleted {
		t.Fatalf("child model did not run %v", err)
	}
}

func TestPlannerIncompleteChildDoesNotUnlockDependency(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	parent, err := a.SetPlan(id, []PlanTask{
		{ID: "work", Goal: "perform the planned work"},
		{ID: "dependent", Goal: "must wait for verified completion", Dependencies: []string{"work"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	wid, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	parent, err = a.ClaimTask(id, "work", wid, 30)
	if err != nil {
		t.Fatal(err)
	}
	task, _ := planTask(&parent, "work")
	child, err := a.Create(task.Goal)
	if err != nil {
		t.Fatal(err)
	}
	worker := AgentWorker{ID: wid, TaskID: "work", RunID: child.Run.ID, Status: "running", CleanupStatus: "pending", StartedAt: time.Now().UTC()}
	_, err = store.Update(child.Run.ID, func(s *Snapshot) error {
		s.Run.ParentRunID = id
		s.Run.ParentTaskID = "work"
		s.Run.RequireVerifiedCompletion = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(id, func(s *Snapshot) error {
		t, e := planTask(s, "work")
		if e != nil {
			return e
		}
		t.ChildRunID = child.Run.ID
		s.Workers = append(s.Workers, worker)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.runChildOperation(context.Background(), id, worker, func(_ *Agent, _ context.Context, runID string) (Snapshot, error) {
		return store.Update(runID, func(s *Snapshot) error {
			s.Run.Status = StatusCompleted
			s.Result = newIncomplete("model reply completed; target success has not been verified")
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	parent, err = store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if parent.Plan.Tasks[0].Status != "uncertain" || len(parent.Plan.Frontier) != 0 {
		t.Fatalf("incomplete Judge result advanced the plan: task=%+v frontier=%v", parent.Plan.Tasks[0], parent.Plan.Frontier)
	}
}

// A real local HTTP provider and real child process exercise both protocol
// boundaries. The fixture deliberately claims success with the wrong predicate.
func TestPlannerHTTPChildPredicateRegression(t *testing.T) {
	for _, predicate := range []string{"observed-proof", "different-proof"} {
		t.Run(predicate, func(t *testing.T) {
			manifest := localManifest(t, "echo")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					Messages []wireMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				message := map[string]any{"role": "assistant"}
				var result ToolResult
				for _, m := range request.Messages {
					if m.Role == "tool" {
						json.Unmarshal([]byte(m.Content), &result)
					}
				}
				if result.EvidenceID == "" {
					message["tool_calls"] = []any{map[string]any{"id": "child-exec", "type": "function", "function": map[string]string{"name": manifest.StableID(), "arguments": `{"message":"observed-proof"}`}}}
				} else {
					claim, _ := json.Marshal(map[string]any{"trace_result": map[string]any{"status": "success", "evidence_ids": []string{result.EvidenceID}}})
					message["content"] = fixtureReport(string(claim))
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": "stop"}}})
			}))
			defer server.Close()
			a, store := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "fixture"})
			// A deliberately uncorrectable provider must remain incomplete until
			// an explicit operator budget pauses it; model stop is not completion.
			limit := 3
			a.ConfigureBudget(BudgetOverride{MaxModelRequests: &limit})
			if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
				t.Fatal(err)
			}
			id := createTestRun(t, a)
			if _, err := a.SetPlan(id, []PlanTask{{ID: "child", Goal: "inspect", SuccessContains: predicate}, {ID: "later", Goal: "follow", Dependencies: []string{"child"}}}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.DispatchTask(context.Background(), id, "child"); err != nil {
				t.Fatal(err)
			}
			s, err := waitAllTestWorkers(a, context.Background(), id, 10)
			if err != nil {
				t.Fatal(err)
			}
			defer a.StopWorkers(context.Background())
			want := "verified"
			if predicate == "different-proof" {
				want = "paused"
			}
			if s.Plan.Tasks[0].Status != want {
				t.Fatalf("task status=%s want %s", s.Plan.Tasks[0].Status, want)
			}
			child, err := store.Load(s.Workers[0].RunID)
			if err != nil {
				t.Fatal(err)
			}
			if child.Actions[0].Result.WorkerPID == 0 {
				t.Fatal("no real process")
			}
			if want == "paused" && (child.Result.Status == "success" || child.Run.Status != StatusPaused || len(child.Requests) != limit || len(s.Plan.Frontier) != 0) {
				t.Fatal("wrong predicate promoted")
			}
			if want == "verified" {
				_, err = a.UpsertAsset(id, Asset{ID: "child-asset", Kind: "file", Value: "proof", Coverage: "verified", Predicate: predicate, EvidenceIDs: s.Plan.Tasks[0].EvidenceIDs, SourceRunID: child.Run.ID})
				if err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPlannerCleanupAndBranchIsolation(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	s, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := a.Branch(id, s.Messages[len(s.Messages)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.SendWorkerMessage(id, "parent", "parent", "parent-only")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.DeliverWorkerMessages(branch.Run.ID); err != nil {
		t.Fatal(err)
	}
	parent, _ := store.Load(id)
	if parent.WorkerMessages[0].Delivered {
		t.Fatal("branch stole parent inbox")
	}
	after, _ := store.Load(branch.Run.ID)
	for _, m := range after.Messages {
		if strings.Contains(m.Content, "parent-only") {
			t.Fatal("branch received parent message")
		}
	}
	for _, cleanup := range []string{"pending", "unknown", "uncertain", ""} {
		_, err = store.Update(id, func(s *Snapshot) error {
			s.Run.Status = StatusCompleted
			s.Workers = []AgentWorker{{ID: "test", Status: "completed", CleanupStatus: cleanup}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.Result(id)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status == "success" || !strings.Contains(result.TerminalReason, "cleanup") {
			t.Fatalf("cleanup %q accepted: %+v", cleanup, result)
		}
	}
}

func TestPlannerPriorityCapacityFactsCoverage(t *testing.T) {
	a, _ := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	tasks := []PlanTask{{ID: "low", Goal: "low", Priority: 1}, {ID: "high", Goal: "high", Priority: 9, Mode: "reason"}}
	s, err := a.SetPlan(id, tasks)
	if err != nil {
		t.Fatal(err)
	}
	if s.Plan.Frontier[0] != "high" {
		t.Fatal("priority lost")
	}
	a.SetPlanCapacity(id, 1)
	if _, err = a.ClaimTask(id, "high", "w", 30); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ClaimTask(id, "low", "v", 30); err == nil {
		t.Fatal("capacity exceeded")
	}
	for _, asset := range []Asset{{ID: "root", Kind: "scope", Value: "root"}, {ID: "leaf", ParentID: "root", Kind: "file", Value: "leaf"}, {ID: "excluded", Kind: "file", Value: "ignored", Scope: "excluded"}} {
		s, err = a.UpsertAsset(id, asset)
		if err != nil {
			t.Fatal(err)
		}
	}
	coverage := Coverage(s)
	if coverage.Total != 2 || coverage.Uncovered != 2 || coverage.Excluded != 1 || coverage.ByParent["root"].Total != 2 {
		t.Fatalf("coverage %+v", coverage)
	}
	projection, err := a.PlannerContext(id)
	if err != nil || !strings.Contains(projection, "asset_added") || !strings.Contains(projection, "leaf") {
		t.Fatalf("facts missing: %s %v", projection, err)
	}
	if _, err = a.UpsertAsset(id, Asset{ID: "root", ParentID: "leaf", Kind: "scope", Value: "root"}); err == nil {
		t.Fatal("hierarchy cycle accepted")
	}
}

func TestPlannerStopWorkersWaitsForActualExit(t *testing.T) {
	p := &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	a.SetPlan(id, []PlanTask{{ID: "x", Goal: "wait"}})
	_, err := a.DispatchTask(context.Background(), id, "x")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not enter model")
	}
	if !a.HasActiveWorkers() {
		t.Fatal("active child missing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = a.StopWorkers(ctx); err != nil {
		t.Fatal(err)
	}
	if a.HasActiveWorkers() {
		t.Fatal("child remains active")
	}
	s, _ := store.Load(id)
	if s.Workers[0].Status == "running" || s.Workers[0].CleanupStatus != "completed" {
		t.Fatalf("cleanup not persisted: %+v", s.Workers)
	}
	if _, err = a.DispatchTask(context.Background(), id, "x"); err == nil {
		t.Fatal("closed dispatcher accepted child")
	}
}

func TestPlannerLargeProcessFixture(t *testing.T) {
	if os.Getenv("TRACE_LARGE_FIXTURE") != "1" {
		return
	}
	for i := 0; i < 4096; i++ {
		fmt.Fprint(os.Stdout, strings.Repeat("0123456789abcdef", 64))
	}
	fmt.Fprint(os.Stdout, "\x00\xff\xe4\xb8\xad-proof-tail")
	os.Exit(0)
}

func TestPlannerLargeCASByteFaithful(t *testing.T) {
	executable, _ := os.Executable()
	manifest := localtool.Manifest{Name: "large", Command: executable, Args: []string{"-test.run=^TestPlannerLargeProcessFixture$"}, Env: map[string]string{"TRACE_LARGE_FIXTURE": "1"}, InputSchema: json.RawMessage(`{"type":"object"}`)}
	a, store := newTestAgent(t, manualProvider(ToolCall{ID: "large", Name: manifest.StableID(), Arguments: `{}`}))
	if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
		t.Fatal(err)
	}
	id := createTestRun(t, a)
	s, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	result := s.Actions[0].Result
	if result.Stdout != "" || len(result.Artifacts) != 1 {
		t.Fatal("large output retained in snapshot")
	}
	output, valid := verifiedOutput(&s, s.Evidence[0].ID)
	if !valid || !strings.HasSuffix(output, "\x00\xff\xe4\xb8\xad-proof-tail") {
		t.Fatal("raw output not restored")
	}
	ref := result.Artifacts[0]
	var recovered bytes.Buffer
	for offset := int64(len(output) - 17); offset < int64(len(output)); {
		page, err := store.ReadArtifact(ref.Hash, offset, 2)
		if err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(page.DataBase64)
		if err != nil {
			t.Fatal(err)
		}
		recovered.Write(data)
		if page.NextOffset != offset+int64(len(data)) {
			t.Fatal("byte offset changed")
		}
		offset = page.NextOffset
	}
	if recovered.String() != output[len(output)-17:] {
		t.Fatal("binary paging corrupted bytes")
	}
	f, err := store.OpenArtifact(ref.Hash)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(f)
	f.Close()
	if string(raw) != output {
		t.Fatal("download differs")
	}
	persisted, err := os.ReadFile(filepath.Join(store.root, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) > 500000 {
		t.Fatalf("snapshot retains duplicate blobs: %d", len(persisted))
	}
	leftovers, _ := filepath.Glob(filepath.Join(store.root, "artifacts", ".*-*"))
	if len(leftovers) > 0 {
		t.Fatalf("capture files leaked %v", leftovers)
	}
	// Corruption with unchanged size/time must still fail authoritative evidence
	// and downloads, even if paged preview metadata was cached earlier.
	artifactPath := filepath.Join(store.root, "artifacts", ref.Hash)
	info, _ := os.Stat(artifactPath)
	raw[0] = 'X'
	if err = os.WriteFile(artifactPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(artifactPath, info.ModTime(), info.ModTime())
	if verifyEvidence(&s, &s.Evidence[0]) != "invalid" {
		t.Fatal("corrupted blob still verified")
	}
	if f, err := store.OpenArtifact(ref.Hash); err == nil {
		f.Close()
		t.Fatal("corrupted download accepted")
	}
}

func TestPlannerHTTPReasonExploreLoop(t *testing.T) {
	manifest := localManifest(t, "echo")
	var sawFacts bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []wireMessage `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&request)
		child := false
		lastTool := ""
		proof := ToolResult{}
		for _, m := range request.Messages {
			if m.Role == "user" && m.Content == "explore-child" {
				child = true
			}
			if m.Role == "tool" {
				lastTool = m.ToolCallID
				json.Unmarshal([]byte(m.Content), &proof)
			}
		}
		name, args, id := "", "", ""
		message := map[string]any{"role": "assistant", "content": "done"}
		if child {
			if lastTool == "" {
				name, args, id = manifest.StableID(), `{"message":"explore-proof"}`, "child-proof"
			}
		} else {
			switch lastTool {
			case "":
				name, args, id = "plan", `{"capacity":1,"tasks":[{"id":"explore","goal":"explore-child","mode":"explore","success_contains":"explore-proof"}]}`, "reason-plan"
			case "reason-plan":
				name, args, id = "worker_dispatch", `{}`, "explore-dispatch"
			case "explore-dispatch":
				name, args, id = "worker_wait", `{"seconds":10}`, "reason-wait"
			case "reason-wait":
				for _, m := range request.Messages {
					if m.Role == "user" && strings.HasPrefix(m.Content, "Planner observations (data):") && strings.Contains(m.Content, "worker_finished") && strings.Contains(m.Content, "verified") {
						sawFacts = true
					}
				}
			}
		}
		if name != "" {
			message["tool_calls"] = []any{map[string]any{"id": id, "type": "function", "function": map[string]string{"name": name, "arguments": args}}}
		} else {
			message["content"] = fixtureReport("done")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": "stop"}}})
	}))
	defer server.Close()
	a, _ := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "fixture"})
	a.AddLocalTools([]localtool.Manifest{manifest})
	defer a.CloseMCP()
	id := createTestRun(t, a)
	s, err := a.Run(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !sawFacts || len(s.Plan.Tasks) != 1 || s.Plan.Tasks[0].Status != "verified" {
		t.Fatalf("planner feedback loop incomplete: facts=%v plan=%+v", sawFacts, s.Plan)
	}
}

func TestPlannerFilePaginationAndAtomicEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lines.txt")
	original := "first\nsecond\nthird\n"
	os.WriteFile(path, []byte(original), 0600)
	value, err := fileOperation("read", FileRequest{Path: path, StartLine: 2, LineCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if value.(map[string]any)["content"] != "second\n" {
		t.Fatal("wrong line page")
	}
	_, err = fileOperation("edit", FileRequest{Path: path, Edits: []FileEdit{{"first", "one"}, {"missing", "two"}}})
	data, _ := os.ReadFile(path)
	if err == nil || string(data) != original {
		t.Fatal("partial multi-edit committed")
	}
	value, err = fileOperation("edit", FileRequest{Path: path, ExpectedHash: promptHash(original), Edits: []FileEdit{{"first", "one"}, {"third", "three"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(value.(map[string]any)["diff"].(string), "+three") {
		t.Fatal("edit diff missing")
	}
	a, _ := newTestAgent(t, successProvider{})
	r := ToolResult{Stdout: "\x00\xff"}
	if err = a.archiveOutput(&r); err != nil {
		t.Fatal(err)
	}
	if len(r.Artifacts) != 1 || r.Stdout != "" {
		t.Fatal("small binary would be JSON-corrupted")
	}
	raw, err := artifactOutput(a.store.root, r, "stdout")
	if err != nil || raw != "\x00\xff" {
		t.Fatal("small binary changed")
	}
}
