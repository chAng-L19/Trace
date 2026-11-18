import { defineConfig } from "vitest/config";
export default defineConfig({ test: { include: ["src/**/*.test.ts"], maxWorkers: 2, testTimeout: 30000 } });
