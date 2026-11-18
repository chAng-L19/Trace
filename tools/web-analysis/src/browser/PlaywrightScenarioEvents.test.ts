import { EventEmitter } from "node:events";
import { expect, test } from "vitest";
import { PlaywrightScenarioEvents } from "./PlaywrightScenarioEvents.js";
import { PlaywrightScenarioSession } from "./PlaywrightScenarioSession.js";
import { BrowserScenarioSecrets } from "./BrowserScenarioSecrets.js";
import { browserScenarioSchema } from "../domain/browserScenario.js";

const scenario = (mode: "launch" | "connect" = "launch") =>
  browserScenarioSchema.parse({
    browser: mode === "launch" ? { mode } : { mode, cdp_endpoint: "http://127.0.0.1:9222", target_id: "fixture" },
    start_url: { url: "http://127.0.0.1/" },
    capture: { events: ["console", "page-errors"] },
    actions: [{ step_id: "wait", action: "wait_for_timeout", duration_ms: 1 }],
  });

test.each([
  [true, ["console", "page-errors"], 2],
  [true, ["console"], 1],
  [true, ["page-errors"], 1],
  [true, [], 0],
  [true, ["frames"], 0],
  [false, ["console", "page-errors"], 0],
] as const)("fixed-binary limits follow selected families: %s %j", (suppressed, enabled, count) => {
  const page = new EventEmitter();
  const events = new PlaywrightScenarioEvents({
    page: page as any, enabled: new Set(enabled), suppressesConsoleEvents: suppressed,
    secrets: BrowserScenarioSecrets.resolve(scenario(), {})!,
  });
  expect(events.limitations()).toHaveLength(count);
  // Do not discard delivered evidence or install a replacement console.
  if (enabled.some((family) => family === "console")) {
    page.emit("console", { location: () => ({ url: "http://127.0.0.1/" }), type: () => "log", text: () => "observed" });
    expect(events.result().items[0]).toMatchObject({ kind: "console", text: "observed" });
  }
});

test.each(["launch", "connect"] as const)("session propagates fixed launch limits, not ownership guesses: %s", async (mode) => {
  const input = scenario(mode);
  const context = Object.assign(new EventEmitter(), {
    setDefaultTimeout() {}, setDefaultNavigationTimeout() {}, async addCookies() {},
  });
  const page = Object.assign(new EventEmitter(), {
    context: () => context, url: () => "http://127.0.0.1/",
    async evaluate() { return false; }, async addInitScript() {},
  });
  const opened = { context, page, browser: { version: () => "fixture" }, cleanup: { async close() {} } };
  const session = await (PlaywrightScenarioSession as any).initializeSession(
    opened, input, BrowserScenarioSecrets.resolve(input, {})!, {},
  );
  try {
    expect(session.eventLimitations().filter((item: string) => item.includes("Fixed CloakBrowser")))
      .toHaveLength(mode === "launch" ? 2 : 0);
  } finally { await session.close(); }
});
