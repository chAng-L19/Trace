// Server-only: TRACE_PI_RUNTIME=/path/runtime/pi node host_context_test.mjs --rounds 100 --report /tmp/host-context.json
// Real host + real pinned runAgentLoop. The sole source insertion is an awaited
// read-only context/RSS probe; no provider/tool/loop implementation is replaced.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { readFile, writeFile } from 'node:fs/promises';
import { createInterface } from 'node:readline';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const option = (name, fallback) => { const i = process.argv.indexOf(name); return i < 0 ? fallback : process.argv[i + 1]; };
const rounds = Number(option('--rounds', '100'));
assert(Number.isInteger(rounds) && rounds >= 30 && rounds <= 1000, 'rounds must be 30..1000');
const runtime = resolve(process.env.TRACE_PI_RUNTIME || here);
const host = await readFile(resolve(here, 'host.mjs'), 'utf8');
const anchor = '  return { context: activeContext };';
assert.equal(host.split(anchor).length, 2, 'host context instrumentation anchor changed');
const instrumented = host.replace(anchor, '  await rpc("__fixture_context", { revision: contextRevision, canonical: canonicalMessages, tools: canonicalTools, converted: activeContext, rss: process.memoryUsage().rss });\n' + anchor);
const versions = {};
for (const name of ['pi-agent-core', 'pi-ai']) {
  const pkg = JSON.parse(await readFile(resolve(runtime, 'node_modules/@earendil-works', name, 'package.json'), 'utf8'));
  assert.equal(pkg.version, '1.4.0', 'fixture must run pinned ' + name); versions[name] = pkg.version;
}
const fixedTime = '2026-10-29T00:00:00Z';
const message = (role, content, id, extra = {}) => ({ role, content, id, created_at: fixedTime, ...extra });
const tool = (name) => ({ type: 'function', function: { name, description: 'fixture ' + name,
  parameters: { type: 'object', properties: { value: { type: 'integer' } }, required: ['value'] } } });
const alpha = tool('alpha'), beta = tool('beta');
const scenarios = [];
let history = [message('user', 'objective: preserve exact context ' + 'stable history '.repeat(1800), 'goal')];
for (let i = 0; i < rounds; i++) {
  const phase = i % 20;
  if (i && phase === 0) history = [message('user', 'compact summary ' + i, 'summary-' + i)];
  if (i % 7 === 0) history.push(message('user', 'late input ' + i + ': exact constraint', 'late-' + i));
  const name = i % 2 ? 'alpha' : 'beta';
  history.push(message('assistant', 'observed ' + i, 'assistant-' + i, { thinking: 'reason-' + i,
    tool_calls: [{ id: 'historic-' + i, name, arguments: JSON.stringify({ value: i }) }] }));
  history.push(message('tool', JSON.stringify({ stdout: 'result-' + i, exit_code: 0 }), 'tool-' + i, { tool_call_id: 'historic-' + i }));
  const tools = phase < 5 ? [alpha] : phase < 10 ? [alpha, beta] : phase < 15 ? [beta] : [];
  scenarios.push({ messages: [message('system', 'system contract epoch ' + Math.floor(i / 3), 'system'), ...structuredClone(history)], tools });
}

