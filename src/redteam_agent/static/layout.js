"use strict";

globalThis.TraceLayout = (() => {
  const { coreRun, formatTime } = globalThis.TraceUI;
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
