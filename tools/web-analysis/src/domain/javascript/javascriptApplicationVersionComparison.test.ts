import { describe, expect, it } from "vitest";

import { createEvidence } from "../evidence.js";
import {
  createJavaScriptApplicationEdge,
  createJavaScriptApplicationGraph,
  createJavaScriptApplicationNode,
  isValidatedImmutableJavaScriptApplicationGraph,
  parseJavaScriptApplicationGraph,
  type JavaScriptApplicationGraph,
} from "./javascriptApplicationGraph.js";
import {
  artifactEvidence,
  completeCoverage,
} from "./javascriptApplicationGraph.fixture.js";
import { compareJavaScriptApplicationVersions } from "./javascriptApplicationVersionComparison.js";
import type { ApplicationNode } from "./javascriptApplicationGraphSchemas.js";
import { resolveJavaScriptSourceMapReference } from "./javascriptSourceMapPaths.js";
import {
  applicationVersionComparisonResultSchema,
  ownedApplicationVersionComparisonResultSchema,
} from "./javascriptApplicationVersionComparisonSchemas.js";

const LEFT_SHA = "a".repeat(64);
const RIGHT_SHA = "b".repeat(64);

const buildGraph = (
  artifactSha: string,
  version: number,
): JavaScriptApplicationGraph => {
  const nodes = ["a", "b", "c"].map((name) =>
    createJavaScriptApplicationNode({
      kind: "javascript-module",
      identity: {
        strategy: "canonical-path",
        stability: "artifact-version",
        artifact_sha256: artifactSha,
        path: `${name}.js`,
      },
      observations: [
        {
          label: `${name}.js`,
          properties: {
            structural_fingerprint_sha256: name
              .charCodeAt(0)
              .toString(16)
              .padStart(64, "0"),
            structural_fingerprint_algorithm: "fixture",
            source_sha256: (name === "b" ? version : 0)
              .toString(16)
              .padStart(64, "0"),
          },
          evidence: artifactEvidence(artifactSha, `${name}.js`),
        },
      ],
    }),
  );
  const [a, b, c] = nodes;
  if (a === undefined || b === undefined || c === undefined)
    throw new Error("Missing fixture node");
  return createJavaScriptApplicationGraph({
    schema: "JavaScriptApplicationGraph",
    root_node_ids: [a.node_id],
    nodes,
    edges: [
      createJavaScriptApplicationEdge({
        source_node_id: a.node_id,
        target_node_id: b.node_id,
        relation: "imports",
        properties: {},
        evidence: artifactEvidence(artifactSha, "a.js"),
      }),
      createJavaScriptApplicationEdge({
        source_node_id: b.node_id,
        target_node_id: c.node_id,
        relation: "imports",
        properties: {},
        evidence: artifactEvidence(artifactSha, "b.js"),
      }),
    ],
    coverage: completeCoverage,
    limitations: [],
  });
};

const compare = () =>
  compareJavaScriptApplicationVersions({
    left: {
      evidenceId: `ev_${"1".repeat(64)}`,
      rootArtifactSha256: LEFT_SHA,
      graph: buildGraph(LEFT_SHA, 1),
    },
    right: {
      evidenceId: `ev_${"2".repeat(64)}`,
      rootArtifactSha256: RIGHT_SHA,
      graph: buildGraph(RIGHT_SHA, 2),
    },
    leftNativeEvidence: [],
    rightNativeEvidence: [],
  });

const unrelatedGraph = (artifactSha: string): JavaScriptApplicationGraph => {
  const node = createJavaScriptApplicationNode({
    kind: "javascript-chunk",
    identity: {
      strategy: "canonical-path",
      stability: "artifact-version",
      artifact_sha256: artifactSha,
      path: "unrelated.js",
    },
    observations: [
      {
        label: "unrelated.js",
        properties: {},
        evidence: artifactEvidence(artifactSha, "unrelated.js"),
      },
    ],
  });
  return createJavaScriptApplicationGraph({
    schema: "JavaScriptApplicationGraph",
    root_node_ids: [node.node_id],
    nodes: [node],
    edges: [],
    coverage: completeCoverage,
    limitations: [],
  });
};

