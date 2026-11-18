package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"trace/internal/localtool"
)

// The gate controls only the pause boundary. Every model decision is made by
// the configured external provider; tool effects are independently counted.
type liveResumeProvider struct {
	Provider
	mu      sync.Mutex
	gated   bool
	entered chan struct{}
}

func (p *liveResumeProvider) Complete(ctx context.Context, req ModelRequest) (ModelResponse, error) {
	for _, m := range req.Messages {
		var result ToolResult
		var out struct {
			Step int `json:"step"`
		}
		if m.Role != "tool" || json.Unmarshal([]byte(m.Content), &result) != nil || json.Unmarshal([]byte(result.Stdout), &out) != nil || out.Step != 1 {
			continue
		}
		p.mu.Lock()
		gate := !p.gated
		p.gated = true
		p.mu.Unlock()
		if gate {
			close(p.entered)
			<-ctx.Done()
			return ModelResponse{}, ctx.Err()
		}
	}
	return p.Provider.Complete(ctx, req)
}

func TestLiveProviderWorkerPauseResume(t *testing.T) {
	configPath := os.Getenv("TRACE_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("set TRACE_LIVE_CONFIG on the test server")
	}
	var c struct {
		Provider struct {
			URL    string `json:"url"`
			Model  string `json:"model"`
			APIKey string `json:"api_key"`
		} `json:"provider"`
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &c); err != nil || c.Provider.APIKey == "" {
		t.Fatal("live provider configuration missing")
	}
	root := os.Getenv("TRACE_LIVE_WORKER_OUTPUT")
	if root == "" {
		root = t.TempDir()
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	root, err = os.MkdirTemp(root, "live-worker-resume-")
	if err != nil {
		t.Fatal(err)
	}
	fixture := `import sys,json,pathlib,fcntl
root=pathlib.Path(__file__).parent
q=json.loads(sys.stdin.readline())
with (root/'lock').open('w') as lock:
 fcntl.flock(lock,fcntl.LOCK_EX)
 p=root/'steps.json';done=json.loads(p.read_text()) if p.exists() else []
 if q['action']=='next':
  assert q['index']==len(done)+1 and len(done)<3
  done.append(q['index']);p.write_text(json.dumps(done));print(json.dumps({'step':q['index']}))
 elif q['action']=='verify':
  assert done==[1,2,3];print('worker-resume-proof')
 elif q['action']=='dependent':
  assert done==[1,2,3];print('dependent-resume-proof')
 else: raise ValueError('unknown action')
`
	script := filepath.Join(root, "steps.py")
	if err = os.WriteFile(script, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	p := &liveResumeProvider{Provider: &OpenAIProvider{BaseURL: c.Provider.URL, Model: c.Provider.Model, APIKey: c.Provider.APIKey}, entered: make(chan struct{})}
	a, err := New(store, p, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.StopWorkers(context.Background()); a.CloseMCP() }()
	m := localtool.Manifest{Name: "resume-audit", Description: "Ordered checkpoint fixture: next index=1..3 exactly once each; verify returns worker-resume-proof only after all three; dependent returns dependent-resume-proof only after all three.", Command: "python3", Args: []string{script}, Mode: localtool.ModeStdio, InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["next","verify","dependent"]},"index":{"type":"integer"}},"required":["action"]}`)}
	if err = a.AddLocalTools([]localtool.Manifest{m}); err != nil {
		t.Fatal(err)
	}
	s, err := a.Create("完成既有计划的work与next两个Worker任务。work已派发且可能暂停，等待原Worker恢复，不重建计划、不retry、不执行其checkpoint。用worker_wait等待work完成，然后worker_dispatch派发next并等待完成。全部任务verified后，才使用exec执行printf 'parent-resume-proof'，最后正常结束。不要请求用户输入。")
	if err != nil {
		t.Fatal(err)
	}
	id := s.Run.ID
	_, err = a.SetPlan(id, []PlanTask{{ID: "work", Goal: "使用resume-audit工具，顺序next index=1、2、3，每次等结果再下一次，已完成的步骤不可重复。最后调用verify。报告真实输出证据并结束。只执行这个任务，不新增计划。", SuccessContains: "worker-resume-proof"}, {ID: "next", Goal: "只调用resume-audit action=dependent，报告实际输出证据并结束，不创建计划。", Dependencies: []string{"work"}, SuccessContains: "dependent-resume-proof"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusRunning
		s.Run.SuccessContains = "parent-resume-proof"
		s.Run.ToolSelection = &ToolSelection{IDs: []string{m.StableID()}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	w, err := a.DispatchTask(ctx, id, "work")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("live parent=%s child=%s model=%s root=%s", id, w.RunID, c.Provider.Model, root)
	select {
	case <-p.entered:
	case <-ctx.Done():
		t.Fatal("first live checkpoint not reached")
	}
	if _, err = a.Pause(id); err != nil {
		t.Fatal(err)
	}
	if _, err = waitAllTestWorkers(a, ctx, id, 10); err != nil {
		t.Fatal(err)
	}
	child, err := store.Load(w.RunID)
	if err != nil || child.Run.Status != StatusPaused {
		t.Fatalf("pause boundary: run=%+v actions=%d error=%v", child.Run, len(child.Actions), err)
	}
	// Tool discovery and failed probes do not advance checkpoint effects.
	// Preserve the exact successful first action instead of assuming it is
	// the child's only action before pause.
	var firstAction Action
	for _, action := range child.Actions {
		if action.LocalName != m.Name || action.Result == nil {
			continue
		}
		var args struct {
			Action string `json:"action"`
			Index  int    `json:"index"`
		}
		if json.Unmarshal([]byte(action.Arguments), &args) == nil && args.Action == "next" && args.Index == 1 {
			if _, ok := verifiedOutput(&child, action.Result.EvidenceID); ok {
				firstAction = action
			}
		}
	}
	b, err = os.ReadFile(filepath.Join(root, "steps.json"))
	var pausedSteps []int
	if err != nil || json.Unmarshal(b, &pausedSteps) != nil || len(pausedSteps) != 1 || pausedSteps[0] != 1 || firstAction.CallID == "" {
		t.Fatal("pause boundary lacks exactly one evidenced checkpoint effect")
	}
	s, err = a.Run(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Run.Status != StatusCompleted || s.Result.Status != "success" || len(s.Workers) != 2 {
		t.Fatalf("parent incomplete: run=%+v result=%+v plan=%+v", s.Run, s.Result, s.Plan)
	}
	for _, task := range s.Plan.Tasks {
		if task.Status != "verified" {
			t.Fatalf("task not verified: %+v", task)
		}
	}
	if s.Workers[0].ID != w.ID || s.Workers[0].RunID != w.RunID {
		t.Fatal("worker identity replaced")
	}
	child, err = store.Load(w.RunID)
	if err != nil || child.Result.Status != "success" {
		t.Fatalf("child incomplete: %v %+v", err, child.Result)
	}
	b, err = os.ReadFile(filepath.Join(root, "steps.json"))
	if err != nil || strings.TrimSpace(string(b)) != "[1, 2, 3]" {
		t.Fatalf("checkpoint effects replayed or missing: %q %v", b, err)
	}
	nexts := map[int]int{}
	firstPreserved := false
	for _, action := range child.Actions {
		if action.CallID == firstAction.CallID && action.RequestID == firstAction.RequestID {
			firstPreserved = valueHash(action) == valueHash(firstAction)
		}
		if action.LocalName == m.Name {
			var args struct {
				Action string `json:"action"`
				Index  int    `json:"index"`
			}
			if json.Unmarshal([]byte(action.Arguments), &args) != nil || action.Result == nil {
				t.Fatal("checkpoint invocation lacks durable arguments or result")
			}
			if args.Action == "next" && action.Result.ExitCode == 0 && action.Result.Error == "" {
				if _, ok := verifiedOutput(&child, action.Result.EvidenceID); !ok {
					t.Fatalf("checkpoint %d lost provenance", args.Index)
				}
				nexts[args.Index]++
			}
		}
	}
	if len(nexts) != 3 || nexts[1] != 1 || nexts[2] != 1 || nexts[3] != 1 || !firstPreserved {
		t.Fatalf("original successful checkpoint actions replayed or replaced: %v first_preserved=%t", nexts, firstPreserved)
	}
	if source, err := os.ReadFile(script); err != nil || string(source) != fixture {
		t.Fatal("resume fixture script changed during execution")
	}
	t.Logf("live resume success parent=%s original-child=%s tasks=%d child-actions=%d checkpoints=%s", id, w.RunID, len(s.Plan.Tasks), len(child.Actions), fmt.Sprint(string(b)))
}
