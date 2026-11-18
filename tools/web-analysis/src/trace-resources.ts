/** Keep the original operation outcome if final resource cleanup also fails. */
export async function withClosedResource<T>(
  resource: { close(): Promise<void> },
  run: () => Promise<T>,
): Promise<T> {
  let result: T | undefined;
  let failure: unknown;
  let failed = false;
  try { result = await run(); } catch (cause) { failure = cause; failed = true; }
  try { await resource.close(); } catch (cleanup) {
    throw Object.assign(new Error("Web analysis resource cleanup failed", { cause: cleanup }), {
      cleanupIncomplete: true,
      cleanupError: cleanup,
      operationError: failed ? failure : undefined,
      partialResult: result,
    });
  }
  if (failed) throw failure;
  return result as T;
}
