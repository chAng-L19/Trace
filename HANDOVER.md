# Trace 交接

更新：2026-11-19。

- 先读 [agent.md](agent.md)，再按 [开发计划.md](开发计划.md) 的逐项验收条件推进。pi、ARTEX、Muteki 为第一参考项，复用保留许可与来源；构建、运行和测试仅在指定服务器。
- 正式服务已发布 `dispatch-lightpanda-20261119`；调度/完成度、编排职责、报告归档/固定下载、同栏顶部消息窗、uncertain 对话隐藏、探索图拖拽/精确关系高亮及 Lightpanda 优先、CloakBrowser 备用均随本次上线。binary、备份和验收身份只记 [DEPLOYMENT.md](DEPLOYMENT.md)。
- 发布前完整一致备份及逐项校验通过。正式 HTTP、工具/Skill、实际进程身份与入口鉴权通过；8 份 Run 业务内容、请求/动作/事件不变。一个历史 completed Run 的启动读取时间戳刷新及派生索引变化已逐字段核对，未恢复真实暂停任务。首次停服待核和后续补验失败记录保留在项目外发布证据中。
- 浏览器完整候选验收为 29 localtool/14 race/45 前端/11 回执及 entrypoint；1 项未改动 web-analysis runtime 跳过。发布前用最终 release 路径复验两组真实浏览器均通过。新发布没有重跑全仓测试、真实模型场次或 UI 像素专项，也未构建新 Docker/TSB 镜像。
- 原历史八 Run 中根 operator_pause/incomplete、七 Worker 已结算，根未到最终报告阶段；b-01/b-03 为已提交 1/4 后关闭。D1/D3 新真实场次、UI-ZOOM-01 两帧贴尾、B9 性能及其余欠项继续按开发计划验收，不从夹具或上线状态外推完成。
- 私有发布证据在项目外 `_work/trace-release-20261119`；前批证据在 `_work/trace-acceptance-20261118` 与 `_work/trace-lightpanda-20261118`。GitHub 源码仓库 `chAng-L19/Trace`，分支 `main`，GitHub 提交身份以推送后 main 的实际 SHA 为准。
