# Trace P2 纯净 Agent 交付

当前源码浏览器优先 Lightpanda，CloakBrowser 0.6.4 为备用。下一次镜像构建还须提供冻结的 `runtime/lightpanda/lightpanda` 与其来源/摘要记录，包含 `.trace/skills/lightpanda`；禁止构建或启动时解析浮动 nightly。三个 Dockerfile 均已接线；尚未构建/发布含 Lightpanda 的新镜像，下面 CloakBrowser 批次为历史记录。Lightpanda 通过 Python Playwright CDP；截图/实际渲染和 UI 验收继续用 headed CloakBrowser。安装与冻结身份见 `examples/lightpanda.md`、`DEPLOYMENT.md`。

## 完整镜像构建入口（2026-11-11）

`Dockerfile.hosted` 从已验收干净基底的不可变ID构建，包含完整操作系统与冻结工具/环境，而非只交付程序或补丁层。当前入口完整替换 Web Analysis 的 `dist`、Linux `node_modules`、package/lock、bridge 和许可，先删除再复制 asc/cloakbrowser/frida/web-analysis 四个 Skill 目录，更新根许可证、程序、运行提示、示例和入口；Go 模型—工具循环与 Python localtool helper 嵌入新程序。构建会从最终活动文件树移除旧 `runtime/pi`，清空旧 `TRACE_PI_RUNTIME`，启动不再依赖 pi npm 运行时；Node 仍供 Web Analysis/CloakBrowser 使用。`Dockerfile.hosted` 保留基底层，因此 `docker save` 归档可能仍含旧层中的 pi 字节；解包运行树和镜像实际文件树应无该目录。最终镜像的 `hosted_startup_test.py` 通过 Go loop/exec 调用镜像内的 `tsecbench_api.py`。

Linux exec 由 `TRACE_EXEC_RESOURCE_MODE` 显式选择执行资源后端。未设置或为 `cgroup` 时保持原严格模式；托管镜像明确设置 `process-group`，以支持平台未委托可写 cgroup 的普通容器。非法值直接报错；委托失败不会自动切换模式。打包验收必须实际走通最终镜像的默认入口、Go loop→exec 与清理，镜像可导入不代表平台运行已通过。

入口保留 `USER trace`、`tini -g` 与 SIGINT。兼容模式要求 Linux pidfd syscall 可用、容器使用独立 PID namespace，且同 UID 的 `/proc` 身份和环境可读；读取权限失败时阻断，不忽略不可见进程。每次调用使用独立进程组锚点与继承 owner 标记，保留共享输出预算、持久归属与 uncertain 不重放。执行期间覆盖原组内清空环境的后代，崩溃恢复时该路径依赖存活且身份匹配的锚点；保留标记的 setsid/double-fork 后代也纳入清理。这是有边界的尽力清理：同时失去锚点和标记、清空标记并脱离原组、改变 UID 或隐藏进程超出其范围，不凭数字 PGID 推断归属。没有逐命令内存/PID 硬隔离，平台容器整体限额仍由平台管理。

恢复使用动作中保存的后端和 boot/PID namespace/UID 身份，当前 ENV 不改写旧归属。同一 namespace 内的控制进程崩溃恢复与容器删除重建不同：重建后 namespace 变化，旧身份拒绝并保留 uncertain，不宣称跨容器自动清理。正式服务器继续使用严格 cgroup 模式。

严格 `cgroup` 模式的宿主必须同时提供：

