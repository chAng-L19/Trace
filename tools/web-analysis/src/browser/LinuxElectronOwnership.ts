import { readlink } from "node:fs/promises";
import type {
  OwnedProcessGroup,
  ProcessOwnershipHost,
  ProcessRunTokenObservation,
  ProcessTableEntry,
} from "../process/ProcessOwnership.js";
import { launcherIdentityFailure } from "../process/ProcessOwnershipIdentity.js";
import { descendantsOf, liveProcesses } from "../process/ProcessOwnershipProcessTree.js";
import { readProcessRunTokens } from "../process/ProcessRunTokenObservations.js";

type Identity = { readonly start: string; readonly executable: string };
type Proof = Identity & { readonly uid: number; readonly group: number };

/**
 * Chromium zygotes can clear their environment, and nondumpable renderers deny
 * /proc/environ reads. Attribute only helpers actually observed beneath the
 * token-authenticated Electron launcher. A process group or filename alone is
 * never authority. This adapter belongs to one capture and never discovers new
 * descendants during cleanup.
 */
export const createLinuxElectronOwnership = (
  ownership: OwnedProcessGroup,
  base: ProcessOwnershipHost,
  executable: (pid: number) => Promise<string> = (pid) => readlink(`/proc/${String(pid)}/exe`),
) => {
  const proofs = new Map<number, Proof>();
  const baseline = new Map(ownership.captureBaseline?.map(({ pid, identity }) => [pid, identity]));
  let launcherIdentity: string | undefined;
  let usedLineageProof = false;

  const identities = async (members: readonly ProcessTableEntry[]) => {
    const starts = await base.processIdentities?.(members);
    const results = new Map<number, Identity>();
    await Promise.all(members.map(async ({ pid }) => {
      const start = starts?.get(pid);
      if (start?.state !== "readable") return;
      try {
        results.set(pid, { start: start.identity, executable: await executable(pid) });
      } catch {
        // Exited, inaccessible, or changed processes remain unproven.
      }
    }));
    return results;
  };

  const tokens = async (members: readonly ProcessTableEntry[], signal?: AbortSignal) => {
    const result = new Map<number, ProcessRunTokenObservation>();
    for await (const { process, observation } of readProcessRunTokens(base, members, signal))
      result.set(process.pid, observation);
    return result;
  };

  const sameIdentity = (left: Identity | undefined, right: Identity | undefined) =>
    left !== undefined && right !== undefined &&
    left.start === right.start && left.executable === right.executable;
  const sameEntry = (left: ProcessTableEntry, right: ProcessTableEntry | undefined) =>
    right !== undefined && left.parentPid === right.parentPid &&
    left.processGroupId === right.processGroupId && left.uid === right.uid;

  /** Bracket the complete parent chain with process-table and start-time reads. */
  const capture = async (): Promise<void> => {
    if (base.platform !== "linux" || base.processIdentities === undefined) return;
    try {
      const before = liveProcesses(await base.listProcesses());
      const root = before.find(({ pid }) => pid === ownership.leaderPid);
      if (root === undefined || root.uid === undefined ||
          launcherIdentityFailure(root, ownership) !== null) return;
      const candidates = [root, ...descendantsOf([root.pid], before)];
      const first = await identities(candidates);
      const observedTokens = await tokens(candidates);
      const after = new Map(liveProcesses(await base.listProcesses()).map(row => [row.pid, row]));
      const second = await identities(candidates.filter(({ pid }) => after.has(pid)));
      const rootToken = (await tokens([root])).get(root.pid);
      const rootIdentity = first.get(root.pid);
      if (!sameEntry(root, after.get(root.pid)) ||
          !sameIdentity(rootIdentity, second.get(root.pid)) ||
          rootIdentity === undefined || rootToken?.state !== "readable" ||
          rootToken.runId !== ownership.runId ||
          observedTokens.get(root.pid)?.state !== "readable") return;
      const initialToken = observedTokens.get(root.pid);
      if (initialToken?.state !== "readable" || initialToken.runId !== ownership.runId) return;
      if (launcherIdentity !== undefined && launcherIdentity !== rootIdentity.start) return;
      launcherIdentity = rootIdentity.start;
      const anchored = new Set([root.pid]);
      for (const child of candidates.slice(1)) {
        const identity = first.get(child.pid);
        const token = observedTokens.get(child.pid);
        if (!anchored.has(child.parentPid) || !sameEntry(child, after.get(child.pid)) ||
            !sameIdentity(identity, second.get(child.pid)) || identity === undefined ||
            baseline.get(child.pid) === identity.start ||
            child.uid !== root.uid || child.processGroupId !== root.processGroupId ||
            identity.executable !== rootIdentity.executable ||
            (token?.state === "readable" && token.runId !== undefined && token.runId !== ownership.runId))
          continue;
        anchored.add(child.pid);
        proofs.set(child.pid, { ...identity, uid: root.uid, group: root.processGroupId });
      }
    } catch {
      // The original token-only path still reports missing proof and fails closed.
    }
  };

  const host: ProcessOwnershipHost = {
    ...base,
    async runTokens(members, signal) {
      const observed = await tokens(members, signal);
      const candidates = members.filter(({ pid }) => {
        const token = observed.get(pid);
        return proofs.has(pid) && (token?.state === "unavailable" ||
          (token?.state === "readable" && token.runId === undefined));
      });
      if (candidates.length === 0) return observed;
      signal?.throwIfAborted();
      const first = await identities(candidates);
      const current = new Map(liveProcesses(await base.listProcesses(signal)).map(row => [row.pid, row]));
      const second = await identities(candidates.filter(({ pid }) => current.has(pid)));
      for (const candidate of candidates) {
        const proof = proofs.get(candidate.pid)!;
        const row = current.get(candidate.pid);
        // Reparenting after launcher exit is allowed only for an already captured
        // start identity; a new PID, executable, UID, or process group is rejected.
        if (row === undefined || row.uid !== proof.uid || row.processGroupId !== proof.group ||
            candidate.uid !== proof.uid || candidate.processGroupId !== proof.group ||
            !sameIdentity(proof, first.get(candidate.pid)) ||
            !sameIdentity(proof, second.get(candidate.pid))) continue;
        usedLineageProof = true;
        observed.set(candidate.pid, {
          state: "readable",
          runId: ownership.runId,
          source: "linux-electron-lineage",
        });
      }
      signal?.throwIfAborted();
      return observed;
    },
  };
  return { host, capture, usesLineageProof: () => usedLineageProof };
};
