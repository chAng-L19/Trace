// Server-only, real pinned pi: TRACE_PI_RUNTIME=/path/runtime/pi node host_tools_test.mjs --report /tmp/pi-tools.json
// --host selects another saved host source. Probes observe context and count
// executable construction; the real loop, validators and execution stay intact.
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { readFile, writeFile } from 'node:fs/promises';
import { createInterface } from 'node:readline';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const option = (name, fallback) => { const i = process.argv.indexOf(name); return i < 0 ? fallback : process.argv[i + 1]; };
const runtime = resolve(process.env.TRACE_PI_RUNTIME || here);
const source = resolve(option('--host', resolve(here, 'host.mjs')));
let host = await readFile(source, 'utf8');
const instrument = (anchor, replacement) => {
  assert.equal(host.split(anchor).length, 2, 'instrumentation anchor changed: ' + anchor);
  host = host.replace(anchor, replacement);
};
host = 'let fixtureBuilds = 0, fixtureManuals = 0, fixtureStart = 0, fixtureManualStart = 0;\n' + host;
instrument('function executable(definition, manual = false) {',
  'function executable(definition, manual = false) {\n  fixtureBuilds++; if (manual) fixtureManuals++;');
instrument('  return { context: activeContext };', `  fixtureStart = fixtureBuilds; fixtureManualStart = fixtureManuals;
  await rpc('__fixture_prepare', { canonical: canonicalTools, converted: activeContext });
  return { context: activeContext };`);
instrument('    prepareTools(response);', `    prepareTools(response);
    await rpc('__fixture_model', { canonical: canonicalTools, tools: activeContext.tools,
      builds: fixtureBuilds - fixtureStart, manuals: fixtureManuals - fixtureManualStart });`);
const versions = {};
for (const name of ['pi-agent-core', 'pi-ai']) {
  const pkg = JSON.parse(await readFile(resolve(runtime, 'node_modules/@earendil-works', name, 'package.json'), 'utf8'));
  assert.equal(pkg.version, '1.4.0', 'requires pinned ' + name); versions[name] = pkg.version;
}
const tool = (name, minimum = 0) => ({ type: 'function', function: { name,
  description: ('fixture ' + name + ' ').repeat(128), parameters: { type: 'object',
    properties: { value: { type: 'integer', minimum } }, required: ['value'] } } });
const read = tool('read'), write = tool('write'), revised = tool('read', 1), beta = tool('beta');
const base = [read, write], changed = [revised, write];
const scenarios = [];
const add = (label, prepared, final = prepared, extra = {}) => scenarios.push({ label, prepared, final,
  parallel: ['read'], finalParallel: ['read'], names: ['read'], ...extra });
for (let i = 0; i < 24; i++) add('stable-' + i, base, base, { names: Array(6).fill('read') });
add('retry-same-name-schema', base, changed);
add('retry-next-canonical', changed);
add('parallel-only', changed, changed, { finalParallel: [], names: ['read', 'read'] });
add('parallel-next-prepare', changed);
add('recovery-new-schema', changed, [beta], { finalParallel: [], names: ['beta'] });
add('recovery-next-canonical', [beta], [beta], { parallel: [], finalParallel: [], names: ['beta'] });
add('model-change-not-canonical', [beta], base, { parallel: [], names: ['read'] });
add('prepare-reverts-to-canonical', [beta], [beta], { parallel: [], finalParallel: [], names: ['beta'] });
add('report-null-clear', base, null, { finalParallel: [], names: [] });
add('report-next-canonical', null, null, { parallel: [], finalParallel: [], names: [] });
add('empty-clear', base, [], { finalParallel: [], names: ['read'] });
add('empty-next-canonical', [], [], { parallel: [], finalParallel: [], names: [] });
add('manual-live', base, base, { names: ['ghost'] });
add('manual-next-turn-cleanup', base);
add('manual-to-known', base, [...base, tool('ghost')], { names: ['ghost'] });
add('known-to-manual', [...base, tool('ghost')], base, { names: ['ghost'] });
add('mixed-sequential', base, base, { names: ['read', 'write'] });
const messages = [
  { role: 'system', content: 'same canonical header' },
  { role: 'user', content: 'fixture objective' },
  { role: 'assistant', tool_calls: [{ id: 'old-read', name: 'read', arguments: '{"value":2}' },
    { id: 'old-beta', name: 'beta', arguments: '{"value":2}' }] },
  { role: 'tool', tool_call_id: 'old-read', content: 'old read result' },
  { role: 'tool', tool_call_id: 'old-beta', content: 'old beta result' },
];
const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
const simplified = (tools) => tools.map((t) => ({ name: t.name, manual: t.manual,
  parameters: t.parameters, executionMode: t.executionMode }));

