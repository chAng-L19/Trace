---
name: api-recon
description: Discover frontend routes, HTTP APIs, and parameters from static and runtime observations.
---

# API Recon

Use existing HTTP, browser, file, and worker tools to discover the task's API surface.
Keep the original target and constraints. Load only the tool families needed for the
current hypothesis; inspect their schemas before calling them.

1. Classify the entry page as SPA, MPA, or a mixture. Collect HTML script references
   and reachable lazy chunks. Record source URLs, hashes, errors, and collection limits.
2. Extract candidate paths, frontend routes, call sites, and parameter names offline.
   Large bundles belong in artifacts; keep bounded candidate summaries in context.
   Static strings alone do not establish HTTP methods, reachability, or authorization.
3. Observe actual outbound requests while rendering relevant routes and exercising
   task-relevant UI states. Preserve method, URL, parameter locations, request body,
   status, response references, trigger, and session context. Cover fetch/XHR, WebSocket,
   and SSE when present. Use multiple input/state samples before inferring required
   or optional parameters.
4. If a client login gate prevents route mounting, an isolated browser context may
   stub bootstrap/menu responses to expose frontend routes. Record all affected
   requests as mock-assisted. Do not treat mocked business success, rendered content,
   or client state as a real backend session or an authorization bypass.
5. Merge static candidates with observed requests and parameter samples. Record each
   endpoint's provenance as static, runtime, or mock-assisted, plus uncertainty and
   the tested input set. Retain unreachable candidates as leads for later verification.
6. Persist the inventory and genuine HTTP exchanges as run-bound artifacts. Use
   baseline/proof/control roles only when those exchanges support an actual test.
   Recon findings are observations, not confirmed vulnerabilities. Delegate a
   separate verification hypothesis when needed; Runtime verifies evidence and goals.

Stop when the requested surface is covered or a stated limit prevents progress.
Report unvisited routes, inaccessible states, sample limits, and missing response
data explicitly. Reuse existing captures before repeating network requests.