async function runFixture(mode) {
  const child = spawn(process.env.TRACE_PI_NODE || process.execPath, ['--input-type=module', '--eval', instrumented],
    { cwd: runtime, stdio: ['pipe', 'pipe', 'pipe'], env: { ...process.env } });
  let stderr = '', fatal, done = false, current = -1, models = 0, finishes = 0;
  let previous, revision = 0, sent = 0, received = 0, probeBytes = 0, heads = 0, schemas = 0, prefixes = 0;
  const observations = [], rss = [], events = [], pendingResults = new Map();
  const watchdog = setTimeout(() => { fatal ||= new Error('host fixture exceeded 120 seconds'); child.kill('SIGKILL'); }, 120000);
  child.stderr.on('data', (data) => { stderr += data; });
  const exit = new Promise((resolveExit, reject) => { child.on('error', reject); child.on('close', (code, signal) => resolveExit({ code, signal })); });
  const send = (id, result) => { const raw = JSON.stringify({ id, result }) + '\n'; sent += Buffer.byteLength(raw); child.stdin.write(raw); };
  const lines = createInterface({ input: child.stdout, crlfDelay: Infinity });
  try {
    for await (const line of lines) {
      const rpc = JSON.parse(line);
      if (rpc.method === '__fixture_context') probeBytes += Buffer.byteLength(line) + 1;
      else received += Buffer.byteLength(line) + 1;
      switch (rpc.method) {
        case 'prepare': {
          current++; assert(current < rounds, 'unexpected extra model turn');
          assert.equal(rpc.params.revision, revision, 'host lost revision');
          const expected = scenarios[current];
          let prefix = 0, head;
          if (mode === 'delta' && previous) {
            if (JSON.stringify(expected.messages[0]) !== JSON.stringify(previous.messages[0])) { head = expected.messages[0]; prefix = 1; }
            while (prefix < expected.messages.length && prefix < previous.messages.length &&
              JSON.stringify(expected.messages[prefix]) === JSON.stringify(previous.messages[prefix])) prefix++;
          }
          const prepared = { request_id: 'fixture-' + current, base_revision: mode === 'delta' && previous ? revision : 0,
            revision: ++revision, message_prefix: prefix, messages: expected.messages.slice(prefix), parallel_tools: [] };
          if (head) { prepared.message_head = head; heads++; }
          if (mode === 'full' || !previous || JSON.stringify(expected.tools) !== JSON.stringify(previous.tools)) { prepared.tools = expected.tools; schemas++; }
          if (prefix > 1) prefixes++;
          previous = expected; send(rpc.id, prepared); break;
        }
        case '__fixture_context': {
          const expected = scenarios[current];
          assert.deepEqual(rpc.params.canonical, expected.messages, mode + ' canonical mismatch round ' + current);
          assert.deepEqual(rpc.params.tools, expected.tools, mode + ' schema mismatch round ' + current);
          assert.deepEqual(rpc.params.converted.tools.map((t) => t.name), expected.tools.map((t) => t.function.name));
          const converted = rpc.params.converted.messages;
          assert.equal(converted.length, expected.messages.length);
          assert.equal(converted[0].content, expected.messages[0].content);
          for (const row of converted) for (const call of Array.isArray(row.content) ? row.content : []) {
            if (call.type !== 'toolCall') continue;
            const original = expected.messages.flatMap((m) => m.tool_calls ?? []).find((c) => c.id === call.id);
            assert(original, 'conversion introduced a call');
            const known = expected.tools.some((t) => t.function.name === original.name);
            assert.deepEqual(call.arguments, known ? JSON.parse(original.arguments) : {});
          }
          observations.push(rpc.params.converted); rss.push(rpc.params.rss); send(rpc.id, { ok: true }); break;
        }
        case 'model': {
          models++; const chosen = scenarios[current].tools[0];
          const response = chosen ? { content: 'execute ' + current, tool_calls: [{ id: 'live-' + current, name: chosen.function.name, arguments: JSON.stringify({ value: current }) }], stop_reason: 'tool_calls' }
            : { content: 'continue ' + current, stop_reason: 'stop' };
          send(rpc.id, { response }); break;
        }
        case 'tool': {
          assert.equal(rpc.params.manual, false); assert.equal(rpc.params.call_id, 'live-' + current);
          assert.equal(rpc.params.name, scenarios[current].tools[0].function.name);
          assert.deepEqual(JSON.parse(rpc.params.arguments), { value: current });
          const content = JSON.stringify({ stdout: 'actual fixture result ' + current, stderr: '', exit_code: 0 });
          pendingResults.set(rpc.params.call_id, content); events.push([current, 'tool', rpc.params.name]);
          send(rpc.id, { content }); break;
        }
        case 'tool_result':
          assert.equal(rpc.params.content, pendingResults.get(rpc.params.call_id));
          assert.equal(rpc.params.is_error, false); pendingResults.delete(rpc.params.call_id);
          send(rpc.id, { ok: true }); break;
        case 'finish':
          finishes++; send(rpc.id, { action: models >= rounds ? 'end' : 'continue' }); break;
        case 'done': assert(!rpc.error, rpc.error); done = true; break;
        default: throw new Error('unexpected host RPC ' + rpc.method);
      }
    }
  } catch (error) { fatal = error; child.kill('SIGKILL'); }
  const status = await exit; clearTimeout(watchdog); child.stdin.destroy();
  if (fatal) throw new Error(mode + ': ' + fatal.stack + '\n' + stderr);
  assert.equal(status.code, 0, mode + ' exited ' + JSON.stringify(status) + ': ' + stderr);
  assert(done && models === rounds && observations.length === rounds && finishes === rounds, 'loop did not finish every canonical turn');
  assert.equal(events.length, scenarios.filter((s) => s.tools.length).length, 'real pi skipped a tool execution');
  assert.equal(pendingResults.size, 0);
  return { observations, events, stats: { mode, rounds, finishes, rpc_sent_bytes: sent, rpc_received_bytes: received,
    probe_bytes_excluded: probeBytes, message_heads: heads, schema_updates: schemas, retained_prefixes: prefixes,
    rss_samples_bytes: rss, rss_peak_bytes: Math.max(...rss), rss_growth_after_warmup_bytes: Math.max(...rss.slice(20)) - Math.min(...rss.slice(10, 20)) } };
}

const baseline = await runFixture('full');
const delta = await runFixture('delta');
assert.deepEqual(delta.observations, baseline.observations, 'incremental converted context differs from canonical rebuild');
assert.deepEqual(delta.events, baseline.events, 'incremental tools execute differently');
assert(delta.stats.message_heads > 0 && delta.stats.retained_prefixes > 0 && delta.stats.schema_updates > 3);
assert(delta.stats.rpc_sent_bytes < baseline.stats.rpc_sent_bytes, 'incremental transport did not reduce RPC bytes');
const growthLimit = Number(option('--max-rss-growth-mib', '96')) * 1024 * 1024;
assert(delta.stats.rss_growth_after_warmup_bytes < growthLimit, 'Node RSS growth exceeded declared fixture ceiling');
const report = { status: 'pass', source: resolve(here, 'host.mjs'), runtime, versions, rounds,
  coverage: ['actual runAgentLoop', 'message_head', 'schema add/delete', 'compaction', 'late input', 'full rebuild equivalence', 'real tool execution'],
  rss_scope: 'Actual child Node RSS; includes pi loop history and probe serialization. Finite fixture measurement, not an unbounded-memory proof.',
  rpc_saving_fraction: 1 - delta.stats.rpc_sent_bytes / baseline.stats.rpc_sent_bytes, baseline: baseline.stats, delta: delta.stats };
const output = JSON.stringify(report, null, 2) + '\n';
if (option('--report')) await writeFile(resolve(option('--report')), output);
process.stdout.write(output);

