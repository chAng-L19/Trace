# Trace 参考实现与取舍

固定版本表示实际审查基线，不代表上游最新。采用前核对版本、许可、文件与测试；以下是吸收范围，不是待开发的功能清单。

| 来源与固定版本 | 有效参考/Trace 落点 | 不搬运的范围 |
| --- | --- | --- |
| pi `1f6f319bb28992a40b99c147fffb23b65a23129d`，MIT；另只读检查 `4afe78de1e51162c01c946dcf080e6a266685a01` | `packages/agent/src/agent-loop.ts`：单循环、工具结果先于终止判定、steering；Trace 依赖仍固定 1.4.0，复用稳定历史转换与工具差分 | 不装 pi CLI，不启动第二循环，不在当批升级依赖 |
| ARTEX `e6ec56999175509551a1e43a3ee3b053d0941bd9`，AGPL-3.0；远端刷新曾失败 | `web/src/components/exploration-graph.tsx` computeLayout 布局、`agent/planner.go` 变更通知及持久化免空写原则；Trace 用原 DOM、原 Store/等待 API，保留来源署名 | 不搬第二调度器或图账本；不为许可不明确的代码直接复制 |
| Muteki `a73535a19fd2c1cddd97dfbeea65f62f4ce136ee`，AGPL-3.0；旧 UI 基线 `144df7666dc78ef331e7746357d31c9e0f57c558` | `coordinator_completion.py` / `coordinator_finalize.py` 对照状态与完成判据；历来参考 `RunWorkbench.tsx`、`ConversationWorkspaceDock.tsx`、`EvidenceChain.tsx` 的分栏和就地查看 | 不移植 CTF 引擎或把模型文本当外部回执 |
| CC Switch `d243d970af87e13a243b005d6af73d0375050486` | ProviderCard、EditProviderDialog、EndpointSpeedTest；保留配置列表/编辑、保存/默认独立和检测就地反馈 | 不引入新框架、依赖或设置存储 |
| Multica 本地 `a8a31668c231be92caf24643ef9d97868286aa24` | `server/cmd/server/task_dispatch_test.go`：独立核对 dispatch、claim 与状态 | 参考许可证含附加条款；不移植队列；Trace 已有 DAG、ClaimTask/WaitWorkers |
| DeepSeek Harness `571902028a75c64c1d767f4ae5fd099485f0ddf0`（早期），`39163dd30f703a79079e2d2446545cff3c453738`（11 月 16 日）；DSH 原型 `8c32a9dc5d72c033816c6a8110b37f40cf94fba4` | 对话展示与消息身份、提交/投递/消费分账、角色选模、低噪音协作 | 不新增独立执行、重试体系、票据域模型 |
| Cairn `8e7e0ea67552383851dfcabfba0c4e9c8d007878`、LuaN1aoAgentv2 `a035429766746866bcab30550cd0aac9c539be04` | 图节点详情、上下游定位、原始产物只读投影 | 不新增图存储或用引用重放动作 |
| sapper-v2、AWPT（固定样本及详细记录见私有历史） | 多词排序、有限方法召回和结构化来源提示；AWPT 只重新表述思路 | 不迁入全局可写知识、tmux watchdog、通用重试、整套扫描器或无明确许可源码 |
| REA `a2d8b94dd50dab8d966239dd63ab84718aa8b4b4`，MIT | 固定 Web/JS/Electron 子集；来源索引 `tools/web-analysis/UPSTREAM.json` | 不恢复完整 REA Runtime、IDA 或旧实时轨迹 |

Trace 自有结果与报告仍须校验原始来源及资源清理，审阅、文件存在或退出 0 不替代业务成功。参考概念、移植代码、验收证据分别记账。待实施或待验收以 [开发计划.md](开发计划.md) 为准。

2026-11-18 归档读取对照上述固定版本：Muteki `apps/web/ui/components/studio-prototype/real/reports.ts` 分开冻结报告、草稿和提交账本；ARTEX `web/src/app/(main)/function/tasks/detail/_tabs/report-tab.tsx` 有正文才显示复制操作；pi `packages/coding-agent/src/core/export-html/index.ts` 从既有会话条目构造独立导出。Trace 继续使用既有报告回执和 CAS；未移植参考代码。

2026-11-18 本轮第一参考项为本地 `_refs/pi`、`_refs/ARTEX`、`_refs/muteki`，HEAD 分别与上表固定版本一致。现成实现可直接采用，沿既有架构适配并保留许可/来源。Muteki `coordinator_completion.py` 的 SAVE/FINISH 分离及平台确认前提用于多 flag 回执判定，`coordinator_schedule_dispatch.py` 的任务所有者和 `coordinator_finalize.py` 的交付用于根职责/报告对照；ARTEX `computeLayout` 用于分层、长边走廊和关系身份；pi `agent-loop.ts` 继续作为工具结果先结算再终止的唯一循环。此次新增逻辑是 Trace 现有结构上的适配，未搬入第二调度器、Store 或 CTF 引擎。服务器验收结果按开发计划与部署文档单列。

