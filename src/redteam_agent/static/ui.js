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
    const node = document.createElement("p");
    node.className = "run-empty";
    node.textContent = text;
    container.append(node);
  }

  function renderRunList({ views, selectedId, search, onSelect }) {
    const list = $("#run-list");
    const needle = String(search || "")
      .trim()
      .toLocaleLowerCase();
    const filtered = views.filter((view) => {
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
      `运行中 ${views.filter((view) => coreRun(view).status === "running").length}`;
    $("#summary-waiting").textContent =
      `待处理 ${views.filter((view) => ["created", "waiting_worker", "paused_budget"].includes(coreRun(view).status)).length}`;
    $("#summary-complete").textContent =
      `已完成 ${views.filter((view) => coreRun(view).status === "completed").length}`;
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
      button.className = `run-item${run.run_id === selectedId ? " selected" : ""}`;
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
      budgetText.textContent = `${used} / ${limit || "-"}`;
      const line = document.createElement("span");
      line.className = "budget-line";
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

  function renderEventTimeline(container, events) {
    container.replaceChildren();
    for (const event of [...events].reverse()) {
      const item = document.createElement("li");
      item.className = "event-item";
      const sequence = document.createElement("span");
      sequence.className = "event-sequence";
      sequence.textContent = `#${event.sequence}`;
      const name = document.createElement("span");
      name.className = "event-name";
      name.textContent = event.event_type;
      const summary = document.createElement("span");
      summary.className = "event-summary";
      summary.textContent = compactPayload(event.payload);
      item.append(sequence, name, summary);
      container.append(item);
    }
  }

  function renderRecordList(container, records, idKey, typeKey) {
    container.replaceChildren();
    if (!records.length) return appendEmpty(container, "暂无记录");
    for (const value of records) {
      const record = document.createElement("article");
      record.className = "record";
      const head = document.createElement("div");
      head.className = "record-head";
      const title = document.createElement("strong");
      title.textContent = valueOr(value[typeKey], "记录");
      const id = document.createElement("code");
      id.textContent = valueOr(value[idKey]);
      const body = document.createElement("p");
      body.textContent = safeJson(value);
      head.append(title, id);
      record.append(head, body);
      container.append(record);
    }
  }

  function renderArtifactList(container, artifacts, runId) {
    container.replaceChildren();
    if (!artifacts.length) return appendEmpty(container, "暂无产物");
    for (const artifact of artifacts) {
      const record = document.createElement("article");
      record.className = "record";
      const head = document.createElement("div");
      head.className = "record-head";
      const title = document.createElement("strong");
      title.textContent = valueOr(artifact.artifact_type, "artifact");
      const link = document.createElement("a");
      const artifactId = artifact.artifact_id || artifact.artifact_ref;
      link.href = `/api/runs/${encodeURIComponent(runId)}/artifacts/${encodeURIComponent(artifactId)}/content`;
      link.download = artifactId;
      link.textContent = "下载";
      const detail = document.createElement("p");
      detail.textContent = safeJson(artifact);
      head.append(title, link);
      record.append(head, detail);
      container.append(record);
    }
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
    detail.textContent = Array.isArray(value) ? value.join(" · ") : String(value);
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
    if (Number(record.confidence) > 0)
      appendInspectorField(
        fields,
        "置信度",
        `${Math.round(Number(record.confidence) * 100)}%`,
      );

    const raw = document.createElement("details");
    raw.className = "inspector-raw";
    const summary = document.createElement("summary");
    summary.textContent = "查看记录结构";
    const pre = document.createElement("pre");
    const { lane, superseded, ...source } = record;
    pre.textContent = safeJson(source);
    raw.append(summary, pre);
    container.append(header, fields, raw);
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
    const rawWasOpen = inspector.querySelector("details")?.open;
    map.replaceChildren();
    inspector.replaceChildren();
    raw.textContent = safeJson(payload || {});
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
        const select = (navigate = false) => {
          map.dataset.selectedRecord = explorationId(record);
          for (const item of buttons) {
            item.classList.toggle("selected", item === button);
            item.setAttribute("aria-pressed", String(item === button));
          }
          renderSearchInspector(inspector, record, () => {
            button.focus({ preventScroll: true });
            button.scrollIntoView({ block: "center" });
          });
          if (navigate && matchMedia("(max-width: 960px)").matches) {
            inspector.scrollIntoView({ block: "start" });
            inspector.querySelector("h3").focus({ preventScroll: true });
          }
        };
        button.addEventListener("click", () => select(true));
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
      inspector.querySelector("details").open = Boolean(rawWasOpen);
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

  function appendStructuredMessage(record, content) {
    const previewValue =
      content?.projection?.structured_summary ||
      content?.structured_summary ||
      content?.output?.projection?.structured_summary ||
      content?.output ||
      content;
    const previewLines = safeJson(previewValue).split("\n");
    const preview = document.createElement("pre");
    preview.className = "message-preview";
    preview.textContent = previewLines.slice(0, 8).join("\n");
    record.append(preview);
    if (previewLines.length > 8) {
      const remaining = document.createElement("span");
      remaining.className = "message-remaining";
      remaining.textContent = `另有 ${previewLines.length - 8} 行`;
      record.append(remaining);
    }
    const details = document.createElement("details");
    details.className = "message-raw";
    const summary = document.createElement("summary");
    summary.textContent = "展开完整结构";
    const raw = document.createElement("pre");
    raw.textContent = safeJson(content);
    details.append(summary, raw);
    record.append(details);
  }

  function renderTranscriptList(container, messages) {
    const expanded = new Set([...container.querySelectorAll(".transcript-message")]
      .filter((node) => node.querySelector("details")?.open).map((node) => node.dataset.messageId));
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
      meta.textContent = [
        message.source_type,
        message.sequence ? `#${message.sequence}` : "",
        formatTime(message.created_at),
      ]
        .filter(Boolean)
        .join(" · ");
      header.append(role, meta);
      record.append(header);
      if (typeof message.content === "string") {
        const body = document.createElement("div");
        body.className = "message-copy";
        body.textContent = message.content;
        record.append(body);
      } else {
        appendStructuredMessage(record, message.content);
        if (expanded.has(record.dataset.messageId)) record.querySelector("details").open = true;
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
