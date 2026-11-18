// Pi owns the conversational loop; Go owns provider I/O and durable transactions.
// Source API: pi e65b3b88c08412bb8ffbd21fb5dc5756064a6e5a (MIT), agent-core 1.4.0.
import { createInterface } from "node:readline";
import { isDeepStrictEqual } from "node:util";
import { runAgentLoop, runToolCall } from "@earendil-works/pi-agent-core";
import { createAssistantMessageEventStream, toToolDeclaration } from "@earendil-works/pi-ai";

class Halt extends Error {}

const controller = new AbortController();
const pending = new Map();
const input = createInterface({ input: process.stdin, crlfDelay: Infinity });
let sequence = 0;
let stopped;
let settled = false;
let activeContext = { messages: [], tools: [] };
let rawArguments = new Map();
let parallelTools = new Set();
let parallelBatch = false;
let activeTools = 0;
const toolWaiters = [];
let contextRevision = 0;
let canonicalMessages = [];
let convertedMessages = [];
let canonicalTools = [];

async function withToolSlot(operation) {
  if (activeTools >= 5) await new Promise((resolve) => toolWaiters.push(resolve));
  activeTools++;
  try { assertRunning(); return await operation(); }
  finally { activeTools--; toolWaiters.shift()?.(); }
}

// This model identifies the transport adapter, not a billed model. Go retains
// authoritative model metadata, token usage, pricing and attachment hydration.
const model = {
  id: "trace-go-provider", name: "Trace Go provider",
  api: "openai-completions", provider: "trace", baseUrl: "",
  reasoning: false, input: ["text"], contextWindow: 0, maxTokens: 0,
  cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 },
};

function stop(error) {
  stopped ??= error;
  controller.abort(stopped);
  for (const waiter of pending.values()) waiter.reject(stopped);
  pending.clear();
}

function assertRunning() {
  if (stopped) throw stopped;
}

function write(value) {
  return new Promise((resolve, reject) => {
    process.stdout.write(JSON.stringify(value) + "\n", (error) => {
      if (error) reject(error);
      else resolve();
    });
  });
}

function rpc(method, params = {}) {
  assertRunning();
  const id = ++sequence;
  return new Promise((resolve, reject) => {
    pending.set(id, { resolve, reject });
    void write({ id, method, params }).catch(stop);
  });
}

input.on("line", (line) => {
  if (settled || stopped) return;
  try {
    const response = JSON.parse(line);
    if (!response || typeof response !== "object" || !Number.isInteger(response.id)) {
      throw new Error("Invalid Go RPC response envelope");
    }
    const waiter = pending.get(response.id);
    if (!waiter) throw new Error(`Unexpected Go RPC response id ${response.id}`);
    if (response.halt || response.result?.halt) {
      stop(new Halt("Go halted the run"));
    } else if (response.error) {
      stop(new Error(response.error));
    } else {
      pending.delete(response.id);
      waiter.resolve(response.result);
    }
  } catch (error) {
    stop(error);
  }
});
input.on("close", () => {
  if (!settled) stop(new Error("Go RPC input closed before run settlement"));
});
input.on("error", stop);
process.stdin.on("error", stop);
process.stdout.on("error", stop);

function usage() {
  // Pi requires numeric usage; these adapter placeholders never enter Go's ledger.
  return { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0,
    cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } };
}

function toolCall(call) {
  const block = { type: "toolCall", id: call.id, name: call.name, arguments: {} };
  const known = activeContext.tools.some((tool) => tool.name === call.name && !tool.manual);
  if (!known) return block;
  try {
    const args = JSON.parse(call.arguments);
    if (args === null || typeof args !== "object" || Array.isArray(args)) {
      throw new Error("tool arguments must be a JSON object");
    }
    block.arguments = args;
  } catch (error) {
    // Let pi's validation path produce the durable tool_result callback.
    block.argumentsError = error.message;
  }
  return block;
}

