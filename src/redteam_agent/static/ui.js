"use strict";

(function attachTraceUi(global) {
  const statusLabels = {
    created: "已创建",
    running: "运行中",
    waiting_worker: "等待 Worker",
    paused_budget: "已暂停",
    cancelling: "取消中",
    cancelled: "已取消",
    completed: "已完成",
    failed: "失败",
  };

  const terminalStatuses = new Set(["completed", "failed", "cancelled"]);

  const $ = (selector) => document.querySelector(selector);
  const $$ = (selector) => [...document.querySelectorAll(selector)];
  const coreRun = (view) => view?.run || {};
  const goal = (view) => view?.goal || {};
  const formatStatus = (status) => statusLabels[status] || status || "未知";
  const valueOr = (value, fallback = "-") =>
    value === null || value === undefined || value === "" ? fallback : value;
  const safeJson = (value) => JSON.stringify(value ?? {}, null, 2);

  function compactPayload(payload) {
    if (!payload || !Object.keys(payload).length) return "-";
    const text = JSON.stringify(payload);
    return text.length > 420 ? `${text.slice(0, 417)}...` : text;
  }

  function formatTime(value) {
    if (!value) return "";
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return "";
    return new Intl.DateTimeFormat("zh-CN", {
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      hour12: false,
    }).format(date);
  }

  function appendEmpty(container, text) {
    const node = document.createElement("div");
    node.className = "run-empty rl-reticle-box";
    node.setAttribute("role", "status");
    node.textContent = text;
    container.append(node);
  }

  function renderRunList({ views, selectedId, search, status, onSelect }) {
    const list = $("#run-list");
    const needle = String(search || "")
      .trim()
      .toLocaleLowerCase();
    const filtered = views.filter((view) => {
      if (status && coreRun(view).status !== status) return false;
      if (!needle) return true;
      const run = coreRun(view);
      const text = [
        run.run_id,
        run.session_id,
        goal(view).objective,
        ...(goal(view).targets || []),
      ]
        .join(" ")
        .toLocaleLowerCase();
      return text.includes(needle);
    });

    list.replaceChildren();
    $("#run-count").textContent = String(filtered.length);
    $("#summary-active").textContent =
      `运行中 ${filtered.filter((view) => coreRun(view).status === "running").length}`;
    $("#summary-waiting").textContent =
      `待处理 ${filtered.filter((view) => ["created", "waiting_worker", "paused_budget"].includes(coreRun(view).status)).length}`;
    $("#summary-complete").textContent =
      `已完成 ${filtered.filter((view) => coreRun(view).status === "completed").length}`;
    $("#empty-state").hidden = views.length > 0 || Boolean(selectedId);

    if (!filtered.length) {
      if (views.length) appendEmpty(list, "没有匹配的运行");
      return;
    }

    for (const view of filtered) {
      const run = coreRun(view);
      const runGoal = goal(view);
      const budget = run.budget || {};
      const used = Number(budget.actions_used || 0);
      const limit = Number(budget.action_limit || 0);
      const percent =
        limit > 0 ? Math.min(100, Math.max(0, (used / limit) * 100)) : 0;
      const targets = runGoal.targets || [];

      const button = document.createElement("button");
      button.type = "button";
      button.className = `run-item rhine-row${run.run_id === selectedId ? " selected" : ""}`;
      button.dataset.runId = run.run_id;
      button.title = valueOr(runGoal.objective, run.session_id);

      const status = document.createElement("span");
      status.className = "run-status";
      status.dataset.status = run.status || "";
      status.textContent = formatStatus(run.status);

      const main = document.createElement("span");
      main.className = "run-main";
      const objective = document.createElement("strong");
      objective.className = "run-objective";
      objective.textContent = valueOr(runGoal.objective, run.session_id);
      const target = document.createElement("span");
      target.className = "run-target";
      target.textContent = targets.length ? targets.join(" · ") : "未绑定目标";
      main.append(objective, target);

      const budgetCell = document.createElement("span");
      budgetCell.className = "run-budget";
      const budgetText = document.createElement("span");
      budgetText.textContent = `${used} / ${budget.action_limit === 0 ? "无上限" : limit || "-"}`;
      const line = document.createElement("span");
      line.className = "budget-line";
      line.hidden = budget.action_limit === 0;
      line.style.setProperty("--budget-percent", `${percent}%`);
      line.append(document.createElement("span"));
      budgetCell.append(budgetText, line);

      const identity = document.createElement("span");
      identity.className = "run-identity";
      const time = document.createElement("time");
      time.dateTime = run.updated_at || run.created_at || "";
      time.textContent =
        formatTime(time.dateTime) || valueOr(view.next_action, "等待动作");
      const id = document.createElement("code");
      id.textContent = run.run_id;
      identity.append(time, id);

      button.append(status, main, budgetCell, identity);
      button.addEventListener("click", () => onSelect(run.run_id));
      list.append(button);
    }
  }

  function trajectoryDetails(step) {
    const labels = { ...statusLabels, success: "执行成功", paused: "已暂停", interrupted: "已中断",
      integrity_error: "完整性校验失败", unknown: "结果未知", waiting_input: "等待输入",
      retry: "重试", transport: "连接异常", rate_limit: "请求限流", auth: "认证失败",
      tool_result_hash_mismatch: "工具结果校验不一致", stop: "正常结束", end_turn: "本轮结束",
      tool_calls: "继续调用工具", tool_use: "继续调用工具", length: "输出达到上限" };
    const readable = (value) => labels[value] || value;
    const details = [step.model, step.provider, step.tool_name];
    if (step.target) details.push(`目标：${step.target}`);
    if (step.method || step.path) details.push(`请求：${[step.method, step.path].filter(Boolean).join(" ")}`);
    if (step.status) details.push(readable(step.status));
    if (step.http_status !== undefined && step.http_status !== "") details.push(`HTTP ${step.http_status}`);
    if (step.exit_code !== undefined && step.exit_code !== "") details.push(`退出码 ${step.exit_code}`);
    if (step.text) details.push(step.text);
    if (step.reason) details.push(`原因：${readable(step.reason)}`);
    if (step.error) details.push(`错误：${readable(step.error)}`);
    if (step.finish_reason) details.push(`结束原因：${readable(step.finish_reason)}`);
    if (step.tool_count !== undefined) details.push(`请求 ${step.tool_count} 个工具`);
    const usage = step.usage || {};
    for (const [label, count] of [["输入", usage.input_tokens ?? usage.prompt_tokens],
      ["输出", usage.output_tokens ?? usage.completion_tokens], ["总计", usage.total_tokens]]) {
      if (count !== undefined && count !== null) details.push(`${label} ${count} token`);
    }
    return details.filter(Boolean).join(" · ");
  }

  function renderEventTimeline(container, events) {
    container.replaceChildren();
    for (const event of [...events].reverse()) {
      const trajectory = event.trajectory || {};
      const item = document.createElement("li");
      item.className = `event-item trajectory-item trajectory-${trajectory.kind || "event"}`;
      const sequence = document.createElement("span");
      sequence.className = "event-sequence";
      sequence.textContent = `#${event.sequence}`;
      const name = document.createElement("span");
      name.className = "event-name";
      name.textContent = trajectory.title || event.event_type;
      const summary = document.createElement("span");
      summary.className = "event-summary";
      summary.textContent = trajectoryDetails(trajectory) || "轨迹步骤已记录";
      item.append(sequence, name, summary);
      container.append(item);
    }
    const terminal = $("#terminal-events");
    terminal.replaceChildren();
    $("#terminal-message").parentElement.hidden = events.length > 0;
    for (const event of events.slice(-30)) {
      const trajectory = event.trajectory || {};
      const line = document.createElement("div");
      line.className = "terminal-line";
      const time = document.createElement("span");
      time.textContent = event.created_at
        ? new Date(event.created_at).toLocaleTimeString("zh-CN", { hour12: false })
        : "--:--:--";
      const sequence = document.createElement("b");
      sequence.textContent = `#${event.sequence}`;
      const message = document.createElement("em");
      const detail = trajectoryDetails(trajectory);
      message.textContent = `${trajectory.title || event.event_type}${detail ? `：${detail}` : ""}`;
      line.append(time, sequence, message);
      terminal.append(line);
    }
  }

  function renderRecordList(container, records, idKey, typeKey) {
    global.TracePresentation.renderEvidence(container, records);
  }

  function renderArtifactList(container, artifacts, runId) {
    global.TracePresentation.renderArtifacts(container, artifacts, runId);
  }

  const explorationKindLabels = {
    hypothesis: "假设",
    lead: "线索",
    attempt: "尝试",
    observed_miss: "未命中",
    coverage_claim: "覆盖声明",
    verified_negative: "负向验证",
    contradiction: "矛盾",
    reopen: "重开",
  };

  const explorationStatusLabels = {
    proposed: "待验证",
    active: "活动",
    observed: "已观察",
    suspended: "已暂停",
    reopened: "已重开",
    closed: "已关闭",
    unverified: "未验证",
  };

  function explorationKind(record) {
    if (record.record_kind) return record.record_kind;
    return record.kind === "exploration_record" ? "hypothesis" : record.kind;
  }

  function explorationId(record) {
    return valueOr(record.record_id || record.attempt_id, "未标识记录");
  }

  function appendInspectorField(list, label, value) {
    if (
      value === null ||
      value === undefined ||
      value === "" ||
      (Array.isArray(value) && !value.length)
    )
      return;
    const term = document.createElement("dt");
    term.textContent = label;
    const detail = document.createElement("dd");
    detail.append(global.TracePresentation.readableValue(value));
    list.append(term, detail);
  }

  function renderSearchInspector(container, record, onReturn) {
    container.replaceChildren();
    const kind = explorationKind(record);
    const header = document.createElement("header");
    header.className = "inspector-heading";
    const kicker = document.createElement("span");
    kicker.textContent =
      `${record.superseded ? "历史 / " : ""}${explorationKindLabels[kind] || kind || "记录"} / ${explorationStatusLabels[record.status] || record.status || "-"}`;
    const title = document.createElement("h3");
    title.tabIndex = -1;
    title.textContent = record.statement || record.lifecycle_action_id || record.action_fingerprint || "探索记录";
    const id = document.createElement("code");
    id.textContent = explorationId(record);
    header.append(kicker, title, id);

    const fields = document.createElement("dl");
    fields.className = "inspector-fields";
    appendInspectorField(fields, "目标", record.target);
    appendInspectorField(fields, "工具", record.tool);
    appendInspectorField(fields, "父记录", record.parent_record_ids);
    appendInspectorField(fields, "证据引用", record.evidence_refs);
    appendInspectorField(fields, "产物引用", record.artifact_refs);
    appendInspectorField(fields, "能力", record.capabilities);
    appendInspectorField(fields, "重开条件", record.reopen_triggers);
    appendInspectorField(fields, "不确定性", record.uncertainty);
    if (record.confidence !== null && record.confidence !== undefined)
      appendInspectorField(
        fields,
        "置信度",
        `${Math.round(Number(record.confidence) * 100)}%`,
      );

    container.append(header, fields);
    global.TracePresentation.details(container, "已测试范围", record.tested_domain);
    global.TracePresentation.details(container, "实测观察", record.observations);
    global.TracePresentation.details(container, "覆盖情况", record.coverage);
    global.TracePresentation.details(container, "尝试内容", record.payload);
    global.TracePresentation.details(container, "补充信息", record.metadata);
    if (onReturn) {
      const back = document.createElement("button");
      back.type = "button";
      back.className = "text-button inspector-back";
      back.textContent = "返回所选记录";
      back.addEventListener("click", onReturn);
      container.append(back);
    }
  }

  function graphLaneFor(record, fallback = "history") {
    if (explorationKind(record) === "contradiction" && record.status !== "closed") return "contradictions";
    if (["active", "reopened", "proposed"].includes(record.status)) return "active";
    if (record.status === "suspended") return "suspended";
    return fallback;
  }

  function renderSearchGraph(map, inspector, raw, payload) {
    const projection = payload?.search_graph || {};
    const records = Array.isArray(payload?.records) ? [...payload.records] : [];
    if (!records.length) {
      const seen = new Set();
      for (const values of [projection.active, projection.suspended, projection.unresolved_contradictions]) {
        for (const record of values || []) {
          if (record.record_id && seen.has(record.record_id)) continue;
          if (record.record_id) seen.add(record.record_id);
          records.push(record);
        }
      }
    }
    const latest = new Map();
    for (const record of records) {
      if (record.hypothesis_id) latest.set(record.hypothesis_id, record.record_id);
    }
    const nodesToRender = records.map((record) => {
      const superseded = Boolean(record.hypothesis_id && latest.get(record.hypothesis_id) !== record.record_id);
      return { ...record, superseded, lane: superseded ? "history" : graphLaneFor(record) };
    });
    for (const attempt of projection.recent_attempts || [])
      nodesToRender.push({ ...attempt, kind: "attempt", lane: "attempts" });

    const lanes = [
      ["active", "活动方向"],
      ["suspended", "暂停方向"],
      ["contradictions", "待解矛盾"],
      ["attempts", "最近尝试"],
      ["history", "历史与结论"],
    ];
    const selectedId = map.dataset.selectedRecord || "";
    const restoreFocus = map.contains(document.activeElement);
    const inspectorFocused = inspector.contains(document.activeElement);
    map.replaceChildren();
    inspector.replaceChildren();
    raw.replaceChildren();
    raw.hidden = true;
    const total = Number(projection.record_count ?? records.length);
    $("#search-count").textContent = payload?.records_truncated
      ? `最近 ${records.length} / 共 ${total} 条记录`
      : `${total} 条记录`;

    if (!nodesToRender.length) {
      delete map.dataset.selectedRecord;
      appendEmpty(map, "当前还没有探索分支。模型产生方向、假设或尝试后会在这里形成状态空间。");
      const empty = document.createElement("p");
      empty.className = "inspector-empty";
      empty.textContent = "等待首条探索记录。";
      inspector.append(empty);
      return;
    }

    const buttons = [];
    for (const [laneId, label] of lanes) {
      const values = nodesToRender.filter((record) => record.lane === laneId);
      if (!values.length) continue;
      const lane = document.createElement("section");
      lane.className = "search-lane";
      lane.dataset.lane = laneId;
      const heading = document.createElement("header");
      const title = document.createElement("h3");
      title.textContent = label;
      const count = document.createElement("span");
      count.textContent = String(values.length);
      heading.append(title, count);
      const nodes = document.createElement("div");
      nodes.className = "search-nodes";
      for (const record of values) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = "search-node";
        button.dataset.status = record.superseded ? "history" : record.status || laneId;
        button.dataset.recordId = explorationId(record);
        button.setAttribute("aria-controls", inspector.id);
        button.setAttribute("aria-pressed", "false");
        const meta = document.createElement("span");
        const kind = explorationKind(record);
        meta.textContent =
          `${record.superseded ? "历史 / " : ""}${explorationKindLabels[kind] || kind || "记录"} / ${explorationStatusLabels[record.status] || record.status || laneId}`;
        const statement = document.createElement("strong");
        statement.textContent = record.statement || record.lifecycle_action_id || record.action_fingerprint || "探索记录";
        const relation = document.createElement("code");
        relation.textContent = record.parent_record_ids?.length
          ? `来自 ${record.parent_record_ids.join(" · ")}`
          : explorationId(record);
        button.append(meta, statement, relation);
        const select = () => {
          map.dataset.selectedRecord = explorationId(record);
          for (const item of buttons) {
            item.classList.toggle("selected", item === button);
            item.setAttribute("aria-pressed", String(item === button));
          }
          renderSearchInspector(inspector, record, () => {
            button.focus({ preventScroll: true });
            button.scrollIntoView({ block: "center" });
          });
        };
        button.addEventListener("click", select);
        buttons.push(button);
        nodes.append(button);
        if (explorationId(record) === selectedId) button.restoreSelection = select;
      }
      lane.append(heading, nodes);
      map.append(lane);
    }

    const selected = buttons.find((button) => button.restoreSelection);
    if (selected) {
      selected.restoreSelection();
      if (restoreFocus) selected.focus({ preventScroll: true });
      if (inspectorFocused) inspector.querySelector("h3").focus({ preventScroll: true });
      return;
    }
    delete map.dataset.selectedRecord;
    const empty = document.createElement("p");
    empty.className = "inspector-empty";
    empty.textContent = "选择一条探索记录以查看状态、来源与引用。";
    inspector.append(empty);
  }

  function transcriptRole(message) {
    const source = String(message.source_type || "").toLocaleLowerCase();
    if (message.role === "tool" || source.includes("tool")) return "tool";
    return ["user", "assistant", "system"].includes(message.role)
      ? message.role
      : "other";
  }

  function renderTranscriptList(container, messages) {
    const expanded = new Map([...container.querySelectorAll(".transcript-message")]
      .map((item) => [item.dataset.messageId, [...item.querySelectorAll("details")]
        .map((detail, index) => detail.open ? index : -1).filter((index) => index >= 0)]));
    container.replaceChildren();
    if (!messages.length) return appendEmpty(container, "暂无会话");
    for (const message of messages) {
      const record = document.createElement("article");
      const roleName = transcriptRole(message);
      record.className = `record transcript-message transcript-${roleName}`;
      record.dataset.messageId = message.message_id || `${message.source_type}:${message.source_id}:${message.sequence}`;
      const header = document.createElement("header");
      header.className = "message-heading";
      const role = document.createElement("span");
      role.className = "transcript-role";
      role.textContent =
        { user: "用户", assistant: "模型", system: "系统", tool: "工具" }[
          roleName
        ] || valueOr(message.role, "记录");
      const meta = document.createElement("span");
      const sources = { original_goal: "任务目标", model_response: "模型回复",
        model_request_projection: "模型上下文", tool_result: "执行结果", system_base: "系统指令",
        action_prompt: "本轮任务", model_execution_stall_nudge: "执行提醒", model_empty_turn_nudge: "空回复提醒" };
      meta.textContent = [
        sources[message.source_type] || message.source_type,
        message.metadata?.model,
        message.sequence ? `#${message.sequence}` : "",
        formatTime(message.created_at),
      ]
        .filter(Boolean)
        .join(" · ");
      header.append(role, meta);
      record.append(header);
      global.TracePresentation.appendMessage(record, message.content, message);
      for (const index of expanded.get(record.dataset.messageId) || []) {
        const detail = record.querySelectorAll("details")[index];
        if (detail) detail.open = true;
      }
      container.append(record);
    }
  }

  global.TraceUI = Object.freeze({
    $,
    $$,
    appendEmpty,
    coreRun,
    formatStatus,
    formatTime,
    goal,
    renderArtifactList,
    renderEventTimeline,
    renderRecordList,
    renderRunList,
    renderSearchGraph,
    renderTranscriptList,
    safeJson,
    statusLabels,
    terminalStatuses,
    valueOr,
  });
})(globalThis);
