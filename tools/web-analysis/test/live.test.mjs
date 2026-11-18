import { test } from "node:test";
import assert from "node:assert/strict";
import { createServer } from "node:http";
import { createServer as createTcpServer } from "node:net";
import { mkdtemp, writeFile, readFile, readdir, readlink, rm } from "node:fs/promises";
import { spawn } from "node:child_process";
import { setTimeout as delay } from "node:timers/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createHash } from "node:crypto";
import { launch } from "cloakbrowser";
import { NativeModuleResolver } from "../dist/browser/modules/NativeModuleResolver.js";
import { execute } from "../dist/trace-operations.js";

async function call(name, input) {
  const result = await execute(name, input);
  assert.equal(result.ok, true, JSON.stringify(result.error, (_key, value) => value instanceof Error ? { ...value, name:value.name, message:value.message, cause:value.cause } : value));
  return result.value.normalized_result;
}

async function freePort() {
  const socket = createTcpServer();
  await new Promise(resolve => socket.listen(0, "127.0.0.1", resolve));
  const port = socket.address().port;
  await new Promise(resolve => socket.close(resolve));
  return port;
}

async function processTable() {
  const entries = await readdir("/proc");
  const rows = await Promise.all(entries.filter(pid => /^\d+$/.test(pid)).map(async pid => {
    try {
      const stat = await readFile(`/proc/${pid}/stat`, "utf8");
      const fields = stat.slice(stat.lastIndexOf(")") + 2).trim().split(/\s+/);
      const command = (await readFile(`/proc/${pid}/cmdline`, "utf8")).replaceAll("\0", " ");
      return { pid: Number(pid), parent: Number(fields[1]), start: fields[19], state: fields[0], command };
    } catch { return null; }
  }));
  return rows.filter(Boolean);
}

function descendants(rows, root) {
  const owned = new Set([root]);
  let size;
  do {
    size = owned.size;
    for (const row of rows) if (owned.has(row.parent)) owned.add(row.pid);
  } while (size !== owned.size);
  return rows.filter(row => row.pid !== root && owned.has(row.pid));
}