- 一个空且有限的父域，下面只有 `controller` 和 `exec`；容器 PID 1（tini）与 Trace 全部在 `controller`，`exec` 本身为空。默认参考值为父域至少2816MiB/576 tasks、controller 1GiB/128 tasks、exec 1.5GiB/384 tasks；三者 swap=0，父域至少另留256MiB/64 tasks。不是全主机资源预留。
- 父域和 exec 开启 `memory pids`，将 exec 子树建组、设置限制、`cgroup.kill` 与必要 `cgroup.procs` 迁移权限委托给实际容器 UID（默认1000；userns/rootless时需按宿主映射UID处理）。只映射所需子树，保持容器中的 `/proc/self/cgroup` 和 `/proc/1/cgroup` 对应 `/sys/fs/cgroup` 下同一真实路径；随意将子树挂到另一目录会被拒绝。host cgroup namespace 加相同路径的受限子树挂载是一种配置方式，不应把整个宿主 cgroup 层级开放写入。
- 支持 `clone3(CLONE_INTO_CGROUP)` 的内核，以及允许该调用的 seccomp/LSM/委托配置。入口只读校验不证明 syscall 或逐 leaf 权限可用；现有 Go worker 仍会在每次 exec 验证、设置有限上限并原子入组，失败保持 `not_started`，没有裸执行 fallback。平台运行验收必须实际走通一次 Go agentloop→exec 与清理。

Docker 的 systemd driver 下，`--cgroup-parent` 只选择父 slice，仍会创建 `docker-*.scope`，不会自动产生上述固定兄弟布局。现有 `scripts/exec-cgroup-isolated.sh` 只用于新建的单进程委托 unit，拒绝已有 tini 的普通容器。选择严格模式时需由平台/宿主启动器预置布局；选择托管镜像的显式兼容模式则不要求该委托和 clone3。

验收分开记录普通容器兼容模式、真实委托域严格模式与最终镜像默认入口。`entrypoint_test.sh` 的 sentinel/锁/失败顺序 fixture 使用 stub，不作为内核正控；`hosted_startup_test.py` 使用合成平台 API 和 Provider，仍不代表正式 TSB 导入、VPN或模型网关已经验收。每批实际结果以根 DEPLOYMENT 最新记录为准。

为避免受限构建盘复制第二份完整rootfs，本入口保留干净基底及新增层；`docker save IMAGE` 导出所有层，接收方无需另取基底。归档历史层仍含被覆盖的旧Web依赖和固定公开安装输入，活动rootfs使用新版本；不得称为单层包或彻底删除旧依赖字节。导出须核验全部层/config/blob身份及敏感残留，镜像内测试不替代传回整包校验。正式服务部署、Store迁移和TSB运行各自需要单独指令。确切本批交付身份与验收见根DEPLOYMENT。

## 2026-11-05 P0 轻量离线能力输入

本批 `Dockerfile`、`Dockerfile.release`、`Dockerfile.hosted` 在构建阶段安装额外的固定小包；不改既有正式镜像，不在运行时联网或 pip。构建上下文新增 `runtime/capabilities/`，其内容必须逐项通过 `docker/capabilities.sha256`。`pack-p2.sh` 要求显式 `TRACE_CAPABILITY_BUNDLE` 指向该服务器冻结目录，先校验再复制锁定的八个文件（含字体与许可）；缺失或 hash 不符立即失败。浏览器迁移历史入口 `Dockerfile.p2` 不等于本次 P0 发布入口。

- 产品包：Ubuntu 22.04 amd64 的 OpenSSH client `1:8.9p1-3ubuntu0.18`、sshpass `1.09-1`、xxd `2:8.2.3995-1ubuntu2.36`，及 libcbor0.8 `0.8.0-2ubuntu1`、libfido2-1 `1.10.0-1`。在指定服务器的已验收基底一次性容器中，经其 Ubuntu apt 索引下载闭包；包本身的 copyright/license 随 dpkg 安装保留。不是任意基底通用安装器，其他架构/基底先重新冻结并验收。
- Python：PyPI `pydash==8.0.5` wheel，SHA256 `b2625f8981862e19911daa07f80ed47b315ce20d9b5eb57aaf97aaf570c3892f`，无网络/no-deps 安装，保留 wheel 的 dist-info/许可证。版本由本批冻结，不等同靶题版本；仅核验该版本实际 `set_` 路径行为。
- `install-capabilities.sh` 仅构建/隔离验收使用；不重复添加 paramiko/pexpect、sshd 或全套扫描器。SSH 服务端包仅在一次性验收容器安装，不进入产品锁定包。
- API 薄辅助入口 `examples/tsecbench_api.py` 随各入口复制到 `/opt/trace/examples/`：一次调用一次请求，分离 HTTP/body，有限层 JSON 解码，不自动重放，错误不制造零分。不是新调度器或第二执行循环。

