// Offline DOM checks using the real web projection shapes. No API writes.
// NODE_PATH may point at the bundled runtime's node_modules.
import assert from "node:assert/strict";
import { readFileSync, mkdirSync } from "node:fs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { chromiumLaunchOptions } from "./playwright_smoke_support.mjs";

const { chromium } = createRequire(import.meta.url)("playwright");
const root = fileURLToPath(new URL("../", import.meta.url));
const staticFile = (name) => root + "src/redteam_agent/static/" + name;
const browser = await chromium.launch(chromiumLaunchOptions());
try {
  const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.setContent('<main style="max-width:1040px;margin:30px auto;padding:20px"><div id="view"></div><div id="search-count"></div><div id="map"></div><aside id="inspector"></aside><div id="raw" hidden></div></main>');
  const appCss = readFileSync(staticFile("app.css"), "utf8")
    .replace(/^@import.*$/gm, "").replace(/@font-face\s*\{[^}]*\}/g, "");
  await page.addStyleTag({ content: appCss });
  await page.addStyleTag({ content: "body { display:block; height:auto; overflow:auto; } main { width:calc(100% - 40px); }" });
  for (const name of ["base.css", "workbench.css"])
    await page.addStyleTag({ path: staticFile(name) });
  await page.addScriptTag({ path: staticFile("presentation.js") });
  await page.addScriptTag({ path: staticFile("ui.js") });

  const render = async (method, payload) => {
    await page.evaluate(({ method, payload }) => TracePresentation[method](document.querySelector("#view"), payload), { method, payload });
    assert.equal(await page.locator("#view pre, #view .json-view").count(), 0);
    assert.doesNotMatch(await page.locator("#view").textContent(), /\[object Object\]|"(?:payload|status|tool_name)"\s*:/);
  };
  await render("renderTools", { revision: "catalog-1", expanded: false, estimated_prompt_bytes: 100,
    tools: { "http.fetch": { name: "http.fetch", version: "1", capabilities: ["page_fetch"], side_effecting: false } },
    visibility: [{ tool: "http.fetch", visible: true, reason: "capability_match" },
      { tool: "shell.exec", visible: false, reason: "deferred_until_expand" }] });
  assert.match(await page.locator("#view").innerText(), /http.fetch.*已提供给模型/s);
  assert.match(await page.locator("#view").innerText(), /shell.exec.*未提供给模型/s);

  const attack = { materialized: true,
    assets: [{ asset_id: "a1", name: "本地登录服务", asset_type: "endpoint", target: "http://localhost/login", status: "verified", evidence_ids: ["e1"] }],
    findings: [{ finding_id: "f1", title: "会话边界检查", severity: "high", status: "verified", reproduction_evidence_ids: ["e1"] }],
    attack_paths: [{ path_id: "p1", title: "会话访问路径", status: "candidate", asset_ids: ["a1", "missing"], finding_ids: ["f1"], evidence_ids: ["e1"], impact: "测试影响描述", relationship_ids: ["r1"] }],
    source_evidence_ids: ["e1"], edges: [{ from: "a1", to: "p1", kind: "asset_to_path" }] };
  await render("renderAttack", attack);
  assert.match(await page.locator("#view").innerText(), /本地登录服务.*会话边界检查.*测试影响描述/s);
  assert.match(await page.locator("#view").innerText(), /missing（当前响应未包含资产详情）/);
  assert.match(await page.locator("#view").innerText(), /待验证/);
  assert.doesNotMatch(await page.locator("#view").innerText(), /第.*步|利用成功/);

  const evidence = { evidence_id: "e1", artifact_type: "http_response", target: "http://localhost/login",
    tool: "http.fetch", verified: false, trust: "unverified", confidence: 0,
    payload: { status_code: 403, body: "<script>window.__executed = true</script>" }, parent_ids: [] };
  await page.evaluate((evidence) => TraceUI.renderRecordList(document.querySelector("#view"), [evidence], "evidence_id", "artifact_type"), evidence);
  assert.match(await page.locator("#view").innerText(), /未验证.*HTTP 状态码.*403/s);
  assert.match(await page.locator("#view").innerText(), /0%/);
  assert.equal(await page.evaluate(() => window.__executed), undefined);

  const artifact = { artifact_id: "artifact/1", artifact_type: "model_tool_result", byte_count: 200,
    media_type: "application/json", preview: { tool_name: "http.fetch", status: "ok" } };
  await page.evaluate((artifact) => TraceUI.renderArtifactList(document.querySelector("#view"), [artifact], "run/1"), artifact);
  assert.equal(await page.locator("#view a").getAttribute("href"), "/api/runs/run%2F1/artifacts/artifact%2F1/content");
  await page.evaluate((artifact) => TraceUI.renderArtifactList(document.querySelector("#view"), [{ ...artifact,
    preview: { head: '{"output":{"status":', tail: '"end"}}', truncated: true, byte_count: 200 } }], "run/1"), artifact);
  assert.match(await page.locator("#view").innerText(), /结构化产物预览已截断/);
  assert.doesNotMatch(await page.locator("#view").textContent(), /"output"|"end"/);

  const transcript = [
    { message_id: "m1", role: "user", source_type: "original_goal", content: { objective: "检查会话边界", targets: ["localhost"] } },
    { message_id: "m2", role: "assistant", source_type: "model_response", content: { text: "正在检查实际响应。", tool_calls: [{ name: "http.fetch", arguments: '{"url":"http://localhost/login"}' }], status: "ok" } },
    { message_id: "m3", role: "tool", run_id: "run/1", source_type: "tool_result", content: { tool_name: "http.fetch", status: "ok", projection: { status_code: 403, body: { preview: "Forbidden", truncated: false, byte_count: 9 }, structured_summary: { status_code: 403 } }, raw: { artifact }, output: { artifact } } },
    { message_id: "m4", role: "system", content: '{"objective":"继续检查","targets":["localhost"]}' },
    { message_id: "m5", role: "assistant", source_type: "model_response", content: {
      text: '{"decision":{"status":"waiting_input","summary":"已采集四组响应"}}',
      structured_output: { decision: { status: "waiting_input", summary: "已采集四组响应" } } } },
  ];
  await page.evaluate((messages) => TraceUI.renderTranscriptList(document.querySelector("#view"), messages), transcript);
  assert.match(await page.locator("#view").innerText(), /检查会话边界.*正在检查实际响应.*请求执行.*Forbidden.*继续检查/s);
  assert.equal(await page.locator("#view pre").count(), 0);
  assert.doesNotMatch(await page.locator("#view").textContent(), /"url":|"objective":|\[object Object\]/);
  assert.match(await page.locator("#view").innerText(), /已采集四组响应/);
  assert.doesNotMatch(await page.locator("#view").textContent(), /"decision"\s*:|"summary"\s*:/);
  await page.locator("#view .transcript-message").nth(2).locator("details").first().evaluate((detail) => { detail.open = true; });
  await page.evaluate((messages) => TraceUI.renderTranscriptList(document.querySelector("#view"), messages), transcript);
  assert.equal(await page.locator("#view .transcript-message").nth(2).locator("details").first().getAttribute("open"), "");

  const search = { records: [{ record_id: "search1", record_kind: "hypothesis", hypothesis_id: "h1",
    status: "active", statement: "检查登录边界", uncertainty: { reason: "需验证" }, metadata: { next: "采集响应" } }], search_graph: { record_count: 1 } };
  await page.evaluate((payload) => TraceUI.renderSearchGraph(document.querySelector("#map"), document.querySelector("#inspector"), document.querySelector("#raw"), payload), search);
  await page.locator("#map button").click();
  assert.match(await page.locator("#inspector").innerText(), /需验证/);
  assert.equal(await page.locator("#inspector pre").count(), 0);
  await page.evaluate((payload) => TraceUI.renderSearchGraph(document.querySelector("#map"), document.querySelector("#inspector"), document.querySelector("#raw"), payload), search);
  assert.equal(await page.locator("#map .selected").count(), 1);
  assert.equal(await page.locator("#raw").textContent(), "");

  const view = { run: { run_id: "run/1", status: "running" }, goal: { objective: "检查会话边界", targets: ["localhost"], criteria: [{ criterion_id: "c1", statement: "采集一手响应" }] } };
  const terminal = { terminal: false, success: false, reason: "goal_predicates_pending", satisfied: [], missing: ["c1"] };
  await render("renderReport", { run: view, terminal, evidence: { nodes: [evidence] }, artifacts: [artifact], report_hash: "hash1" });
  assert.match(await page.locator("#view").innerText(), /尚未形成最终结论.*采集一手响应/s);
  await render("renderTransparency", { run: view,
    model: { actions: [{ request_id: "req1", provider: "local", model: "test", response: null }], observations: [], totals: { input_tokens: null, output_tokens: 0, total_tokens: null, requests: 1, missing_requests: 1, usage_complete: false, known_totals: { input_tokens: 20, output_tokens: 0, total_tokens: 20 } } },
    context: { latest: { token_projection: { estimated_context_tokens: 23, provider_context_tokens: null } }, compaction_boundaries: [] },
    tools: { selected: ["http.fetch"], expanded: false }, events: { count: 4, first_sequence: 1, last_sequence: 4 }, evidence: { nodes: [evidence], edges: [] }, artifacts: [artifact] });
  assert.match(await page.locator("#view").innerText(), /输入 token\s*未提供/);
  assert.match(await page.locator("#view").innerText(), /输出 token\s*0/);
  assert.match(await page.locator("#view").innerText(), /尚无响应记录/);
  await render("renderSystem", { service: "Trace", platform: "windows", provider: { configured: true, ready: false, reason: "missing_credentials", model: "test" }, control_plane: { database: "local.sqlite3", root: "local-data" } });
  assert.match(await page.locator("#view").innerText(), /缺少凭据.*local.sqlite3/s);

  for (const method of ["renderTools", "renderAttack", "renderTransparency", "renderReport", "renderSystem"])
    await render(method, {});
  for (const width of [1440, 1024, 390]) {
    await page.setViewportSize({ width, height: 1000 });
    await page.locator("#map").evaluate((element) => { element.hidden = true; });
    await page.locator("#inspector").evaluate((element) => { element.hidden = true; });
    await page.locator("#search-count").evaluate((element) => { element.hidden = true; });
    await render("renderAttack", attack);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false, `overflow at ${width}px`);
    if (width === 1440) {
      mkdirSync(root + "playwright-output", { recursive: true });
      await page.screenshot({ path: root + "playwright-output/presentation-attack.png", fullPage: true });
    }
  }
  assert.deepEqual(errors, []);
  console.log("PASS: real projection shapes, readable messages, references, empty states, missing usage, preserved selection and viewport bounds");
} finally {
  await browser.close();
}
