import { randomBytes } from 'node:crypto';
import { readFileSync, renameSync, unlinkSync, writeFileSync } from 'node:fs';
import { hostname } from 'node:os';

/**
 * One server per Kode home. The home can be on a network file system that other hosts
 * mount (a home directory shared over NFS), and two servers on one home break each other:
 * each binds the hook socket over the other's and both write the same SQLite file. The
 * server that runs holds a lease file, `<home>/server.lock`, and rewrites it every
 * BEAT_MS; a lease not rewritten for STALE_MS is free. File locks aren't used because
 * they aren't reliable across NFS clients.
 */
export interface Lease {
  host: string;
  pid: number;
  version: string;
  port: number;
  startedAt: number;
  beatAt: number;
}

export const BEAT_MS = 10_000;
export const STALE_MS = 45_000;
/** How long after writing the lease it's read back, so of two servers starting at once only one goes on. */
const SETTLE_MS = 1000;
const POLL_MS = 500;
/**
 * `serve`'s exit status when another server holds the home. The systemd unit doesn't restart on
 * it (RestartPreventExitStatus), and it isn't counted as a failed start of an update.
 */
export const EXIT_BUSY = 75;

export interface InstanceDeps {
  host: string;
  pid: number;
  now: () => number;
  alive: (pid: number) => boolean;
  sleep: (ms: number) => Promise<void>;
  /** For tests, whose fresh homes have no rival: how long after writing the lease it's read back (default SETTLE_MS). */
  settleMs?: number;
}

function pidAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (e) {
    // EPERM: it exists, but belongs to someone else.
    return (e as NodeJS.ErrnoException).code === 'EPERM';
  }
}

export const defaultDeps = (): InstanceDeps => ({
  host: hostname(),
  pid: process.pid,
  now: Date.now,
  alive: pidAlive,
  sleep: (ms) => new Promise((r) => setTimeout(r, ms)),
});

/** Another server holds the lease; the message says where it runs and what to do. */
export class InstanceBusyError extends Error {
  constructor(readonly lease: Lease, home: string) {
    super(
      `Kode is already running for ${home} on ${lease.host} (pid ${lease.pid}${lease.port ? `, port ${lease.port}` : ''}). ` +
        `Only one server may use a Kode home at a time. Stop it there first.`,
    );
  }
}

export function readLease(file: string): Lease | null {
  try {
    const l = JSON.parse(readFileSync(file, 'utf8')) as Partial<Lease>;
    if (typeof l.host !== 'string' || typeof l.pid !== 'number' || typeof l.beatAt !== 'number') return null;
    return { version: '', port: 0, startedAt: l.beatAt, ...l } as Lease;
  } catch {
    return null;
  }
}

function writeLease(file: string, lease: Lease): void {
  const tmp = `${file}.tmp-${randomBytes(4).toString('hex')}`;
  writeFileSync(tmp, JSON.stringify(lease) + '\n');
  renameSync(tmp, file);
}

const same = (a: Lease | null, b: Lease) => !!a && a.host === b.host && a.pid === b.pid && a.startedAt === b.startedAt;

/** Whether a lease still stands in the way of `deps` (this process) starting. */
function held(l: Lease, deps: InstanceDeps): boolean {
  if (deps.now() - l.beatAt > STALE_MS) return false;
  if (l.host !== deps.host) return true;
  return l.pid !== deps.pid && deps.alive(l.pid);
}

/** The lease when another live process holds it (a server on this home, running here or elsewhere), else null. */
export function heldLease(file: string, deps: InstanceDeps = defaultDeps()): Lease | null {
  const l = readLease(file);
  return l && held(l, deps) ? l : null;
}

/** Throws InstanceBusyError when a live server holds the home's lease (for `install`, which mustn't run beside it on another host). */
export function assertFree(file: string, home: string, deps: InstanceDeps = defaultDeps()): void {
  const l = readLease(file);
  if (l && l.host !== deps.host && held(l, deps)) throw new InstanceBusyError(l, home);
}

export class InstanceLock {
  private constructor(
    private file: string,
    private lease: Lease,
    private deps: InstanceDeps,
  ) {}

  /**
   * Takes the home's lease, or throws InstanceBusyError. A lease from another host that is still
   * beating is refused at once. On this host a live pid proves nothing (after a crash the pid can
   * belong to another process by now), and an old server may still be exiting (an update's
   * restart), so the start waits until the lease is released, its pid exits or it goes stale, and
   * is refused only once the lease beats again.
   */
  static async acquire(file: string, home: string, me: { version: string; port: number }, deps: InstanceDeps = defaultDeps()): Promise<InstanceLock> {
    let waitingOn: Lease | null = null;
    for (;;) {
      const cur = readLease(file);
      if (cur && held(cur, deps)) {
        const beat = !!waitingOn && same(waitingOn, cur) && cur.beatAt > waitingOn.beatAt;
        if (cur.host === deps.host && !beat) {
          if (!waitingOn || !same(waitingOn, cur)) waitingOn = cur;
          await deps.sleep(POLL_MS);
          continue;
        }
        throw new InstanceBusyError(cur, home);
      }
      const now = deps.now();
      const lease: Lease = { host: deps.host, pid: deps.pid, version: me.version, port: me.port, startedAt: now, beatAt: now };
      writeLease(file, lease);
      await deps.sleep(deps.settleMs ?? SETTLE_MS);
      // Another server that wrote after us wins; the next pass reports it.
      if (same(readLease(file), lease)) return new InstanceLock(file, lease, deps);
    }
  }

  /**
   * Rewrites the lease's time. False when another server has taken the lease (this one
   * stalled for longer than STALE_MS): the caller must stop.
   */
  beat(): boolean {
    const cur = readLease(this.file);
    if (cur && !same(cur, this.lease)) return false;
    this.lease = { ...this.lease, beatAt: this.deps.now() };
    try {
      writeLease(this.file, this.lease);
    } catch {
      // A failed write only makes the lease look older; the next beat tries again.
    }
    return true;
  }

  /** Gives the lease up, unless another server has taken it since. */
  release(): void {
    if (!same(readLease(this.file), this.lease)) return;
    try {
      unlinkSync(this.file);
    } catch {
      // already gone
    }
  }
}
