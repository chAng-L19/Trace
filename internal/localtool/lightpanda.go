package localtool

import (
	"context"
	_ "embed"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Installed prerequisites only: no binary execution, driver startup or download.
func discoverLightpanda(ctx context.Context, python string) (string, DependencyStatus) {
	status := DependencyStatus{Name: "Lightpanda", Detail: "Python 3 runtime missing"}
	if python == "" {
		return "", status
	}
	name := "lightpanda"
	if configured := os.Getenv("LIGHTPANDA_BINARY_PATH"); configured != "" {
		path, err := filepath.Abs(configured)
		if err != nil {
			status.Detail = err.Error()
			return "", status
		}
		name = path
	}
	path, err := exec.LookPath(name)
	if err != nil {
		status.Detail = err.Error()
		return "", status
	}
	path, err = filepath.Abs(path)
	if err != nil {
		status.Detail = err.Error()
		return "", status
	}
	output, err := exec.CommandContext(ctx, python, "-B", "-c", "from playwright.sync_api import sync_playwright; import psutil").CombinedOutput()
	if err != nil {
		status.Detail = strings.TrimSpace(string(output))
		if status.Detail == "" {
			status.Detail = err.Error()
		}
		return "", status
	}
	status.Available = true
	status.Detail = path + "; playwright.sync_api and psutil importable with " + python + "; CDP not tested"
	return path, status
}

//go:embed lightpanda_action.py
var lightpandaSource string

func lightpandaManifest(python, binary string) Manifest {
	return Manifest{
		Name:        "lightpanda",
		Description: "首选浏览器 Lightpanda：导航、DOM、表单和 JavaScript。使用同步 Python Playwright CDP，变量 page、context、browser、args，观察赋给 result。每次调用独立浏览器/context并关闭，不保留登录状态。截图、实际渲染和原生缩放使用备用 cloakbrowser；错误不自动换浏览器重放，先核对可能已执行的副作用。",
		GitHubURL:   "https://github.com/lightpanda-io/browser",
		Command:     python, Args: []string{"-c", lightpandaSource}, Mode: ModeStdio,
		TimeoutSeconds: 120, CancelGraceSeconds: 10,
		Env:         map[string]string{"PYTHONIOENCODING": "utf-8", "TRACE_BROWSER_RUN_ID": "{{run_id}}", "LIGHTPANDA_BINARY_PATH": binary, "LIGHTPANDA_DISABLE_TELEMETRY": "true"},
		InputSchema: json.RawMessage(`{"type":"object","properties":{"code":{"type":"string","description":"Synchronous Playwright Python through Lightpanda CDP. Variables: page, context, browser, args, result. Example: page.goto(args['url']); result={'title':page.title(),'text':page.locator('body').inner_text()}. Each call uses a fresh browser and closes it. Use backup cloakbrowser for screenshots/actual rendering; inspect partial effects after failure and never automatically replay a submission."},"args":{"type":"object","description":"Literal input exposed as args; never interpolated into code."}},"required":["code"]}`),
	}
}
