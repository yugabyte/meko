import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { autoSave, settle, type AutoSaveStatus } from '../../src/web/autosave.js';

type Patch = Record<string, number | string | boolean>;

/** A saver whose requests answer only when the test says, recording what was sent and reported. */
function harness() {
  const sent: Patch[] = [];
  const answers: { resolve: (x: Patch) => void; reject: (e: Error) => void }[] = [];
  const saved: [Patch, Patch[]][] = [];
  const failed: Patch[] = [];
  const states: string[] = [];
  let last: AutoSaveStatus | null = null;
  const saver = autoSave(
    (patch: Patch) => {
      sent.push(patch);
      return new Promise<Patch>((resolve, reject) => answers.push({ resolve, reject }));
    },
    {
      onSaved: (_, patch, queued) => saved.push([patch, [...queued]]),
      onFailed: (_, patch) => failed.push(patch),
      onStatus: (s) => {
        last = s;
        states.push(s.state);
      },
    },
  );
  return { saver, sent, answers, saved, failed, states, status: () => last };
}

describe('autoSave', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('sends each change on its own, one at a time, telling each answer what still waits', async () => {
    const h = harness();
    void h.saver.save({ a: 1 });
    void h.saver.save({ b: 2 });
    void h.saver.save({ b: 3 });
    expect(h.sent).toEqual([{ a: 1 }]);
    h.answers[0]!.resolve({});
    await vi.waitFor(() => expect(h.sent).toEqual([{ a: 1 }, { b: 2 }]));
    h.answers[1]!.resolve({});
    await vi.waitFor(() => expect(h.sent).toHaveLength(3));
    h.answers[2]!.resolve({});
    expect(await h.saver.flush()).toBe(true);
    expect(h.saved).toEqual([[{ a: 1 }, [{ b: 2 }, { b: 3 }]], [{ b: 2 }, [{ b: 3 }]], [{ b: 3 }, []]]);
    expect(h.states).toEqual(['saving', 'saving', 'saving', 'saved']);
    vi.advanceTimersByTime(2500);
    expect(h.states.at(-1)).toBe('idle');
  });

  it("keeps a failure through the run's later saves, and the next run starts clean", async () => {
    const h = harness();
    const run = h.saver.save({ a: 1 });
    void h.saver.save({ b: 2 });
    h.answers[0]!.reject(new Error('List at least one repos folder.'));
    await vi.waitFor(() => expect(h.sent).toHaveLength(2));
    h.answers[1]!.resolve({});
    expect(await run).toBe(false);
    expect(h.failed).toEqual([{ a: 1 }]);
    expect(h.status()).toEqual({ state: 'idle', error: new Error('List at least one repos folder.') });
    const next = h.saver.save({ c: 3 });
    h.answers[2]!.resolve({});
    expect(await next).toBe(true);
    expect(h.status()).toEqual({ state: 'saved', error: null });
  });
});

describe('settle', () => {
  const cur = { a: true, b: 'typed', c: 1 };
  const answer = { a: false, b: 'saved', c: 2 };
  it.each([
    ['takes the fields the change sent from the answer', { a: false }, [], { a: false, b: 'typed', c: 1 }],
    ['keeps a field a waiting change will set', { a: false, c: 2 }, [{ a: true }], { a: true, b: 'typed', c: 2 }],
  ])('%s', (_, patch, queued, want) => {
    expect(settle(cur, answer, patch, queued)).toEqual(want);
  });
});
