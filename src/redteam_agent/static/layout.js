"use strict";

globalThis.TraceLayout = (() => {
  const { coreRun, formatTime } = globalThis.TraceUI;
  const context = document.querySelector("#run-context");
  const compact = matchMedia("(max-width: 992px)");
  const syncContext = () => { context.open = !compact.matches; };
  compact.addEventListener("change", syncContext);
  syncContext();

  function renderContext(view) {
    if (!view) return;
    const run = coreRun(view);
    const fields = { session: run.session_id, workflow: run.metadata?.workflow_id, updated: formatTime(run.updated_at) };
    for (const [key, value] of Object.entries(fields)) {
      document.querySelector(`#context-${key}`).textContent = value || "-";
    }
  }

  return Object.freeze({ renderContext });
})();
