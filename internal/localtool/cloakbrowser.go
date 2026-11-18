package localtool

import (
	"context"
	_ "embed"
	"encoding/json"
	"os/exec"
	"strings"
)

//go:embed browser_action.py
var cloakBrowserSource string

// Discovery imports the SDK only: launch/ensure_binary are execution/install
// operations and must never download Chromium while listing available tools.
func cloakBrowserPreset(ctx context.Context, python string) (Manifest, DependencyStatus) {
	status := DependencyStatus{Name: "CloakBrowser", Detail: "Install cloakbrowser==0.6.4 and psutil in Trace's Python environment; preinstall its Chromium binary"}
	if python == "" {
		return Manifest{}, status
	}
	output, err := exec.CommandContext(ctx, python, "-c", "import cloakbrowser, psutil; assert cloakbrowser.__version__ == '0.6.4', 'cloakbrowser==0.6.4 required'; print(cloakbrowser.__version__)").CombinedOutput()
	if err != nil {
		status.Detail = strings.TrimSpace(string(output))
		if status.Detail == "" {
			status.Detail = err.Error()
		}
		return Manifest{}, status
	}
	status.Available, status.Detail = true, python+" (CloakBrowser "+strings.TrimSpace(string(output))+"; browser/display checked at execution)"
	return cloakBrowserManifest(python), status
}

func cloakBrowserManifest(python string) Manifest {
	return Manifest{
		Name:        "cloakbrowser",
		Description: "浏览器工具：使用 CloakBrowser Python SDK，以有界面 Chromium 浏览、点击、填写、读取 DOM、执行 JavaScript、截图。code 使用同步 Playwright API，变量 page、context、browser、args，结果赋给 result。固定 binary 不透传 console/pageerror，空事件不证明无日志或无页面错误；原生 page.set_content 会在 DOM 已写入后超时。替换当前文档请显式调用 set_document(html, ready_selector=..., timeout=...)，单次写入并核验就绪和 URL，不等价原生 API；失败先核对副作用，不重放。每调用独立 browser/context，结束自动关闭且不保留登录状态；关联步骤一次完成。Linux 无 DISPLAY 时使用 Xvfb，仍为 headless=False；未提供远程桌面。失败不回退其他浏览器。截图用 page.screenshot(path=...) 后注册文件。",
		GitHubURL:   "https://github.com/CloakHQ/CloakBrowser",
		Command:     python, Args: []string{"-c", cloakBrowserSource}, Mode: ModeStdio,
		TimeoutSeconds: 120, CancelGraceSeconds: 10,
		Env:         map[string]string{"PYTHONIOENCODING": "utf-8", "TRACE_BROWSER_RUN_ID": "{{run_id}}", "CLOAKBROWSER_AUTO_UPDATE": "false"},
		InputSchema: json.RawMessage(`{"type":"object","properties":{"code":{"type":"string","description":"Synchronous Playwright Python statements through CloakBrowser. Variables: page, context, browser, args, result, set_document. Example: page.goto(args['url']); result={'title':page.title(),'text':page.locator('body').inner_text()}. Fixed binary suppresses console/pageerror events: empty events do not establish absence of logs/errors. Native page.set_content times out after applying DOM effects; use explicit set_document(args['html'], ready_selector='#ready', timeout=10000) to replace the current document once and check readiness/unchanged URL. This helper is not a native API patch; timeout bounds readiness, outer worker bounds execution; inspect effects after failure and never auto-retry. Fresh headed browser/context, closed after the call."},"args":{"type":"object","description":"Literal input values exposed to Python as args; never interpolated into code."}},"required":["code"]}`),
	}
}
