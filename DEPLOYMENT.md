# Trace 部署与验收

## 版本身份

| 状态 | 版本或构建 | SHA-256（binary） | 证据 |
| --- | --- | --- | --- |
| 正式，2026-11-19T12:48:14.814286+08:00（上海）发布 | workspace-layout-20261119 | `7bab4744550a6df172628a94f82667568cdd3c7ffe832fd53cea5998a33d6307` | 私有 trace-workspace-layout-20261119/release-receipt.json；备份与线上身份校验通过 |
| 历史正式，2026-11-19 10:43:50（上海）发布 | dispatch-lightpanda-20261119 | `de4951b4801f55933c91392c40c67b7b8e1e4df834c60f852f47f057c3bfe911` | 私有 trace-release-20261119/release-receipt.json；备份与线上身份校验通过 |
| 历史正式，已被 11-19 替代 | optimize-pi-20261117 | `b8a71dc31f619e8821c8e635f3657e39f228cd464d84686969608eb29c7d21eb` | 私有 validation/optimize-20261117、release-optimize-20261117 发布回执 |
| 隔离验收，已并入 11-19 正式版 | Lightpanda 优先 / CloakBrowser 备用（2026-11-18） | `de4951b4801f55933c91392c40c67b7b8e1e4df834c60f852f47f057c3bfe911` | 私有 trace-lightpanda-20261118/evidence-final；独立签收通过 |
| 隔离验收，已并入 11-19 正式版 | 调度/报告/工作区 R3 + D2 R4（2026-11-18） | `acf7a55b4e4b73e57283699803c3f2369685bfa76970cbb8ad0c0aa7ff32e972` | 私有 trace-acceptance-20261118/final-evidence、supplement-evidence-r4；独立复核完成，D2 补充签收通过 |
| 隔离验收，未发布 | UI 设置候选（2026-11-17） | `ca5f1718547e2f080ce9805f5a99510506350ce54751db63b7d4c8ef4d56f01e` | 私有 validation/ui-settings-20261117、trace-20261117 回执 |
| 镜像，尚未同步随后修复 | trace:full-agent-tsb-20261112 | `68dfb5a71bed513217a0677d40b9d2f1dc47dae74ad2fcaff0f00707d686235a` | 2026-11-12 全量镜像回执 |

11-17 旧 UI 隔离验收输入清单：1188 项，SHA-256 `d3d0901f4950488c74d371df7554a10f3f0c71d52c013487addcf01bd0d9d4f2`；正式部署源码清单 SHA-256 `f8386dacf59e2909d6f99fd4443ae00786e2e00acf422de69674ad67f591dada`。文档压缩仅本地，未同步正式服务器源码或冻结清单。

## 2026-11-19 通用调度与工作区修复发布

- 正式版本 `workspace-layout-20261119`，上海时间 `2026-11-19T12:48:14.814286+08:00`。采用 pi/ARTEX/Muteki 的容量准入、完成后释放和来源绑定交接机制，沿现有 Store/pi 循环适配；不添加平台或题型专用求解流程。实际顶层 `prompts/agent.md` 与验收源码同步，source/payload 清单重新生成。
- 验收：容量 JSON/SQLite 原子性和相关 race、prepared-request 资源/交付约束、真实 pi 两条补交负控、结果边界、任务/生命周期/验收相关组、报告文件 HTTP、vet/build 全通过。前端 42 份 Node、相关 Go、四组固定 CloakBrowser headed/Xvfb 通过；覆盖真实通知、图拖动/关系高亮、390/1024/1440 视口和原生 200%，最终 1440 首屏截图人工复核。独立审查两个 P1 均关闭。仅此范围，不声称全仓或新真实场次通过，不替代历史 UI-ZOOM-01 两帧贴尾专项。
- 停旧 controller 后完整备份旧 release、state（含 CAS/inputs）、workspace、systemd/nginx 及模块配置链接目标；`1,344,716,800` B、22,330 项逐一核对内容 hash、类型、权限、UID/GID 和链接。归档 SHA256 `c3152c219ad29e4a464fe2782e4691bfd9b2fc3c0e5f7d6521749093f519b8ad`，路径 `/var/backups/trace-workspace-layout-20261119/rollback.tar`；目录 0700、归档 0600。旧完整 release 保留，未做回滚演练。
- 上线核对实际 `/proc/exe` binary、运行用户/cwd/state/config 和主备依赖路径；HTTP 200、异 Host/Origin 403、公共入口未认证 401；四份实际 HTTP 资源 hash 与源码一致。工具 83 项、Skill 5 项，Lightpanda/CloakBrowser 和首选 Skill 正常；最终提示词同步。7 份 Run 业务内容、请求/动作/事件不变，0 份仅 updated_at 刷新，未恢复暂停 Run。
- 私有原件保留在服务器 `validation/workspace-layout-20261119` 和本地项目外 `_work/trace-workspace-layout-20261119`，日志根因在 `_work/trace-scheduler-20261119`。先期失败记录保留：测试缺委托 cgroup、报告下载夹具缺合法报告、补交 raw toolcall 仍送 pi；均定位后修复并由 R4 复验通过，没有放宽产品报告/验收约束。本次不构建或发布 Docker/TSB 镜像。