本轮图鼠标实测暴露：平行 Bézier 路径虽不同，16px 命中笔画仍可重叠。沿 ARTEX 的既有分层与重心排序，参考 Cairn 固定版本 Dagre 独立轨道/edgesep=20 原则适配相邻列平行边，保留命中宽度及单边曲线；未引入 Dagre 库。R3 验收包含两图两关系的真实半弧长中点唯一命中域、反序/坡向几何、长边避节点与清除负控。短视口结合 ARTEX 视口预算和 Muteki 原生 scroll-padding 处理固定输入遮挡。具体通过范围及截图限制见部署说明。

2026-11-18 浏览器增量：按用户指定以 Lightpanda 为首选、CloakBrowser 备用。检索 pi/ARTEX/Muteki 未发现可直接复用的 Lightpanda/CDP 适配；参考 Muteki task-owned browser 生命周期，复用 Trace 原 localtool/JSONL/Worker，而非复制上游内置 Agent/MCP 循环。官方 Lightpanda 源码固定 `166f503a7b55a287539bf3fd4f18419600c1bb8d`，CLI/CDP 仅做最小 Python 协议适配；冻结 binary 版本/hash、真实验收及发布边界见 DEPLOYMENT.md。网页像素渲染、web-analysis 场景采集和 UI 验收仍采用备用 CloakBrowser，不宣称 Lightpanda 等价图形浏览器。

2026-11-19 通用调度修复对照：ARTEX `e6ec56999175509551a1e43a3ee3b053d0941bd9` 的 `server/conversations.go:622` 在锁内按空闲容量启动、结束后再补位，以及 `db/tasks.go:439` 重复排队保留原顺序；Trace 用原 Store 事务把 plan 与 capacity 同时发布，沿用 ClaimTask/SubscribeScheduling，不增加参考项目的第二队列。Muteki `a73535a19fd2c1cddd97dfbeea65f62f4ce136ee` 的 `muteki/solver/worker_result.py:1`、`:19` 将来源绑定声明与结束交接分开，适配到现有子运行结果/结算回执；不引入 competition 实现。pi `1f6f319bb28992a40b99c147fffb23b65a23129d` 的 `packages/coding-agent/src/core/tools/file-mutation-queue.ts:29` 保留真实操作结束后再释放资源的顺序；Trace 保留原 cleanup/uncertain 约束，不以取消请求当释放证明。以上为固定版本的机制适配，未复制额外运行循环、任务存储或平台流程。

## 2026-11-19 运行时审计参考刷新

本批对 `_refs` 九个 Git 项目逐项执行 fetch 并仅快进干净分支；detached 项目按原 origin 默认分支更新。pi 更新至 `5a14e69c067d40dfbff4f2fff7d3b34c897e2dc9`；CC Switch 至 `6a6ad60e7d6bf97d1cbb82c3149af373afbd97b7`；Multica 至 `528cd82711f2f99eedd04e4d3a68c6c31b514659`；DeepSeek Harness 至 `2e39e70496aa02986ca6a820c5d497e816b69d7e`；sapper-v2 至 `7b75e6f1f5d5b4ae0c55878b0dd0994d9b08f1fb`。Muteki、Cairn、DSH 原型与上表版本一致。上表及较早段落继续表示对应历史审查基线；刷新参考源码不升级产品 pi 1.4.0 或冻结浏览器依赖。

ARTEX 原 `Autumn-27/ARTEX` 远端返回 repository not found；保留干净的 `main` 缓存 `e6ec56999175509551a1e43a3ee3b053d0941bd9`，新增 `community` 远端并 fetch `RuoJi6/ARTEX` 的 `community/main` 为 `6997a4661921395072290becc50380db8dacb04e`（AGPL-3.0）。社区分支与原上游身份分别记录，不改写历史来源；可用 `git show community/main:server/engine.go` 读取本轮对照。

本轮入口/状态链对照：ARTEX 社区 `server/task_control.go` 的统一控制与显式准入、`server/engine.go` 领取前终态检查，和 `server/conversations.go:750` 在停止后不消耗排队任务；pi `packages/agent/src/agent.ts` 的 abort 与实际退出分离、`agent-loop.ts` 的本轮消息与工具结果分列，以及 `agent-session.ts` 的上下文恢复；Muteki `muteki/src/reports.py` 冻结来源与渲染正文分离、`muteki/swarm/worker_session.py` 的退出分类。Trace 沿既有 Store 事务、访次结算和 pi 恢复路径适配，不迁入新队列、状态字段或执行循环。验收结果按开发计划与部署文档记录。

2026-11-19 外部工具目录对照：ARTEX 缓存 `e6ec56999175509551a1e43a3ee3b053d0941bd9` 的 `web/src/app/(main)/system/tools/page.tsx` 源项目卡片与详情入口、Muteki `a73535a19fd2c1cddd97dfbeea65f62f4ce136ee` 的 `apps/web/ui/components/ExtensionSettings.tsx` 注册/协议观察分列。Trace 沿现有 local-tools/settings 渲染仅展示真实 MCP 与 Lightpanda/CloakBrowser 浏览器扩展，本地命令无论是否有 GitHub 来源均隐藏，既有选择合并按可见 ID 更新并保留隐藏选择；复用既有实现，不新增适配器或依赖。