function assistant(response, timestamp = Date.now()) {
  const content = [];
  if (response.thinking) content.push({ type: "thinking", thinking: response.thinking });
  if (response.content) content.push({ type: "text", text: response.content });
  for (const call of response.tool_calls ?? []) content.push(toolCall(call));
  let reason = response.stop_reason || (response.tool_calls?.length ? "tool_calls" : "stop");
  if (reason === "tool_calls") reason = "toolUse";
  if (response.error) reason = "error";
  return { role: "assistant", content, api: model.api, provider: model.provider,
    model: model.id, usage: usage(), stopReason: reason, timestamp,
    ...(response.error ? { errorMessage: response.error } : {}) };
}

function transcript(messages, prefix = 0) {
  const names = new Map();
  for (const message of messages.slice(0, prefix)) {
    for (const call of message.tool_calls ?? []) names.set(call.id, call.name);
  }
  return convertedMessages.slice(0, prefix).concat(messages.slice(prefix).map((message) => {
    const timestamp = Date.parse(message.created_at) || 0;
    if (message.role === "assistant") {
      for (const call of message.tool_calls ?? []) names.set(call.id, call.name);
      return assistant(message, timestamp);
    }
    if (message.role === "tool") {
      return { role: "toolResult", toolCallId: message.tool_call_id,
        toolName: names.get(message.tool_call_id) ?? "",
        content: [{ type: "text", text: message.content }], isError: false, timestamp };
    }
    return { role: message.role, content: message.content, timestamp };
  }));
}

function executable(definition, manual = false) {
  const tool = definition.function;
  return {
    name: tool.name, label: tool.name, description: tool.description ?? "",
    manual,
    parameters: tool.parameters ?? { type: "object" }, executionMode: !manual && parallelTools.has(tool.name) ? "parallel" : "sequential",
    async execute(callId, args) {
      // Trace journals the original arguments. Reject pi coercion rather than
      // validating one value and executing a different durable value.
      if (!manual && !isDeepStrictEqual(JSON.parse(rawArguments.get(callId)), args)) {
        throw new Error("Tool arguments changed during schema validation; submit schema-correct arguments without coercion");
      }
      const result = await withToolSlot(() => rpc("tool", { call_id: callId, name: tool.name,
        arguments: manual ? rawArguments.get(callId) : JSON.stringify(args), manual, parallel: parallelBatch }));
      assertRunning();
      let isError = result.is_error === true;
      try {
        const saved = JSON.parse(result.content);
        isError ||= saved.exit_code !== undefined && saved.exit_code !== 0;
        isError ||= Boolean(saved.error) || saved.mcp_is_error === true;
      } catch {}
      return { content: [{ type: "text", text: result.content }],
        details: {}, isError };
    },
  };
}

async function prepareRequest() {
  let prepared = await rpc("prepare", { revision: contextRevision });
  while (prepared.recovery) {
    parallelTools = new Set(prepared.parallel_tools ?? []);
    activeContext = { messages: [], tools: (prepared.tools ?? []).map((definition) => executable(definition)) };
    prepareTools(prepared.recovery);
    const message = assistant(prepared.recovery);
    for (const call of message.content.filter((part) => part.type === "toolCall")) {
      const outcome = await runToolCall(call, { tools: activeContext.tools,
        assistantMessage: message, context: activeContext, signal: controller.signal });
      // runToolCall catches execute errors, including Go's halt sentinel.
      // Stop outside its catch before persisting any synthetic result.
      assertRunning();
      await emit({ type: "message_end", message: { role: "toolResult",
        toolCallId: call.id, toolName: call.name,
        content: outcome.result.content, isError: outcome.isError } });
    }
    prepared = await rpc("prepare", { revision: contextRevision });
  }
  parallelTools = new Set(prepared.parallel_tools ?? []);
  if (prepared.base_revision !== 0 && prepared.base_revision !== contextRevision) {
    contextRevision = 0;
    return prepareRequest();
  }
  const prefix = prepared.base_revision === 0 ? 0 : prepared.message_prefix;
  if (!Number.isInteger(prefix) || prefix < 0 || prefix > canonicalMessages.length) throw new Error("Invalid context prefix");
  canonicalMessages = canonicalMessages.slice(0, prefix).concat(prepared.messages ?? []);
  if (prepared.message_head) canonicalMessages[0] = prepared.message_head;
  contextRevision = prepared.revision;
  if (Object.hasOwn(prepared, "tools")) canonicalTools = prepared.tools ?? [];
  const tools = canonicalTools.map((definition) => executable(definition));
  activeContext.tools = tools;
  const messages = transcript(canonicalMessages, Object.hasOwn(prepared, "tools") ? 0 : prefix);
  // A changed system head does not invalidate the converted history prefix.
  // Tool-schema changes still rebuild it because known calls affect arguments.
  if (prepared.message_head) messages[0] = transcript([prepared.message_head])[0];
  convertedMessages = messages.slice();
  // prepareRequest runs after pi's tool-declaration pass. Replacing canonical
  // context must therefore include its tool declarations in the same update.
  if (messages[0]?.role === "system") {
    messages[0] = { ...messages[0], toolsAdded: tools.map(toToolDeclaration) };
  } else if (tools.length) {
    messages.unshift({ role: "system", content: "", timestamp: 0,
      toolsAdded: tools.map(toToolDeclaration) });
  }
  activeContext = { messages, tools };
  return { context: activeContext };
}