## 2026-11-19 早期正式发布

- 按用户“发布，github也更新”指令，将已验收的调度/完成度、编排者职责、报告归档与固定版本下载、运行对话同栏顶部消息窗、隐藏 uncertain 对话、探索图拖拽及链路高亮和浏览器主备调整部署为 `dispatch-lightpanda-20261119`。pi 1.4.0、JSON Store 和正式 cgroup 模式保持原配置。GitHub 源码仓库为 `chAng-L19/Trace`，目标分支 `main`；服务器发布见私有发布回执；GitHub 提交身份以推送后 main 的实际 SHA 为准，逐文件核对结果另存私有 Git 回执。
- 新 release 复制完整冻结 runtime；web-analysis 编译输入与旧正式版逐项一致，保留其已安装依赖和构建闭包。Lightpanda 使用下方已验收冻结实物，主备浏览器实际工具及启用的 Lightpanda Skill 已从正式 API 核对。发布前用新 release 路径复验真实 Lightpanda HTTPActions 的成功/错误/非 JSON/取消，以及 CloakBrowser 有界面操作与截图；2 组通过、0 跳过。本次未重跑全仓测试。
- 停旧 controller 后完整备份 state（含 CAS/inputs）、workspace、旧 release、systemd 与 nginx 配置及模块配置链接目标；`1,446,031,360` B、22,325 项文件内容/类型/权限/UID/GID/链接逐项校验通过。SHA256 `4839f3e194baafd107be47ea008d7d55205061a7fc7b3be4e10c010424d94422`；服务器 root 专有目录 `/var/backups/trace-dispatch-lightpanda-20261119`，目录 0700、归档 0600，旧 release 保留。未做实际回滚演练。
- 正式验收：实际加载 binary 与表中摘要一致，服务用户/组、cwd、真实 state/config 路径及主备依赖 Env 正确；HTTP 200，异 Host/Origin 403，公共入口未认证 401。8 份 Run 的业务内容、状态、请求/动作/事件及暂停原因均未变，7 份 JSON 字节一致；1 份历史 completed Run 仅启动读取校验刷新 `updated_at`，对应显示索引只更新同一时间、文件代次与时间字符串长度引起的偏移，各记录摘要逐项验证一致。其余 state/CAS/inputs/config/workspace 内容、权限和属主逐项一致，未恢复暂停任务。
- 首次切换因快照摘要变化停服待核；精确差异和源码路径经独立复核后继续。补验首次还发现时间小数位截尾使显示索引偏移变化，已按时间长度差与每条记录摘要精确核对后完成验收；早期失败和原始前后数据均保留。产品源码和已验收 binary 未为此改动。
- 本次只发布服务器服务及源码；未构建、pack 或发布新的 Docker/TSB 镜像。D1/D3 新真实场次、UI-ZOOM-01 与性能等保留欠项不因上线关闭。以下历史记录中的“未发布”“仅本地更新”均描述各批验收当时，当前状态以本节及版本表为准。

## 主要验收与适用边界