服务器隔离验收使用 `python3 docker/benchmark_contract_test.py` 与 `sudo -n python3 docker/capability_runtime_test.py --source ABS_SOURCE --bundle ABS_BUNDLE --report ABS_REPORT`。后者仅复用已有镜像启动 `--network none` 自有容器，完整 SSH 认证/host-key/命令正负控、固定 CloakBrowser 0.6.4 headed/Xvfb 实际 DOM 点击/PNG；清理后核查容器不存在。源码检查、离线容器验收、正式新镜像构建及平台测评是四个独立状态，不能互相替代。确切回执见根 DEPLOYMENT。

## TSecBench 托管上传（2026-11-02 官网核对）

11月3日历史交付使用 `docker/Dockerfile.hosted` 合并单rootfs层；11月6日当前入口采用前节所述完整多层导出，并补齐Web产物/依赖、Skill和许可。二者都只从已验收干净镜像的不可变ID复用冻结依赖，不从正式state/workspace取构建材料、不安装联网依赖。`hosted_startup_test.py` 经过默认CMD、真实Go agent loop/exec与持久化检查合成平台API流程，含缺变量/健康失败负控；不是正式平台接入验收。包身份/验收结果见根DEPLOYMENT。

用户指定 hosted 准备页要求：先完成平台本地模式接入，再上传启动后自动解题的 Docker 镜像归档（≤3GB）。托管运行由平台负责，不要求用户在本机 docker load/run；下方历史手工命令不能替代托管上传。产品构建/运行仍仅在指定 Ubuntu 服务器执行。

托管容器无公网，所有模型上游须匹配页面白名单，再把域名追加 `.tsecbench.gw`、协议改为 `http`、保留路径；模型与key须匹配同一上游。支持基础 Chat Completions / OpenAI Responses / Anthropic Messages API。页面“从 .env 导入”粘贴配置正文后解析；BENCHMARK_BASE_URL/TOKEN由平台自动注入，不写入配套`.env`。模板不是已验证可用配置，正式服务Provider不能直接充当托管配置。

平台资源8核16G、20G总存储，运行依赖离线冻结、镜像无真实密钥/会话/题目历史记忆。官方当前提示词仍要求 `http://10.0.100.58` 的 `status=ok` 健康预检，现入口保留；尚未在平台沙箱验证。旧包已被用户删除，新交付与未完成的平台验收状态见根开发计划T1/DEPLOYMENT。

## 2026-10-30 CloakBrowser 迁移构建（历史）

旧镜像标签及回滚目录已于2026-11-06按用户指令清理；以下为历史构建记录，不代表当前服务器可用入口。

当时 `Dockerfile.p2` 保留原 `-f` / `BASE_IMAGE` 接口，默认输入为当时服务器使用的已验收 `trace:frontend-docker-20261029`。仅该批新镜像移除 `runtime/chromium`、独立 `runtime/playwright` CLI、Python `DrissionPage` 包及其 `.dist-info`，替换完整 `tools/web-analysis`，加入官方 **CloakBrowser SDK 0.6.4** 与冻结官方浏览器。DrissionGet/DrissionRecord 及其他工具不随该批清理。第二阶段从 `scratch` 复制合并 rootfs，删除的浏览器不再保留在最终历史层。该批原有回滚保留状态已由11月6日清理记录覆盖。

本轮全部安装、构建、测试与浏览器验收仅在 `ubuntu@193.112.174.4` 执行；本机只编辑和静态阅读。Docker 构建仅复制服务器预先冻结的依赖，不联网安装 Python/npm/浏览器。经典构建器使用显式独立上下文和根 `.dockerignore`，不依赖 `Dockerfile.p2.dockerignore` 的自动识别，也不安装 buildx。

