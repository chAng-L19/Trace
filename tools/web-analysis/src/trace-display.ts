import { spawn } from "node:child_process";
import { createInterface } from "node:readline";
import { withClosedResource } from "./trace-resources.js";

/** Keep Node as worker owner so SIGTERM reaches the operation's AbortController. */
export async function withBrowserDisplay<T>(
  operation: string, input: any, signal: AbortSignal, run: () => Promise<T>,
): Promise<T> {
  const launches = operation === "trace_web_module_imports" ||
    operation === "capture_electron_scenario" ||
    (operation === "capture_browser_scenario" && input?.browser?.mode === "launch");
  if (process.platform !== "linux" || process.env.DISPLAY || !launches) return run();
  signal.throwIfAborted();
  const previous = { DISPLAY: process.env.DISPLAY, XAUTHORITY: process.env.XAUTHORITY };
  const holder = spawn("xvfb-run", ["-a", process.execPath, "-e",
    'console.log(JSON.stringify({DISPLAY:process.env.DISPLAY,XAUTHORITY:process.env.XAUTHORITY})); process.stdin.resume();',
  ], { stdio: ["pipe", "pipe", "pipe"] });
  holder.stderr.pipe(process.stderr, { end: false });
  // Startup failure (e.g. missing Xvfb/xauth) is retained alongside its exit status.
  let startupError: Error | undefined;
  holder.on("error", error => { startupError = error; });
  holder.stdin.on("error", () => {}); // early child exit is reported by ready/close
  const completed = new Promise<{ code: number | null; signal: string | null }>(resolve =>
    holder.once("close", (code, signal) => resolve({ code, signal })));
  const lines = createInterface({ input: holder.stdout });
  const ready = new Promise<string>((resolve, reject) => {
    lines.once("line", resolve);
    holder.once("error", reject);
    holder.once("close", () => reject(new Error("Xvfb display holder exited before readiness")));
  });
  const cancelStartup = () => holder.stdin.end();
  signal.addEventListener("abort", cancelStartup, { once: true });
  return withClosedResource({ close: async () => {
    signal.removeEventListener("abort", cancelStartup);
    lines.close();
    // Operation cleanup closes the browser first; EOF then releases Xvfb.
    holder.stdin.end();
    const result = await completed;
    for (const name of ["DISPLAY", "XAUTHORITY"] as const) {
      if (previous[name] === undefined) delete process.env[name];
      else process.env[name] = previous[name];
    }
    if (startupError) throw startupError;
    if (result.code !== 0)
      throw new Error(`Xvfb display holder cleanup failed: exit=${result.code}, signal=${result.signal}`);
  } }, async () => {
    const display = JSON.parse(await ready) as { DISPLAY?: string; XAUTHORITY?: string };
    signal.removeEventListener("abort", cancelStartup);
    signal.throwIfAborted();
    if (!display.DISPLAY) throw new Error("Xvfb display holder returned no DISPLAY");
    process.env.DISPLAY = display.DISPLAY;
    if (display.XAUTHORITY) process.env.XAUTHORITY = display.XAUTHORITY;
    return run();
  });
}
