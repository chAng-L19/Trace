import type { AnalysisProfileCommitment } from "../domain/analysisProfile.js";
import type { JsonValue } from "../domain/jsonValue.js";
import type { EvidenceLocation } from "../domain/evidence.js";
import type { EvidenceSubjectTarget } from "../domain/evidence.js";
import { jsonValueSchema } from "../domain/jsonValue.js";
import type { ProgressReporter } from "./ProgressReporter.js";
import {
  relationshipCoverageSchema,
  type RelationshipCoverage,
} from "../domain/relationshipCoverage.js";

export interface ExecutionOptions {
  readonly signal?: AbortSignal;
  readonly progress?: ProgressReporter;
  /**
   * Set when a procedure selector came from provider output, such as a
   * revisited callee, rather than from the caller. A provider-derived
   * selector is never the caller's input mistake. Absent means the caller
   * supplied it.
   */
  readonly selectorOrigin?: "provider";
}

/** Atomic successful provider observation with exact producing provenance. */
export interface AnalysisExecution {
  readonly result: JsonValue;
  readonly rawResult: JsonValue | null;
  readonly provider: ProviderIdentity;
  readonly analysisProfile?: AnalysisProfileCommitment;
  readonly limitations: readonly string[];
  /** Provider-owned completeness assessment for caller/callee observations. */
  readonly relationshipCoverage?: RelationshipCoverage;
  readonly locations: readonly EvidenceLocation[];
  readonly subject: EvidenceSubjectTarget | null;
}

/**
 * Build a validated atomic provider observation at an adapter boundary.
 *
 * Pass `rawResult` only for a provider representation distinct from
 * `result`. Omitting it records `null` rather than a second complete copy of
 * the normalized result in every Evidence record and tool response.
 */
export const createAnalysisExecution = (
  result: unknown,
  provider: ProviderIdentity,
  options: {
    readonly rawResult?: unknown;
    readonly analysisProfile?: AnalysisProfileCommitment;
    readonly limitations?: readonly string[];
    readonly relationshipCoverage?: RelationshipCoverage;
    readonly locations?: readonly EvidenceLocation[];
    readonly subject?: EvidenceSubjectTarget;
  } = {},
): AnalysisExecution => ({
  result: jsonValueSchema.parse(result),
  rawResult:
    options.rawResult === undefined
      ? null
      : jsonValueSchema.parse(options.rawResult),
  provider,
  ...(options.analysisProfile === undefined
    ? {}
    : { analysisProfile: structuredClone(options.analysisProfile) }),
  limitations: [...(options.limitations ?? [])],
  ...(options.relationshipCoverage === undefined
    ? {}
    : {
        relationshipCoverage: relationshipCoverageSchema.parse(
          options.relationshipCoverage,
        ),
      }),
  locations: [...(options.locations ?? [])],
  subject: options.subject ?? null,
});

export interface ProviderIdentity {
  readonly id: string;
  readonly name: string;
  readonly version: string | null;
}
