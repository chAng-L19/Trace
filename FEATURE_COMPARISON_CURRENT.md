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
