import { expect, it, vi } from "vitest";
import { createLinuxElectronOwnership } from "./browser/LinuxElectronOwnership.js";
import { cleanupOwnedProcessGroup, type OwnedProcessGroup, type ProcessOwnershipHost, type ProcessTableEntry } from "./process/ProcessOwnership.js";
import { observeOwnedProcessLineage } from "./process/ProcessOwnershipObservation.js";

const RUN = "capture-ownership-test";
const EXE = "/opt/electron/electron";
const fixture = () => {
  const ownership: OwnedProcessGroup = {
    runId: RUN, leaderPid: 10, processGroupId: 10,
    expectedParentPid: 1, expectedCommand: EXE,
  };
  let rows: ProcessTableEntry[] = [
    { pid: 10, parentPid: 1, processGroupId: 10, uid: 1000, state: "S", command: EXE },
    { pid: 11, parentPid: 10, processGroupId: 10, uid: 1000, state: "S", command: `${EXE} --type=zygote` },
    { pid: 12, parentPid: 11, processGroupId: 10, uid: 1000, state: "S", command: `${EXE} --type=renderer` },
  ];
  const starts = new Map([[10, "start-10"], [11, "start-11"], [12, "start-12"]]);
  const exes = new Map([[10, EXE], [11, EXE], [12, EXE]]);
  const runIds = new Map<number, string | undefined>([[10, RUN]]);
  const inaccessible = new Set<number>([12]);
  const signalGroup = vi.fn(() => { rows = []; });
  const host: ProcessOwnershipHost = {
    platform: "linux",
    listProcesses: vi.fn(async () => rows.map(row => ({ ...row }))),
    environment: async (pid) => {
      if (inaccessible.has(pid)) throw Object.assign(new Error("permission denied"), { code: "EACCES" });
      const token = runIds.get(pid);
      return token === undefined ? {} : { REA_PROCESS_RUN_ID: token };
    },
    processIdentities: async (members) => new Map(members.map(({ pid }) => {
      const identity = starts.get(pid);
      return [pid, identity === undefined
        ? { state: "unavailable" as const, reason: "gone" }
        : { state: "readable" as const, identity }];
    })),
    signalGroup,
  };
  const adapter = createLinuxElectronOwnership(ownership, host, async pid => {
    const exe = exes.get(pid);
    if (exe === undefined) throw new Error("exe unavailable");
    return exe;
  });
  return { ownership, adapter, host, starts, exes, runIds, inaccessible, signalGroup,
    rows: () => rows, setRows: (value: ProcessTableEntry[]) => { rows = value; } };
};

it("proves token-cleared zygote and nondumpable renderer through stable Linux lineage before cleanup", async () => {
  const f = fixture();
  await f.adapter.capture();
  const lineage = await observeOwnedProcessLineage(f.ownership, f.adapter.host);
  expect(lineage.status).toBe("verified");
  const observed = await f.adapter.host.runTokens!(f.rows());
  expect(observed.get(11)).toEqual({ state: "readable", runId: RUN, source: "linux-electron-lineage" });
  expect(observed.get(12)).toEqual({ state: "readable", runId: RUN, source: "linux-electron-lineage" });
  expect(await cleanupOwnedProcessGroup(f.ownership, f.adapter.host)).toMatchObject({ cleaned: true, signaled: true });
  expect(f.signalGroup).toHaveBeenCalledWith(10, "SIGKILL");
  expect(f.adapter.usesLineageProof()).toBe(true);
});

it.each(["missing", "foreign"])("requires the launcher's real token: %s", async mode => {
  const f = fixture();
  f.runIds.set(10, mode === "foreign" ? "other-capture" : undefined);
  await f.adapter.capture();
  expect((await observeOwnedProcessLineage(f.ownership, f.adapter.host)).status).toBe("unavailable");
  expect((await f.adapter.host.runTokens!(f.rows())).get(11)).not.toHaveProperty("source");
});

it.each(["uid", "executable", "group", "parent", "foreign-token"])("rejects mismatched descendant %s and its descendants", async kind => {
  const f = fixture();
  if (kind === "uid") f.setRows(f.rows().map(row => row.pid === 11 ? { ...row, uid: 1001 } : row));
  if (kind === "executable") f.exes.set(11, "/opt/unrelated");
  if (kind === "group") f.setRows(f.rows().map(row => row.pid === 11 ? { ...row, processGroupId: 20 } : row));
  if (kind === "parent") f.setRows(f.rows().map(row => row.pid === 11 ? { ...row, parentPid: 99 } : row));
  if (kind === "foreign-token") f.runIds.set(11, "other-capture");
  await f.adapter.capture();
  const observations = await f.adapter.host.runTokens!(f.rows());
  expect(observations.get(11)).not.toHaveProperty("source");
  expect(observations.get(12)).not.toHaveProperty("source");
  expect((await cleanupOwnedProcessGroup(f.ownership, f.adapter.host)).cleaned).toBe(false);
  expect(f.signalGroup).not.toHaveBeenCalled();
});

