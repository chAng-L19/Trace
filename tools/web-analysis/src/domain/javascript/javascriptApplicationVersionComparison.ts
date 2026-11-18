import { digestCanonicalValue } from "../canonicalDigest.js";
import { uniqueSorted } from "../canonicalOrdering.js";
import type { Evidence } from "../evidence.js";
import { buildJavaScriptApplicationChangeGraph } from "./javascriptApplicationChangeGraph.js";
import {
  ownedApplicationVersionComparisonResultSchema,
  type ApplicationVersionComparisonResult,
} from "./javascriptApplicationVersionComparisonSchemas.js";
import type { JavaScriptApplicationGraph } from "./javascriptApplicationGraph.js";
import { classifyJavaScriptApplicationVersions } from "./javascriptApplicationVersionItems.js";
import { matchJavaScriptApplicationVersions } from "./javascriptApplicationVersionKeys.js";

/** Pure inputs after application-layer Evidence authentication. */
export interface ApplicationVersionComparisonProjectionInput {
  readonly left: {
    readonly evidenceId: string;
    readonly rootArtifactSha256: string;
    readonly graph: JavaScriptApplicationGraph;
  };
  readonly right: {
    readonly evidenceId: string;
    readonly rootArtifactSha256: string;
    readonly graph: JavaScriptApplicationGraph;
  };
  readonly leftNativeEvidence: readonly Evidence[];
  readonly rightNativeEvidence: readonly Evidence[];
}

/** Compare application entities with ordered unique-only identity tiers. */
export const compareJavaScriptApplicationVersions = (
  input: ApplicationVersionComparisonProjectionInput,
): ApplicationVersionComparisonResult => {
  const matching = matchJavaScriptApplicationVersions(
    input.left.graph.nodes,
    input.right.graph.nodes,
  );
  const projection = classifyJavaScriptApplicationVersions(matching, {
    leftGraph: input.left.graph,
    rightGraph: input.right.graph,
    leftEvidenceId: input.left.evidenceId,
    rightEvidenceId: input.right.evidenceId,
    leftNativeEvidence: input.leftNativeEvidence,
    rightNativeEvidence: input.rightNativeEvidence,
  });
  const changeGraph = buildJavaScriptApplicationChangeGraph({
    left: input.left.graph,
    right: input.right.graph,
    leftEvidenceId: input.left.evidenceId,
    rightEvidenceId: input.right.evidenceId,
    items: projection.items,
  });
  const evidenceLinks = uniqueSorted(
    projection.items.flatMap(({ evidence_links: links }) => links),
  );
  const omissions = {
    left_graph_omitted_count: input.left.graph.coverage.omitted_count,
    right_graph_omitted_count: input.right.graph.coverage.omitted_count,
  };
  const coverage = comparisonCoverage(input, omissions);
  const counts = summarizeItems(projection.items);
  const semantic = {
    left: {
      evidence_id: input.left.evidenceId,
      graph_id: input.left.graph.graph_id,
      root_artifact_sha256: input.left.rootArtifactSha256,
    },
    right: {
      evidence_id: input.right.evidenceId,
      graph_id: input.right.graph.graph_id,
      root_artifact_sha256: input.right.rootArtifactSha256,
    },
    summary: counts.summary,
    matching: counts.matching,
    items: projection.items,
    graph: changeGraph.graph,
    coverage,
    evidence_links: evidenceLinks,
    limitations: comparisonLimitations(input),
  };
  return ownedApplicationVersionComparisonResultSchema.parse({
    ...semantic,
    comparison_id: `javc_${digestCanonicalValue(semantic, "Application version comparison")}`,
  });
};

const summarizeItems = (
  items: readonly ApplicationVersionComparisonResult["items"][number][],
): Pick<ApplicationVersionComparisonResult, "summary" | "matching"> => {
  const summary = { unchanged: 0, added: 0, removed: 0, changed: 0, unknown: 0 };
  const basisCounts = {
    "exact-node-identity": 0,
    "exact-content-digest": 0,
    "exact-module-source-digest": 0,
    "source-map-identity": 0,
    "structural-fingerprint": 0,
    "semantic-key": 0,
  };
  let ambiguous = 0;
  let unmatched = 0;
  for (const item of items) {
    summary[item.status] += 1;
    if (item.match.basis !== "none") basisCounts[item.match.basis] += 1;
    if (item.match.status === "ambiguous") ambiguous += 1;
    if (item.match.status === "unmatched") unmatched += 1;
  }
  return {
    summary,
    matching: {
      exact_node_identity: basisCounts["exact-node-identity"],
      exact_content_digest: basisCounts["exact-content-digest"],
      exact_module_source_digest: basisCounts["exact-module-source-digest"],
      source_map_identity: basisCounts["source-map-identity"],
      structural_fingerprint: basisCounts["structural-fingerprint"],
      semantic_key: basisCounts["semantic-key"],
      ambiguous,
      unmatched,
    },
  };
};

type ComparisonOmissions = ApplicationVersionComparisonResult["coverage"];
type ComparisonOmissionCounts = Pick<
  ComparisonOmissions,
  "left_graph_omitted_count" | "right_graph_omitted_count"
>;

const comparisonCoverage = (
  input: ApplicationVersionComparisonProjectionInput,
  omissions: ComparisonOmissionCounts,
): ApplicationVersionComparisonResult["coverage"] => {
  const context = {
    left_graph_status: input.left.graph.coverage.status,
    right_graph_status: input.right.graph.coverage.status,
  };
  const sourceGraphsComplete =
    input.left.graph.coverage.status === "complete" &&
    input.right.graph.coverage.status === "complete";
  const sourceGraphsTruncated =
    input.left.graph.coverage.truncated || input.right.graph.coverage.truncated;
  return {
    ...context,
    ...omissions,
    status: sourceGraphsComplete
      ? "complete-within-inputs"
      : sourceGraphsTruncated
        ? "truncated"
        : "partial",
  };
};

const comparisonLimitations = (
  input: ApplicationVersionComparisonProjectionInput,
): string[] =>
  uniqueSorted([
    "Application comparison never uses bundler module ordinals as persistent identity and performs no fuzzy pairing.",
    "Exact digest equality proves byte identity only; source-map, structural-fingerprint, and semantic-key matches retain inferred confidence.",
    "Minified names and chunk locations are not assumed stable across versions.",
    "The comparison reads retained graphs and Evidence only; it does not execute either application.",
    "Native Evidence is linked only by exact subject digest; provider snapshot reuse remains target/provider/profile exact.",
    ...(input.left.graph.coverage.status === "complete" &&
    input.right.graph.coverage.status === "complete"
      ? []
      : [
          "At least one source graph is incomplete; unmatched entities on the opposite side remain unknown rather than added or removed.",
        ]),
  ]);
