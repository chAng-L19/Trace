package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Copy this single opt-in test unchanged into both acceptance source trees.
// It uses only pre-P1 APIs and the real pi loop. A fixture pass is one observed
// sample, not evidence of general model quality or a provider invoice.
func TestLiveP1GeneralTaskMatrix(t *testing.T) {
	configPath := os.Getenv("TRACE_LIVE_CONFIG")
	root := os.Getenv("TRACE_LIVE_P1_OUTPUT")
	if configPath == "" || root == "" {
		t.Skip("set TRACE_LIVE_CONFIG and TRACE_LIVE_P1_OUTPUT on the designated server")
	}
	type providerConfig struct {
		URL               string                       `json:"url"`
		APIKey            string                       `json:"api_key"`
		Model             string                       `json:"model"`
		ImageInput        bool                         `json:"image_input"`
		ReasoningEfforts  []string                     `json:"reasoning_efforts"`
		ModelCapabilities map[string]ModelCapabilities `json:"model_capabilities"`
	}
	var config struct {
		Provider   providerConfig            `json:"provider"`
		Providers  map[string]providerConfig `json:"providers"`
		RoleModels RoleModels                `json:"role_models"`
		Context    ContextConfig             `json:"context"`
		Costs      CostConfig                `json:"costs"`
	}
	raw, err := os.ReadFile(configPath)
	if err != nil || json.Unmarshal(raw, &config) != nil || config.Provider.URL == "" || config.Provider.Model == "" || config.Provider.APIKey == "" {
		t.Fatal("live configuration missing or invalid")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	root, err = os.MkdirTemp(root, "general-")
	if err != nil {
		t.Fatal(err)
	}
	type task struct {
		name, goal, verify string
		inputs             map[string]string
		mutable            string
	}
	tasks := []task{
		{"research", "阅读 current.json 和 historical.json，以 current.json 的明确版本为当前依据，输出 report.md：中文简述组件、版本、重试次数与超时，引用 source_id；另输出 facts.json，键为 component/version/retry_attempts/timeout_ms/source_id。历史版本只作对照，不混入当前事实。", `
facts=json.loads((root/'facts.json').read_text())
assert facts==json.loads((root/'current.json').read_text())
report=(root/'report.md').read_text()
assert all(str(x) in report for x in ['Meridian','2.4',3,800,'guide-v2.4'])
`, map[string]string{
			"current.json":    "{\"component\":\"Meridian\",\"version\":\"2.4\",\"retry_attempts\":3,\"timeout_ms\":800,\"source_id\":\"guide-v2.4\"}\n",
			"historical.json": "{\"component\":\"Meridian\",\"version\":\"1.9\",\"retry_attempts\":7,\"timeout_ms\":1200,\"source_id\":\"guide-v1.9\"}\n",
		}, ""},
		{"code", "修复 subject.py 的 chunk 函数：按 n 分组保留全部元素，最后不足 n 的组也保留，空输入返回空列表，n<=0 应抛 ValueError。只修改 subject.py；执行真实测试并保留 review.md 说明根因、修改和验证。", `
import importlib.util
spec=importlib.util.spec_from_file_location('subject',root/'subject.py')
module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
for n in [1,2,3,5,10]:
 for length in [0,1,2,5,11]:
  data=list(range(length))
  assert module.chunk(data,n)==[data[i:i+n] for i in range(0,length,n)]
for n in [0,-1]:
 try: module.chunk([1],n)
 except ValueError: pass
 else: raise AssertionError('nonpositive n accepted')
assert len((root/'review.md').read_text().strip())>20
`, map[string]string{"subject.py": "def chunk(items, n):\n    return [items[i:i+n] for i in range(0, len(items)-n, n)]\n"}, "subject.py"},
		{"file", "读取 sales.csv，按 item 汇总 quantity*unit_price，负数quantity为退货；金额须保留两位小数的字符串。生成 totals.json，精确形状 {\"items\":{\"A\":\"...\",\"B\":\"...\"},\"grand_total\":\"...\"}；并写 summary.md 说明计算方式。用实际文件执行验证。", `
result=json.loads((root/'totals.json').read_text())
assert result=={'items':{'A':'17.00','B':'4.50'},'grand_total':'21.50'}
assert len((root/'summary.md').read_text().strip())>20
`, map[string]string{"sales.csv": "item,quantity,unit_price\nA,3,4.50\nB,2,2.25\nA,1,8.00\nA,-1,4.50\n"}, ""},
	}
	for _, tc := range tasks {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(root, tc.name)
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			for name, body := range tc.inputs {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			verifier := "import json,pathlib\nroot=pathlib.Path(__file__).resolve().parent\n" + tc.verify + "\nprint(json.dumps({'case':" + fmt.Sprintf("%q", tc.name) + ",'verified':True},sort_keys=True))\n"
			verifierPath := filepath.Join(dir, "verify.py")
			if err := os.WriteFile(verifierPath, []byte(verifier), 0600); err != nil {
				t.Fatal(err)
			}
			store, err := NewStore(filepath.Join(dir, "state"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			makeProvider := func(p providerConfig) *OpenAIProvider {
				return &OpenAIProvider{BaseURL: p.URL, APIKey: p.APIKey, Model: p.Model, ImageInput: p.ImageInput, ReasoningEfforts: p.ReasoningEfforts, ModelCapabilities: p.ModelCapabilities}
			}
			provider := makeProvider(config.Provider)
			a, err := New(store, provider, "")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { a.StopWorkers(context.Background()); a.CloseMCP() }()
			providers := map[string]*OpenAIProvider{}
			for name, p := range config.Providers {
				providers[name] = makeProvider(p)
			}
			if err := a.ConfigureModels(providers, config.RoleModels); err != nil {
				t.Fatal("invalid configured model route")
			}
			if err := a.ConfigureContext(config.Context); err != nil {
				t.Fatal("invalid context configuration")
			}
			if err := a.ConfigureCosts(config.Costs); err != nil {
				t.Fatal("invalid live pricing configuration")
			}
			command := "python3 '" + strings.ReplaceAll(verifierPath, "'", "'\"'\"'") + "'"
			goal := tc.goal + " 工作目录为 " + dir + "。不要修改输入文件（代码任务仅 subject.py 可修改）或 verify.py，不创建Worker/计划。完成后使用exec运行 " + command + "，根据真实返回的Evidence ID调用evidence_check验收 deliverable，然后输出来源绑定的中英双语总结报告。只访问本任务目录，不读取配置或环境变量。"
			snap, err := a.Create(goal)
			if err != nil {
				t.Fatal(err)
			}
			id := snap.Run.ID
			budget := Budget{MaxModelRequests: 24, MaxActions: 48}
			_, err = store.Update(id, func(s *Snapshot) error { s.Run.Budget = budget; s.Run.ToolSelection = &ToolSelection{}; return nil })
			if err != nil {
				t.Fatal(err)
			}
			requirements := []VerificationRequirement{{ID: "deliverable", Description: "Original verifier executed successfully against actual artifact bytes", Category: "goal", ToolID: "exec", Predicate: EvidencePredicate{Kind: "json_equals", Pointer: "", Expected: map[string]any{"case": tc.name, "verified": true}}}}
			if _, err = a.SetVerificationRequirements(id, requirements); err != nil {
				t.Fatal(err)
			}
			started := time.Now()
			defer func() {
				s, loadErr := store.Load(id)
				if loadErr != nil {
					t.Errorf("read retained state: %v", loadErr)
					return
				}
				report, usageErr := store.UsageReport(id, config.Costs)
				if usageErr != nil {
					t.Errorf("usage projection failed")
					return
				}
				// Use JSON rather than new Go fields so the same runner compiles on baseline.
				body, _ := json.Marshal(s.Responses)
				var responseMetadata []map[string]any
				json.Unmarshal(body, &responseMetadata)
				measurements := []map[string]any{}
				for _, record := range responseMetadata {
					response, _ := record["response"].(map[string]any)
					measurements = append(measurements, map[string]any{"request_id": record["request_id"], "reported_model": response["reported_model"], "provider_response_id": response["provider_response_id"], "diagnostics": response["diagnostics"], "reported_cost": response["reported_cost"]})
				}
				index := map[string]any{"case": tc.name, "run_id": id, "model_selection": s.Run.ModelSelection, "context": s.Run.Context, "fixture_sha256": valueHash(tc.inputs), "verifier_sha256": promptHash(verifier), "budget": budget, "elapsed_ms": time.Since(started).Milliseconds(), "status": s.Run.Status, "result_status": s.Result.Status, "requirements_unmet": unmetRequirements(&s), "usage": report, "responses": measurements, "quality_pass": !t.Failed(), "scope": "single observed sample; absent timings/cost remain unknown; not a provider invoice"}
				encoded, _ := json.MarshalIndent(index, "", "  ")
				if err := os.WriteFile(filepath.Join(dir, "measurement.json"), encoded, 0600); err != nil {
					t.Error(err)
				}
				t.Logf("retained case=%s run=%s output=%s requests=%d actions=%d", tc.name, id, dir, len(s.Requests), len(s.Actions))
			}()
			_, runErr := a.Run(context.Background(), id)
			snap, err = store.Load(id)
			if err != nil || runErr != nil || snap.Run.Status != StatusCompleted || snap.Result.Status != "success" || len(unmetRequirements(&snap)) != 0 {
				t.Fatal("live task did not satisfy frozen requirements; inspect retained state")
			}
			for name, body := range tc.inputs {
				if name == tc.mutable {
					continue
				}
				actual, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || string(actual) != body {
					t.Fatalf("immutable input changed: %s", name)
				}
			}
			actualVerifier, err := os.ReadFile(verifierPath)
			if err != nil || string(actualVerifier) != verifier {
				t.Fatal("verifier changed")
			}
			check := latestCheck(&snap, "deliverable")
			if check == nil || check.Status != "pass" || !referencesCurrent(&snap, check.References) {
				t.Fatal("fresh source check missing")
			}
			found := false
			// A harmless task-directory prefix preserves the same original verifier.
			// No trailing command or replacement stdout is accepted as provenance.
			quotedDir := "'" + strings.ReplaceAll(dir, "'", "'\"'\"'") + "'"
			for _, action := range snap.Actions {
				var args struct {
					Command string `json:"command"`
				}
				if action.Name != "exec" || json.Unmarshal([]byte(action.Arguments), &args) != nil {
					continue
				}
				originalCommand := args.Command == command || args.Command == "cd "+quotedDir+" && "+command
				if !strings.ContainsAny(dir, " \t\n'\"\\;$`&|<>()*?[]{}!#~") && args.Command == "cd "+dir+" && "+command {
					originalCommand = true
				}
				if originalCommand && action.Result != nil && action.Result.ExitCode == 0 && hasString(check.EvidenceIDs, action.Result.EvidenceID) {
					found = true
				}
			}
			if !found {
				t.Fatal("passing check was not tied to original verifier command")
			}
			// Independent process verification is outside model feedback and not a model request.
			output, err := exec.Command("python3", verifierPath).CombinedOutput()
			if err != nil {
				t.Fatalf("independent artifact check failed: %s", output)
			}
		})
	}
}