const nativeEvidence = (sha256: string | undefined, ordinal: number) =>
  createEvidence(
    sha256 === undefined
      ? undefined
      : {
          path: `/fixture/native-${ordinal}.js`,
          sha256,
          format: "javascript",
        },
    { id: "fixture", name: "Fixture", version: "1" },
    {
      operation: "analyze_javascript_module",
      parameters: { ordinal },
      confidence: "observed",
      authority: "shipped-artifact",
      result: { ordinal },
    },
  );

describe("application version comparison result", () => {
  it("counts all status and matching tiers without conflating none with a matched basis", () => {
    const graphForCounts = (sha: string) => {
      const node = (
        kind: ApplicationNode["kind"],
        path: string,
        properties: ApplicationNode["observations"][number]["properties"] = {},
        identity: ApplicationNode["identity"] = {
          strategy: "canonical-path", stability: "artifact-version", artifact_sha256: sha, path,
        },
        evidenceDigest = sha,
      ) => createJavaScriptApplicationNode({
        kind, identity,
        observations: [{ label: path, properties, evidence: artifactEvidence(evidenceDigest, path) }],
      });
      const nodes = [
        node("javascript-chunk", "identity.js", {}, {
          strategy: "canonical-path", stability: "artifact-version", artifact_sha256: LEFT_SHA, path: "identity.js",
        }, LEFT_SHA),
        node("javascript-asset", "content.js", {}, {
          strategy: "content-digest", stability: "global-exact", sha256: "c".repeat(64),
        }, "c".repeat(64)),
        node("javascript-module", "source.js", { source_sha256: "d".repeat(64) }),
        createJavaScriptApplicationNode({
          kind: "source-module",
          identity: {
            strategy: "source-map-original", stability: "source-map-exact", source_map_sha256: sha,
            original_source: "mapped.js", source_root: null, source_sha256: sha,
          },
          observations: [{
            label: "mapped.js",
            source_map_reference: resolveJavaScriptSourceMapReference("mapped.js", null, "bundle.js.map"),
            properties: { content_sha256: sha },
            evidence: artifactEvidence(sha, "bundle.js.map", "ast-static-analysis"),
          }],
        }),
        node("javascript-module", "structural.js", {
          structural_fingerprint_sha256: "e".repeat(64), structural_fingerprint_algorithm: "fixture", revision: sha,
        }),
        node("endpoint", "endpoint.js", { endpoint_kind: "http", value: "/unique" }),
        node("endpoint", "ambiguous-1.js", { endpoint_kind: "http", value: "/ambiguous" }),
        node("endpoint", "ambiguous-2.js", { endpoint_kind: "http", value: "/ambiguous" }),
        node("javascript-chunk", `${sha}.js`),
      ];
      return createJavaScriptApplicationGraph({
        schema: "JavaScriptApplicationGraph", root_node_ids: [nodes[0]!.node_id],
        nodes, edges: [], coverage: completeCoverage, limitations: [],
      });
    };
    const result = compareJavaScriptApplicationVersions({
      left: { evidenceId: `ev_${"1".repeat(64)}`, rootArtifactSha256: LEFT_SHA, graph: graphForCounts(LEFT_SHA) },
      right: { evidenceId: `ev_${"2".repeat(64)}`, rootArtifactSha256: RIGHT_SHA, graph: graphForCounts(RIGHT_SHA) },
      leftNativeEvidence: [], rightNativeEvidence: [],
    });
    expect(result.matching).toEqual({
      exact_node_identity: 1, exact_content_digest: 1, exact_module_source_digest: 1,
      source_map_identity: 1, structural_fingerprint: 1, semantic_key: 1,
      ambiguous: 4, unmatched: 2,
    });
    for (const status of ["unchanged", "added", "removed", "changed", "unknown"] as const) {
      const expected = result.items.filter(item => item.status === status).length;
      expect(expected, status).toBeGreaterThan(0);
      expect(result.summary[status], status).toBe(expected);
    }
    expect(result.summary.unknown).toBe(4);
    expect(result.summary.added).toBe(1);
    expect(result.summary.removed).toBe(1);
  });

  it("links every exact digest observation only to its paired or unmatched item", () => {
    const left = buildGraph(LEFT_SHA, 1);
    const right = buildGraph(RIGHT_SHA, 2);
    const leftEvidenceId = `ev_${"1".repeat(64)}`;
    const rightEvidenceId = `ev_${"2".repeat(64)}`;
    const leftBDigest = `${"0".repeat(63)}1`;
    const leftNative = [
      nativeEvidence(leftBDigest, 1),
      nativeEvidence(leftBDigest, 2),
      nativeEvidence("9".repeat(64), 3),
    ];
    const rightNative = [
      nativeEvidence("0".repeat(64), 4),
      nativeEvidence(undefined, 6),
    ];
    const unrelatedRight = unrelatedGraph(RIGHT_SHA);
    const unmatched = compareJavaScriptApplicationVersions({
      left: {
        evidenceId: leftEvidenceId,
        rootArtifactSha256: LEFT_SHA,
        graph: left,
      },
      right: {
        evidenceId: rightEvidenceId,
        rootArtifactSha256: RIGHT_SHA,
        graph: unrelatedRight,
      },
      leftNativeEvidence: leftNative,
      rightNativeEvidence: rightNative,
    });
    const leftB = left.nodes.find(({ observations }) =>
      observations.some(({ label }) => label === "b.js"),
    );
    if (leftB === undefined) throw new Error("Missing left b.js node");
    const unmatchedB = unmatched.items.find(
      ({ left_node_id: id }) => id === leftB.node_id,
    );
    expect(unmatchedB?.evidence_links).toEqual(
      [
        leftEvidenceId,
        rightEvidenceId,
        leftNative[0]!.evidence_id,
        leftNative[1]!.evidence_id,
      ].sort(),
    );
    const unrelatedItem = unmatched.items.find(
      ({ right_node_id: id }) => id === unrelatedRight.nodes[0]?.node_id,
    );
    expect(unrelatedItem?.evidence_links).toEqual(
      [leftEvidenceId, rightEvidenceId].sort(),
    );

    const paired = compareJavaScriptApplicationVersions({
      left: {
        evidenceId: leftEvidenceId,
        rootArtifactSha256: LEFT_SHA,
        graph: left,
      },
      right: {
        evidenceId: rightEvidenceId,
        rootArtifactSha256: RIGHT_SHA,
        graph: right,
      },
      leftNativeEvidence: [nativeEvidence("0".repeat(64), 5)],
      rightNativeEvidence: rightNative,
    });
    const leftA = left.nodes.find(({ observations }) =>
      observations.some(({ label }) => label === "a.js"),
    );
    if (leftA === undefined) throw new Error("Missing left a.js node");
    const pairedA = paired.items.find(
      ({ left_node_id: id }) => id === leftA.node_id,
    );
    expect(pairedA?.match.status).toBe("matched");
    expect(pairedA?.evidence_links).toEqual(
      [
        leftEvidenceId,
        rightEvidenceId,
        nativeEvidence("0".repeat(64), 5).evidence_id,
        rightNative[0]!.evidence_id,
      ].sort(),
    );
  });

  it("returns a sealed change graph that passes complete validation", () => {
    const result = compare();
    expect(
      result.graph.edges.some(({ relation }) => relation === "changed_from"),
    ).toBe(true);
    expect(isValidatedImmutableJavaScriptApplicationGraph(result.graph)).toBe(
      true,
    );
    expect(Object.isFrozen(result.graph.edges)).toBe(true);
    expect(parseJavaScriptApplicationGraph(result.graph)).toEqual(result.graph);
    expect(applicationVersionComparisonResultSchema.parse(result)).toEqual(
      result,
    );
  });

  it("does not reuse a change graph without an owned proof", () => {
    const result = compare();
    const copy = structuredClone(result.graph);
    expect(isValidatedImmutableJavaScriptApplicationGraph(copy)).toBe(false);
    expect(() =>
      ownedApplicationVersionComparisonResultSchema.parse({
        ...result,
        graph: copy,
      }),
    ).toThrow();
    expect(
      applicationVersionComparisonResultSchema.parse({ ...result, graph: copy })
        .graph,
    ).toEqual(result.graph);
  });

  it("keeps coverage validation for owned results", () => {
    const result = compare();
    expect(() =>
      ownedApplicationVersionComparisonResultSchema.parse({
        ...result,
        coverage: { ...result.coverage, status: "partial" },
      }),
    ).toThrow("Comparison coverage must match source graph completeness");
  });
});