`scratch` 不继承元数据；保留非浏览器 ENV、trace 用户、工作目录、tini/-g、SIGINT、CMD 和 Ubuntu 版本标签。浏览器 ENV 的明确差异：删除旧 `TRACE_BROWSER_EXECUTABLE`、`REA_BROWSER_EXECUTABLE`、`PLAYWRIGHT_MCP_EXECUTABLE_PATH`、`PLAYWRIGHT_MCP_SANDBOX`；PATH 移除独立 Playwright CLI；PYTHONPATH 最前加入 CloakBrowser 冻结 site-packages。固定 `CLOAKBROWSER_BINARY_PATH=/opt/trace/runtime/cloakbrowser/binary/chrome` 与 `CLOAKBROWSER_AUTO_UPDATE=false`。原镜像没有 VOLUME/EXPOSE/HEALTHCHECK，COPY 不改 owner。

Python/JS 均通过 CloakBrowser 官方 SDK 以 **headless=False / false** 启动；无 DISPLAY 的 Linux 浏览器操作使用 Xvfb，非浏览器 CLI 不启动显示进程。Python `playwright` 和 JS `playwright-core` 是 SDK 必需的协议依赖，保留不等于保留独立 Playwright CLI 产品。缺失冻结二进制即明确报错，不在请求时下载、不回退普通 Chromium。Electron 显式可执行文件分析能力继续保留，不把任意 Electron 程序替换为浏览器。

### 必需构建材料

```text
runtime/cloakbrowser/
  site-packages/       # 服务器冻结 cloakbrowser==0.6.4 及全部 Python 依赖/metadata
  binary/
    chrome            # 官方下载/校验后完整解压目录中的可执行文件
    ...               # 同目录资源、共享库、locale 等原样保留
tools/web-analysis/   # 新版 dist、package/lock、冻结 node_modules 与许可
.trace/skills/cloakbrowser/ # 仅新增浏览器 Skill；asc/frida 从基底原样保留
bin/trace             # 本批服务器构建
```

binary 不是只复制一个 chrome 文件；官方包原生共享库和资源一起保留。site-packages 必须匹配冻结 Python 的 Linux/amd64 与 Python ABI，包含 SDK 的 Python Playwright 驱动。SDK 为 MIT；浏览器及其第三方许可随原包保留，不把 SDK MIT 宣称为浏览器全部组件的许可。准备阶段记录下载来源、固定版本与校验结果；运行环境不重新解析浮动版本。

仅在指定服务器的隔离源码目录执行；先保留正式配置/state/workspace，不使用历史交付目录作为输出：

```sh
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags="-s -w" -o bin/trace ./cmd/trace
base=$(docker image inspect --format '{{.Id}}' trace:frontend-docker-20261029)
context=$(mktemp -d /tmp/trace-docker-context.XXXXXX)
mkdir -p "$context/bin" "$context/prompts" "$context/examples" "$context/docker" \
  "$context/runtime" "$context/tools" "$context/.trace/skills"
cp -a runtime/cloakbrowser "$context/runtime/"
cp -a tools/web-analysis "$context/tools/"
cp -a .trace/skills/cloakbrowser "$context/.trace/skills/"
cp bin/trace "$context/bin/"
cp prompts/*.md "$context/prompts/"
cp -a licenses "$context/"
cp examples/tsecbench-task.md "$context/examples/"
cp docker/Dockerfile.p2 docker/entrypoint.sh "$context/docker/"
cp docker/Dockerfile.p2.dockerignore "$context/.dockerignore"
DOCKER_BUILDKIT=0 docker build --pull=false -f "$context/docker/Dockerfile.p2" \
  --build-arg BASE_IMAGE="$base" -t trace:cloakbrowser-20261030 "$context"
python3 docker/image_test.py --base "$base" --image trace:cloakbrowser-20261030 \
  --report /tmp/trace-cloakbrowser-image.json
python3 docker/p2_runtime_test.py --image trace:cloakbrowser-20261030 \
  --report /tmp/trace-cloakbrowser-runtime.json
bash docker/entrypoint_test.sh
```

