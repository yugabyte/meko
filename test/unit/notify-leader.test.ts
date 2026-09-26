import { describe, expect, it } from 'vitest';
import { notifyLeader } from '../../src/web/notifyLeader.js';

/** Just enough of the Web Locks API, for one lock shared by several pages. */
function fakeLocks() {
  interface Req { page: string; cb: () => Promise<unknown>; reject: (e: unknown) => void }
  let holder: Req | null = null;
  let queue: Req[] = [];
  const grant = (r: Req) => {
    holder = r;
    void r.cb(); // our callback never settles: the lock is held until stolen or the page closes
  };
  return {
    page(page: string) {
      return {
        request: (_name: string, opts: { steal?: boolean }, cb: () => Promise<unknown>) =>
          new Promise((_resolve, reject) => {
            const r = { page, cb, reject };
            if (opts.steal) {
              holder?.reject(new Error('AbortError'));
              grant(r);
            } else if (!holder) grant(r);
            else queue.push(r);
          }),
      } as unknown as Pick<LockManager, 'request'>;
    },
    close(page: string) {
      queue = queue.filter((r) => r.page !== page);
      if (holder?.page === page) {
        holder = null;
        const next = queue.shift();
        if (next) grant(next);
      }
    },
    queued: (page: string) => queue.filter((r) => r.page === page).length,
  };
}

const settle = () => new Promise((r) => setTimeout(r, 0));

describe('which page shows notifications', () => {
  it('is the page used last, and passes on when that page closes', async () => {
    const locks = fakeLocks();
    const a = notifyLeader(locks.page('a'));
    const b = notifyLeader(locks.page('b'));
    await settle();
    expect([a.leader, b.leader]).toEqual([true, false]);

    b.claim(); // the person switches to b
    await settle();
    expect([a.leader, b.leader]).toEqual([false, true]);

    a.claim();
    await settle();
    b.claim();
    await settle();
    a.claim();
    await settle();
    expect([a.leader, b.leader]).toEqual([true, false]);
    // Switching back and forth doesn't pile up queued requests.
    expect([locks.queued('a'), locks.queued('b')]).toEqual([1, 1]);

    locks.close('a');
    await settle();
    expect(b.leader).toBe(true);
  });

  it('lets every page notify when the browser has no Web Locks', () => {
    expect(notifyLeader(undefined).leader).toBe(true);
  });
});
