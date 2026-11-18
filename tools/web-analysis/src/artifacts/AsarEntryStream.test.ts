import { Readable } from "node:stream";
import { describe, expect, it } from "vitest";
import { ArtifactReaderFailure } from "./ArtifactReader.js";
import { asarFailure, closeAsarHandle, readValidatedAsarEntry } from "./AsarEntryStream.js";

const drain = async (stream: Readable) => {
  for await (const _chunk of stream) { /* consume through all validation gates */ }
};

describe("shared ASAR failure classification", () => {
  it("preserves typed failures and distinguishes filesystem errors from malformed input", async () => {
    const original = new ArtifactReaderFailure("integrity", "original", { cause: "digest" });
    expect(asarFailure("fixture.asar", "read", original)).toBe(original);
    const io = Object.assign(new Error("missing"), { errno: -2, syscall: "open", code: "ENOENT" });
    expect(asarFailure("fixture.asar", "open", io)).toMatchObject({
      reason: "io", message: "Could not open ASAR at fixture.asar: missing", cause: io,
    });
    const malformed = Object.assign(new Error("bad header"), { code: "ENOENT" });
    expect(asarFailure("fixture.asar", "read header", malformed)).toMatchObject({
      reason: "format", message: "Malformed ASAR during read header: fixture.asar", cause: malformed,
    });
    const source = Readable.from((async function* () { throw original; })());
    await expect(drain(readValidatedAsarEntry(source, 0, "member", "fixture.asar"))).rejects.toBe(original);
    expect(source.destroyed).toBe(true);
  });

  it.each([
    [2, "ASAR entry size disagrees with its header: member"],
    [0, "ASAR entry exceeded its declared size: member"],
  ])("retains declared-size validation for %i bytes", async (size, message) => {
    const source = Readable.from([Buffer.from("x")]);
    await expect(drain(readValidatedAsarEntry(source, size, "member", "fixture.asar")))
      .rejects.toMatchObject({ reason: "integrity", message });
    expect(source.destroyed).toBe(true);
  });

  it("closes before cancellation and preserves cleanup failure precedence", async () => {
    const signal = AbortSignal.abort();
    let closed = false;
    await expect(closeAsarHandle(async () => { closed = true; }, signal))
      .rejects.toMatchObject({ reason: "cancelled" });
    expect(closed).toBe(true);
    const cleanup = new Error("close failed");
    await expect(closeAsarHandle(async () => { throw cleanup; }, signal)).rejects.toBe(cleanup);
  });
});