独立上下文只含上述 COPY 材料，不复制其他 runtime、非浏览器 Skill 或用户数据。P2 只复制 `skills/cloakbrowser`，其余 Skill 的基底内容、mode 和 owner 原样保留；仅删除 `drissionpage` 与 `playwright-cli` 两个旧浏览器 Skill。验收/日志归档后，核对 `$context` 仍是本次创建的绝对目录且无构建使用，再精确清理；失败上下文保留排查，不批量清理 `/tmp`。

`image_test.py` 明确区分迁移与冻结 parity：环境只接受上述精确变化；路径/类型/模式/owner/size/symlink/xattrs 只允许本轮产品/许可覆盖、浏览器替换目录、`runtime/pi` 的精确删除及 `skills/drissionpage`、`skills/playwright-cli`、`skills/cloakbrowser` 变化，并报告各替换根的实际变化数量。其他 Skill、runtime、dpkg、系统锚点与 owner 保持相同，禁止笼统豁免整个 runtime。测试不做内容 hash 等价承诺，也不把更换了浏览器的镜像称为“全依赖冻结完全相同”。

离线 smoke 使用 `--network none`，检查非 root 可写、空 state/workspace、无旧 pi host 目录与环境依赖、其余 Python 工具、Web Analysis 目录；分别以 Python 和 JS SDK 在 Xvfb 中实际打开页面，查 `/proc` 确认固定 CloakBrowser 路径与 **无 `--headless` 参数**。缺失 binary 负对照必须失败且不回退；旧 CLI、DrissionPage 与旧浏览器目录必须不存在。它验证 SDK 与镜像运行契约，不替代实际 Trace localtool/JS 操作链和生命周期回归；`p2_runtime_test.py` 继续覆盖真实 exec/MCP、tini 回收和单次 SIGINT。正式验收/发布结果以项目 DEPLOYMENT 为准，不因新增脚本即宣称通过。

构建前检查 Docker data-root 可用空间并记录实际基底大小；完整 rootfs、冻结浏览器和临时上下文占用另计。scratch 合并不是增量缓存优化，更新产品仍重新传输整层，旧镜像和缓存仍占盘；不承诺冷启动更快，不批量 prune、删除历史镜像或重写旧包。

### 从本批已验收镜像生成新包

```sh
bash docker/pack-p2.sh trace:cloakbrowser-20261030 /tmp/trace-cloakbrowser-delivery-20261030
```

保留既有 `trace-p2-linux-amd64/`、Linux tar 与 Docker tar 文件命名接口，输出根必须全新。脚本除 pi 版本外核对 Python/JS CloakBrowser 0.6.4、Python Playwright metadata、可执行 binary/chrome 及旧运行目录缺失。包内 run-linux.sh 前置新 Python 依赖、固定 binary 路径与禁用自动更新；宿主须有 Xvfb/xauth 和 Dockerfile 列出的图形共享库。纯实体包依赖宿主库，不是任意 Linux 自包含包。

来源：CloakHQ/CloakBrowser `b1a00eda61efe71da2a1a38831fa6fe861f5d91d` 的 `cloakbrowser/browser.py`、`cloakbrowser/download.py`、`js/src/playwright.ts`、`js/src/download.ts`，采用官方 SDK 与显式 binary override/auto-update ENV；0.6.4 没有 xvfb/auto_update launch 参数，显示进程由 Trace 适配器管理。原 Docker 依赖/产品缓存顺序参考 Muteki `c60c8feeff69a64aa5abfbda113f3f33bff30af4`；继续沿用 tini，不引入 Worker supervisor。

## 原 P2 验收与交付记录

**以下均为 2026-10-29 历史快照。旧 Chromium/DrissionPage/Playwright CLI、旧镜像标签和构建命令只解释原交付，不适用于上方 CloakBrowser 新包，也不重写历史验收。**

本批已在指定服务器重建最终 P2 镜像；真实模型→exec→模型闭环、MCP搜索→选择→调用及SIGINT清理、tini回收、入口哨兵恢复、100轮pi上下文等价均通过。最终全量Go已通过，race/容量回归与交付发布状态以项目DEPLOYMENT记录为准；这不代表B9混合500ms目标通过。

