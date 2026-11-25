package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"trace/internal/agent"
)

// Server-only regression: a controller crash leaves a durable pending batch but
// no Web goroutine. Explicit pause/resume must not strand the only UI entrypoint.
func TestControllerRestartPendingBatchKeepsResumeReachable(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			testControllerRestartPendingBatch(t, backend == "sqlite")
		})
	}
}

func testControllerRestartPendingBatch(t *testing.T, sqlite bool) {
	node := testNodeBinary()
	if _, err := exec.LookPath(node); err != nil {
		t.Fatal("Node is required to render the production controls:", err)
	}
	var modelCalls atomic.Int32
	var observedUnknown atomic.Bool
	var pauseAfterObservation func() error
	provider := providerFunc(func(_ context.Context, request agent.ModelRequest) (agent.ModelResponse, error) {
		modelCalls.Add(1)
		for _, message := range request.Messages {
			var result agent.ToolResult
			if message.Role == "tool" && message.ToolCallID == "interrupted-call" && json.Unmarshal([]byte(message.Content), &result) == nil && result.OutcomeUncertain {
				observedUnknown.Store(true)
				// Receiving the original observation proves resume reached the
				// model. No cleanup receipt exists, so stop explicitly instead of
				// requiring the production completion gate to accept this fixture.
				if err := pauseAfterObservation(); err != nil {
					return agent.ModelResponse{}, err
				}
				return agent.ModelResponse{Content: "Recovered without replaying the interrupted tool."}, nil
			}
		}
		return agent.ModelResponse{}, fmt.Errorf("recovered request did not contain the original unknown tool outcome")
	})
	root := t.TempDir()
	oldStore, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	oldAgent, err := agent.New(oldStore, provider, "")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := oldAgent.Create("controller restart pending batch")
	if err != nil {
		t.Fatal(err)
	}
	id, requestID := snap.Run.ID, snap.Run.ID+"-0001"
	tool, ok := oldAgent.Registry().Lookup("exec")
	if !ok {
		t.Fatal("exec definition missing")
	}
	_, err = oldStore.Update(id, func(s *agent.Snapshot) error {
		s.Run.Status, s.Run.RequestSeq = agent.StatusWaitingTool, 1
		s.Requests = []agent.ModelRequest{{ID: requestID, Tools: []agent.ToolDefinition{tool.Definition}}}
		call := agent.ToolCall{ID: "interrupted-call", Name: "exec", Arguments: `{"command":"must-not-replay"}`}
		s.Messages = []agent.Message{{Role: "assistant", RequestID: requestID, ToolCalls: []agent.ToolCall{call}}}
		s.Actions = []agent.Action{{RequestID: requestID, CallID: call.ID, Name: call.Name, Adapter: "exec", Arguments: call.Arguments, Status: "started"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = oldStore.Close(); err != nil {
		t.Fatal(err)
	}
	if sqlite {
		if err = agent.MigrateStoreToSQLite(root); err != nil {
			t.Fatal(err)
		}
	}
	store, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a, err := agent.New(store, provider, "")
	if err != nil {
		t.Fatal(err)
	}
	pauseAfterObservation = func() error {
		_, err := a.Pause(id)
		return err
	}
	if err = a.RecoverExecResources(); err != nil {
		t.Fatal(err)
	}
	s, err := New(a, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	before, err := store.Load(id)
	if err != nil || len(before.Actions) != 1 || before.Actions[0].Status != "uncertain" || before.Run.Status != agent.StatusWaitingTool || modelCalls.Load() != 0 {
		t.Fatalf("restart changed execution semantics: status=%s actions=%+v model_calls=%d err=%v", before.Run.Status, before.Actions, modelCalls.Load(), err)
	}
	handler := s.Handler()
	for attempt := 0; attempt < 2; attempt++ {
		request(t, handler, "POST", "/api/runs/"+id+"/pause", map[string]any{}, 200)
		request(t, handler, "POST", "/api/runs/"+id+"/resume", map[string]any{}, 200)
		done := make(chan struct{})
		go func() { s.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			s.cancel()
			t.Fatal("explicit resume did not settle")
		}
		after, err := store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(after.Actions, before.Actions) {
			t.Fatal("resume changed or replayed the historical action")
		}
		t.Logf("resume %d: status=%s active=%v model_calls=%d requests=%d", attempt+1, after.Run.Status, s.active[id], modelCalls.Load(), len(after.Requests))
		if modelCalls.Load() > 0 {
			t.Fatal("first resume crossed the existing paused pending-action boundary")
		}
	}
	response := request(t, handler, "GET", "/api/runs/"+id, nil, 200)
	// Render both actual controls; do not duplicate their status predicates in Go.
	const render = `
import { renderPage } from './static/views.js';
import fs from 'node:fs';
import vm from 'node:vm';
let text = '';
for await (const chunk of process.stdin) text += chunk;
const snap = JSON.parse(text);
const state = {page:'history',historyDetail:true,snap,runs:[snap.run],selected:snap.run.id,tools:[],skills:[],config:{provider:{}}};
const detail = renderPage(state).match(/<button\b[^>]*data-action="resume"[^>]*>/)?.[0];
const archive = renderPage({...state,historyDetail:false}).match(/<button\b[^>]*data-action="archive-run"[^>]*>/)?.[0];
const guarded=[true,undefined].every(active=>{
  const run={...snap.run};if(active===undefined)delete run.execution_active;else run.execution_active=active;
  const button=renderPage({...state,snap:{...snap,run}}).match(/<button\b[^>]*data-action="resume"[^>]*>/)?.[0];
  return !!button&&/\bdisabled\b/.test(button);
});
const idleHint=renderPage(state).includes('当前未在工作台运行');
const source = fs.readFileSync('./static/app.js','utf8');
let tick,refreshes=0;
const pollState={historyDetail:true,selected:snap.run.id,runLoading:false,workspaceReadAt:0,snap:{run:{...snap.run,execution_active:true}}};
const context=vm.createContext({document:{hidden:false},state:pollState,source:{readyState:1},EventSource:{OPEN:1},runLoads:new Map(),uiNow:()=>6000,refreshRun:()=>{refreshes++;return Promise.resolve()},refreshList:()=>Promise.resolve(),setInterval:fn=>{tick=fn}});
vm.runInContext(source.slice(source.lastIndexOf('setInterval(()=>{if(document.hidden)return;')),context);
tick();const releasedOwnerRefresh=refreshes===1;
pollState.snap.run.execution_active=false;tick();const idleDoesNotPoll=refreshes===1;
process.stdout.write(JSON.stringify({detail:!!detail&&!/\bdisabled\b/.test(detail),archive:!!archive&&!/\bdisabled\b/.test(archive),guarded,idleHint,releasedOwnerRefresh,idleDoesNotPoll}));
`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "--input-type=module", "--eval", render)
	cmd.Stdin = bytes.NewReader(response.Body.Bytes())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render controls: %v: %s", err, output)
	}
	var controls struct{ Detail, Archive, Guarded, IdleHint, ReleasedOwnerRefresh, IdleDoesNotPoll bool }
	if err = json.Unmarshal(output, &controls); err != nil {
		t.Fatalf("render output: %v: %s", err, output)
	}
	if !controls.Detail || !controls.Archive {
		t.Fatalf("idle pending batch has no reachable resume: detail=%v archive=%v model_calls=%d; Pause/Resume returned waiting_tool without an executor", controls.Detail, controls.Archive, modelCalls.Load())
	}
	if !controls.ReleasedOwnerRefresh || !controls.IdleDoesNotPoll {
		t.Fatalf("execution ownership refresh with open SSE: %+v", controls)
	}
	if !controls.Guarded || !controls.IdleHint {
		t.Fatalf("active/unknown ownership guards or idle recovery notice missing: %+v", controls)
	}
	// This is the same route the now-reachable production resume button uses.
	request(t, handler, "POST", "/api/runs/"+id+"/resume", map[string]any{}, 200)
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		s.cancel()
		t.Fatal("reachable resume did not stop after observing the original pending batch")
	}
	after, err := store.Load(id)
	if err != nil || after.Run.Status != agent.StatusPaused || after.Run.PauseReason != "operator_pause" || modelCalls.Load() != 1 || !observedUnknown.Load() || !reflect.DeepEqual(after.Actions, before.Actions) {
		t.Fatalf("recovery did not deliver the original unknown outcome without replay: status=%s pause_reason=%s model_calls=%d observed_unknown=%v actions_unchanged=%v err=%v", after.Run.Status, after.Run.PauseReason, modelCalls.Load(), observedUnknown.Load(), reflect.DeepEqual(after.Actions, before.Actions), err)
	}
}

func TestExecutionOwnershipProjectionIsReadOnlyAndConsistent(t *testing.T) {
	root := t.TempDir()
	store, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	a, err := agent.New(store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(a, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	snap, err := a.Create("ownership projection")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	before, err := os.ReadFile(filepath.Join(root, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"/api/runs", "/api/runs/" + id, "/api/runs/" + id + "?view=workspace", "/api/runs/" + id + "?view=workspace&activity=true", "/api/runs/" + id + "/messages"}
	for _, active := range []bool{false, true} {
		s.mu.Lock()
		if active {
			s.active[id] = true
		} else {
			delete(s.active, id)
		}
		s.mu.Unlock()
		for _, path := range paths {
			response := request(t, s.Handler(), "GET", path, nil, 200)
			var run map[string]json.RawMessage
			if path == "/api/runs" {
				var runs []map[string]json.RawMessage
				if err = json.Unmarshal(response.Body.Bytes(), &runs); err != nil || len(runs) != 1 {
					t.Fatalf("run list: %s: %v", response.Body.Bytes(), err)
				}
				run = runs[0]
			} else {
				var value struct{ Run map[string]json.RawMessage }
				if err = json.Unmarshal(response.Body.Bytes(), &value); err != nil {
					t.Fatal(err)
				}
				run = value.Run
			}
			var got bool
			if err = json.Unmarshal(run["execution_active"], &got); err != nil || got != active {
				t.Errorf("%s ownership=%s, want %v: %v", path, run["execution_active"], active, err)
			}
		}
	}
	s.mu.Lock()
	delete(s.active, id)
	s.mu.Unlock()
	after, err := os.ReadFile(filepath.Join(root, id+".json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("ownership GET changed persisted state", err)
	}
	if _, err := os.Stat(filepath.Join(root, id+".lock")); !os.IsNotExist(err) {
		t.Fatal("ownership GET created or probed the execution lock", err)
	}
	child, err := a.Create("Agent-owned child")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(child.Run.ID, func(v *agent.Snapshot) error {
		v.Run.ParentRunID, v.Run.ParentTaskID, v.Run.Status = id, "task", agent.StatusWaitingTool
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, s.Handler(), "GET", "/api/runs/"+child.Run.ID, nil, 200)
	var value struct{ Run map[string]json.RawMessage }
	if err = json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if _, known := value.Run["execution_active"]; known {
		t.Fatal("absence from Web map claimed an Agent-owned child was idle")
	}
	// A process outside Web can own the original lock. The display field must
	// never bypass admission or persist this rejected attempt onto the Run.
	unlock, err := store.AcquireRunLock(id)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	request(t, s.Handler(), "POST", "/api/runs/"+id+"/resume", map[string]any{}, 200)
	s.wg.Wait()
	after, err = os.ReadFile(filepath.Join(root, id+".json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("Web resume bypassed an external execution lock or changed its ledger", err)
	}
}

// testNodeBinary is the Node used by browser/render fixtures (TRACE_NODE or PATH).
func testNodeBinary() string {
	if node := os.Getenv("TRACE_NODE"); node != "" {
		return node
	}
	return "node"
}
