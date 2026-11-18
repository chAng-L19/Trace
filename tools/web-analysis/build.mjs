import { readdir } from "node:fs/promises";
import { build } from "esbuild";

// Transpile the selected runtime closure without bundling; provider subprocess
// URLs retain the upstream directory layout. No REA runtime is installed.
const entries = (await readdir("src", { recursive: true }))
  .filter(name => name.endsWith(".ts") && !name.endsWith(".test.ts") && !name.endsWith(".fixture.ts"))
  .map(name => `src/${name}`);
await build({ entryPoints: entries, outdir: "dist", outbase: "src", platform: "node", format: "esm", target: "node22", sourcemap: true });
