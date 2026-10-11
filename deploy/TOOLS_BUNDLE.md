# 单文件离线工具发行包

`scripts/build_tool_bundle.py` 在构建机联网取得依赖，产出一个 `tar.gz`。
部署机只需这个包和预先准备好的宿主环境，不需要源码、另一个 wheel、pip 索引或工具下载。
运行时 `tool_prepare` 仍只做诊断；本构建器直接复用 `runtime/tool_setup.py` 的官方归档下载、校验、解包、启动验证和相对 receipt。

## 默认覆盖范围

默认 `--profile all`、`--cloud all`。包内包含：

| 适配器依赖 | 随包内容 |
| --- | --- |
| Trace 本身与标准库适配器 | 项目 wheel（CLI、Web、内置适配器、静态资源等）及其依赖 |
| 多架构反汇编 | `capstone==5.0.9` |
| Frida 本地进程接口 | `frida==17.17.0` |
| 完整浏览器会话、DOM、截图 | `playwright==1.62.0` 及 Python 依赖；manifest 固定版本 Chromium |
| 二进制分析与图分析 | manifest 固定版本 Rizin 官方 portable 文件 |
| AWS / Azure / 腾讯云 | `awscli` / `azure-cli` / `tccli` 及递归依赖 |
| 华为云 / 火山引擎 / 百度云 / 京东云 | `huaweicloudsdkecs` / `volcengine-python-sdk` / `bce-python-sdk` / `jdcloud-sdk` 及递归依赖 |
| GCP / 阿里云 | manifest 固定版本 `gcloud` / `aliyun` 官方 portable 文件 |
| Sliver 现有 gRPC 适配器 | 项目 wheel 的 `sliver` extra（`grpcio>=1.74,<2`、`protobuf>=5,<7`）及依赖 |

三个引擎版本与四项 portable 的版本、URL、校验信息以**所选项目 wheel 内的**
`tool_manifest.json` 为准。云 Python 包在构建时由 pip 联合解析；未人为声称这些未固定的版本可跨时间重现。
解析冲突、缺包、缺 wheel/构建依赖、平台不支持、工具启动失败均导致构建失败，不输出“全量成功”的包。
`bundle.json` 记录 profile、clouds、`full_catalog`、目标平台/Python、直接需求与全部 wheel 文件名（含实际版本）。
每个 portable 保留原始 receipt、文件摘要和相对 `active.json`，不接受构建机 PATH 上的工具代替包内文件。

这里的 `full_catalog=true` 表示当前上述适配器的本地引擎依赖齐备，**不是所有上游产品功能齐备**。
不包含 Rizin 的额外反编译插件、任意自定义外部 MCP 服务、Sliver/Cobalt Strike 服务端、目标设备上的 Frida server、云凭据或远端资源。
不会通过删掉项目功能或把原生 fallback 的 partial 状态改名来宣称集成。
外部 API、模型、云查询或远端 C2 操作本身仍需要相应连接和配置；“离线”指部署和依赖准备无需联网。
Playwright MCP preset 使用包内 `redteam_agent.runtime.browser_mcp`，提供 17 个浏览器工具，不调用 npm/npx；这是 Trace 的 MCP 适配器，不是微软官方 MCP 包。用户自定义 MCP 入口保持原配置。
Docker worker 是宿主 Docker 接口；Docker daemon 和任务指定的镜像不在应用包内。缺镜像时 Docker 可能按自身默认行为拉取，不能称这一外部路径离线自足。

## 构建（由发行方执行，可联网）

在 **Linux x86_64、Python 3.11–3.13** 上执行，推荐构建机与目标机使用相同发行版和 Python minor。
需要 Python 的 `pip`、`venv`/`ensurepip`；sdist-only 云依赖可能需要构建机的编译工具。
不支持在 Windows 上生成 Linux 二进制包，也不把虚拟环境跨机器直接复制。

```sh
cd /path/to/codex-redteam-agent
python3.12 scripts/build_tool_bundle.py --output dist/trace-tools-all-linux-x86_64-py312.tar.gz
```

重用现有项目 wheel，不重复构建项目：