async function runFixture(mode) {
  const child = spawn(process.env.TRACE_PI_NODE || process.execPath, ['--input-type=module', '--eval', host],
    { cwd: runtime, stdio: ['pipe', 'pipe', 'pipe'], env: { ...process.env } });
  let current = -1, revision = 0, previous, stderr = '', fatal, done = false, models = 0, finishes = 0;
  let modelBytes = 0, schemas = 0, active = 0, peak = 0;
  const observations = [], events = [], measurements = [], pending = new Map(), timers = new Set();
  const watchdog = setTimeout(() => { fatal ||= new Error('fixture exceeded 120 seconds'); child.kill('SIGKILL'); }, 120000);
  child.stderr.on('data', (data) => { stderr += data; });
  const exit = new Promise((resolveExit, reject) => { child.on('error', reject); child.on('close', (code, signal) => resolveExit({ code, signal })); });
  const send = (id, result) => child.stdin.write(JSON.stringify({ id, result }) + '\n');
  try {
    for await (const line of createInterface({ input: child.stdout, crlfDelay: Infinity })) {
      const rpc = JSON.parse(line);
      const scenario = scenarios[current];
      switch (rpc.method) {
        case 'prepare': {
          assert(++current < scenarios.length, 'unexpected turn');
          assert.equal(rpc.params.revision, revision);
          const next = scenarios[current];
          const prepared = { base_revision: revision, revision: ++revision, message_prefix: previous ? messages.length : 0,
            messages: previous ? [] : messages, parallel_tools: next.parallel };
          if (!previous || !same(previous.prepared, next.prepared)) prepared.tools = next.prepared;
          previous = next; send(rpc.id, prepared); break;
        }
        case '__fixture_prepare': {
          const expected = scenario.prepared ?? [];
          assert.deepEqual(rpc.params.canonical, expected, scenario.label + ' canonical schema');
          assert.deepEqual(rpc.params.converted.tools.map((t) => t.name), expected.map((t) => t.function.name), scenario.label + ' stale manual tools');
          assert(rpc.params.converted.tools.every((t) => !t.manual), scenario.label + ' retained manual marker');
          const history = rpc.params.converted.messages.find((m) => m.role === 'assistant');
          for (const call of history.content) {
            assert.deepEqual(call.arguments, expected.some((t) => t.function.name === call.name) ? { value: 2 } : {}, scenario.label + ' retained conversion used wrong schema');
          }
          observations.push(rpc.params.converted); send(rpc.id, { ok: true }); break;
        }
        case 'model': {
          models++;
          const response = { content: 'fixture ' + current, tool_calls: scenario.names.map((name, i) =>
            ({ id: current + '-' + i, name, arguments: '{"value":2}' })) };
          const completed = { response };
          if (mode === 'full' || !same(scenario.prepared, scenario.final) || !same(scenario.parallel, scenario.finalParallel)) {
            completed.tools = scenario.final; completed.parallel_tools = scenario.finalParallel; schemas++;
          }
          modelBytes += Buffer.byteLength(JSON.stringify({ id: rpc.id, result: completed }) + '\n');
          send(rpc.id, completed); break;
        }
        case '__fixture_model': {
          assert.deepEqual(rpc.params.canonical, scenario.prepared ?? [], scenario.label + ' model advanced canonical baseline');
          const final = scenario.final ?? [];
          const manualNames = [...new Set(scenario.names.filter((name) => !final.some((t) => t.function.name === name)))];
          const expected = final.map((t) => ({ name: t.function.name, manual: false, parameters: t.function.parameters,
            executionMode: scenario.finalParallel.includes(t.function.name) ? 'parallel' : 'sequential' }));
          expected.push(...manualNames.map((name) => ({ name, manual: true, parameters: { type: 'object' }, executionMode: 'sequential' })));
          assert.deepEqual(simplified(rpc.params.tools), expected, scenario.label + ' final executable tools');
          measurements.push({ label: scenario.label, builds: rpc.params.builds, manuals: rpc.params.manuals });
          send(rpc.id, { ok: true }); break;
        }
        case 'tool': {
          const known = (scenario.final ?? []).some((t) => t.function.name === rpc.params.name);
          assert.equal(rpc.params.manual, !known, scenario.label + ' execution kind');
          assert.deepEqual(JSON.parse(rpc.params.arguments), { value: 2 }, scenario.label + ' validated arguments');
          assert.equal(rpc.params.parallel, scenario.names.length > 1 && scenario.names.every((name) => scenario.finalParallel.includes(name)), scenario.label + ' execution mode');
          events.push(['tool', current, rpc.params.call_id, rpc.params.name, rpc.params.manual, rpc.params.parallel]);
          const content = JSON.stringify({ stdout: rpc.params.call_id, exit_code: 0 });
          pending.set(rpc.params.call_id, content); peak = Math.max(peak, ++active);
          const timer = setTimeout(() => { timers.delete(timer); active--; send(rpc.id, { content }); }, 10);
          timers.add(timer); break;
        }
        case 'tool_result':
          assert.equal(rpc.params.content, pending.get(rpc.params.call_id));
          assert.equal(rpc.params.is_error, false); pending.delete(rpc.params.call_id);
          events.push(['result', current, rpc.params.call_id]); send(rpc.id, { ok: true }); break;
        case 'finish':
          finishes++; assert.equal(pending.size, 0); send(rpc.id, { action: models === scenarios.length ? 'end' : 'continue' }); break;
        case 'done': assert(!rpc.error, rpc.error); done = true; break;
        default: throw new Error('unexpected RPC ' + rpc.method);
      }
    }
  } catch (error) { fatal = error; child.kill('SIGKILL'); }
  const status = await exit; clearTimeout(watchdog); for (const timer of timers) clearTimeout(timer); child.stdin.destroy();
  if (fatal) throw new Error(mode + ': ' + fatal.stack + '\n' + stderr);
  assert.equal(status.code, 0, JSON.stringify(status) + ': ' + stderr);
  assert(done && models === scenarios.length && finishes === scenarios.length);
  assert.equal(peak, 5, 'parallel tool slots changed');
  for (let i = 0; i < scenarios.length; i++) {
    assert.deepEqual(events.filter((e) => e[0] === 'result' && e[1] === i).map((e) => e[2]),
      scenarios[i].names.map((_, n) => i + '-' + n), 'result declaration order');
  }
  return { observations, events, measurements, schemas, modelBytes, peak };
}

