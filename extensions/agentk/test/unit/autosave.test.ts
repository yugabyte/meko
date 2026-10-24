import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { autoSave, type AutoSaveStatus } from '../../src/web/autosave.js';

type Patch = Record<string, number | string>;

/** A saver whose requests answer only when the test says, recording what was sent and reported. */
function harness() {
  const sent: Patch[] = [];
  const answers: { resolve: (x: Patch) => void; reject: (e: Error) => void }[] = [];
  const saved: Patch[] = [];
  const failed: Patch[] = [];
  const states: string[] = [];
  let last: AutoSaveStatus | null = null;
  const saver = autoSave(
    (patch: Patch) => {
      sent.push(patch);
      return new Promise<Patch>((resolve, reject) => answers.push({ resolve, reject }));
    },
    {
      onSaved: (_, patch) => saved.push(patch),
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

  it('sends one request at a time and merges the changes made meanwhile into the next', async () => {
    const h = harness();
    void h.saver.save({ a: 1 });
    void h.saver.save({ b: 2 });
    void h.saver.save({ b: 3, c: 4 });
    expect(h.sent).toEqual([{ a: 1 }]);
    h.answers[0]!.resolve({});
    await vi.waitFor(() => expect(h.sent).toEqual([{ a: 1 }, { b: 3, c: 4 }]));
    h.answers[1]!.resolve({});
    await h.saver.flush();
    expect(h.saved).toEqual([{ a: 1 }, { b: 3, c: 4 }]);
    expect(h.states).toEqual(['saving', 'saving', 'saved']);
    vi.advanceTimersByTime(2500);
    expect(h.states.at(-1)).toBe('idle');
  });

  it('reports a failed save, and the next save still goes', async () => {
    const h = harness();
    void h.saver.save({ a: 1 });
    h.answers[0]!.reject(new Error('List at least one repos folder.'));
    await h.saver.flush();
    expect(h.failed).toEqual([{ a: 1 }]);
    expect(h.status()).toEqual({ state: 'idle', error: new Error('List at least one repos folder.') });
    const done = h.saver.save({ b: 2 });
    h.answers[1]!.resolve({});
    await done;
    expect(h.saved).toEqual([{ b: 2 }]);
    expect(h.status()).toEqual({ state: 'saved', error: null });
  });
});
