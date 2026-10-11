"use strict";

(function attachTracePresentation(global) {
  const labels = {
    objective: "任务目标", targets: "目标范围", target: "目标", constraints: "约束",
    success_predicates: "成功条件", statement: "描述", description: "描述", text: "内容",
    status: "状态", status_code: "HTTP 状态码", http_status: "HTTP 状态码",
    tool: "工具", tool_name: "工具", tool_calls: "工具调用", arguments: "参数",
    input: "输入", output: "输出", result: "结果", results: "结果", error: "错误",
    reason: "原因", finish_reason: "结束原因", method: "请求方法", url: "URL",
    path: "路径", headers: "响应头", request_headers: "请求头", body: "响应内容",
    response_body: "响应内容", request: "请求", response: "响应", command: "命令",
    stdout: "标准输出", stderr: "错误输出", exit_code: "退出码", preview: "内容预览",
    truncated: "内容截断", byte_count: "字节数", elapsed_ms: "耗时（毫秒）",
    duration_ms: "耗时（毫秒）", timing: "耗时", baseline: "基线", observed: "实测结果",
    different: "存在差异", changed_fields: "变化字段", comparison: "对照结果",
    enumeration: "枚举结果", routes: "路由", paths: "路径", endpoints: "端点",
    urls: "URL", parameters: "参数", assets: "资产", count: "数量", sample: "样本",
    complete: "结果完整", coverage: "覆盖范围", payload: "记录内容", metadata: "补充信息",
    confidence: "置信度", verified: "已验证", verifier: "验证器", trust: "可信状态",
    artifact_type: "产物类型", media_type: "文件类型", content_hash: "内容 SHA-256",
    sha256: "SHA-256", input_hash: "输入摘要", output_hash: "输出摘要",
    artifact_id: "产物编号", artifact_ref: "产物引用", evidence_id: "证据编号",
    evidence_refs: "证据引用", evidence_ids: "证据引用", parent_ids: "上游证据",
    artifact_refs: "产物引用", action_id: "动作编号", attempt_id: "尝试编号",
    request_id: "请求编号", call_id: "调用编号", run_id: "运行编号", branch_id: "分支",
    created_at: "记录时间", source_id: "来源编号", source: "来源", capabilities: "能力",
    structured_output: "模型决策", structured_summary: "结果摘要", impact: "影响",
    satisfied: "已满足条件", missing: "未满足条件", next_action: "下一动作",
    missing_capabilities: "缺少能力", summary: "摘要", name: "名称", type: "类型",
    asset_ids: "关联资产", finding_ids: "关联发现", relationship_ids: "关系引用",
    reproduction_evidence_ids: "复现证据", impact_evidence_ids: "影响证据",
    negative_control_evidence_ids: "负向对照证据", cleanup_evidence_ids: "清理证据",
    attributes: "资产属性", version: "版本", schema_hash: "参数 schema 摘要",
    estimated_context_tokens: "估算上下文 token", provider_context_tokens: "服务商上下文 token",
    selected_tokens: "已选上下文 token", reserved_output_tokens: "预留输出 token",
    projection_bytes: "上下文字节数", cache_read_tokens: "缓存读取 token",
    cache_write_tokens: "缓存写入 token", context_overflow_tokens: "溢出 token",
    compaction_ids: "压缩记录", overflow_retry: "溢出重试次数", token_projection: "上下文用量",
    input_tokens: "输入 token", output_tokens: "输出 token", total_tokens: "总 token",
    requests: "请求数", missing_requests: "缺失用量的请求数", usage_complete: "用量完整",
    known_totals: "已知用量合计", missing_fields: "字段缺失次数", usage: "用量",
    root: "数据目录", database: "数据库", python: "Python 版本", shell: "Shell",
    platform: "平台", platform_release: "系统版本", managed_mcp_config: "MCP 配置路径",
    modalities: "输入模态", max_context_tokens: "上下文 token 上限",
    native_system_role: "原生系统角色", native_tool_calls: "原生工具调用",
    parallel_tool_calls: "并行工具调用", structured_output_support: "结构化输出",
    streaming: "流式响应", usage_reporting: "用量回报", configured: "已配置", ready: "就绪",
    protected: "保留内容", _omitted_keys: "省略字段数", _omitted_items: "省略条目数",
    head: "开头预览", tail: "结尾预览", output_bytes: "输出字节数",
    line_count: "行数", truncation_reason: "截断原因", head_bytes: "开头字节数",
    tail_bytes: "结尾字节数", from: "来源", to: "关联对象", kind: "类型",
    tactical_update: "当前策略", active_hypothesis_id: "当前假设", current_task: "当前任务",
    hypothesis_id: "假设编号", tested_domain: "已测试范围", observations: "实测观察",
    commit_lifecycle_gate: "提交当前阶段证据", terminal: "任务已结束", success: "成功条件满足",
    priority: "优先级", intent_id: "意图编号", parent_intent_id: "上游意图",
    record_id: "记录编号", parent_record_ids: "上游记录", plan_revision: "计划版本",
    action_fingerprint: "动作指纹", runtime_branch_id: "执行分支", session_branch_id: "会话分支",
    focus: "当前探索方向", authority: "来源权威", selected: "已选工具", visible: "提供给模型",
    expanded: "目录已展开", enabled: "已启用", input_schema: "输入字段", retryable: "可重试",
    missing_dependencies: "缺少依赖",
  };
  const states = {
    created: "已创建", running: "运行中", completed: "已完成", failed: "失败",
    cancelled: "已取消", cancelling: "取消中", waiting_worker: "等待执行环境",
    paused_budget: "预算暂停", candidate: "待验证", verified: "已验证",
    unverified: "未验证", observed: "已观察", proposed: "待验证", active: "进行中",
    suspended: "已暂停", closed: "已关闭", reopened: "已重开", success: "成功",
    succeeded: "成功", ok: "成功", error: "错误", blocked: "受阻", pending: "等待中",
    critical: "严重", high: "高", medium: "中", low: "低", info: "信息",
    dependency_unavailable: "依赖不可用", explicit_expand: "已按请求展开目录",
    capability_match: "匹配本次任务所需能力", builtin_default: "默认提供的内置工具",
    read_only_default: "默认提供的只读工具", deferred_until_expand: "等待展开工具目录",
    provider_not_configured: "尚未配置模型", missing_credentials: "缺少凭据",
    service_closing: "服务正在关闭", await_model: "等待模型", unknown: "未知",
    goal_predicates_pending: "成功条件仍待验证", goal_contract_satisfied: "任务成功条件已满足",
    asset_to_path: "资产关联路径", finding_to_path: "发现关联路径", evidence_to_path: "证据支持路径",
    parent: "上游证据", model_declared_not_evidence: "模型声明，未作为证据",
  };
  const kindNames = {
    tool_output: "工具输出", model_tool_result: "工具调用结果", http_response: "HTTP 响应",
    "http-response": "HTTP 响应", "verified-response": "已验证响应",
    response: "响应", screenshot: "截图", report: "报告", request: "请求",
    file: "文件", evidence: "证据", model_request: "模型请求", model_response: "模型回复",
  };
  const present = (value) => value !== undefined && value !== null && value !== "";
  const state = (value) => states[value] || value;
  const kind = (value) => kindNames[value] || value || "记录";
  const node = (tag, className = "", text) => {
    const element = document.createElement(tag);
    if (className) element.className = className;
    if (present(text)) element.textContent = String(text);
    return element;
  };

  function decode(value) {
    if (typeof value !== "string" || !/^[\s]*[\[{]/.test(value)) return value;
    try { return JSON.parse(value); } catch { return value; }
  }

  function copy(container, text) {
    if (present(text)) container.append(typeof text === "string" && /^\s*[\[{]/.test(text)
      ? readableValue(text) : node("div", "presentation-copy", text));
  }

  function readableValue(value, key = "") {
    value = decode(value);
    if (value === null || value === undefined) return node("span", "presentation-muted", "未提供");
    if (typeof value === "boolean") return node("span", "", value ? "是" : "否");
    if (Array.isArray(value)) {
      if (!value.length) return node("span", "presentation-muted", "无");
      const list = node("ul", "presentation-values");
      for (const item of value) {
        const entry = node("li");
        entry.append(readableValue(item, key));
        list.append(entry);
      }
      return list;
    }
    if (typeof value === "object") {
      const list = node("dl", "presentation-fields");
      for (const [name, child] of Object.entries(value)) field(list, labels[name] || name, child, name);
      if (!list.children.length) return node("span", "presentation-muted", "无字段");
      return list;
    }
    if (typeof value === "string" && /^\s*(?:\{\s*"|\[\s*[\{"\[])/.test(value))
      return node("span", "presentation-muted", "结构化内容不完整");
    if (typeof value === "string" && /^(?:evidence_id|evidence_refs|evidence_ids|parent_ids|.*_evidence_ids)$/.test(key)) {
      const link = node("a", "presentation-download", value);
      link.href = "#evidence";
      link.dataset.evidenceRef = value;
      link.title = "查看证据正文与来源";
      return link;
    }
    const translated = ["status", "reason", "severity", "trust", "next_action", "kind", "authority"].includes(key)
      ? state(value) : key === "artifact_type" ? kind(value) : value;
    return node("span", "presentation-value", translated);
  }

  function field(list, title, value, key = "") {
    if (value === undefined || value === "") return;
    const detail = node("dd");
    detail.append(readableValue(value, key));
    list.append(node("dt", "", title), detail);
  }

  function fields(container, values) {
    const list = node("dl", "presentation-fields");
    for (const [key, value] of Object.entries(values)) field(list, labels[key] || key, value, key);
    if (list.children.length) container.append(list);
    return list;
  }

  function details(container, title, value) {
    if (value === undefined || value === null || value === "") return;
    if (typeof value === "object" && !Object.keys(value).length) return;
    const disclosure = node("details", "presentation-details");
    disclosure.append(node("summary", "", title), readableValue(value));
    container.append(disclosure);
    return disclosure;
  }

  function section(container, title, subtitle) {
    const band = node("section", "presentation-section");
    band.append(node("h3", "", title));
    if (present(subtitle)) band.append(node("div", "presentation-muted", subtitle));
    container.append(band);
    return band;
  }

  function record(container, title, subtitle) {
    const entry = node("article", "presentation-record");
    const header = node("header", "presentation-heading");
    header.append(node("h4", "", title));
    if (present(subtitle)) header.append(node("span", "presentation-muted", subtitle));
    entry.append(header);
    container.append(entry);
    return entry;
  }

  function reset(container) {
    container.replaceChildren();
    container.classList.add("readable-view");
    container.classList.remove("json-view");
  }

  function empty(container, text) { container.append(node("div", "presentation-empty", text)); }

  function artifactLink(container, runId, artifact) {
    const id = artifact.artifact_id || artifact.artifact_ref;
    if (!runId || !id) return;
    const link = node("a", "presentation-download", "下载产物");
    link.href = `/api/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(id)}/content`;
    link.download = id;
    container.append(link);
  }

  function renderTools(container, result) {
    reset(container);
    const visibility = result.visibility || [];
    const signatures = result.tools || {};
    const catalog = section(container, "本次任务的工具目录");
    fields(catalog, { "可见工具": visibility.filter((item) => item.visible === true).length,
      "目录已展开": result.expanded, "提示字节数（估算）": result.estimated_prompt_bytes });
    if (!visibility.length) empty(catalog, "尚无工具可见性记录");
    for (const item of visibility) {
      const signature = signatures[item.tool] || {};
      const entry = record(catalog, item.tool, item.visible === true ? "已提供给模型" : item.visible === false ? "未提供给模型" : "可见状态未提供");
      copy(entry, state(item.reason));
      fields(entry, { capabilities: signature.capabilities, version: signature.version,
        source: signature.source, "会产生副作用": signature.side_effecting,
        "支持执行状态核对": signature.supports_reconcile,
        missing_dependencies: signature.missing_dependencies });
      details(entry, "目录引用", { schema_hash: signature.schema_hash,
        "描述摘要": signature.description_hash, "目录版本": result.revision });
    }
  }

  function finding(container, value) {
    const entry = record(container, value.title, [state(value.severity), state(value.status)].filter(Boolean).join(" · "));
    copy(entry, value.description);
    fields(entry, { target: value.target, asset_ids: value.asset_ids,
      reproduction_evidence_ids: value.reproduction_evidence_ids, impact_evidence_ids: value.impact_evidence_ids,
      negative_control_evidence_ids: value.negative_control_evidence_ids, cleanup_evidence_ids: value.cleanup_evidence_ids });
    details(entry, "发现引用与补充信息", { "发现编号": value.finding_id, metadata: value.metadata });
    return entry;
  }

  function renderAttack(container, result, showFindings = true) {
    reset(container);
    const paths = result.attack_paths || [];
    const assets = result.assets || [];
    const findings = result.findings || [];
    const assetsById = new Map(assets.map((item) => [item.asset_id, item]));
    const findingsById = new Map(findings.map((item) => [item.finding_id, item]));
    const pathSection = section(container, "攻击路径", `${paths.length} 条路径 · ${assets.length} 个资产 · ${findings.length} 项发现`);
    if (!paths.length) empty(pathSection, "尚未记录攻击路径");
    for (const path of paths) {
      const entry = record(pathSection, path.title, state(path.status));
      const relation = node("dl", "presentation-fields attack-relations");
      field(relation, "关联资产", (path.asset_ids || []).map((id) => {
        const asset = assetsById.get(id);
        return asset ? `${asset.name}${asset.target ? ` · ${asset.target}` : ""}` : `${id}（当前响应未包含资产详情）`;
      }));
      field(relation, "关联发现", (path.finding_ids || []).map((id) => {
        const value = findingsById.get(id);
        return value ? `${value.title} · ${state(value.severity)} · ${state(value.status)}` : `${id}（当前响应未包含发现详情）`;
      }));
      field(relation, "影响", path.impact);
      field(relation, "支持证据", path.evidence_ids, "evidence_ids");
      entry.append(relation);
      details(entry, "路径引用与补充信息", { "路径编号": path.path_id,
        relationship_ids: path.relationship_ids, metadata: path.metadata });
    }
    const assetSection = section(container, "关联资产");
    if (!assets.length) empty(assetSection, "暂无资产记录");
    for (const asset of assets) {
      const entry = record(assetSection, asset.name, [asset.asset_type, state(asset.status)].filter(Boolean).join(" · "));
      fields(entry, { target: asset.target, evidence_ids: asset.evidence_ids });
      details(entry, "资产属性与引用", { "资产编号": asset.asset_id, attributes: asset.attributes });
    }
    if (showFindings) {
      const findingSection = section(container, "已记录的发现");
      if (!findings.length) empty(findingSection, "暂无发现记录");
      findings.forEach((value) => finding(findingSection, value));
    }
    details(container, "证据来源与图关系", { "来源证据": result.source_evidence_ids, "关系": result.edges });
  }

  function renderEvidence(container, records) {
    reset(container);
    if (!records.length) return empty(container, "暂无证据");
    for (const value of records) {
      const verification = value.verified === true ? "已验证" : value.verified === false ? "未验证" : "验证状态未提供";
      const entry = record(container, kind(value.artifact_type), verification);
      entry.dataset.evidenceId = value.evidence_id || "";
      fields(entry, { target: value.target, tool: value.tool, trust: value.trust,
        verifier: value.verifier, confidence: value.confidence == null ? value.confidence : `${Math.round(value.confidence * 100)}%` });
      if (value.payload !== undefined) {
        const payload = section(entry, "观察内容");
        payload.append(readableValue(value.payload));
      }
      details(entry, "证据来源与引用", { evidence_id: value.evidence_id, action_id: value.action_id,
        parent_ids: value.parent_ids, content_hash: value.content_hash,
        created_at: value.created_at, "执行来源": value.provenance, metadata: value.metadata });
    }
  }

  function renderArtifacts(container, artifacts, runId) {
    reset(container);
    if (!artifacts.length) return empty(container, "暂无产物");
    for (const artifact of artifacts) {
      const entry = record(container, kind(artifact.artifact_type), artifact.media_type);
      fields(entry, { byte_count: artifact.byte_count, tool_name: artifact.metadata?.tool_name,
        created_at: artifact.created_at });
      if (artifact.preview !== undefined && artifact.preview !== null) {
        const preview = section(entry, "内容预览");
        const data = decode(artifact.preview);
        if (data && typeof data === "object" && !Array.isArray(data) && ("head" in data || "tail" in data)) {
          const { head, tail, ...stats } = data;
          const json = artifact.media_type === "application/json";
          if (!json || data.truncated !== true) {
            if (present(head)) preview.append(readableValue(head));
            if (data.truncated && present(tail)) preview.append(readableValue(tail));
          } else copy(preview, "结构化产物预览已截断");
          fields(preview, stats);
        } else preview.append(readableValue(data));
      }
      artifactLink(entry, runId || artifact.run_id, artifact);
      details(entry, "产物来源与引用", { artifact_id: artifact.artifact_id || artifact.artifact_ref,
        content_hash: artifact.content_hash, metadata: artifact.metadata });
    }
  }

  function renderTerminal(container, terminal, criteria = []) {
    const band = section(container, "任务结论");
    const byId = new Map(criteria.map((item) => [item.criterion_id, item.statement]));
    const describe = (ids) => ids?.map((id) => byId.get(id) || id);
    const concluded = terminal.terminal === true;
    copy(band, concluded ? terminal.success === true ? "已结束，成功条件已满足" : "已结束，成功条件未满足" : terminal.terminal === false ? "尚未形成最终结论" : "结论状态未提供");
    fields(band, { reason: terminal.reason, satisfied: describe(terminal.satisfied),
      missing: describe(terminal.missing), evidence_refs: terminal.evidence_refs });
    details(band, "结论补充信息", terminal.metadata);
  }

  function renderReport(container, result) {
    reset(container);
    const view = result.run || {};
    const run = view.run || {};
    const goal = view.goal || {};
    const overview = section(container, goal.objective || "任务报告", state(run.status));
    fields(overview, { targets: goal.targets, next_action: view.next_action,
      missing_capabilities: view.missing_capabilities });
    renderTerminal(container, result.terminal || view.terminal || {}, goal.criteria || []);
    const findings = section(container, "有证据支持的发现");
    if (!result.findings?.length) empty(findings, "暂无有证据支持的发现");
    for (const value of result.findings || []) {
      const entry = finding(findings, value);
      fields(entry, { "复现步骤": value.reproduction_steps, impact: value.impact, "修复建议": value.remediation });
    }
    const paths = section(container, "攻击路径与资产");
    renderAttack(paths, result, false);
    const evidenceSection = section(container, "支撑证据");
    const evidenceBody = node("div");
    evidenceSection.append(evidenceBody);
    renderEvidence(evidenceBody, result.evidence?.nodes || []);
    const artifactSection = section(container, "交付产物");
    const artifactBody = node("div");
    artifactSection.append(artifactBody);
    renderArtifacts(artifactBody, result.artifacts || [], run.run_id);
    details(container, "报告引用", { run_id: run.run_id, "报告摘要": result.report_hash });
  }

  function renderTransparency(container, result) {
    reset(container);
    const model = result.model || {};
    const totals = model.totals || {};
    const usage = section(container, "模型用量");
    const counts = node("dl", "presentation-fields");
    for (const key of ["input_tokens", "output_tokens", "total_tokens"])
      field(counts, labels[key], totals[key] ?? null);
    field(counts, "请求数", totals.requests ?? null);
    field(counts, "缺失用量的请求数", totals.missing_requests ?? null);
    usage.append(counts);
    if (totals.usage_complete === false) details(usage, "已知部分用量与缺失字段", {
      known_totals: totals.known_totals, missing_fields: totals.missing_fields });
    if (model.discussion_totals?.requests) {
      const discussion = section(container, "结果讨论用量");
      fields(discussion, model.discussion_totals);
    }
    const actions = section(container, "模型请求与响应");
    if (!model.actions?.length) empty(actions, "暂无模型请求记录");
    for (const action of model.actions || []) {
      const entry = record(actions, [action.provider, action.model].filter(Boolean).join(" · ") || "模型请求",
        action.response ? state(action.response.status) : "尚无响应记录");
      fields(entry, { created_at: action.created_at, request_id: action.request_id });
      details(entry, "响应用量", action.response?.usage);
      details(entry, "请求与响应引用", { "提示摘要": action.prompt_hash,
        "响应摘要": action.response?.response_hash, "能力摘要": action.capabilities_hash });
    }
    const observations = section(container, "工具执行记录");
    if (!model.observations?.length) empty(observations, "暂无工具执行记录");
    for (const observation of model.observations || []) {
      const entry = record(observations, observation.tool_name, state(observation.status));
      fields(entry, { request_id: observation.request_id, call_id: observation.call_id,
        action_id: observation.action_id, created_at: observation.created_at });
      details(entry, "执行引用", { "观察编号": observation.observation_id,
        input_hash: observation.input_hash, output_hash: observation.output_hash });
    }
    const context = section(container, "上下文与压缩");
    const latest = result.context?.latest;
    if (latest) {
      fields(context, { created_at: latest.created_at });
      context.append(readableValue(latest.token_projection));
      details(context, "上下文引用", { "快照编号": latest.snapshot_id, "上下文摘要": latest.context_hash });
    } else empty(context, "暂无上下文快照");
    for (const boundary of result.context?.compaction_boundaries || []) {
      const entry = record(context, "会话压缩", `${boundary.source_count} 条消息`);
      fields(entry, { created_at: boundary.created_at });
      details(entry, "压缩来源", { "摘要编号": boundary.summary_id, "来源消息": boundary.source_message_ids,
        "来源摘要": boundary.source_hash, "压缩摘要": boundary.summary_hash });
    }
    const catalog = section(container, "提供给模型的工具");
    fields(catalog, { "目录已展开": result.tools?.expanded,
      "提示字节数（估算）": result.tools?.estimated_prompt_bytes, "工具": result.tools?.selected });
    details(catalog, "工具可见性原因", result.tools?.visibility);
    const lineage = section(container, "证据与事件索引");
    fields(lineage, { "证据数": result.evidence?.nodes?.length, "产物数": result.artifacts?.length,
      "事件数": result.events?.count, "首条事件序号": result.events?.first_sequence,
      "最后事件序号": result.events?.last_sequence });
    details(lineage, "证据关系", result.evidence?.edges);
  }

  function renderSystem(container, result) {
    reset(container);
    const provider = result.provider || {};
    const model = section(container, "当前模型", provider.ready === true ? "可用" : provider.ready === false ? "未就绪" : "就绪状态未提供");
    fields(model, { configured: provider.configured, "服务商": provider.name, "模型": provider.model,
      reason: provider.reason });
    const capabilities = { ...provider.capabilities };
    if (capabilities.structured_output !== undefined) {
      capabilities.structured_output_support = capabilities.structured_output;
      delete capabilities.structured_output;
    }
    details(model, "模型能力", capabilities);
    const system = section(container, "运行环境");
    fields(system, { "服务": result.service, platform: result.platform, "接口版本": result.schema_version,
      "需要登录": result.auth?.required });
    fields(system, result.control_plane || {});
    details(system, "运行配置来源", result.configuration);
    details(system, "运行状态", result.run_statuses?.map(state));
  }

  function appendToolResult(container, content, runId) {
    fields(container, { tool_name: content.tool_name, status: content.status, error: content.error });
    const projection = content.projection;
    if (projection) {
      fields(container, { status_code: projection.status_code, request: projection.request,
        headers: projection.headers, timing: projection.timing });
      if (projection.body) {
        const body = section(container, "响应内容");
        body.append(readableValue(projection.body.preview));
        fields(body, { byte_count: projection.body.byte_count, truncated: projection.body.truncated });
      }
      if (projection.comparison) section(container, "对照结果").append(readableValue(projection.comparison));
      if (projection.enumeration) section(container, "枚举结果").append(readableValue(projection.enumeration));
      details(container, "完整结果摘要", projection.structured_summary);
    } else if (content.output !== undefined) section(container, "执行结果").append(readableValue(content.output));
    const artifact = content.raw?.artifact || content.output?.artifact;
    if (artifact) artifactLink(container, runId, artifact);
    details(container, "调用与结果引用", { call_id: content.call_id, input_hash: content.input_hash,
      output_hash: content.output_hash, "原始输出": content.raw });
  }

  function appendMessage(container, value, message = {}) {
    const content = decode(value);
    if (typeof content === "string") return copy(container, content);
    if (content === null || content === undefined) return empty(container, "消息内容未提供");
    if (Array.isArray(content)) {
      for (const part of content) appendMessage(container, part, message);
      return;
    }
    if (typeof content !== "object") return copy(container, content);
    if (message.role === "tool" || content.projection || (content.tool_name && "output" in content))
      return appendToolResult(container, content, message.run_id);
    if (content.type === "text" || content.type === "input_text" || content.type === "output_text")
      return copy(container, content.text);
    copy(container, content.text);
    if (content.objective !== undefined) {
      copy(container, content.objective);
      fields(container, { targets: content.targets, constraints: content.constraints,
        success_predicates: content.success_predicates, missing_capabilities: content.missing_capabilities });
    }
    for (const call of content.tool_calls || []) {
      const entry = record(container, call.name || call.function?.name || call.tool_name || "工具调用", "请求执行");
      const args = decode(call.arguments ?? call.function?.arguments ?? call.input);
      if (args !== undefined) entry.append(readableValue(args));
      details(entry, "调用引用", { call_id: call.call_id || call.id });
    }
    if (content.structured_output && Object.keys(content.structured_output).length)
      section(container, "模型决策").append(readableValue(content.structured_output));
    const rest = Object.fromEntries(Object.entries(content).filter(([key]) =>
      !["text", "objective", "targets", "constraints", "success_predicates", "missing_capabilities", "tool_calls", "structured_output"].includes(key)));
    if (content.text || content.objective || content.tool_calls?.length)
      details(container, "消息状态与引用", rest);
    else if (Object.keys(rest).length) container.append(readableValue(rest));
  }

  global.TracePresentation = Object.freeze({ renderTools, renderAttack, renderTransparency,
    renderReport, renderSystem, renderEvidence, renderArtifacts, appendMessage, readableValue, fields, details });
})(globalThis);