function prepareTools(response) {
  rawArguments = new Map((response.tool_calls ?? []).map((call) => [call.id, call.arguments]));
  parallelBatch = (response.tool_calls?.length ?? 0) > 1 && response.tool_calls.every((call) => parallelTools.has(call.name));
  // Missing tools remain manual handoffs for both live and recovered batches.
  for (const call of response.tool_calls ?? []) {
    if (!activeContext.tools.some((tool) => tool.name === call.name)) {
      activeContext.tools.push(executable({ function: { name: call.name,
        description: "Trace manual tool handoff", parameters: { type: "object" } } }, true));
    }
  }
}

async function streamFn() {
  const stream = createAssistantMessageEventStream();
  const timestamp = Date.now();
  let message;
  try {
    const completed = await rpc("model");
    const response = completed.response ?? completed;
    if (Object.hasOwn(completed, "tools")) {
      parallelTools = new Set(completed.parallel_tools ?? []);
      activeContext.tools = (completed.tools ?? []).map((definition) => executable(definition));
    }
    prepareTools(response);
    message = assistant(response, timestamp);
  } catch (error) {
    message = assistant({ error: error.message }, timestamp);
    if (error instanceof Halt) message.stopReason = "aborted";
  }
  stream.push({ type: "start", partial: { ...message, content: [], stopReason: "pending" } });
  if (message.stopReason === "error" || message.stopReason === "aborted") {
    stream.push({ type: "error", reason: message.stopReason, error: message });
  } else {
    stream.push({ type: "done", reason: message.stopReason, message });
  }
  stream.end(message);
  return stream;
}

async function emit(event) {
  // Pi catches errors thrown by execute/hooks and converts them to tool results.
  // An awaited event sink is outside those catches: stop here before a synthetic
  // result reaches Go after a halt, cancellation, or failed store transaction.
  assertRunning();
  if (event.type === "message_end" && event.message.role === "toolResult") {
    const message = event.message;
    await rpc("tool_result", { call_id: message.toolCallId, name: message.toolName,
      content: message.content.map((part) => part.type === "text" ? part.text : "").join("\n"),
      is_error: message.isError });
  }
}

let failure;
try {
  await runAgentLoop([], { messages: [], tools: [] }, {
    model, convertToLlm: (messages) => messages, toolExecution: "parallel",
    prepareRequest, finishTurn: () => rpc("finish"),
  }, emit, controller.signal, streamFn);
} catch (error) {
  const cause = stopped ?? error;
  if (!(cause instanceof Halt)) failure = cause.message ?? String(cause);
} finally {
  settled = true;
  input.close();
  process.stdin.pause();
}
if (failure) {
  process.stderr.write(`Trace pi host: ${failure}\n`);
  process.exitCode = 1;
}
await write({ method: "done", ...(failure ? { error: failure } : {}) });
