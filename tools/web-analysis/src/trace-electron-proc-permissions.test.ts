import { expect, it, vi } from "vitest";
import { createLinuxElectronOwnership } from "./browser/LinuxElectronOwnership.js";
import {
  cleanupOwnedProcessGroup,
  type OwnedProcessGroup,
  type ProcessOwnershipHost,
  type ProcessTableEntry,
} from "./process/ProcessOwnership.js";

// Linux ptrace read checks cover both /proc/PID/environ and /proc/PID/exe.
// Unlike the existing environment-only fixture, deny both observations here.
// These tests preserve unresolved cleanup; they do not grant ownership from
// command text, matching UID/PGID, or a historical proof with missing identity.
const fixture = () => {
  const runId = "electron-proc-permissions";
  const executable = "/opt/electron/electron";
  const ownership: OwnedProcessGroup = {
    runId, leaderPid: 10, processGroupId: 10,
    expectedParentPid: 1, expectedCommand: executable,
  };
  let rows: ProcessTableEntry[] = [
    { pid: 10, parentPid: 1, processGroupId: 10, uid: 1000, state: "S", command: executable },
    { pid: 11, parentPid: 10, processGroupId: 10, uid: 1000, state: "S", command: `${executable} --type=renderer` },
  ];
  const denied = { environment: false, executable: false };
  let childToken: string | undefined;
  const permissionError = (path: string) => Object.assign(
    new Error(`EACCES: permission denied, '${path}'`), { code: "EACCES" },
  );
  const signalGroup = vi.fn(() => { rows = []; });
  const host: ProcessOwnershipHost = {
    platform: "linux",
    listProcesses: async () => rows.map(row => ({ ...row })),
    processIdentities: async members => new Map(members.map(({ pid }) => [
      pid, { state: "readable" as const, identity: `linux-starttime:${pid}` },
    ])),
    environment: async pid => {
      if (pid === 11 && denied.environment) throw permissionError(`/proc/${pid}/environ`);
      const token = pid === 10 ? runId : childToken;
      return token === undefined ? {} : { REA_PROCESS_RUN_ID: token };
    },
    signalGroup,
  };
  const adapter = createLinuxElectronOwnership(ownership, host, async pid => {
    if (pid === 11 && denied.executable) throw permissionError(`/proc/${pid}/exe`);
    return executable;
  });
  return { ownership, adapter, denied, signalGroup, rows: () => rows,
    giveChildToken: () => { childToken = runId; } };
};

it("keeps simultaneous environ/exe EACCES unresolved during initial lineage capture", async () => {
  const f = fixture();
  f.denied.environment = f.denied.executable = true;
  await f.adapter.capture();
  expect((await f.adapter.host.runTokens!(f.rows())).get(11)).toMatchObject({ state: "unavailable" });
  expect(await cleanupOwnedProcessGroup(f.ownership, f.adapter.host)).toMatchObject({
    cleaned: false,
    reason: expect.stringContaining("environment_errno_EACCES=1"),
    failures: [expect.objectContaining({ pid: 11, reason: "environment-unreadable" })],
  });
  expect(f.signalGroup).not.toHaveBeenCalled();
  expect(f.adapter.usesLineageProof()).toBe(false);
});

it("does not reuse historical executable proof when a captured helper becomes unreadable", async () => {
  const f = fixture();
  await f.adapter.capture();
  f.denied.environment = f.denied.executable = true;
  expect((await f.adapter.host.runTokens!(f.rows())).get(11)).toMatchObject({ state: "unavailable" });
  expect((await cleanupOwnedProcessGroup(f.ownership, f.adapter.host)).cleaned).toBe(false);
  expect(f.signalGroup).not.toHaveBeenCalled();
  expect(f.adapter.usesLineageProof()).toBe(false);
});

it("retains token-based cleanup when exe is inaccessible but the real run token is readable", async () => {
  const f = fixture();
  f.giveChildToken();
  f.denied.executable = true;
  await f.adapter.capture();
  expect(await cleanupOwnedProcessGroup(f.ownership, f.adapter.host)).toMatchObject({ cleaned: true, signaled: true });
  expect(f.signalGroup).toHaveBeenCalledWith(10, "SIGKILL");
  expect(f.adapter.usesLineageProof()).toBe(false);
});
