# Trace 交接

更新：2026-11-20。

- 先读 [agent.md](agent.md)，再按 [开发计划.md](开发计划.md) 的逐项验收条件推进。Trace 是通用求解 Agent；pi、ARTEX、Muteki 为第一参考项。构建、运行、测试仅在指定服务器。
- 当前正式 `mcp-catalog-20261119`，2026-11-20T10:17:20.303266+08:00（上海）发布，binary `cfd339039bf165bbfacce5aee44cb5460058e7c00f3c2208bd51210ded77a838`。本地命令从卡片/接口选择器隐藏，只留真实 MCP 与两浏览器扩展，开关持久化；停用选择恢复、秘密身份与刷新失败旧目录已修复。浏览器先 Lightpanda、备用 CloakBrowser，HTTP/受管 curl 使用 curl_cffi 0.17.0；12 个保存 helper 精确迁移。正式环境 3 次回环 GET、实际进程/HTTP/鉴权及 7 份历史 Run 业务核对通过，暂停未恢复。专项验收与边界见 DEPLOYMENT.md；未构建新镜像或启动新模型场次。
- 前批正式服务 `runtime-audit-20261119`：五类运行时缺陷的逐项条件见开发计划；RuntimeAudit 22、相关 148、race 21、HTTP 6 组及 vet/build 全绿，独立审查无剩余 P0/P1。完整备份与实际进程/HTTP/工具校验通过，7 份历史 Run 业务不变、暂停未恢复；本轮未跑全仓、新真实场次、前端或新镜像。refs 五项更新、三项当前，ARTEX 原远端例外和社区来源已记录。
- 11 月 19 日前批清理已完成，当时仅保留当前 release 和最新完整回滚归档；本次发布保留上一 release 用于回滚。三份旧 release、两份过期备份、旧验收目录/临时文件、未使用的旧镜像及 VPN 配置已清理，服务器释放约 8.11 GiB。无 VPN 进程/隧道/额外策略规则，OpenVPN 总服务禁用。本机十批临时副本、系统 Trace 临时文件和孤立测试 .env 已移入回收站，实际约 295.10 MiB；121 份精简证据迁至 `_records/trace-20261119`。保留源码/.git/refs/回归测试与正式数据；清理记录见部署文档。
- 前批 `workspace-layout-20261119` 的布局与通用调度随正式版保留；binary、备份与上线身份见 [DEPLOYMENT.md](DEPLOYMENT.md)。容量与计划同一 Store 事务提交；首个模型根计划必须显式设容量或沿用已配置容量，超过上限的任务保持 pending，不创建空等 Worker。容量来自已观察资源约束，不硬编码平台配额。
- 已执行子任务缺业务声明时，沿现有审计请求补交来源绑定结果；该回合零工具且拒绝实际工具执行。诚实的部分成果保留 incomplete，strict/requirements/predicate 及无效引用仍受原验收约束。结算回执分列运行、业务和清理状态，普通根聊天不增加结构化要求。
- 唯一真实 #notice 位于运行对话同栏顶部；补充指令、附件、类型和 mailbox 回右栏，外层滚动不压缩对话。图拖动放大/链路高亮保留；Lightpanda 优先、CloakBrowser 备用保持原配置。
- 前批验收：服务器后端专项、相关 race、真实 pi 补交负控、报告下载 HTTP、vet/build 通过；42 份 Node、前端相关 Go 与四组 CloakBrowser headed/Xvfb 通过，覆盖 390/1024/1440 视口与原生 200%。独立最终审查无剩余 P0/P1。未重跑全仓、真实模型场次或新建 Docker/TSB 镜像；历史 UI-ZOOM-01 两帧专项和其余欠项仍按开发计划。
- 前批停止样本确证：首次计划遗漏 capacity 继承默认五，外部容量三未进入准入；根生成资源不足长轮询。部分 Worker 自然语言交回已有结果，缺 trace_result 导致 completed/incomplete；本场已核对四次实际接受后关闭，未从这些记录确证未提交即关闭。历史样本未回写、未重放，编排者继续负责分发、验收与报告。
- 前批发布：一致备份与逐项内容/权限/属主校验通过；正式 HTTP、实际进程、工具/Skill、Host/Origin 与入口鉴权通过。7 份 Run 业务内容、请求/动作/事件不变，仅允许启动读取刷新 updated_at；暂停任务未恢复。私有证据在项目外 `_records/trace-20261119/trace-workspace-layout-20261119`，日志根因在 `_records/trace-20261119/trace-scheduler-20261119/root-cause.redacted.json`。
- 本地完整 Git 为 `E:/cli/trace/.git`，远端 `chAng-L19/Trace`、分支 `main`；本批提交身份以实际推送后 main SHA 和项目外 Git 回执为准。
