import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

import { execute } from "./trace-operations.js";
import { withClosedResource } from "./trace-resources.js";
import { ArtifactResourceScope } from "./artifacts/ArtifactResourceScope.js";
import { analyzeJavaScriptApplication } from "./application/javascript/JavaScriptApplicationService.js";
import { exportWebScripts } from "./application/WebScriptExportService.js";
import {
  compareApplicationVersionsEvidence,
  compareJavaScriptExportShapesEvidence,
  traceApplicationFeatureEvidence,
} from "./application/javascript/JavaScriptApplicationWorkflowService.js";
import { reconcileJavaScriptRuntimeEvidence } from "./application/javascript/JavaScriptRuntimeReconciliationService.js";
import {
  JAVASCRIPT_APPLICATION_EVIDENCE_EXAMPLE as application,
  JAVASCRIPT_RUNTIME_RECONCILIATION_EXAMPLE as runtime,
} from "./contracts/javascript/javascriptRuntimeReconciliationExample.fixture.js";
import { createEvidence, type Evidence } from "./domain/evidence.js";
import { javascriptApplicationAnalysisResultSchema } from "./domain/javascript/javascriptApplicationAnalysis.js";
import { scriptScenarioFixture } from "../tests/fixtures/webScriptCapture.js";

const result = javascriptApplicationAnalysisResultSchema.parse(application.normalized_result);
const reissue = (
  provider = application.provider,
  sha256 = result.root_artifact_sha256,
  normalizedResult = result,
  parameters = application.parameters,
) => createEvidence(
  { path: result.input_path, sha256, format: result.format },
  provider,
  {
    predicateType: application.predicate_type,
    operation: application.operation,
    parameters,
    result: normalizedResult,
    confidence: application.confidence,
    authority: application.authority,
  },
);
const right = reissue(undefined, undefined, undefined, { input_path: result.input_path });
const cases = (left: Evidence = application) => [
  {
    operation: "trace_application_feature",
    input: { application: left, seed: { kind: "node-id", value: result.graph.root_node_ids[0] } },
    previous: traceApplicationFeatureEvidence,
  },
  {
    operation: "compare_application_versions",
    input: { left, right },
    previous: compareApplicationVersionsEvidence,
  },
  {
    operation: "compare_javascript_export_shapes",
    input: { left, right, left_module_path: "renderer.js", left_export_name: "example", right_module_path: "renderer.js", right_export_name: "example" },
    previous: compareJavaScriptExportShapesEvidence,
  },
  {
    operation: "reconcile_javascript_runtime",
    input: {
      ...runtime,
      static_layers: [{ analysis: left, runtime_mappings: [
        { kind: "url-prefix", prefix: "https://fixture.test" },
        { kind: "url-prefix", prefix: "https://fixture.test/" },
      ] }],
    },
    previous: reconcileJavaScriptRuntimeEvidence,
  },
];

describe("registered validated operation boundaries", () => {
  for (const { operation, input, previous } of cases()) {
    it(`${operation} preserves the complete parsed-service result`, async () => {
      const expected = previous(input);
      expect(expected.ok).toBe(true);
      expect(await execute(operation, input)).toEqual(expected);
    });

    it(`${operation} rejects invalid adapter and direct-service input`, async () => {
      await expect(execute(operation, {})).rejects.toThrow();
      expect(previous({}).ok).toBe(false);
    });
  }

  it("retains evidence digest, provider, subject and graph authentication", async () => {
    const staleGraph = structuredClone(result);
    staleGraph.graph.graph_id = `jag_${"9".repeat(64)}`;
    const invalidSources = [
      { ...application, evidence_id: `ev_${"9".repeat(64)}` },
      reissue({ ...application.provider, id: "other-provider" }),
      reissue(undefined, "9".repeat(64)),
      reissue(undefined, undefined, staleGraph),
    ];
    for (const source of invalidSources) {
      for (const { operation, input, previous } of cases(source)) {
        expect(previous(input).ok, operation).toBe(false);
        // A malformed envelope may fail in schema.parse; authenticated but
        // incompatible evidence must still fail in the application layer.
        const rejected = await execute(operation, input).then(
          value => value.ok === false,
          () => true,
        );
        expect(rejected, operation).toBe(true);
      }
    }
  });

  it("rejects duplicate native identities and identical version identities", async () => {
    const feature = cases()[0]!;
    await expect(execute(feature.operation, {
      ...feature.input, native_observations: [application, application],
    })).rejects.toThrow("Native observations must be unique");
    await expect(execute("compare_application_versions", {
      left: application, right,
      left_native_observations: [application, application],
    })).rejects.toThrow("Native observations must be unique");
    for (const testCase of cases().slice(1, 3)) {
      await expect(execute(testCase.operation, {
        ...testCase.input, right: application,
      })).rejects.toThrow("must be distinct");
    }
  });

  it("preserves worker analysis and script publication results with owned cleanup", async () => {
    const directory = await mkdtemp(join(tmpdir(), "trace-validated-"));
    try {
      await writeFile(join(directory, "main.js"), "export const value = 7;\n");
      const analysisInput = { input_path: directory };
      const scope = new ArtifactResourceScope();
      const expectedAnalysis = await withClosedResource(scope, () =>
        analyzeJavaScriptApplication(analysisInput, scope));
      expect(expectedAnalysis.ok).toBe(true);
      expect(await execute("analyze_javascript_application", analysisInput)).toEqual(expectedAnalysis);

      const capture = join(directory, "capture.json");
      const output = join(directory, "scripts");
      await writeFile(capture, JSON.stringify(scriptScenarioFixture()));
      const exportInput = { capture_path: capture, output_directory: output };
      const exportScope = new ArtifactResourceScope();
      const expectedExport = await withClosedResource(exportScope, () =>
        exportWebScripts(exportInput, exportScope));
      expect(expectedExport.ok).toBe(true);
      const manifest = await readFile(join(output, "manifest.json"), "utf8");
      await rm(output, { recursive: true });
      expect(await execute("export_web_scripts", exportInput)).toEqual(expectedExport);
      expect(await readFile(join(output, "manifest.json"), "utf8")).toBe(manifest);
    } finally {
      await rm(directory, { recursive: true, force: true });
    }
  }, 120000);

  for (const [operation, previous] of [
    ["analyze_javascript_application", analyzeJavaScriptApplication],
    ["export_web_scripts", exportWebScripts],
  ] as const) {
    it(`${operation} retains unknown-input validation and pre-aborted rejection`, async () => {
      await expect(execute(operation, {})).rejects.toThrow();
      const scope = new ArtifactResourceScope();
      const invalid = await withClosedResource(scope, () => previous({}, scope));
      expect(invalid.ok).toBe(false);
      const abort = new Error("fixture abort");
      await expect(execute(operation, {}, AbortSignal.abort(abort))).rejects.toBe(abort);
    });
  }
});