```sh
python3.12 scripts/build_tool_bundle.py \
  --wheel /absolute/path/trace_agent-0.1.0-py3-none-any.whl \
  --output dist/trace-tools-all-linux-x86_64-py312.tar.gz
```

已有输出文件会报错，请使用新的发布文件名。构建只归档新的临时 staging 目录，不将共享工作区的 state、密钥、下载缓存或整个源码目录打包。
`--wheel` 使用该 wheel 的代码与 manifest；因此应由发行方先确认它含有待发布的当前修改。
未提供 `--wheel` 时，正常的 setuptools wheel 构建可能在源码树创建 build/egg-info；这不是离线包内容。
官方归档沿用 manifest 的 HTTPS 官方地址，Python 依赖来自 PyPI；源码分发会在构建阶段转换为 wheel。
构建用新 venv 从本包 wheelhouse 执行 `--no-index --no-deps --find-links` 安装、`pip check`、模块导入和 portable 启动验证。
Playwright 验证包括实际 `sync_api`/`async_api` 导入，不只检查顶层命名空间。
`bce-python-sdk` 的导入名为 `baidubce`，`volcengine-python-sdk` 为 `volcenginesdkecs`，`jdcloud-sdk` 为 `jdcloud_sdk`；包名不直接当作导入名。
同时选择华为、火山、百度、京东时（默认 full），构建和部署校验都会执行随包 `cloud_sdk_smoke.py`，
用真实 SDK、mock HTTP 传输验证签名/临时令牌/响应解析；其所有四家依赖必须齐备，缺 `baidubce` 不能跳过。
依赖冲突会直接失败，需在后续发行中处理兼容版本；不会跳过某个云提供商后继续标为全量。
下载/启动预算可用 `--setup-timeout 7200` 调整（仅 portable setup 阶段）。

开发 fixture 或明确裁剪：

```sh
# 只有项目 wheel 与其基础依赖，不含额外引擎。
python3.12 scripts/build_tool_bundle.py --profile core --output dist/trace-core.tar.gz
# 项目 + Playwright/依赖 + Chromium。
python3.12 scripts/build_tool_bundle.py --profile browser --output dist/trace-browser.tar.gz
# 三个 Python 引擎、Chromium、Rizin、Sliver 依赖，加选 AWS/华为/GCP。
python3.12 scripts/build_tool_bundle.py --cloud aws,huawei,gcp --output dist/trace-selected.tar.gz
```

`--cloud` 可取 `all`、`none` 或逗号分隔的
`aws,azure,tencent,huawei,volcengine,baidu,jdcloud,gcp,aliyun`。
`core/browser` 默认不选云；`all` 默认选全部。任何裁剪组合均记录 `full_catalog=false`，不能作为全量发行交付。

## 离线宿主机要求

这是完整的**应用/引擎依赖包**，不是包含内核、Python 和系统动态库的系统镜像。
目标机预先提供与构建相同 minor 的 CPython、venv/ensurepip、POSIX shell、tar、GNU coreutils、兼容的 glibc/libstdc++。
manifest 当前只提供 Linux x86_64，Alpine/musl 或 ARM 不在此包支持范围内。
本机编译的云依赖 wheel 也要求目标机 ABI/动态库兼容。

Chromium/Playwright 还依赖宿主机的 NSS/NSPR、ATK/AT-SPI、X11/XCB、Xcomposite/Xdamage/Xfixes/Xrandr、
GBM/DRM、xkbcommon、ALSA、CUPS、Cairo/Pango、GLib、字体和 CA 证书等。
具体发行版的软件包名及 `t64` 后缀随系统变化；应在制作宿主系统镜像时使用该版本 Playwright 的官方系统依赖清单准备好。
现有 Dockerfile 的 apt/`playwright install-deps` 属于系统镜像准备，不能证明这个 tar.gz 自带系统库。
构建机同样需要这些库才能通过浏览器启动验证。目标机校验会实际启动随包 Chromium；缺库直接失败，安装脚本不调用 apt、yum、`playwright install` 或运行时 setup。

