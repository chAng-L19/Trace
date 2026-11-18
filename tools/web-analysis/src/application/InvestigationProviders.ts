/** Provider identity for verified publication of retained website scripts. */
export const WEB_SCRIPT_EXPORT_PROVIDER = {
  id: "rea-web-script-export",
  name: "REA captured script export",
  version: "1",
} as const;

/** Provider identity for deterministic static JavaScript application analysis. */
export const JAVASCRIPT_APPLICATION_PROVIDER = {
  id: "rea-javascript-application",
  name: "REA JavaScript application analyzer",
  version: "1",
} as const;

/** Provider identity for static/passive-runtime JavaScript reconciliation. */
export const JAVASCRIPT_RUNTIME_RECONCILIATION_PROVIDER = {
  id: "rea-javascript-runtime-reconciliation",
  name: "REA JavaScript runtime reconciliation",
  version: "1",
} as const;

/** Provider identity for graph-native feature tracing and version comparison. */
export const JAVASCRIPT_APPLICATION_WORKFLOW_PROVIDER = {
  id: "rea-javascript-application-workflows",
  name: "REA JavaScript application workflows",
  version: "1",
} as const;
