package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"trace/internal/localtool"
)

// Opt-in server acceptance: the real configured provider must advance a
// filesystem audit protocol over many requests, then cite its final evidence.
// The independent fixture validates order and source hashes, not model prose.
func TestLiveProviderLongTask(t *testing.T) {
	configPath := os.Getenv("TRACE_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("set TRACE_LIVE_CONFIG on the test server")
	}
	// Explicit wire tags keep the secret exclusively in server memory.
	var config struct {
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
	if err = json.Unmarshal(b, &config); err != nil {
		t.Fatal(err)
	}
	if config.Provider.APIKey == "" || config.Provider.Model == "" {
		t.Fatal("live provider configuration missing")
	}
	root := os.Getenv("TRACE_LIVE_OUTPUT")
	if root == "" {
		root = t.TempDir()
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	root, err = os.MkdirTemp(root, "live-long-")
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "audit.py")
	const stages = 18
	fixture := `import sys,json,pathlib,hashlib,time,fcntl
root=pathlib.Path(__file__).parent
request=json.loads(sys.stdin.readline())
with (root/'audit.lock').open('w') as lock:
 fcntl.flock(lock,fcntl.LOCK_EX)
 state=root/'checkpoints.json'
 done=json.loads(state.read_text()) if state.exists() else []
 action=request['action']
 if action=='next':
  if request['index']!=len(done)+1 or len(done)>=18: raise ValueError('wrong checkpoint order')
  path=root/('module-%02d.js'%request['index'])
  raw=path.read_bytes()
  time.sleep(20)
  item={'index':request['index'],'sha256':hashlib.sha256(raw).hexdigest(),'bytes':len(raw),'has_dynamic_code':b'eval(' in raw}
  done.append(item);state.write_text(json.dumps(done));print(json.dumps({'checkpoint':item,'next_index':len(done)+1,'remaining':18-len(done)}))
 elif action=='verify':
  valid=len(done)==18 and all(item['index']==i and item['sha256']==hashlib.sha256((root/('module-%02d.js'%i)).read_bytes()).hexdigest() for i,item in enumerate(done,1))
  print(json.dumps({'complete':valid,'checked':len(done),'dynamic_code_modules':[x['index'] for x in done if x['has_dynamic_code']]}))
 else: raise ValueError('unknown action')
`
	if err = os.WriteFile(script, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	expectedModules := map[int]string{}
	for i := 1; i <= stages; i++ {
		body := fmt.Sprintf("export function step%d(input){return input+%d;}\n", i, i)
		if i%6 == 0 {
			body += "export const dynamic=input=>eval(input);\n"
		}
		if err = os.WriteFile(filepath.Join(root, fmt.Sprintf("module-%02d.js", i)), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		expectedModules[i] = promptHash(body)
	}
	store, err := NewStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(store, &OpenAIProvider{BaseURL: config.Provider.URL, Model: config.Provider.Model, APIKey: config.Provider.APIKey}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.StopWorkers(context.Background()); a.CloseMCP() }()
	manifest := localtool.Manifest{Name: "staged-source-audit", Description: "Audit one real source module at a time. action=next requires index 1..18 in exact order; each returns its hash and dynamic code observation after 20 seconds. action=verify rehashes all modules and returns complete only for all 18 correct checkpoints.", Command: "python3", Args: []string{script}, Mode: localtool.ModeStdio, InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["next","verify"]},"index":{"type":"integer"}},"required":["action"]}`)}
	if err = a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
		t.Fatal(err)
	}
	goal := "执行完整18个模块的分阶段源码审计。使用已选中的 staged-source-audit，从index=1开始逐次调用next；必须等上一个工具结果返回再决定下一个index，每次请求只调用一个next，不并发、不用exec代替、不改fixture。所有18个完成后调用verify，并用其真实Evidence ID调用evidence_check验收all-modules。最终报告动态代码模块及完整任务证据。每个模块耗时20秒是正常工作，持续完成全部任务。"
	s, err := a.Create(goal)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Update(s.Run.ID, func(s *Snapshot) error {
		s.Run.ToolSelection = &ToolSelection{IDs: []string{manifest.StableID()}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.SetVerificationRequirements(s.Run.ID, []VerificationRequirement{{ID: "all-modules", Description: "independent source hash verification proves every ordered module was audited", Category: "goal", ToolID: manifest.StableID(), Predicate: EvidencePredicate{Kind: "json_equals", Pointer: "/complete", Expected: true}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("live run=%s model=%s root=%s", s.Run.ID, config.Provider.Model, root)
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	s, err = a.Run(ctx, s.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s.Run.Status != StatusCompleted || s.Result.Status != "success" || len(s.Requests) < stages+1 || time.Since(started) < stages*20*time.Second {
		t.Fatalf("incomplete live task: status=%s result=%s requests=%d elapsed=%s", s.Run.Status, s.Result.Status, len(s.Requests), time.Since(started))
	}
	var checkpoints []struct {
		Index          int    `json:"index"`
		SHA256         string `json:"sha256"`
		Bytes          int    `json:"bytes"`
		HasDynamicCode bool   `json:"has_dynamic_code"`
	}
	b, err = os.ReadFile(filepath.Join(root, "checkpoints.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &checkpoints); err != nil || len(checkpoints) != stages {
		t.Fatal("independent checkpoint ledger incomplete")
	}
	for i, checkpoint := range checkpoints {
		index := i + 1
		source, err := os.ReadFile(filepath.Join(root, fmt.Sprintf("module-%02d.js", index)))
		if err != nil || checkpoint.Index != index || checkpoint.SHA256 != expectedModules[index] || promptHash(string(source)) != expectedModules[index] || checkpoint.Bytes != len(source) || checkpoint.HasDynamicCode != (index%6 == 0) {
			t.Fatalf("checkpoint %d does not match the original source fixture", index)
		}
	}
	if source, err := os.ReadFile(script); err != nil || string(source) != fixture {
		t.Fatal("audit fixture script changed during execution")
	}
	for _, e := range s.Evidence {
		if e.Type != "tool_execution" {
			continue
		}
		var result ToolResult
		if err := json.Unmarshal([]byte(e.Content), &result); err != nil {
			t.Fatalf("invalid execution evidence %s: %v", e.ID, err)
		}
		success := result.ExitCode == 0 && result.Error == "" && !result.Interrupted && !result.MCPIsError && !result.OutcomeUncertain && !result.WorkerOutcomeUncertain
		verification := verifyEvidence(&s, &e)
		if success && verification != "verified" {
			t.Fatalf("successful execution evidence lost provenance: %s (%s)", e.ID, verification)
		}
		if !success {
			if verification == "verified" {
				t.Fatalf("failed execution accepted as verified: %s", e.ID)
			}
			// Round 1 completed all 18 checkpoints but ev-63 exited 1.
			// Retain that failed attempt; recovery does not make it success.
			t.Logf("retained failed execution evidence=%s exit=%d verification=%s", e.ID, result.ExitCode, verification)
		}
	}
	check := latestCheck(&s, "all-modules")
	if check == nil || check.Status != "pass" || len(check.EvidenceIDs) == 0 || !referencesCurrent(&s, check.References) {
		t.Fatal("all-modules has no current passing evidence check")
	}
	if len(s.Result.EvidenceIDs) == 0 {
		t.Fatal("final result does not cite execution evidence")
	}
	for _, id := range append(append([]string{}, check.EvidenceIDs...), s.Result.EvidenceIDs...) {
		if _, ok := verifiedOutput(&s, id); !ok {
			t.Fatalf("accepted goal or final result cites unverified evidence: %s", id)
		}
	}
	if _, err = store.Report(s.Run.ID); err != nil {
		t.Fatal(err)
	}
	t.Logf("live task complete elapsed=%s requests=%d actions=%d result=%s", time.Since(started), len(s.Requests), len(s.Actions), s.Result.Status)
}
