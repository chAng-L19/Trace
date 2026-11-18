import { expect, test, vi } from "vitest";
import { access } from "node:fs/promises";
import { cloakBrowserExecutable } from "./browser/CloakBrowser.js";
import { openPlaywrightScenarioBrowser } from "./browser/PlaywrightScenarioBrowser.js";
import { NativeModuleResolver } from "./browser/modules/NativeModuleResolver.js";
import { browserScenarioSchema } from "./domain/browserScenario.js";
import { browserScenarioBrowserSchema } from "./domain/browserScenarioValues.js";

test("browser requests expose neither a headless switch nor a generic executable", () => {
  expect(browserScenarioBrowserSchema.parse({ mode: "launch" })).toEqual({ mode: "launch" });
  for (const override of [{ headless: true }, { headless: false }, { executable_path: process.execPath }])
    expect(browserScenarioBrowserSchema.safeParse({ mode: "launch", ...override }).success).toBe(false);
  expect(browserScenarioBrowserSchema.safeParse({
    mode: "connect", cdp_endpoint: "http://127.0.0.1:9222", target_id: "page",
  }).success).toBe(true);
});

test("runtime requires a deployment binary rather than downloading or falling back", async () => {
  await expect(cloakBrowserExecutable({})).rejects.toThrow("CLOAKBROWSER_BINARY_PATH");
  await expect(cloakBrowserExecutable({ CLOAKBROWSER_BINARY_PATH: "chrome" })).rejects.toThrow("absolute path");
  await expect(cloakBrowserExecutable({ CLOAKBROWSER_BINARY_PATH: process.execPath })).resolves.toBe(process.execPath);
});

test("scenario uses the headed SDK contract and cleans its owned profile", async () => {
  const close = vi.fn(async () => {});
  const context = { close, pages: () => [{}], browser: () => ({ version: () => "fixture" }) };
  const launchPersistentContext = vi.fn(async () => context);
  const scenario = browserScenarioSchema.parse({
    browser: { mode: "launch" }, start_url: { url: "http://127.0.0.1/" },
    actions: [{ step_id: "wait", action: "wait_for_timeout", duration_ms: 1 }],
  });
  const opened = await openPlaywrightScenarioBrowser(scenario, {
    CLOAKBROWSER_BINARY_PATH: process.execPath, DISPLAY: ":123", XAUTHORITY: "/tmp/xauth",
  }, { launcher: { launchPersistentContext, connectOverCDP: vi.fn() } as any });
  try {
    expect(launchPersistentContext).toHaveBeenCalledWith(expect.objectContaining({
      headless: false, userDataDir: opened.profilePath, locale: "en-US", timezone: "UTC",
      launchOptions: expect.objectContaining({
        executablePath: process.execPath, env: { DISPLAY: ":123", XAUTHORITY: "/tmp/xauth" },
      }),
    }));
  } finally { await opened.cleanup.close(); }
  expect(close).toHaveBeenCalledTimes(1);
  await expect(access(opened.profilePath!)).rejects.toThrow();
});

test("module resolver calls only headed CloakBrowser and preserves launch failure", async () => {
  const launcher = vi.fn(async () => { throw new Error("fixture launch failure"); });
  const resolver = new NativeModuleResolver({ CLOAKBROWSER_BINARY_PATH: process.execPath }, launcher);
  try {
    const result = await resolver.resolve({ importerUrl: "http://127.0.0.1/app.js", importMap: null, specifiers: ["./dep.js"] });
    expect(result.ok).toBe(false);
    expect(launcher).toHaveBeenCalledTimes(1);
    expect(launcher).toHaveBeenCalledWith(expect.objectContaining({
      headless: false, launchOptions: expect.objectContaining({ executablePath: process.execPath }),
    }));
  } finally { await resolver.close(); }
});
