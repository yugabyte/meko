import { existsSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  assertFree,
  InstanceBusyError,
  InstanceLock,
  readLease,
  STALE_MS,
  type InstanceDeps,
  type Lease,
} from '../../src/server/instance.js';
import { tempDir } from '../helpers.js';

describe('InstanceLock', () => {
  let dir: string;
  let file: string;
  /** A fake clock that sleeps instantly, and the pids that are alive. */
  let clock: number;
  let alive: Set<number>;
  beforeEach(() => {
    dir = tempDir('akl-lock-');
    file = join(dir, 'server.lock');
    clock = 1_000_000;
    alive = new Set([100, 200]);
  });
  afterEach(() => rmSync(dir, { recursive: true, force: true }));

  function deps(host: string, pid: number, onSleep: () => void = () => undefined): InstanceDeps {
    return {
      host,
      pid,
      now: () => clock,
      alive: (p) => alive.has(p),
      sleep: async (ms) => {
        clock += ms;
        onSleep();
      },
    };
  }

  const lease = (over: Partial<Lease>): Lease => ({ host: 'other', pid: 200, version: '1', port: 7777, startedAt: clock, beatAt: clock, ...over });
  const put = (l: Lease) => writeFileSync(file, JSON.stringify(l));
  const me = { version: '2', port: 7777 };

  it('takes a free home, and gives it up again', async () => {
    const lock = await InstanceLock.acquire(file, '~/.agent-k-local', me, deps('box', 100));
    expect(readLease(file)).toMatchObject({ host: 'box', pid: 100, version: '2', port: 7777 });
    lock.release();
    expect(existsSync(file)).toBe(false);
    expect(readdirSync(dir)).toEqual([]); // no temp files left
  });

  it('refuses a home a live server on another host holds, naming that host', async () => {
    put(lease({ host: 'build-1' }));
    const err = await InstanceLock.acquire(file, '~/.agent-k-local', me, deps('box', 100)).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(InstanceBusyError);
    expect((err as Error).message).toMatch(/already running for ~\/\.agent-k-local on build-1 \(pid 200, port 7777\)/);
    expect(readLease(file)?.host).toBe('build-1');
    // Even when that pid means nothing here.
    alive.clear();
    await expect(InstanceLock.acquire(file, '~/.agent-k-local', me, deps('box', 100))).rejects.toBeInstanceOf(InstanceBusyError);
  });

  it('takes over a lease that stopped beating', async () => {
    put(lease({ host: 'build-1', beatAt: clock - STALE_MS - 1 }));
    await InstanceLock.acquire(file, '~/.agent-k-local', me, deps('box', 100));
    expect(readLease(file)).toMatchObject({ host: 'box', pid: 100 });
  });

  it('takes over from a dead server on this host straight away', async () => {
    put(lease({ host: 'box', pid: 300 }));
    const start = clock;
    await InstanceLock.acquire(file, '~/.agent-k-local', me, deps('box', 100));
    expect(readLease(file)?.pid).toBe(100);
    expect(clock - start).toBeLessThan(5000);
  });

  it("waits for this host's old server to exit (an update's restart)", async () => {
    put(lease({ host: 'box', pid: 200 }));
    let sleeps = 0;
    await InstanceLock.acquire(file, '~/.agent-k-local', me, deps('box', 100, () => {
      if (++sleeps === 3) alive.delete(200);
    }));
    expect(readLease(file)?.pid).toBe(100);
  });

  it('takes over from a crashed server whose pid another process has now, once its lease goes stale', async () => {
    // 200 is alive, but it never beats: not a server.
    put(lease({ host: 'box', pid: 200 }));
    const start = clock;
    await InstanceLock.acquire(file, '~/.agent-k-local', me, deps('box', 100));
    expect(readLease(file)?.pid).toBe(100);
    expect(clock - start).toBeGreaterThanOrEqual(STALE_MS);
  });

  it('refuses a server on this host once its lease beats', async () => {
    const other = lease({ host: 'box', pid: 200 });
    put(other);
    let sleeps = 0;
    const start = clock;
    const err = await InstanceLock.acquire(file, '~/.agent-k-local', me, deps('box', 100, () => {
      if (++sleeps === 4) put({ ...other, beatAt: clock });
    })).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(InstanceBusyError);
    expect(clock - start).toBeLessThan(STALE_MS);
    expect(readLease(file)?.pid).toBe(200);
  });

  it('lets only one of two servers starting at once go on', async () => {
    // B writes its lease while A waits to read its own back.
    const b = lease({ host: 'build-1', pid: 200 });
    const err = await InstanceLock.acquire(file, '~/.agent-k-local', me, deps('box', 100, () => put(b))).catch((e: unknown) => e);
    expect(err).toBeInstanceOf(InstanceBusyError);
    expect(readLease(file)).toEqual(b);
  });

  it('beats while it holds the lease, and reports when another server took it', async () => {
    const lock = await InstanceLock.acquire(file, '~/.agent-k-local', me, deps('box', 100));
    clock += 10_000;
    expect(lock.beat()).toBe(true);
    expect(readLease(file)?.beatAt).toBe(clock);
    // Deleted by hand: still ours, written again.
    rmSync(file);
    expect(lock.beat()).toBe(true);
    expect(readLease(file)?.pid).toBe(100);

    put(lease({ host: 'build-1' }));
    expect(lock.beat()).toBe(false);
    lock.release();
    expect(readLease(file)?.host).toBe('build-1'); // not ours to remove
  });

  it('ignores a lease file it cannot read', async () => {
    writeFileSync(file, 'not json');
    await InstanceLock.acquire(file, '~/.agent-k-local', me, deps('box', 100));
    expect(readLease(file)?.pid).toBe(100);
  });

  it('assertFree refuses only a live lease from another host', () => {
    expect(() => assertFree(file, '~/.agent-k-local', deps('box', 100))).not.toThrow();
    put(lease({ host: 'box', pid: 200 })); // this host: install restarts it anyway
    expect(() => assertFree(file, '~/.agent-k-local', deps('box', 100))).not.toThrow();
    put(lease({ host: 'build-1', beatAt: clock - STALE_MS - 1 }));
    expect(() => assertFree(file, '~/.agent-k-local', deps('box', 100))).not.toThrow();
    put(lease({ host: 'build-1' }));
    expect(() => assertFree(file, '~/.agent-k-local', deps('box', 100))).toThrow(InstanceBusyError);
  });
});
