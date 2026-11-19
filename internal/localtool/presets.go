package localtool

import (
	"context"
	_ "embed"
	"encoding/json"
	"os/exec"
	"strings"
)

//go:embed helper.py
var helperSource string

// DependencyStatus is a discovery observation, not persisted execution evidence.
type DependencyStatus struct {
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Detail    string `json:"detail"`
}

// Presets discovers installed runtimes without installing anything. Python source
// travels in argv and arguments in JSONL stdin, so no checkout or helper file is
// needed when the Trace binary is launched from another working directory.
func Presets(ctx context.Context) ([]Manifest, []DependencyStatus) {
	var manifests []Manifest
	python, modules, paths, detail := discoverPython(ctx)
	status := []DependencyStatus{{Name: "Python 3", Available: python != "", Detail: detail}}
	if python != "" {
		for _, spec := range pythonPresets {
			if spec.module != "" && modules[spec.module] != "" {
				continue
			}
			manifest := Manifest{
				Name: spec.name, Description: spec.description, InputSchema: json.RawMessage(spec.schema),
				Command: python, Args: []string{"-c", helperSource, spec.name}, Mode: ModeStdio,
				Env: map[string]string{"PYTHONIOENCODING": "utf-8"},
			}
			if spec.name == "capability-inventory" {
				manifest.TimeoutSeconds = 10
			}
			manifests = append(manifests, manifest)
		}
		if modules["droidasc"] == "" {
			manifests = append(manifests, Manifest{
				Name: "droidasc", Description: "分析 Android APK 的类与清单，查找字符串、类型、方法及字段引用，按需反编译指定类。使用 droidasc 原生命令参数，示例见 asc Skill。",
				Command: python, Args: []string{"-m", "droidasc", "{{argv...}}"},
				GitHubURL: "https://github.com/MG1937/ASC", LocalPath: paths["droidasc"],
				InputSchema: json.RawMessage(`{"type":"object","properties":{"argv":{"type":"array","items":{"type":"string"},"description":"Native droidasc arguments, e.g. [listclass, /path/app.apk, --prefix, com.example]. Commands: listclass, getmanifest, findrefs, getclass."}},"required":["argv"]}`),
				Env:         map[string]string{"PYTHONIOENCODING": "utf-8", "PYTHONPATH": paths["droidasc"]},
			})
		}
		if modules["frida"] == "" {
			manifests = append(manifests, Manifest{
				Name: "frida", Description: "使用 Frida 枚举设备与进程、附加本地进程或执行 JavaScript 探针。每次 instrument 调用返回前都会卸载脚本并分离会话。",
				Command: python, Args: []string{"-c", helperSource, "frida"}, Mode: ModeStdio,
				InputSchema: json.RawMessage(`{"type":"object","properties":{"action":{"type":"string","enum":["devices","processes","instrument"],"default":"devices"},"device_id":{"type":"string"},"pid":{"type":"integer","minimum":1},"process":{"type":"string"},"script":{"type":"string"},"duration_ms":{"type":"integer","minimum":0,"default":1000}},"required":[]}`),
				Env:         map[string]string{"PYTHONIOENCODING": "utf-8"},
			})
		}
	}
	for _, module := range []string{"capstone", "droidasc", "frida"} {
		moduleDetail := modules[module]
		if python == "" {
			moduleDetail = "Python 3 runtime missing"
		} else if moduleDetail == "" {
			moduleDetail = "importable with " + python
		}
		status = append(status, DependencyStatus{Name: module, Available: python != "" && modules[module] == "", Detail: moduleDetail})
	}
	var disassembler string
	for _, name := range []string{"rizin", "radare2", "r2"} {
		if path, err := exec.LookPath(name); err == nil {
			disassembler = path
			break
		}
	}
	status = append(status, DependencyStatus{Name: "Rizin/radare2", Available: disassembler != "", Detail: disassembler})
	if disassembler != "" {
		manifests = append(manifests, Manifest{
			Name: "radare2", Description: "使用已安装的 Rizin/radare2 分析本地二进制，接收原生分析命令并保留完整原始输出。",
			Command: disassembler, Args: []string{"-q", "-c", "{{commands}}", "--", "{{path}}"},
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"commands":{"type":"string","description":"Native command sequence, e.g. aaa;aflj"}},"required":["path","commands"]}`),
		})
	} else {
		status[len(status)-1].Detail = "rizin, radare2 and r2 not found on PATH"
	}
	webManifests, webStatus := webAnalysisPresets(ctx)
	manifests = append(manifests, webManifests...)
	status = append(status, webStatus)
	lightpanda, lightpandaStatus := discoverLightpanda(ctx, python)
	if lightpandaStatus.Available {
		manifests = append(manifests, lightpandaManifest(python, lightpanda))
	}
	status = append(status, lightpandaStatus)
	cloak, cloakStatus := cloakBrowserPreset(ctx, python)
	if cloakStatus.Available {
		manifests = append(manifests, cloak)
	}
	status = append(status, cloakStatus)
	return manifests, status
}

func discoverPython(ctx context.Context) (string, map[string]string, map[string]string, string) {
	const probe = `import importlib,json,os,sys
if sys.version_info < (3, 9): raise RuntimeError("Python 3.9+ required")
modules = {}
paths = {}
for name in ("capstone", "droidasc", "frida"):
 try:
  module = importlib.import_module(name)
  modules[name] = ""
  paths[name] = os.path.dirname(os.path.dirname(module.__file__))
 except Exception as exc:
  modules[name] = str(exc)
print(json.dumps({"version":sys.version.split()[0],"modules":modules,"paths":paths}))`
	var failures []string
	for _, name := range []string{"python", "python3", "py"} {
		path, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		output, err := exec.CommandContext(ctx, path, "-c", probe).CombinedOutput()
		var found struct {
			Version string            `json:"version"`
			Modules map[string]string `json:"modules"`
			Paths   map[string]string `json:"paths"`
		}
		if err == nil && json.Unmarshal(output, &found) == nil && found.Version != "" {
			return path, found.Modules, found.Paths, path + " (" + found.Version + ")"
		}
		failures = append(failures, name+": "+strings.TrimSpace(string(output)))
		if ctx.Err() != nil {
			break
		}
	}
	if len(failures) == 0 {
		failures = append(failures, "python, python3 and py not found on PATH")
	}
	return "", nil, nil, strings.Join(failures, "; ")
}

var pythonPresets = []struct {
	name, description, module, schema string
}{
	{"capability-inventory", "Read-only offline capability inventory: fixed PATH tools, Python module specs/distribution versions, Lightpanda/CloakBrowser binary, protocol and display prerequisites, and installed alternatives. Does not import optional packages, launch tools/browser, connect, install or verify SSH login/egress. Read once before choosing tools; missing search matches are not proof a capability is absent.", "", `{"type":"object","properties":{},"additionalProperties":false}`},
	{"http-request", "发送 HTTP 请求，返回状态码、响应头和限量正文；HTTP 错误状态保留为观测。TLS 默认校验证书，缺链或自签证书需显式 verify_tls=false，结果标明是否校验。", "", `{"type":"object","properties":{"url":{"type":"string"},"method":{"type":"string","default":"GET"},"headers":{"type":"object","additionalProperties":{"type":"string"}},"body":{"type":"string"},"verify_tls":{"type":"boolean","default":true,"description":"是否校验 HTTPS 证书；仅显式 false 时跳过，结果保留校验状态。"},"timeout":{"type":"number","default":10},"max_bytes":{"type":"integer","minimum":1,"default":1048576}},"required":["url"]}`},
	{"page-fetch", "读取网页标题、可见正文、链接与表单，不执行 JavaScript；交互浏览优先 Lightpanda；截图/实际渲染或其不可用时使用备用 CloakBrowser。TLS 默认校验证书，缺链或自签证书需显式 verify_tls=false，结果标明是否校验。", "", `{"type":"object","properties":{"url":{"type":"string"},"headers":{"type":"object","additionalProperties":{"type":"string"}},"verify_tls":{"type":"boolean","default":true,"description":"是否校验 HTTPS 证书；仅显式 false 时跳过，结果保留校验状态。"},"timeout":{"type":"number","default":10},"max_bytes":{"type":"integer","minimum":1,"default":1048576}},"required":["url"]}`},
	{"dns-resolve", "通过系统解析器查询主机名，返回 IPv4 和 IPv6 地址。", "", `{"type":"object","properties":{"host":{"type":"string"},"port":{"type":"integer","default":0}},"required":["host"]}`},
	{"port-probe", "连接指定主机的 TCP 端口，逐项返回连通性观测。", "", `{"type":"object","properties":{"host":{"type":"string"},"ports":{"type":"array","items":{"type":"integer","minimum":1,"maximum":65535}},"timeout":{"type":"number","default":2}},"required":["host","ports"]}`},
	{"binary-info", "读取本地文件大小、SHA-256 及 PE/ELF/Mach-O 文件头，不执行文件。", "", `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`},
	{"strings", "提取本地文件中的 ASCII 或 UTF-16LE 可打印字符串，保留字节偏移。", "", `{"type":"object","properties":{"path":{"type":"string"},"min_length":{"type":"integer","minimum":1,"default":4},"limit":{"type":"integer","minimum":1,"default":1000},"encoding":{"type":"string","enum":["ascii","utf-16le"],"default":"ascii"}},"required":["path"]}`},
	{"analysis", "生成二进制静态摘要，包含文件哈希、格式、文件头和字符串；摘要不推断程序行为。", "", `{"type":"object","properties":{"path":{"type":"string"},"min_length":{"type":"integer","minimum":1,"default":4},"limit":{"type":"integer","minimum":1,"default":100}},"required":["path"]}`},
	{"code-search", "按文本或正则搜索本地源码，返回命中的文件、行号和内容，不跟随目录符号链接。", "", `{"type":"object","properties":{"path":{"type":"string"},"pattern":{"type":"string"},"regex":{"type":"boolean","default":false},"glob":{"type":"string","default":"*"},"limit":{"type":"integer","minimum":1,"default":200}},"required":["path","pattern"]}`},
	{"python-ast-audit", "用 AST 审查 Python 源码中的潜在危险调用，支持导入别名和 subprocess shell=True；仅静态启发式提示，需复核。", "", `{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer","minimum":1,"default":200}},"required":["path"]}`},
	{"disassemble", "使用 Capstone 从指定文件偏移反汇编原始字节；指令地址由调用方提供，不自动推断虚拟地址。", "capstone", `{"type":"object","properties":{"path":{"type":"string"},"arch":{"type":"string","enum":["x86","x64","arm","thumb","arm64","mips","mips64"]},"offset":{"type":"integer","minimum":0,"default":0},"max_bytes":{"type":"integer","minimum":1,"default":4096},"address":{"type":"integer","minimum":0,"default":0},"max_instructions":{"type":"integer","minimum":1,"default":256},"endian":{"type":"string","enum":["little","big"],"default":"little"}},"required":["path","arch"]}`},
}
