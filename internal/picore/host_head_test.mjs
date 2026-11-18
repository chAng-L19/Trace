// Server-only, real pinned pi loop: TRACE_PI_RUNTIME=/path/runtime/pi node host_head_test.mjs --report /tmp/pi-head.json
// --host /path/baseline/host.mjs runs the same fixture against the old host; it
// writes the measurements before failing the retained-head conversion gate.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { readFile, writeFile } from 'node:fs/promises';
import { createInterface } from 'node:readline';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const option = (name, fallback) => { const i = process.argv.indexOf(name); return i < 0 ? fallback : process.argv[i + 1]; };
const runtime = resolve(process.env.TRACE_PI_RUNTIME || here);
const source = resolve(option('--host', resolve(here, 'host.mjs')));
const historySize = Number(option('--history', '128'));
assert(Number.isInteger(historySize) && historySize > 0 && historySize <= 1000);
let host = await readFile(source, 'utf8');
const instrument = (anchor, replacement) => {
  assert.equal(host.split(anchor).length, 2, 'instrumentation anchor changed: ' + anchor);
  host = host.replace(anchor, replacement);
};
// Only add counters and an awaited read-only probe. No loop, provider, tool or
// context conversion implementation is replaced by this instrumentation.
host = `let fixtureConversions = 0, fixtureIdentity = 0;
const fixtureIDs = new WeakMap();
const fixtureID = (message) => { if (!fixtureIDs.has(message)) fixtureIDs.set(message, ++fixtureIdentity); return fixtureIDs.get(message); };
` + host;
instrument('function assistant(response, timestamp = Date.now()) {', 'function assistant(response, timestamp = Date.now()) {\n  fixtureConversions++;');
instrument('async function prepareRequest() {', `async function prepareRequest() {
  const fixtureStart = fixtureConversions;
  const fixturePrevious = convertedMessages.slice();
  const fixturePreviousJSON = JSON.stringify(fixturePrevious);`);
instrument('  return { context: activeContext };', `  await rpc("__fixture_context", {
    canonical: canonicalMessages, tools: canonicalTools, converted: activeContext,
    ids: convertedMessages.map(fixtureID), conversions: fixtureConversions - fixtureStart,
    previous_unchanged: JSON.stringify(fixturePrevious) === fixturePreviousJSON,
    declaration_detached: canonicalMessages[0]?.role !== "system" || activeContext.messages[0] !== convertedMessages[0],
  });
  return { context: activeContext };`);
const versions = {};
for (const name of ['pi-agent-core', 'pi-ai']) {
  const pkg = JSON.parse(await readFile(resolve(runtime, 'node_modules/@earendil-works', name, 'package.json'), 'utf8'));
  assert.equal(pkg.version, '1.4.0', 'fixture requires pinned ' + name);
  versions[name] = pkg.version;
}
const stamp = '2026-11-17T00:00:00Z';
const message = (role, content, extra = {}) => ({ role, content, created_at: stamp, ...extra });
const tool = (name, minimum = 0) => ({ type: 'function', function: { name, description: 'fixture ' + name,
  parameters: { type: 'object', properties: { value: { type: 'integer', minimum } }, required: ['value'] } } });
const alpha = tool('alpha'), beta = tool('beta');
const tail = (id, name = 'alpha') => [message('assistant', 'observation ' + id, {
  tool_calls: [{ id, name, arguments: JSON.stringify({ value: 2, payload: 'x'.repeat(4096) }) }] }),
message('tool', JSON.stringify({ stdout: id, exit_code: 0 }), { tool_call_id: id })];
let history = [message('user', 'objective'), ...Array.from({ length: historySize }, (_, i) => tail('old-' + i, i % 2 ? 'alpha' : 'beta')).flat()];
const scenarios = [];
const add = (label, head, tools = [alpha, beta], extra = {}) => scenarios.push({ label,
  messages: head === null ? history.slice() : [message('system', head), ...history], tools, ...extra });
