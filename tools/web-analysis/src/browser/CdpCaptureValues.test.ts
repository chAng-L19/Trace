import { afterEach, describe, expect, it, vi } from "vitest";

import { executionContextKey as eventExecutionContextKey } from "./CdpCaptureEventHelpers.js";
import {
  delayWithCancellation,
  executionContextKey,
  numberArray,
} from "./CdpCaptureValues.js";

describe("cancellable browser observation delays", () => {
  afterEach(() => vi.useRealTimers());

  it("schedules durations beyond the native timer range in consecutive waits", async () => {
    vi.useFakeTimers();
    let completed = false;
    const waiting = delayWithCancellation(
      2_147_483_648,
      "observe_javascript_runtime",
    ).then(() => {
      completed = true;
    });

    expect(vi.getTimerCount()).toBe(1);
    await vi.advanceTimersByTimeAsync(2_147_483_647);
    expect(completed).toBe(false);
    expect(vi.getTimerCount()).toBe(1);

    await vi.advanceTimersByTimeAsync(1);
    await waiting;
    expect(completed).toBe(true);
  });
});

describe("CDP numeric protocol values", () => {
  it("filters invalid array items while preserving finite values and their order", () => {
    const values = numberArray([
      1, "2", null, undefined, NaN, Infinity, -Infinity,
      1.5, Number.MAX_SAFE_INTEGER + 1, Number.MIN_SAFE_INTEGER - 1,
      -1, -0, 0, true, {}, [],
    ]);

    expect(values).toEqual([
      1, 1.5, Number.MAX_SAFE_INTEGER + 1, Number.MIN_SAFE_INTEGER - 1,
      -1, -0, 0,
    ]);
    expect(Object.is(values[5], -0)).toBe(true);
  });

  it("returns an empty array for empty arrays and non-array inputs", () => {
    for (const value of [[], undefined, null, "", "1", 0, {}])
      expect(numberArray(value)).toEqual([]);
  });

  it("preserves safe integer context keys and the event helper export", () => {
    expect(eventExecutionContextKey).toBe(executionContextKey);
    for (const [value, expected] of [
      [1, "1"],
      [-1, "-1"],
      [0, "0"],
      [-0, "0"],
      [Number.MAX_SAFE_INTEGER, "9007199254740991"],
      [Number.MIN_SAFE_INTEGER, "-9007199254740991"],
      [1.5, null],
      [Number.MAX_SAFE_INTEGER + 1, null],
      [Number.MIN_SAFE_INTEGER - 1, null],
      [NaN, null],
      [Infinity, null],
      [-Infinity, null],
      ["1", null],
      ["", null],
      [null, null],
      [undefined, null],
      [true, null],
      [{}, null],
      [[], null],
    ] as const)
      expect(executionContextKey(value)).toBe(expected);
  });
});
