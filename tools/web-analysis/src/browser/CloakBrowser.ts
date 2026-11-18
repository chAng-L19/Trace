import { access } from "node:fs/promises";
import { constants } from "node:fs";
import { isAbsolute } from "node:path";

/** Deployment verifies the official binary; operations never download or select another engine. */
export async function cloakBrowserExecutable(
  environment: Readonly<Record<string, string | undefined>> = process.env,
): Promise<string> {
  const executable = environment.CLOAKBROWSER_BINARY_PATH;
  if (!executable || !isAbsolute(executable))
    throw new Error(
      "Set CLOAKBROWSER_BINARY_PATH to the absolute path of the deployment-verified CloakBrowser binary; runtime download and ordinary Chromium fallback are disabled.",
    );
  await access(executable, constants.X_OK);
  return executable;
}
