package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trace/internal/localtool"
)

// Server-only, opt-in acceptance using the real configured provider throughout.
// Reference contracts inspected: Muteki 144df766, pentest/judgement.py (exact
// evidence snapshots); Harness 57190202, tool-bash/tests/integration.spec.ts
// (real process effects and session events). No reference runtime is imported.
// This test does not prescribe or claim same-request multi-tool batching.
func TestLiveProviderRuntimeMatrix(t *testing.T) {
	configPath := os.Getenv("TRACE_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("set TRACE_LIVE_CONFIG on the test server")
	}
	var config struct {
		Provider struct {
			URL    string `json:"url"`
			Model  string `json:"model"`
			APIKey string `json:"api_key"`
		} `json:"provider"`
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(raw, &config) != nil || config.Provider.APIKey == "" || config.Provider.Model == "" {
		t.Fatal("live provider configuration missing or invalid")
	}
	// Never copy the configuration, API key, or HTTP headers into fixture files.
	root := os.Getenv("TRACE_LIVE_MATRIX_OUTPUT")
	if root == "" {
		root, err = os.MkdirTemp("", "trace-live-matrix-")
	} else {
		root, err = filepath.Abs(root)
		if err == nil {
			err = os.MkdirAll(root, 0700)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately not t.TempDir: failed runs also retain raw state for audit.
	t.Logf("live matrix model=%s retained-output=%s", config.Provider.Model, root)
	const label = "中文结果：证据链 ✓ / café / 日本語 / 🧪"
	input, err := json.Marshal(map[string]any{"numbers": []int{13, 17, 12}, "label": label})
	if err != nil {
		t.Fatal(err)
	}
	inputHash := promptHash(string(input))
	const script = `import fcntl, hashlib, json, pathlib, sys
root = pathlib.Path(__file__).resolve().parent
raw = (root / '输入.json').read_bytes()
data = json.loads(raw)
digest = hashlib.sha256(raw).hexdigest()
if sys.argv[1:] == ['--sum']:
    result = json.dumps({'sum': sum(data['numbers']), 'sha256': digest})
    (root / 'sum.json').write_text(result)
    print(result)
else:
    request = json.loads(sys.stdin.readline())
    if request['operation'] == 'audit':
        assert request['sha256'] == digest, 'digest must come from observed source bytes'
        print(json.dumps({'ready': True, 'sum': sum(data['numbers']), 'sha256': digest, 'label': data['label']}, ensure_ascii=False))
    elif request['operation'] == 'recover':
        with (root / 'attempts.lock').open('w') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX)
            path = root / 'attempts.json'
            attempt = json.loads(path.read_text()) + 1 if path.exists() else 1
            path.write_text(json.dumps(attempt))
            print(json.dumps({'ready': True, 'label': data['label']}, ensure_ascii=False), flush=True)
            if attempt == 1:
                print('transient fixture failure; retry the same operation after recording failed evidence', file=sys.stderr)
                sys.exit(23)
    else:
        raise ValueError('unknown operation')
`
	for _, name := range []string{"short_exec", "mixed_unicode", "recoverable_failure"} {
		t.Run(name, func(t *testing.T) {
			dir, err := os.MkdirTemp(root, name+"-")
			if err != nil {
				t.Fatal(err)
			}
			inputPath, scriptPath := filepath.Join(dir, "输入.json"), filepath.Join(dir, "fixture.py")
			for path, content := range map[string][]byte{inputPath: input, scriptPath: []byte(script)} {
				if err := os.WriteFile(path, content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			store, err := NewStore(filepath.Join(dir, "state"))
			if err != nil {
				t.Fatal(err)
			}
			a, err := New(store, &OpenAIProvider{BaseURL: config.Provider.URL, Model: config.Provider.Model, APIKey: config.Provider.APIKey}, "")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { a.StopWorkers(context.Background()); a.CloseMCP() }()
			manifest := localtool.Manifest{
				Name: "live-runtime-matrix", Command: "python3", Args: []string{scriptPath}, Mode: localtool.ModeStdio,
				Description: "Read the actual fixture source. audit requires its observed sha256 and returns ready, sum, sha256 and Unicode label. recover deliberately exits 23 once despite ready=true stdout, then succeeds on retry; a nonzero exit is never success evidence.",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"operation":{"type":"string","enum":["audit","recover"]},"sha256":{"type":"string"}},"required":["operation"]}`),
			}
			if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
				t.Fatal(err)
			}
			// Shell quoting is for the Ubuntu acceptance server, including spaces
			// or apostrophes in an operator-selected output directory.
			command := "python3 '" + strings.ReplaceAll(scriptPath, "'", "'\"'\"'") + "' --sum"
			requirement := func(id, tool, pointer string, expected any) VerificationRequirement {
				return VerificationRequirement{ID: id, Description: "固定验收：" + id, Category: "goal", ToolID: tool, Predicate: EvidencePredicate{Kind: "json_equals", Pointer: pointer, Expected: expected}}
			}
			sumProof := map[string]any{"sum": 42, "sha256": inputHash}
			requirements := []VerificationRequirement{requirement("sum", "exec", "", sumProof)}
			wanted := []string{"exec", "evidence_check"}
			goal := fmt.Sprintf("完成短任务：使用exec真实执行 %s，读取输入文件并计算numbers总和和SHA256。不要用echo/printf伪造输出，也不要改输入或fixture脚本。用该执行返回的真实Evidence ID调用evidence_check验收sum，最后简短报告。", command)
			switch name {
			case "mixed_unicode":
				requirements = append(requirements,
					requirement("source", "read", "/content", string(input)),
					requirement("unicode-audit", manifest.StableID(), "", map[string]any{"ready": true, "sum": 42, "sha256": inputHash, "label": label}))
				wanted = append(wanted, "read", manifest.StableID())
				goal = fmt.Sprintf("完成中文文件的交叉核验。先用read读取 %s 的完整内容；再用exec真实执行 %s 计算数字总和和SHA256；使用已选中的live-runtime-matrix工具operation=audit，把实际观察到的sha256作为参数。所有输入和fixture脚本保持原样。分别使用read、exec、本地工具的真实Evidence ID，通过evidence_check验收source、sum、unicode-audit。每项只引用对应工具证据，中文、重音字符和emoji须保持原文。自行决定请求与工具调度，不创建Worker或计划，最终简短报告实际结果。", inputPath, command)
			case "recoverable_failure":
				requirements = []VerificationRequirement{requirement("recovered", manifest.StableID(), "", map[string]any{"ready": true, "label": label})}
				wanted = []string{manifest.StableID(), "evidence_check"}
				goal = "测试真实进程失败后的恢复。使用已选中的live-runtime-matrix工具operation=recover。第一次返回的JSON看似成功但进程exit_code=23；先用该失败调用的真实Evidence ID执行evidence_check验收recovered并观察非pass结果。之后根据工具错误提示重试operation=recover，用退出码为0的全新Evidence ID再次验收recovered。只对新成功证据报告完成，不修改文件、不通过exec代替、不创建Worker或计划。"
			}
			goal += " 所有验收通过后正常结束；失败stdout即使匹配判据也不是成功证据。不读取配置、环境变量或任务目录之外的文件。"
			s, err := a.Create(goal)
			if err != nil {
				t.Fatal(err)
			}
			id := s.Run.ID
			_, err = store.Update(id, func(s *Snapshot) error {
				s.Run.ToolSelection = &ToolSelection{IDs: []string{manifest.StableID()}}
				s.Run.Budget = Budget{MaxModelRequests: 20, MaxActions: 32, MaxSeconds: 240}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = a.SetVerificationRequirements(id, requirements); err != nil {
				t.Fatal(err)
			}
			t.Logf("run=%s case=%s retained=%s", id, name, dir)
			// Store already persists requests, responses, events, actions and CAS.
			// Save a discoverable index even when Run or assertions fail.
			defer func() {
				index, e := json.MarshalIndent(map[string]any{"run_id": id, "case": name, "model": config.Provider.Model, "state": filepath.Join(dir, "state"), "input_sha256": inputHash, "fixture_sha256": promptHash(script), "tool_ids": wanted}, "", "  ")
				if e == nil {
					e = os.WriteFile(filepath.Join(dir, "audit-index.json"), index, 0600)
				}
				if e != nil {
					t.Errorf("persist audit index: %v", e)
				}
				if report, e := store.Report(id); e != nil {
					t.Errorf("generate report: %v", e)
				} else if e = os.WriteFile(filepath.Join(dir, "report.md"), []byte(report), 0600); e != nil {
					t.Errorf("persist report: %v", e)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			started := time.Now()
			_, runErr := a.Run(ctx, id)
			// All assertions use a freshly loaded durable ledger, not a model reply.
			s, err = store.Load(id)
			if err != nil {
				t.Fatal(err)
			}
			if runErr != nil {
				t.Fatal("live Run failed; inspect retained state and provider response records")
			}
			if s.Run.Status != StatusCompleted || s.Result.Status != "success" || len(s.Requests) == 0 || len(s.Responses) == 0 {
				t.Fatalf("incomplete run: status=%s result=%s requests=%d responses=%d", s.Run.Status, s.Result.Status, len(s.Requests), len(s.Responses))
			}
			if valueHash(s.Requirements) != valueHash(requirements) || len(unmetRequirements(&s)) != 0 {
				t.Fatal("fixed requirements changed or remain unmet")
			}
			seen := map[string]bool{}
			failedIDs := []string{}
			for _, action := range s.Actions {
				if action.Result == nil || action.Status != "completed" {
					t.Fatalf("unsettled action: %s", action.CallID)
				}
				var ev *Evidence
				for i := range s.Evidence {
					if s.Evidence[i].CallID == action.CallID && s.Evidence[i].RequestID == action.RequestID {
						ev = &s.Evidence[i]
						break
					}
				}
				if ev == nil {
					t.Fatalf("action lacks raw evidence: %s", action.CallID)
				}
				verified := verifyEvidence(&s, ev) == "verified"
				if action.Result.ExitCode == 0 && action.Result.Error == "" {
					if !verified {
						t.Fatalf("successful action provenance invalid: %s", action.CallID)
					}
					seen[action.Name] = true
				} else if verified {
					t.Fatalf("failed action accepted as verified: %s", action.CallID)
				}
				if (action.Adapter == "exec" || action.Adapter == "local") && action.Result.WorkerPID <= 0 {
					t.Fatalf("process action lacks real worker PID: %s", action.CallID)
				}
				if name == "recoverable_failure" && action.Name == manifest.StableID() && action.Result.ExitCode == 23 {
					matched, e := predicateMatches(requirements[0].Predicate, action.Result.Stdout)
					status, _ := checkPredicate(&s, requirements[0].Predicate, []string{ev.ID}, manifest.StableID(), "", 0)
					if e != nil || !matched || status == "pass" {
						t.Fatal("failed-but-matching stdout negative control was not rejected")
					}
					failedIDs = append(failedIDs, ev.ID)
				}
			}
			for _, tool := range wanted {
				if !seen[tool] {
					t.Errorf("missing successful real tool invocation: %s", tool)
				}
			}
			for _, req := range requirements {
				check := latestCheck(&s, req.ID)
				if check == nil || check.Status != "pass" || !referencesCurrent(&s, check.References) {
					t.Fatalf("missing durable verified check: %s", req.ID)
				}
				status, reason := checkPredicate(&s, req.Predicate, check.EvidenceIDs, req.ToolID, req.ArgumentsHash, 0)
				if status != "pass" {
					t.Fatalf("independent predicate recheck %s: %s", req.ID, reason)
				}
			}
			if name == "recoverable_failure" {
				if len(failedIDs) != 1 {
					t.Fatalf("expected one actual exit-23 failure, got %d", len(failedIDs))
				}
				rejected := false
				failureIndex, successIndex, recoveryCalls := -1, len(s.Actions), 0
				for i, action := range s.Actions {
					if action.Result.EvidenceID == failedIDs[0] {
						failureIndex = i
					}
					if hasString(latestCheck(&s, "recovered").EvidenceIDs, action.Result.EvidenceID) {
						successIndex = min(successIndex, i)
					}
					var args struct{ Operation string }
					if action.Name == manifest.StableID() && json.Unmarshal([]byte(action.Arguments), &args) == nil && args.Operation == "recover" && (action.Result.ExitCode == 0 || action.Result.ExitCode == 23) {
						recoveryCalls++
					}
				}
				for _, check := range s.Checks {
					if hasString(check.EvidenceIDs, failedIDs[0]) {
						if check.Status == "pass" {
							t.Fatal("failed evidence contributed to a passing check")
						}
					}
				}
				// CheckEvidence rejects unverified references before appending a
				// VerificationCheck. Its failed builtin action is the durable proof.
				for i, action := range s.Actions {
					if action.Name != "evidence_check" || action.Result.ExitCode == 0 {
						continue
					}
					var args struct {
						RequirementID string   `json:"requirement_id"`
						EvidenceIDs   []string `json:"evidence_ids"`
					}
					if json.Unmarshal([]byte(action.Arguments), &args) == nil && args.RequirementID == "recovered" && hasString(args.EvidenceIDs, failedIDs[0]) && failureIndex >= 0 && failureIndex < i && i < successIndex {
						rejected = true
					}
				}
				attemptsRaw, e := os.ReadFile(filepath.Join(dir, "attempts.json"))
				var attempts int
				if e != nil || json.Unmarshal(attemptsRaw, &attempts) != nil || attempts < 2 || attempts != recoveryCalls || !rejected {
					t.Fatal("missing durable failed-check/retry process effects")
				}
				if hasString(s.Result.EvidenceIDs, failedIDs[0]) {
					t.Fatal("final result cites failed execution")
				}
			} else {
				// Independent process side effect rules out an echo of the
				// expected answer instead of actually running the source probe.
				receipt, e := os.ReadFile(filepath.Join(dir, "sum.json"))
				var result map[string]any
				if e != nil || json.Unmarshal(receipt, &result) != nil || valueHash(result) != valueHash(sumProof) {
					t.Fatal("source-computing process receipt missing or incorrect")
				}
			}
			for path, expected := range map[string]string{inputPath: inputHash, scriptPath: promptHash(script)} {
				content, e := os.ReadFile(path)
				if e != nil || promptHash(string(content)) != expected {
					t.Fatalf("fixture mutated: %s", path)
				}
			}
			t.Logf("verified case=%s requests=%d actions=%d tools=%d elapsed=%s", name, len(s.Requests), len(s.Actions), len(seen), time.Since(started))
		})
	}
}