## 交付边界

- 本批镜像名：`trace:p2-20261029`；冻结依赖基底：`trace:p1-20261029`。仅在服务器 `ubuntu@193.112.174.4` 构建、运行与验收。
- 唯一模型—工具循环是 Go 内置 `internal/agentloop`（移植 pi agent-loop 轮次语义，2026-11-24 起），镜像不再需要 pi npm 包或 Node pi host。Go 同时负责 Provider、工具执行、持久状态和证据账本。
- 纯净 Linux Agent 包只携带 `bin/trace`、`prompts`、`.trace/skills`、`tools`、`runtime`、`licenses`、必要 Docker/启动材料、环境模板及官方任务提示词；另交付 `docker save` 镜像归档。
- 不带 Trace 整个源码树、开发计划、部署记录、测试日志、历史会话、答题记忆、用户 `state` / `workspace`、真实 API key、已保存 Provider/MCP 配置或 `.env`。运行库和工具随附的源文件、锁文件及许可属于依赖材料，不等于复制开发工作区。Docker镜像包含系统工具及共享库；Linux tar依赖宿主OS系统库，不是任意Linux自包含包。
- 根 `licenses`、依赖内许可及 `tools/web-analysis/LICENSE` / `UPSTREAM.json` 原样保留。Muteki 适配代码的 AGPL-3.0 与 pi 的 MIT 不混同；纯运行包不替代另行履行的对应源码交付义务。
- 本批不生成 SHA256SUMS，不做历史 hash 对比；只检查实际版本、运行行为和纯净目录边界。打包成功不等于功能验收通过。

## 容器与运行契约

PID1 为 `/usr/bin/tini -g -- /opt/trace/docker/entrypoint.sh`，停止信号为 **SIGINT**。入口最终 `exec trace`，不中转后台进程。默认 `benchmark`；显式传入 `version`、`create`、`run` 等参数时直接执行 Trace CLI。

benchmark 使用 `examples/tsecbench-task.md`，不增加比赛调度器。平台注入 `BENCHMARK_BASE_URL` / `BENCHMARK_TOKEN`；模型 URL、模型名及 key 从部署环境注入。入口仅在 VPN 健康检查 HTTP 成功且 JSON `status=ok` 后继续。默认预算 `TRACE_MAX_SECONDS=21180`、`TRACE_MAX_ROUNDS=0`、`TRACE_MAX_ACTIONS=0`，后两项为不限；无进展不另加隐式轮数暂停。

`/state/service/hosted-run.json` 使用临时文件完成后原子改名；创建失败不发布部分哨兵，空哨兵可重试，有效哨兵沿用原会话。新任务使用新 state 卷；重启同任务可复用原卷。挂载旧 state 会带回用户配置和历史，这属于运行时恢复，不属于纯净镜像内容。

Node/Python、Chromium、Playwright CLI、DrissionPage、REA web/JavaScript/Electron 子集和其余已冻结工具由基底保留；启动时不安装大环境。可用项目及版本以实际 `runtime` / `tools` 为准，不把可选未安装工具写成已交付。浏览器在 Docker 内沿用既有 sandbox 设置，裸机启动不因此全局关闭 sandbox。

## 服务器最短流程

以下源码路径和镜像名属于本批；执行前须同步最终代码并保留既有正式 state/config。不要把本机工作区或正式数据目录作为打包输入。

### 1. 构建 P2 镜像

`docker/Dockerfile.p2` 从干净 P1 镜像复用冻结依赖，只更新本轮二进制、运行提示词、入口和许可。它删除旧 `runtime/pi` 目录，缺 tini 时仅补这个小包；不重装 Node/Python/浏览器。

```sh
cd /tmp/trace-p2-20261029/round3-source
mkdir -p bin
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=false -trimpath -ldflags="-s -w" -o bin/trace ./cmd/trace
docker build --pull=false -f docker/Dockerfile.p2 \
  --build-arg BASE_IMAGE=trace:p1-20261029 -t trace:p2-20261029 .
```

