import { createInterface } from "node:readline";
import { readFile, writeFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import { z } from "zod";
import { catalog, execute } from "./trace-operations.js";
import { withBrowserDisplay } from "./trace-display.js";

const requestSchema = z.strictObject({
  operation: z.string().min(1),
  input: z.unknown().optional(),
  input_file: z.string().min(1).optional(),
  output_file: z.string().min(1).optional(),
}).refine(v => !(v.input !== undefined && v.input_file !== undefined), "Choose input or input_file");

function errorRecord(cause: unknown): unknown {
  if (!(cause instanceof Error)) return { message: String(cause) };
  return { ...cause, name: cause.name, message: cause.message };
}
const stringify = (value: unknown) => JSON.stringify(value, (_key, item) => item instanceof Error ? errorRecord(item) : item);
const operationIndex = process.argv.indexOf("--operation");
async function* inputs(signal: AbortSignal) {
  const reader = createInterface({ input: process.stdin, crlfDelay: Infinity, signal });
  try {
    if (operationIndex >= 0) {
      // Trace passes one JSON object and closes stdin; it may be pretty-printed.
      let input = "";
      for await (const line of reader) input += line + "\n";
      if (!signal.aborted) yield input;
    } else {
      yield* reader;
    }
  } finally {
    reader.close();
    process.stdin.pause();
  }
}

if (process.argv.includes("--catalog")) {
  process.stdout.write(JSON.stringify({ name: "trace-web-analysis", version: "1.0.0", operations: catalog() }) + "\n");
} else {
  const abort = new AbortController();
  process.once("SIGTERM", () => { process.exitCode = 143; abort.abort(); });
  process.once("SIGINT", () => { process.exitCode = 130; abort.abort(); });
  for await (const line of inputs(abort.signal)) {
    if (abort.signal.aborted) break;
    if (!line.trim()) continue;
    let response: any;
    try {
      const request = requestSchema.parse(operationIndex >= 0
        ? { operation: process.argv[operationIndex + 1], input: JSON.parse(line) }
        : JSON.parse(line));
      const input = request.input_file ? JSON.parse(await readFile(request.input_file, "utf8")) : request.input ?? {};
      response = await withBrowserDisplay(request.operation, input, abort.signal,
        () => execute(request.operation, input, abort.signal));
      if (!response.ok) response = { ok: false, error: errorRecord(response.error) };
      if (request.output_file) {
        const data = stringify(response) + "\n";
        await writeFile(request.output_file, data, { flag: "wx" });
        response = { ok: response.ok, output_file: request.output_file, sha256: createHash("sha256").update(data).digest("hex"), bytes: Buffer.byteLength(data), evidence_id: response.value?.evidence_id };
      }
    } catch (cause) { response = { ok: false, error: errorRecord(cause), ...(response === undefined ? {} : { partialResult: response }) }; }
    process.stdout.write(stringify(response) + "\n");
    if (!response.ok && !abort.signal.aborted) process.exitCode = 1;
    if (abort.signal.aborted) break;
  }
}
