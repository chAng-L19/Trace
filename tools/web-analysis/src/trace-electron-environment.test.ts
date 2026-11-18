import { expect, it } from "vitest";
import { dirname } from "node:path";
import { PlaywrightElectronActiveProvider } from "./browser/PlaywrightElectronActiveProvider.js";
import { electronActiveObservationInputSchema } from "./domain/javascript/electronActiveObservation.js";

it("preserves the X11 display and its authentication file at the Electron launch boundary", async () => {
  let launched: Record<string, string | undefined> | undefined;
  const provider = new PlaywrightElectronActiveProvider({
    DISPLAY: ":123",
    XAUTHORITY: "/tmp/selected-xvfb/Xauthority",
    PATH: "/usr/bin",
    UNRELATED_SECRET: "not-for-electron",
  }, async options => {
    launched = options?.env;
    throw new Error("launch boundary observed");
  });
  const result = await provider.capture({
    ...electronActiveObservationInputSchema.parse({
      executable_path: process.execPath,
      application_path: process.execPath,
    }),
    application_root: dirname(process.execPath),
  });
  expect(result.ok).toBe(false);
  expect(launched).toMatchObject({ DISPLAY: ":123", XAUTHORITY: "/tmp/selected-xvfb/Xauthority" });
  expect(launched).not.toHaveProperty("UNRELATED_SECRET");
});