基底必须是此前冻结的干净镜像，不含真实 key/config/history；继承镜像会保留其所有旧层，后续删除目录不等于从归档中消除旧数据。依赖缺失时应先补齐既有依赖基底，不在启动脚本动态安装。

### 2. 独立验收

```sh
cd /tmp/trace-p2-20261029/round3-source
python3 docker/p2_runtime_test.py --image trace:p2-20261029 \
  --report /tmp/p2-docker-runtime.json
bash docker/entrypoint_test.sh
```

- Docker 脚本经真实 ENTRYPOINT 分别执行 exec/MCP fixture，检查活容器 double-fork 回收、真实 MCP 进程存在、单次 SIGINT 后原宿主 PID 身份消失、未误报成功及自有容器清理。namespace teardown 也会回收进程，此项不替代 Go 层独立优雅清理排序测试。
- entrypoint 脚本检查创建失败、空哨兵恢复和有效哨兵复用；它是入口逻辑 fixture，不是在线比赛验收。Go/浏览器等完整回归仍由本批主验收流程负责。

### 3. 从已验收镜像打包

`pack-p2.sh` 创建但不启动临时容器，仅提取明确运行目录并移除临时容器；不安装依赖、不执行 Trace、不复制源工作区。输出目录必须不存在；失败的部分输出保留供排查，不冒称发布成功。仅在本批验收完成后执行：

```sh
cd /tmp/trace-p2-20261029/round3-source
bash docker/pack-p2.sh trace:p2-20261029 /tmp/trace-p2-delivery-20261029
```

输出：

- `trace-p2-linux-amd64/`：纯净实体运行目录。
- `trace-p2-linux-amd64.tar.gz`：该运行目录归档。
- `trace-p2-docker.tar.gz`：本批镜像 `docker save | gzip` 归档。
- `image-id.txt`：两种包共同使用的不可变镜像 ID；tag 被并发移动时也不会让镜像包与目录包来自不同镜像。
- `image-config-id.txt`：归档 config 的 SHA256；兼容 classic Docker 与 containerd 对 image ID 的不同表示。

脚本检查实际 pi 两个包为 1.4.0；目录材料来自已验收镜像而非旧发行版。发布前仍须确认镜像及运行目录无用户配置/历史/真实密钥，保留许可材料；本 README 不替代该检查结果。

## 使用交付物

```sh
docker load -i trace-p2-docker.tar.gz
IMAGE_ID=$(cat image-id.txt)
docker image inspect "$IMAGE_ID" >/dev/null 2>&1 || IMAGE_ID=$(cat image-config-id.txt)
docker run --rm "$IMAGE_ID" version
# hosted.env 在包外单独准备；不要放真实 key 到镜像或公开归档。
docker run --name trace-p2-bench --env-file /secure/path/hosted.env \
  -e BENCHMARK_BASE_URL -e BENCHMARK_TOKEN "$IMAGE_ID"

# Linux amd64 实体包
tar -xzf trace-p2-linux-amd64.tar.gz
cd trace-p2-linux-amd64
bash docker/run-linux.sh version
bash docker/run-linux.sh  # 默认网页监听 127.0.0.1:8765
```

环境字段参考包内 `docker/hosted.env.example`，实际 key 仅通过包外私密环境文件或平台变量注入。模板中的模型/网关是配置示例，不代表本批做过在线服务可用性验证；裸机使用自身可达地址，托管网关取决于平台网络。首次网页配置由用户保存；裸机系统库需求参照 `docker/Dockerfile`，Electron 图形场景按需配置 Xvfb。

实体运行目录同时提供完整 Docker 构建所需的冻结依赖；`docker/Dockerfile` 面向干净 Ubuntu22.04 基底，`docker/Dockerfile.p2` 仅用于服务器本轮增量构建，不应把二者混用：

```sh
# 在实体包根目录：全新基底重建会安装系统 apt 库，但不重新下载冻结语言/浏览器依赖。
docker build -f docker/Dockerfile -t trace:p2-rebuilt .
```