const full = await runFixture('full'), delta = await runFixture('delta');
assert.deepEqual(delta.observations, full.observations, 'canonical context differs');
for (const kind of ['tool', 'result']) {
  assert.deepEqual(delta.events.filter((event) => event[0] === kind), full.events.filter((event) => event[0] === kind), kind + ' declaration order differs');
}
assert(delta.measurements.slice(0, 24).every((m) => m.builds === 0), 'stable model rebuilt executables');
assert(full.measurements.slice(0, 24).every((m) => m.builds === base.length));
assert(delta.modelBytes < full.modelBytes, 'model IPC did not shrink');
const report = { status: 'pass', source, runtime, versions, rounds: scenarios.length,
  stable_model_schemas: { full: 24, delta: 0 }, model_schema_sets: { full: full.schemas, delta: delta.schemas },
  model_ipc_bytes: { full: full.modelBytes, delta: delta.modelBytes },
  stable_model_executable_builds: { full: 24 * base.length, delta: 0 }, peak_tool_callbacks: delta.peak,
  contexts_arguments_execution_and_result_order_equal: true, measurements: delta.measurements,
  scope: 'Deterministic RPC peer and real pi 1.4.0 loop; prepare construction excluded. No model latency, token or RSS claim.' };
const output = JSON.stringify(report, null, 2) + '\n';
if (option('--report')) await writeFile(resolve(option('--report')), output);
process.stdout.write(output);