- **11-18 浏览器主备调整**：按用户指令，导航/DOM/表单/JS 优先 Lightpanda；CloakBrowser 0.6.4 保留备用及真实图形渲染/截图/原生缩放。新增独立 localtool，不使用上游内置 Agent 或新增模型循环。仅预装 binary，发现/请求不下载或更新；已执行后错误/uncertain 不跨浏览器自动重放。
- **冻结依赖及许可**：Lightpanda 官方 Linux x86_64 binary 版本 `1.1.0-nightly.12253+166f503a7`，77,989,192 B，SHA256 `bcb4bb1ae80098e9ba7cb47ca69b46dbc8f413176b7987ac5195a392a6cebe3f`；官方源码 `166f503a7b55a287539bf3fd4f18419600c1bb8d`，AGPL-3.0。官方 nightly 下载 URL 可变，本轮冻结实物两端核对摘要，Docker 使用 `docker/lightpanda.sha256` 离线校验，不解析浮动 URL。官方二进制先在服务器下载超时，随后本地仅下载/校验并上传，产品执行均在指定服务器隔离目录。
- **浏览器本轮通过范围**：build/vet；localtool 29 组通过、1 项未改动的 web-analysis runtime 跳过；相关 race 14 组通过、0 跳过；Node 45/45；HTTP 回执 11 和 entrypoint 通过。覆盖真实 Lightpanda CDP 的 DOM/JS、中文 args、只一次 POST、提交后错误/取消不重放、非 JSON 结果、原生 fd1/子进程诊断仅 stderr，以及启动前取消、外部 listener、driver SIGSTOP 后清理、探针后端口接管的负控；普通及 race 均实际执行。原 CloakBrowser headed 真实浏览器、取消和 set_document 回归通过。
- **源码与证据身份**：验收源包 2,937,955 B，SHA256 `dd324df95cda942e16446c8b2da62db71f40db666569d05bb7c8ded90621c8bd`；1206 项清单 SHA256 `9512b639abf493d1d274e283c3f0aca93efab2e979d50ba53c045cf21d58a608`，服务器逐项无差异。binary 构建后唯一变化为 Dockerfile.release 追加 web-analysis Skill COPY；编译输入不变，追加后再次运行 11 回执/构建契约通过。证据包 126,135 B，SHA256 `885a1d3a0cd6d25ab7588f317cbd112958ea9b96d818c3b9d8f92d0743b6cbf8`，两端一致、可解包且 18 项逐哈希核对。原件在项目外 `E:/cli/_work/trace-lightpanda-20261118/evidence-final`，早期原件单独保留。
- **浏览器验收边界**：探针后接管用例是实际 TCP + fake Playwright 的传输所有权负控，不是完整 WebSocket 接管；真实 WebSocket/CDP 正控是另外的 HTTPActions。离线镜像只验源码接线/构建输入契约，未构建、实际 pack 或发布新镜像；未跑 web-analysis runtime、UI 像素回归、模型场次或性能对比。2026-11-18 17:47:25（上海）确认正式仍 optimize-pi-20261117、PID 1251841、原 binary hash，无活动测试单元；未恢复真实暂停 Run。验收后维护文档仅本地同步，正式源码与服务未改。

