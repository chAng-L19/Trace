import { test } from "node:test";
import assert from "node:assert/strict";
import { withClosedResource } from "../dist/trace-resources.js";
import { execute } from "../dist/trace-operations.js";

test("cleanup failure retains the completed operation result", async () => {
  const result = { ok:true, value:{ evidence_id:"retained" } };
  const cleanup = new Error("pending browser transport");
  await assert.rejects(withClosedResource({close:async()=>{throw cleanup;}},async()=>result), error => {
    assert.equal(error.cleanupIncomplete,true);
    assert.equal(error.cleanupError,cleanup);
    assert.equal(error.partialResult,result);
    return true;
  });
});

test("resource close executes after operation failure without masking it", async () => {
  const original = new Error("operation failed"); let closed = false;
  await assert.rejects(withClosedResource({close:async()=>{closed=true;}},async()=>{throw original;}), error=>error===original);
  assert.equal(closed,true);
});

test("pre-cancelled requests stop before schema parsing or resource acquisition", async () => {
  const reason = new Error("caller cancelled");
  await assert.rejects(execute("analyze_javascript_application", {}, AbortSignal.abort(reason)), error => error === reason);
});