add('initial', 'system 0');
for (let i = 1; i <= 24; i++) add('head-only-' + i, 'system ' + i);
history = [...history, ...tail('new-tail')];
add('head-and-tail', 'system with tail');
add('same-name-schema', 'schema update', [tool('alpha', 1), beta]);
add('known-to-manual', 'tool deleted', []);
add('manual-to-known', 'tool restored');
add('manual-live-call', 'manual live', [alpha, beta], { live: 'ghost' });
add('manual-not-cached', 'manual removed');
add('forced-full', 'full reset', [alpha, beta], { forceFull: true });
history = [message('user', 'compacted summary'), ...tail('compact')];
add('compaction', 'compacted system');
add('unchanged', 'compacted system');
add('no-system', null);
history = [...history, ...tail('no-system-tail')];
add('no-system-tail', null);
add('system-restored', 'restored system');
add('tools-empty', 'empty schema', []);
add('empty-schema-head', 'empty schema 2', []);

async function runFixture(mode) {
  const child = spawn(process.env.TRACE_PI_NODE || process.execPath, ['--input-type=module', '--eval', host],
    { cwd: runtime, stdio: ['pipe', 'pipe', 'pipe'], env: { ...process.env } });
  let current = -1, revision = 0, previous, prepared, previousIDs = [], stderr = '', fatal, done = false;
  let models = 0, finishes = 0;
  const observations = [], events = [], measurements = [], pending = new Map();
  const watchdog = setTimeout(() => { fatal ||= new Error('fixture exceeded 120 seconds'); child.kill('SIGKILL'); }, 120000);
  child.stderr.on('data', (data) => { stderr += data; });
  const exit = new Promise((resolveExit, reject) => { child.on('error', reject); child.on('close', (code, signal) => resolveExit({ code, signal })); });
  const send = (id, result) => child.stdin.write(JSON.stringify({ id, result }) + '\n');
  try {
    for await (const line of createInterface({ input: child.stdout, crlfDelay: Infinity })) {
      const rpc = JSON.parse(line);
      switch (rpc.method) {
        case 'prepare': {
          assert(++current < scenarios.length, 'unexpected model turn');
          assert.equal(rpc.params.revision, revision);
          const expected = scenarios[current];
          const delta = mode === 'delta' && previous && !expected.forceFull;
          let prefix = 0, head;
          if (delta) {
            if (expected.messages[0]?.role === 'system' && previous.messages[0]?.role === 'system' &&
                JSON.stringify(expected.messages[0]) !== JSON.stringify(previous.messages[0])) { head = expected.messages[0]; prefix = 1; }
            while (prefix < expected.messages.length && prefix < previous.messages.length &&
              JSON.stringify(expected.messages[prefix]) === JSON.stringify(previous.messages[prefix])) prefix++;
          }
          prepared = { base_revision: delta ? revision : 0, revision: ++revision, message_prefix: prefix,
            messages: expected.messages.slice(prefix), parallel_tools: [] };
          if (head) prepared.message_head = head;
          if (!delta || JSON.stringify(expected.tools) !== JSON.stringify(previous.tools)) prepared.tools = expected.tools;
          previous = expected;
          send(rpc.id, prepared); break;
        }
        case '__fixture_context': {
          const expected = scenarios[current], actual = rpc.params;
          assert.deepEqual(actual.canonical, expected.messages, expected.label + ' canonical');
          assert.deepEqual(actual.tools, expected.tools, expected.label + ' schemas');
          assert.deepEqual(actual.converted.tools.map((t) => ({ name: t.name, parameters: t.parameters })),
            expected.tools.map((t) => ({ name: t.function.name, parameters: t.function.parameters })));
          assert(actual.previous_unchanged, expected.label + ' mutated previous converted context');
          const offset = expected.messages[0]?.role !== 'system' && expected.tools.length ? 1 : 0;
          const converted = actual.converted.messages;
          assert.equal(converted.length, expected.messages.length + offset);
          assert.equal(actual.ids.length, expected.messages.length, 'synthetic declaration entered canonical cache');
          assert.equal(converted[offset].content, expected.messages[0].content);
          for (const row of converted) {
            if (row.role === 'toolResult') {
              const original = expected.messages.flatMap((m) => m.tool_calls ?? []).find((c) => c.id === row.toolCallId);
              assert.equal(row.toolName, original.name, 'retained prefix lost tool-result name');
            }
            for (const call of Array.isArray(row.content) ? row.content : []) {
              if (call.type !== 'toolCall') continue;
              const original = expected.messages.flatMap((m) => m.tool_calls ?? []).find((c) => c.id === call.id);
              assert(original, 'invented historical call');
              assert.deepEqual(call.arguments, expected.tools.some((t) => t.function.name === original.name) ? JSON.parse(original.arguments) : {});
            }
          }
          const prefix = Object.hasOwn(prepared, 'tools') ? 0 : prepared.message_prefix;
          const firstRetained = prepared.message_head ? 1 : 0;
          const retained = actual.ids.slice(firstRetained, prefix).filter((id, i) => id === previousIDs[i + firstRetained]).length;
          const expectedConversions = expected.messages.slice(prefix).filter((m) => m.role === 'assistant').length;
          measurements.push({ label: expected.label, conversions: actual.conversions, expected_conversions: expectedConversions,
            retained, expected_retained: Math.max(0, prefix - firstRetained), declaration_detached: actual.declaration_detached });
          previousIDs = actual.ids;
          observations.push(createHash('sha256').update(JSON.stringify(actual.converted)).digest('hex'));
          send(rpc.id, { ok: true }); break;
        }
        case 'model': {
          models++;
          const name = scenarios[current].live ?? scenarios[current].tools[0]?.function.name;
          const response = name ? { tool_calls: [{ id: 'live-' + current, name, arguments: '{"value":2}' }], stop_reason: 'tool_calls' }
            : { content: 'continue', stop_reason: 'stop' };
          send(rpc.id, { response }); break;
        }
        case 'tool': {
          const expected = scenarios[current], name = expected.live ?? expected.tools[0]?.function.name;
          assert.equal(rpc.params.name, name);
          assert.equal(rpc.params.manual, !expected.tools.some((t) => t.function.name === name));
          assert.deepEqual(JSON.parse(rpc.params.arguments), { value: 2 });
          const content = JSON.stringify({ stdout: 'fixture result ' + current, exit_code: 0 });
          pending.set(rpc.params.call_id, content); events.push([current, rpc.params.name, rpc.params.manual]);
          send(rpc.id, { content }); break;
        }
        case 'tool_result':
          assert.equal(rpc.params.content, pending.get(rpc.params.call_id));
          assert.equal(rpc.params.is_error, false); pending.delete(rpc.params.call_id);
          send(rpc.id, { ok: true }); break;
        case 'finish':
          finishes++; send(rpc.id, { action: models === scenarios.length ? 'end' : 'continue' }); break;
        case 'done': assert(!rpc.error, rpc.error); done = true; break;
        default: throw new Error('unexpected host RPC ' + rpc.method);
      }
    }
  } catch (error) { fatal = error; child.kill('SIGKILL'); }
  const status = await exit; clearTimeout(watchdog); child.stdin.destroy();
  if (fatal) throw new Error(mode + ': ' + fatal.stack + '\n' + stderr);
  assert.equal(status.code, 0, JSON.stringify(status) + ': ' + stderr);
  assert(done && models === scenarios.length && finishes === scenarios.length);
  assert.equal(pending.size, 0);
  assert.equal(events.length, scenarios.filter((s) => s.live || s.tools.length).length);
  return { observations, events, measurements };
}

const full = await runFixture('full'), delta = await runFixture('delta');
assert.deepEqual(delta.observations, full.observations, 'delta context differs from full rebuild');
assert.deepEqual(delta.events, full.events, 'delta execution differs from full rebuild');
const violations = delta.measurements.filter((m) => m.conversions !== m.expected_conversions ||
  m.retained !== m.expected_retained || !m.declaration_detached);
const total = (run) => run.measurements.reduce((n, row) => n + row.conversions, 0);
const report = { status: violations.length ? 'fail' : 'pass', source, runtime, versions, history_size: historySize,
  rounds: scenarios.length, full_assistant_conversions: total(full), delta_assistant_conversions: total(delta),
  full_delta_contexts_and_execution_equal: true, measurements: delta.measurements, violations,
  scope: 'Real pinned pi loop with deterministic RPC peer; exact conversion counts, not model latency, token or RSS savings.' };
const output = JSON.stringify(report, null, 2) + '\n';
if (option('--report')) await writeFile(resolve(option('--report')), output);
process.stdout.write(output);
assert.equal(violations.length, 0, 'system-head updates rebuilt retained history or aliased tool declarations');