## 离线部署（只交付这一个 tar.gz）

在目标机器断网环境中执行：

```sh
tar -xzf trace-tools-all-linux-x86_64-py312.tar.gz
TRACE_PYTHON=python3.12 sh trace-tool-bundle/install-tool-bundle.sh
"$HOME/.local/share/trace/trace-service" web
```

自定义安装位置：

```sh
TRACE_PYTHON=python3.12 TRACE_INSTALL_PREFIX=/opt/trace \
  sh trace-tool-bundle/install-tool-bundle.sh
/opt/trace/trace-service web
```

默认安装路径为 `${XDG_DATA_HOME:-$HOME/.local/share}/trace`，可用 `TRACE_INSTALL_PREFIX` 指定绝对路径。
每次创建新的 release venv，只安装包内全部 wheels，显式使用
`--no-index --no-deps --find-links`；`--no-deps` 防止 wheel metadata 的直接 URL 引起隐式联网，依赖闭包由构建解析并由 `pip check` 检查。
安装校验和服务入口禁用 gcloud 的自动更新检查及遥测，避免本地版本探测顺带访问网络。
部署还检查 wheel 文件列表、Python/平台、Python 引擎版本、云模块/CLI、portable receipts/全部文件摘要与本机启动。
缺失或失败会以非零退出，原 `current` 保持不变；未激活的失败 release 留在 releases 中供检查。
成功后才保存 `previous` 并原子切换 `current`，保留旧 release。

通过 `trace-service web|mcp|cli ...` 启动：包装脚本把 `TRACE_BIN`、`TRACE_TOOLS_HOME` 和 PATH 指向该 release，
并复用原有 `run-trace.sh`。portable 不复制到用户缓存，不覆盖已有 tools/state/key。
此入口固定使用随包引擎，不受旧的 `TRACE_TOOLS_HOME`/`TRACE_BIN` 指向另一发行的影响。
runtime 的 `prepare`、browser adapter 依赖检查和 doctor 均识别 managed receipts；`--tools-dir` 会传到实际查找过程。
doctor 会区分 managed/system，云 SDK 缺失会报告 partial/unavailable，显式浏览器探测失败会返回非零退出码。
直接调用 venv 的 `bin/trace` 时需自行设置 `TRACE_TOOLS_HOME=<release>/tools`；推荐使用包装入口。
已有 `TRACE_HOME`、默认 state 目录、配置和凭据继续由原 runtime 管理，安装器不初始化或覆盖它们。
root 安装保留 `chown --reference="$prefix" -R "$release"` 的权限修复，并让 releases 父目录继承相同属主：服务账号运行前需由运维预先把 prefix 设为该账号所有。

部署后验证（不下载）：

```sh
"$HOME/.local/share/trace/trace-service" cli doctor --json --probe-browser
# full 构建已执行；需要复跑四家 SDK 契约检查时：
"$HOME/.local/share/trace/current/bin/python" -B \
  "$HOME/.local/share/trace/current/cloud_sdk_smoke.py"
```

## 无网络 smoke

```sh
python scripts/tool_bundle_smoke.py
```

Smoke 使用临时 wheel/官方归档形状的 fixture，mock 构建依赖获取与可执行引擎启动，验证默认覆盖、依赖传递、归档内容、相对 receipts 搬迁和缺件失败。
另用真实 pip/venv 从本地 fixture wheel 解析普通依赖和 extra，验证缺传递依赖时 `pip check` 失败、补齐后通过；全程 `--no-index`。
runtime 回归覆盖仅有 managed Chromium、显式 tools root、目标机缺系统库以及云 SDK 缺失的诊断。
shell 测试以 mock Python/CLI 执行真实安装脚本，验证离线 pip 参数、校验失败不切换发行、已有 state/key 不变以及入口的工具路径。
需要 POSIX sh（Windows 可使用 Git for Windows 的 sh）。没有 sh 时 smoke 报错，不能当作安装器测试已通过。
这不会执行真实下载/部署，也不代表真实供应商包兼容性或所有动态库已通过测试；正式构建及目标机验证由上述发布命令完成。
