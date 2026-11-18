import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, writeFile, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { spawn } from "node:child_process";
import { historicalHar } from "../tests/fixtures/historicalHar.ts";

function command(args, input = "") {
  return new Promise((resolve, reject) => {
    const child = spawn(process.execPath, ["dist/trace-cli.js", ...args], { stdio: ["pipe", "pipe", "pipe"] });
    let stdout="", stderr="";
    child.stdout.on("data", data => stdout += data);
    child.stderr.on("data", data => stderr += data);
    child.on("error", reject);
    child.on("close", code => resolve({ code, stdout, stderr }));
    child.stdin.end(input);
  });
}

test("catalog includes the full selected operation boundary", async () => {
  const result = await command(["--catalog"]);
  assert.equal(result.code, 0, result.stderr);
  const catalog=JSON.parse(result.stdout);
  assert.equal(catalog.operations.length, 28);
  for (const operation of catalog.operations) assert.match(operation.description, /[\u4e00-\u9fff]/);
  for (const name of ["analyze_javascript_application", "trace_javascript_semantics", "inspect_web_network_capture", "capture_browser_scenario", "capture_electron_scenario"])
    assert.ok(catalog.operations.some(operation => operation.name === name && operation.input_schema));
  const scenario = catalog.operations.find(operation => operation.name === "capture_browser_scenario");
  assert.match(scenario.description, /CloakBrowser/);
  const browserSchema = JSON.stringify(scenario.input_schema.properties.browser);
  assert.ok(!browserSchema.includes('\"headless\"'));
  assert.ok(!browserSchema.includes('\"executable_path\"'));
});

test("SIGTERM exits an idle JSONL process while stdin remains open", { timeout:10000 }, async () => {
  const child = spawn(process.execPath, ["dist/trace-cli.js"], { stdio:["pipe","pipe","pipe"] });
  try {
    // Receiving a response proves handlers and the input reader are installed.
    const first = new Promise(resolve => child.stdout.once("data", resolve));
    child.stdin.write('{"operation":"analyze_javascript_application","input":{}}\n');
    await first;
    const finished = new Promise(resolve => child.once("exit", (code, signal) => resolve({code,signal})));
    child.kill("SIGTERM");
    const result = await finished;
    assert.equal(result.code,143);
    assert.equal(result.signal,null);
  } finally { child.stdin.destroy(); if (child.exitCode === null) child.kill("SIGKILL"); }
});

test("named localtool accepts a formatted JSON object on stdin", async () => {
  const result=await command(["--operation","analyze_javascript_application"], '{\n  "input_path": 5\n}\n');
  assert.equal(result.code,1);
  const envelope=JSON.parse(result.stdout);
  assert.equal(envelope.ok,false);
  assert.equal(envelope.error.name,"ZodError");
});

test("JSONL performs real directory analysis through the isolated semantic worker", { timeout: 120000 }, async () => {
  const dir=await mkdtemp(join(tmpdir(), "trace-web-analysis-test-"));
  try {
    await writeFile(join(dir,"package.json"), JSON.stringify({ name:"trace-fixture", main:"main.cjs" }));
    await writeFile(join(dir,"main.cjs"), 'const {BrowserWindow,ipcMain}=require("electron"); function echo(value){return value;} ipcMain.handle("echo",(_event,value)=>echo(value)); new BrowserWindow({webPreferences:{preload:"./preload.cjs",contextIsolation:true}});');
    await writeFile(join(dir,"preload.cjs"), 'const {contextBridge,ipcRenderer}=require("electron"); contextBridge.exposeInMainWorld("bridge",{echo:value=>ipcRenderer.invoke("echo",value)});');
    const output=join(dir,"result.json");
    const result=await command([], JSON.stringify({ operation:"analyze_javascript_application", input:{input_path:dir}, output_file:output })+"\n");
    assert.equal(result.code,0,result.stdout+result.stderr);
    const envelope=JSON.parse(await readFile(output,"utf8"));
    assert.equal(envelope.ok,true);
    const analysis=envelope.value.normalized_result;
    assert.ok(analysis.semantic_graph.nodes.length>0);
    assert.ok(analysis.graph.nodes.length>0);
    assert.equal(analysis.summary.browser_windows,1);
    assert.ok(analysis.summary.ipc.operations>=2);
    assert.ok(analysis.statistics.parsed_javascript_files>=2);
    assert.ok(envelope.value.evidence_id);
  } finally { await rm(dir,{ recursive:true,force:true }); }
});

test("invalid input is a structured failure and existing output files remain intact", async () => {
  const dir=await mkdtemp(join(tmpdir(),"trace-web-output-test-"));
  try {
    const output=join(dir,"existing.json"); await writeFile(output,"keep");
    const invalid=await command([],JSON.stringify({operation:"analyze_javascript_application",input:{}})+"\n");
    assert.equal(invalid.code,1); assert.equal(JSON.parse(invalid.stdout).ok,false);
    const result=await command([],JSON.stringify({operation:"list_browser_targets",input:{cdp_endpoint:"http://127.0.0.1:1"},output_file:output})+"\n");
    assert.equal(result.code,1); assert.equal(await readFile(output,"utf8"),"keep");
  } finally { await rm(dir,{recursive:true,force:true}); }
});

test("HAR inspection traverses the actual owned decoder process and retains original records", { timeout: 60000 }, async () => {
  const dir=await mkdtemp(join(tmpdir(),"trace-har-test-"));
  try {
    const path=join(dir,"capture.har");
    await writeFile(path,JSON.stringify(historicalHar()));
    const result=await command([],JSON.stringify({operation:"inspect_web_network_capture",input:{capture_path:path,format:"har"}})+"\n");
    assert.equal(result.code,0,result.stdout+result.stderr);
    const envelope=JSON.parse(result.stdout);
    assert.equal(envelope.ok,true);
    const capture=envelope.value.normalized_result;
    assert.equal(capture.total_records,1);
    assert.equal(capture.records[0].ordinal,0);
    assert.ok(JSON.stringify(capture).includes("雪😀"));
  } finally { await rm(dir,{recursive:true,force:true}); }
});