it.each(["start", "executable", "uid", "group", "foreign-token"])("revalidates captured helper %s before giving cleanup authority", async kind => {
  const f = fixture();
  await f.adapter.capture();
  if (kind === "start") f.starts.set(11, "reused-pid");
  if (kind === "executable") f.exes.set(11, "/opt/unrelated");
  if (kind === "uid") f.setRows(f.rows().map(row => row.pid === 11 ? { ...row, uid: 1001 } : row));
  if (kind === "group") f.setRows(f.rows().map(row => row.pid === 11 ? { ...row, processGroupId: 20 } : row));
  if (kind === "foreign-token") f.runIds.set(11, "other-capture");
  expect((await f.adapter.host.runTokens!(f.rows())).get(11)).not.toHaveProperty("source");
  expect((await cleanupOwnedProcessGroup(f.ownership, f.adapter.host)).cleaned).toBe(false);
  expect(f.signalGroup).not.toHaveBeenCalled();
});

it("keeps captured identities after reparenting when the launcher exits", async () => {
  const f = fixture();
  await f.adapter.capture();
  f.setRows(f.rows().filter(row => row.pid !== 10).map(row => row.pid === 11 ? { ...row, parentPid: 1 } : row));
  expect((await cleanupOwnedProcessGroup(f.ownership, f.adapter.host)).cleaned).toBe(true);
  expect(f.signalGroup).toHaveBeenCalledWith(10, "SIGKILL");
});

it("does not trust an unrelated process that shares the Electron group and executable", async () => {
  const f = fixture();
  f.setRows([...f.rows(), { pid: 13, parentPid: 99, processGroupId: 10, uid: 1000, state: "S", command: EXE }]);
  f.starts.set(13, "start-13");
  f.exes.set(13, EXE);
  await f.adapter.capture();
  expect((await cleanupOwnedProcessGroup(f.ownership, f.adapter.host)).cleaned).toBe(false);
  expect(f.signalGroup).not.toHaveBeenCalled();
});

it("does not enroll new tokenless descendants during cleanup", async () => {
  const f = fixture();
  await f.adapter.capture();
  f.setRows([...f.rows(), { pid: 13, parentPid: 11, processGroupId: 10, uid: 1000, state: "S", command: EXE }]);
  f.starts.set(13, "start-13");
  f.exes.set(13, EXE);
  expect((await cleanupOwnedProcessGroup(f.ownership, f.adapter.host)).cleaned).toBe(false);
  expect(f.signalGroup).not.toHaveBeenCalled();
});

it("rejects a parent-chain change between the capture snapshots", async () => {
  const f = fixture();
  const original = f.rows().map(row => ({ ...row }));
  vi.mocked(f.host.listProcesses).mockResolvedValueOnce(original).mockImplementation(async () =>
    original.map(row => row.pid === 11 ? { ...row, parentPid: 99 } : row));
  await f.adapter.capture();
  expect((await f.adapter.host.runTokens!(original)).get(11)).not.toHaveProperty("source");
});

it("rejects PID reuse between the capture identity snapshots", async () => {
  const f = fixture();
  const original = f.host.processIdentities!;
  let reads = 0;
  f.host.processIdentities = async (...args) => {
    if (++reads === 2) f.starts.set(11, "reused-between-reads");
    return original(...args);
  };
  await f.adapter.capture();
  expect((await f.adapter.host.runTokens!(f.rows())).get(11)).not.toHaveProperty("source");
});

it("does not enroll an unchanged process from the prelaunch baseline", async () => {
  const f = fixture();
  const adapter = createLinuxElectronOwnership({
    ...f.ownership, captureBaseline: [{ pid: 11, identity: "start-11" }],
  }, f.host, async () => EXE);
  await adapter.capture();
  const observations = await adapter.host.runTokens!(f.rows());
  expect(observations.get(11)).not.toHaveProperty("source");
  expect(observations.get(12)).not.toHaveProperty("source");
});