- **11-18 R3 本轮**：指定服务器独立 `/tmp/trace-acceptance-20261118.dXHggL`，构建/vet、45/45 前端（0 跳过）、123 组 Agent、6 组 Agent race、8 pi、11 个真实 HTTP 回执夹具通过；报告 HTTP 2 组普通与配对 race 通过。R3 Agent、Agent race、pi 三测试二进制与已跑的 R2 产物 SHA256 完全一致。固定 CloakBrowser 0.6.4 headed/Xvfb 的 Workspace、Exploration、旧 P0 三组分别 8.47s、9.07s、19.09s，通过同栏加载/历史、scrollTop=801 深滚、真实 queued ACK、uncertain 隐藏、两图斜向扩大、具体关系鼠标命中/淡化/键盘及原整页无重叠断言。
- **R3 身份及原件**：源码包 2,902,476 B，SHA256 `b44db31f500e2fcde69e61592c67cc2cdf168721638c23238d9c9d9e299a2745`；1198 文件清单 SHA256 `cb9f3cd47aed41f5e82d9f8be8cfb392b9b49377a4bd882b658f040b1e963418`，服务器逐文件无变化。最终证据包 3,163,292 B，SHA256 `994e8baf24c2c101dd4b87c989ed54bc8cebb4429e7259a1912fe68c7dc7db3e`，两端一致，可解包且 55 文件逐哈希核对。原件在本地项目外 `E:/cli/_work/trace-acceptance-20261118/final-evidence`。R2 平行命中和 native200 点击失败、早期 Node 夹具失败均另保留，未用通过日志覆盖失败。
- **D2 R4 补充**：独立复核发现列表把 0/0 当完成，与提交/汇总入口不一致；仅修改 `examples/tsecbench_api.py` 和对应 HTTP 测试，统一为总数大于零且全部提交才完成。列表 0/0 false 接受、0/0 true 明确报错，提交/汇总均保持未完成；独立有界单元运行 11 个回执测试 9.433s 全部通过。R4 验收源码包 2,891,877 B，SHA256 `0e36be1d2518ec677315fe0074d643ae674b833890980c08422303993cf7329f`，1198 文件清单 SHA256 `e008127101981c2fcecb778319cfcdebeb1eaacbb1554cea46e9bc8b0a1dfa72`；逐文件确认仅上述两项变化，R3 原件完整保留。补充证据包 59,693 B，SHA256 `9c9a8b159fbc1fc89b63360611c8f5b4289f99bb92f500a7e28339f42e893c8b`，两端一致且 5 项逐哈希通过；其余代码沿用 R3 已验原件，没有重跑全仓。
- **R3 边界**：process-group 夹具在宿主遇同 UID sd-pam `/proc/environ` 拒绝而失败；源码校验保持，在严格 cgroup 内独立 PID 域用最终 Agent 二进制补验 JSON/SQLite 均通过。正式模式的 cgroup 真实崩溃恢复已单列通过。三个普通视口为 1440/1024/390；原生 200% 实测 DPR=2、CSS 720×439，真实图节点点击、几何和文本流通过，但固定 SDK 的 PNG 呈裁剪迹象，不当完整视觉证明；UI-ZOOM-01 两帧贴尾不随之关闭。平行关系测试通过 DOM 专项关系夹具，非持久化关系投影端到端证明。本轮未跑全仓全部测试、真实新平台场次、性能/收益对照或发布。
- **独立签收**：R3 实现与最终只读审查分开；审查发现的固定版本、单语 CAS 投影、uncertain 摘要计数及零分母问题均已修正。R4 补充只读复核核对 1198 项源码与 5 项证据并关闭 D2；D1/D3 仅签机制/实际请求，模型新真实 Run 行为、平台收益和旧 UI-ZOOM-01 仍开放。独审记录存于项目外 `trace-acceptance-20261118/independent-review.md`。
- **正式状态及清理**：2026-11-18 05:25:57 UTC 只读确认 current 仍指 optimize-pi-20261117、PID 1251841、binary SHA256 与版本表相同；测试单元无活动进程。未恢复/改写正式 Run、未发布或回滚。验收后的五份维护文档仅本地更新；交付源码与 R4 验收清单的差异仅限这些文档，未同步正式服务器源码。

- **11-18 早期工作区候选（历史批次）**：指定服务器前端 41/41、零跳过；`TestWorkspaceBrowserInteractions` 在 CloakBrowser 0.6.4 headed/Xvfb、1440 × 1000、隔离 JSON Store 连续两轮通过。覆盖加载/历史分页和对话内滚动、真实 HTTP 200 `queued` 回执按 operation 回查且输入坐标不变、已显示调用转 `uncertain` 后隐藏且正常卡片与原始动作/事件保留。简图高度 220 → 330 px，完整图 440 → 550 px；两图均核对鼠标/键盘高亮和清除、刷新保留、关系移除后清除。未验证消息实际投递给模型。
- **身份及边界**：原候选源码包 SHA-256 `e20530f713491c3c48fe8790c291e7d70aa22349726c38553bfbf37fc36ffdd4`，本轮唯一产品覆盖文件为 `exploration.js`（SHA-256 `09a345402a8c4fe180191ffde212d278cc6783507ff229bc0313d9794161bb9a`），其余覆盖为三个浏览器夹具文件。私有证据包 `workspace-browser-evidence-20261118.tar.gz` 两端 SHA-256 `74a640ce6a80a04a84986c2389bfc31e736d37553db0fc1a7f8bfec22fb33bf0`，含 `baseline` 旧整页重叠断言失败、`focused-1` 键盘失败、`focused-2` 夹具编译错误、`focused-3` 键盘持续失败以及 `focused-4` / `focused-repeat` 成功回执。该早期批次尚未关闭旧整页失败，且未覆盖非 1440 视口、原生 200%、全仓回归、独立审查和发布；后续覆盖范围以上方 R3/R4 记录为准。

