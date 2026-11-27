## Worker role

- Workers execute assigned tasks and may subdelegate within shared limits. Parents retain orchestration and shared resource leases; never duplicate an active owner's probes or maintain a second queue/ownership ledger. The root writes no long-running retry helpers.
- Execute only the assigned task and preserve its acceptance and source-bound handoff. Workers own collection.

- On internal slot denial, a registered Worker may claim one local task in its current slot using its exact Worker ID, original checks and model route. External quotas/root restrictions still bind. New dispatch needs a slot; worker_wait without active children returns immediately.
- Only unfinished children checkpoint with worker_yield; finished children return final results. Checkpoints retain validated facts, artifact versions, external state, bounded negatives, local evidence IDs and next action, proving no completion/cleanup. Handoff never replaces dependency ordering.

## Execution

- For webpage interaction, use Lightpanda first for navigation/DOM/forms/JavaScript; CloakBrowser for unavailable/unsupported capability, screenshots, rendering or native zoom. Use frozen binaries; no discovery/call downloads/updates or standalone Chromium fallback. Never change site security controls to pass a call. Inspect effects after failure; never auto-replay across browsers.
- When browsers are unsuitable/unavailable, HTTP/API/downloads use curl_cffi via http-request/page-fetch, official curl-cffi CLI or Python curl_cffi.requests. Managed curl uses `curl get URL`, not legacy flags. No native curl/urllib/requests bypass; missing curl_cffi is a dependency gap. Keep TLS verification enabled; no auto-retry or browser-to-HTTP replay.
- Discovery matches every term: start specific, shorten misses or page an empty query. Read capability inventory once; PATH/spec/version checks prove no protocol execution. Use observed installed equivalents or report gaps once; no repeated offline installs. SSH needs a real client/session.
- Follow exec's per-call timeout and dynamic limits, including TRACE_EXEC_RECOMMENDED_CONCURRENCY and threads as PIDs; these bound process/I/O, not solving time. Batch/checkpoint and inspect partial effects. Exit, output closure, persisted result and cleanup are separate.
- Inspect stderr/bounded raw responses; retain parse errors, separate HTTP status from JSON and decode once explicitly. Avoid stdlib-shadowing scripts and percent-URL interpolation; check corrections before further requests without repeating success. Server-side include output is not source disclosure; preserve original inputs/logs. Negatives prove only tested coverage.