test("no-DISPLAY CLI SIGTERM reaches Node and closes CloakBrowser plus Xvfb", {
  skip: process.platform !== "linux" || !process.env.CLOAKBROWSER_BINARY_PATH,
  timeout: 30000,
}, async () => {
  let reached;
  const ready = new Promise(resolve => { reached = resolve; });
  const server = createServer((_request, response) => {
    response.setHeader("Content-Type", "text/html");
    response.end("<!doctype html><title>Trace cancel fixture</title>Ready");
    reached();
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  const cli = spawn(process.execPath, ["dist/trace-cli.js", "--operation", "capture_browser_scenario"], {
    stdio: ["pipe", "pipe", "pipe"], env: { ...process.env, DISPLAY: "", XAUTHORITY: "" },
  });
  let stdout = "", stderr = "", owned = [];
  cli.stdout.on("data", data => stdout += data);
  cli.stderr.on("data", data => stderr += data);
  const finished = new Promise((resolve, reject) => {
    cli.once("error", reject); cli.once("close", (code, signal) => resolve({ code, signal }));
  });
  try {
    cli.stdin.end(JSON.stringify({
      browser: { mode: "launch" }, start_url: { url: `http://127.0.0.1:${server.address().port}/` },
      actions: [{ step_id: "long-wait", action: "wait_for_timeout", duration_ms: 60000 }],
    }));
    await Promise.race([ready, finished.then(result => { throw new Error(`CLI exited before navigation: ${JSON.stringify(result)} ${stderr} ${stdout}`); })]);
    await delay(100);
    assert.equal(await readlink(`/proc/${cli.pid}/exe`), await readlink("/proc/self/exe"), "worker root must remain Node");
    owned = descendants(await processTable(), cli.pid);
    assert.ok(owned.some(row => /(^|\/)Xvfb(?: |$)/.test(row.command)), JSON.stringify(owned));
    assert.ok(owned.some(row => row.command.includes(process.env.CLOAKBROWSER_BINARY_PATH)), JSON.stringify(owned));
    cli.kill("SIGTERM"); // Same root-only graceful signal used by the Go Worker.
    assert.deepEqual(await finished, { code: 143, signal: null }, stderr + stdout);
    // REA retains a valid evidence envelope after action cancellation; completion
    // belongs to the step/capture, not the envelope's serialization success.
    const envelope = JSON.parse(stdout);
    assert.equal(envelope.ok, true, stdout);
    const capture = envelope.value.normalized_result;
    assert.equal(capture.completeness.status, "incomplete", stdout);
    assert.ok(capture.completeness.missing_sections.includes("action"), stdout);
    assert.equal(capture.browser.mode, "launch");
    assert.equal(capture.browser.process_ownership, "provider-owned");
    assert.equal(capture.browser.cleanup, "terminated-owned-process");
    const wait = capture.steps.find(step => step.step_id === "long-wait");
    assert.ok(wait, stdout);
    assert.equal(wait.action, "wait_for_timeout");
    assert.equal(wait.status, "cancelled");
    assert.equal(wait.completeness.status, "incomplete");
    assert.ok(wait.completeness.missing_sections.includes("action"));
    assert.ok(wait.elapsed_ms < 60000, "requested wait completed instead of being interrupted");
    const remaining = await processTable();
    assert.deepEqual(remaining.filter(row => row.state !== "Z" && owned.some(old => old.pid === row.pid && old.start === row.start)), [], "owned browser/display processes remain after root exit");
  } finally {
    if (cli.exitCode === null && cli.signalCode === null) cli.kill("SIGKILL");
    // Clean only identities captured under this fixture, even when the assertion fails.
    for (const row of await processTable()) {
      if (owned.some(old => old.pid === row.pid && old.start === row.start)) {
        try { process.kill(row.pid, "SIGKILL"); } catch {}
      }
    }
    await new Promise(resolve => server.close(resolve));
  }
});

test("live CloakBrowser: headed CDP, scenario, screenshot and native module resolution", {
  skip: !process.env.CLOAKBROWSER_BINARY_PATH && "set CLOAKBROWSER_BINARY_PATH to the deployment-verified server binary and use xvfb-run",
  timeout: 120000,
}, async () => {
  const server = createServer((request, response) => {
    if (request.url === "/api/ping") { response.setHeader("Content-Type", "application/json"); response.end('{"ok":true}'); return; }
    if (request.url === "/app.js") { response.setHeader("Content-Type", "application/javascript"); response.end('function ping(){fetch("/api/ping");} document.querySelector("#ping").addEventListener("click",ping); setInterval(ping,100);'); return; }
    response.setHeader("Content-Type", "text/html");
    response.end('<!doctype html><title>Trace live fixture</title><button id="ping">Ping</button><script src="/app.js"></script>');
  });
  await new Promise(resolve => server.listen(0, "127.0.0.1", resolve));
  const origin = `http://127.0.0.1:${server.address().port}`;
  const artifacts = await mkdtemp(join(tmpdir(), "trace-browser-test-"));
  let browser;
  try {
    const port = await freePort();
    browser = await launch({ headless: false, args: [`--remote-debugging-port=${port}`, "--no-sandbox"] });
    const page = await browser.newPage(); await page.goto(origin);
    const scope = { cdp_endpoint: `http://127.0.0.1:${port}`, allowed_origins: [origin] };
    const targets = await call("list_browser_targets", scope);
    const target = targets.targets.find(item => item.title === "Trace live fixture");
    assert.ok(target, JSON.stringify(targets));
    const selected = { ...scope, target_id: target.target_id };
    const inspected = await call("inspect_web_page", { ...selected, observation_ms: 300, include_script_sources: true });
    assert.ok(inspected.limitations.some(item => item.includes("Console capture is passive")));
    assert.ok(inspected.completeness.attach_limited_sections.includes("console_events"));
    assert.ok(!inspected.completeness.unavailable_sections.includes("console_events"));
    assert.ok(JSON.stringify(inspected).includes("function ping()"));
    const capturePath = join(artifacts, "page.json");
    await writeFile(capturePath, JSON.stringify(inspected));
    const exported = await call("export_web_scripts", { capture_path: capturePath, output_directory: join(artifacts, "scripts") });
    assert.ok(JSON.stringify(exported).includes("debugger-source-utf8"));
    const manifestBytes = await readFile(exported.manifest.path);
    assert.equal(createHash("sha256").update(manifestBytes).digest("hex"), exported.manifest.sha256);
    assert.equal(manifestBytes.length, exported.manifest.bytes);
    assert.equal(createHash("sha256").update(await readFile(capturePath)).digest("hex"), exported.capture_sha256);
    for (const script of exported.scripts.filter(script => script.content.state === "exported")) {
      const originalBytes = await readFile(join(exported.output_directory, 'files', script.content.relative_path));
      assert.equal(createHash("sha256").update(originalBytes).digest("hex"), script.content.sha256);
      assert.equal(originalBytes.length, script.content.bytes);
    }
    assert.ok(exported.analysis_input?.input_path, "actual export did not publish an analysis input");
    const handedOff = await call("analyze_javascript_application", { input_path: exported.analysis_input.input_path });
    assert.ok(handedOff.statistics.parsed_javascript_files > 0, "exported bytes were not analyzed");
    // Metadata-only observation is not silently upgraded into captured source.
    const metadataCapture = await call("inspect_web_page", { ...selected, observation_ms: 0, include_script_sources: false });
    const metadataPath = join(artifacts, "metadata-only.json");
    await writeFile(metadataPath, JSON.stringify(metadataCapture));
    const partialExport = await call("export_web_scripts", { capture_path: metadataPath, output_directory: join(artifacts, "metadata-scripts") });
    assert.ok(partialExport.scripts.some(script => script.content.state === "unavailable"));
    assert.equal(partialExport.analysis_input, null);
    const listeners = await call("inspect_web_event_listeners", { ...selected, selector: "#ping" });
    assert.ok(JSON.stringify(listeners).includes("click"));
    const execution = await call("observe_web_execution", { ...selected, observation_ms: 500 });
    assert.equal(execution.coverage.state, "captured");
    assert.ok(execution.requests.some(request => request.url.endsWith("/api/ping")));
    assert.equal(execution.instrumentation.cleanup, "confirmed");
    // Exercise the CLI cancellation boundary, including requested evidence file.
    const cli = spawn(process.execPath, ["dist/trace-cli.js"], {stdio:["pipe","pipe","pipe"]});
    let stderr = ""; cli.stderr.on("data", data => stderr += data);
    try {
      const ready = new Promise(resolve => cli.stdout.once("data",resolve));
      cli.stdin.write('{"operation":"analyze_javascript_application","input":{}}\n');
      await ready;
      const cancelledOutput = join(artifacts,"cancelled.json");
      const finished = new Promise(resolve => cli.once("exit",code=>resolve(code)));
      cli.stdin.write(JSON.stringify({operation:"observe_web_execution",input:{...selected,observation_ms:10000},output_file:cancelledOutput})+"\n");
      await delay(750);
      cli.kill("SIGTERM");
      assert.equal(await finished,143,stderr);
      const cancelled = JSON.parse(await readFile(cancelledOutput,"utf8"));
      assert.equal(cancelled.ok,false);
      assert.ok(cancelled.error);
      // A subsequent fresh observation confirms the external page remains usable.
      const afterCancel = await call("inspect_web_event_listeners", {...selected,selector:"#ping"});
      assert.equal(afterCancel.cleanup,"confirmed");
    } finally { cli.stdin.destroy(); if (cli.exitCode === null) cli.kill("SIGKILL"); }
    const screenshot = await call("capture_web_screenshot", selected);
    assert.ok(JSON.stringify(screenshot).includes("image/png"));
    const bundle = await call("analyze_web_bundle", { ...selected, observation_ms: 200 });
    assert.ok(JSON.stringify(bundle).includes("/api/ping"));
    const scenarioInput = {
      browser: { mode:"launch" },
      start_url: { url:origin },
      actions: [{ step_id:"click", action:"click", locator:{kind:"css",selector:"#ping"} }],
      capture: { at_end:["dom","url"] },
    };
    const scenario = await call("capture_browser_scenario", scenarioInput);
    assert.equal(scenario.browser.cleanup, "terminated-owned-process");
    assert.ok(JSON.stringify(scenario).includes("Ping"));
    const selectedEvents = await call("capture_browser_scenario", {
      ...scenarioInput, capture: { at_end: ["dom", "url"], events: ["console", "page-errors"] },
    });
    assert.equal(selectedEvents.browser.cleanup, "terminated-owned-process");
    assert.equal(selectedEvents.completeness.status, "incomplete");
    assert.equal(selectedEvents.completeness.equality_eligible, false);
    assert.ok(selectedEvents.completeness.missing_sections.includes("events"));
    assert.ok(JSON.stringify(selectedEvents).includes("Fixed CloakBrowser binary suppresses console"));
    assert.ok(JSON.stringify(selectedEvents).includes("Fixed CloakBrowser binary suppresses page-errors"));
    assert.ok(!JSON.stringify(scenario).includes("Fixed CloakBrowser binary suppresses"));
    assert.equal(scenario.completeness.status, "complete");
    assert.equal(scenario.completeness.equality_eligible, true);
    // The named CLI supplies Xvfb itself; static/catalog requests never need it.
    const launched = spawn(process.execPath, ["dist/trace-cli.js", "--operation", "capture_browser_scenario"], {
      stdio: ["pipe", "pipe", "pipe"], env: { ...process.env, DISPLAY: "" },
    });
    let cliOutput = "", cliError = "";
    launched.stdout.on("data", data => cliOutput += data);
    launched.stderr.on("data", data => cliError += data);
    const completed = new Promise((resolve, reject) => {
      launched.once("error", reject);
      launched.once("close", resolve);
    });
    launched.stdin.end(JSON.stringify(scenarioInput));
    assert.equal(await completed, 0, cliOutput + cliError);
    const cliCapture = JSON.parse(cliOutput);
    assert.equal(cliCapture.ok, true, cliOutput);
    assert.equal(cliCapture.value.normalized_result.browser.cleanup, "terminated-owned-process");
    const resolver = new NativeModuleResolver(process.env);
    try {
      const modules = await resolver.resolve({
        importerUrl: `${origin}/app.js`, importMap: null, specifiers: ["./dependency.js"],
      });
      assert.equal(modules.ok, true, JSON.stringify(modules.error));
      assert.equal(modules.value.engine.id, "cloakbrowser-native-module-resolver");
      assert.equal(modules.value.resolutions[0].url, `${origin}/dependency.js`);
      assert.ok(modules.value.diagnostics.some(item => item.includes("suppresses console events")));
    } finally { await resolver.close(); }
  } finally {
    if (browser) await browser.close();
    await new Promise(resolve => server.close(resolve));
    await rm(artifacts, { recursive:true, force:true });
  }
});

test("live Electron: actual IPC hook, window observation and owned-process cleanup", {
  skip: !process.env.TRACE_TEST_ELECTRON && "set TRACE_TEST_ELECTRON and use an X display (e.g. xvfb-run)",
  timeout: 120000,
}, async () => {
  const app = await mkdtemp(join(tmpdir(), "trace-electron-test-"));
  try {
    await writeFile(join(app, "package.json"), JSON.stringify({ name:"trace-electron-fixture", version:"1.0.0", main:"main.cjs" }));
    await writeFile(join(app, "main.cjs"), 'const {app,BrowserWindow,ipcMain}=require("electron"); const path=require("node:path"); app.setPath("userData",path.join(__dirname,"user-data")); app.setPath("crashDumps",path.join(__dirname,"crash-dumps")); ipcMain.handle("trace:ping",()=>"pong"); app.whenReady().then(()=>{const win=new BrowserWindow({show:false,webPreferences:{preload:path.join(__dirname,"preload.cjs"),contextIsolation:true}});win.loadFile(path.join(__dirname,"index.html"));});');
    await writeFile(join(app, "preload.cjs"), 'const {contextBridge,ipcRenderer}=require("electron");contextBridge.exposeInMainWorld("traceAPI",{ping:()=>ipcRenderer.invoke("trace:ping")});');
    await writeFile(join(app, "index.html"), '<!doctype html><title>Trace Electron fixture</title><button id="ping">Ping</button><div id="result"></div><script src="renderer.js"></script>');
    await writeFile(join(app, "renderer.js"), 'document.querySelector("#ping").addEventListener("click", async () => { try { const value = await window.traceAPI.ping(); document.querySelector("#result").textContent = value; document.title = "Trace Electron " + value; } catch (error) { document.title = "Trace Electron error: " + error.message; } });');
    const capture = await call("capture_electron_scenario", {
      executable_path: process.env.TRACE_TEST_ELECTRON,
      application_path: app, application_root: app, args: ["--no-sandbox"],
      actions: [{step_id:"ready",kind:"wait",duration_ms:500},{step_id:"ping",kind:"click",selector:"#ping"},{step_id:"settle",kind:"wait",duration_ms:500}],
    });
    assert.ok(capture.windows.length > 0);
    assert.ok(capture.actions.every(action => action.status === "completed"));
    assert.ok(capture.windows.some(window => window.title === "Trace Electron pong"), JSON.stringify(capture.windows));
    assert.ok(capture.ipc.events.some(event => event.channel === "trace:ping" &&
      event.kind === "main-handler-invocation" && event.direction === "renderer-to-main" &&
      event.result_shape === "string" && event.error === false), JSON.stringify(capture.ipc));
    assert.equal(capture.application.cleanup, "terminated-owned-process");
  } finally { await rm(app, { recursive:true, force:true }); }
});