- **11-17 正式优化**：按需 usage fallback，已验证同源子报告免无变化提交，pi 回包复用工具声明。Agent 88 和 picore 8 的普通及配对 race，build/vet，pi 上下文/头/工具差分夹具通过；上线 HTTP/认证/错误 Host、82 工具、4 Skill、43 静态资源核对通过。25 会话中 23 个 JSON 字节一致，两个已完成子 Run 仅时间戳规范化。夹具结果不外推模型延迟或成本。
- **11-17 UI 旧候选**：Go 20 组普通及配对 race、40 份前端 Node、build/vet 通过；固定 CloakBrowser 0.6.4 headed/Xvfb 核对配置保存/默认切换、密钥替换/清除、重启持久、409 草稿和检测隔离。旧右侧布局滚动检查通过；人工 notice 几何不等于真实消息 ACK。本地后续工作区改动不继承该结论。
- **11-17 累计优化**：大历史逐记录读、SQLite 稀疏控制行、归档去重、重复内容复用、派发免空写、pi 头转换复用。229 MiB 单副本 SQLite HTTP 首暂停 104.725 → 17.539 秒仅为当批对照；JSON 仍接近限制，代码收益与资源限额调整分开，不外推并发/WAN。原 B9 三页 p95 996 ms FAIL，单活页 288 ms PASS 不替代。兼容性：SQLite schema 1/2 → 3 为 DDL，不搬历史，旧 SQLite controller 必须先停。
- **11-13 至 11-12**：自动续行、遗留动作核对、答题中断和结果纠偏、暂停会话删除及回退专项已按各批上线记录发布。T3 消息幂等、SQLite 功能、浏览器与报告隔离等历史测试各有限定；不视为本轮全仓回归。
- **11-12 TSB 镜像**：38 层，971795456 B，整包 SHA-256 `899d38976241e5e895e9d07d0d70fc44468e3e91305be144aac7cc9aa7e17431`；历史整包及闭包已验，当前本地旧 tar 未见。普通 Docker 合成启动/生命周期、离线浏览器和归档完整性通过，但没有正式平台导入/VPN/模型网关验收。镜像采用显式 process-group，不提供逐命令内存/PID 硬隔离；正式服务保持 cgroup。

## 发布、存储与回滚

1. 冻结源码和依赖；在指定服务器使用独立目录、配置、state 和资源域运行构建与验收。保留源码/二进制清单、逐测试退出码和失败原件。浏览器使用固定 binary，不自动下载。
2. 正式发布需明确指令。停旧 controller 后备份整份一致 state（涵盖 CAS/inputs）与旧 release，逐文件验 hash，再切换 current。核对权限、实际加载 binary、服务、配置和状态；勿用正式数据跑隔离夹具。
3. 回滚先停新 controller，再从一致停服备份恢复完整旧 release 与相匹配的 state、配置及 CAS；禁止双版本混跑或只换 binary。若无实际迁移或新版本期间产生数据，仍先按现场确定一致性和保留策略，不覆盖新用户数据。SQLite 降级用迁移前备份，或按新版导出路径重建且保留 CAS/inputs；JSON 导出本身不是全备份。
4. 已清理的旧 release 路径不作为回滚来源。11-17 已核对保留的完整旧 release 和一致 state 回滚归档；本批未做回滚演练。前一批 dispatch-pi 发布的属主权限失败已留档，先恢复旧版再修新目录，未改共享 runtime。

详细旧记录及失败边界保留在本地私有 Markdown 历史和原验收回执中；不随源码发布。
