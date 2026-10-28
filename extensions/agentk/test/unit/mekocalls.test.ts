import { afterEach, describe, expect, it, vi } from 'vitest';
import { MEKO_MIN_SHOW_MS, MekoCalls, MekoHold } from '../../src/server/mekocalls.js';

describe('Meko calls under way, as hooks report them', () => {
  it('opens a call at its start and closes it at its end, counting a key started twice (two Kiro calls of one tool), and ignores an end that never started', () => {
    const calls = new MekoCalls();
    calls.end('s1', 'k');
    expect(calls.active('s1')).toBe(false);
    calls.start('s1', 'k');
    calls.start('s1', 'k');
    calls.end('s1', 'k');
    expect(calls.active('s1')).toBe(true);
    calls.end('s1', 'k');
    calls.end('s1', 'k');
    expect(calls.active('s1')).toBe(false);
  });

  it('clears a session at once (the turn ended), and keeps no more than its cap or a key too long', () => {
    const calls = new MekoCalls();
    expect(calls.start('s1', 'x'.repeat(201))).toBe(false);
    expect(calls.active('s1')).toBe(false);
    for (let i = 0; i < 25; i++) expect(calls.start('s1', `k${i}`)).toBe(i < 20);
    for (let i = 0; i < 20; i++) calls.end('s1', `k${i}`);
    // Past the cap, the extra starts were dropped, so their ends find nothing.
    expect(calls.active('s1')).toBe(false);
    calls.start('s1', 'a');
    calls.start('s2', 'b');
    calls.clear('s1');
    expect([calls.active('s1'), calls.active('s2')]).toEqual([false, true]);
  });
});

describe("the hold on the dot's Meko m", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  function hold() {
    vi.useFakeTimers();
    let now = 1_000_000;
    const changes: string[] = [];
    const h = new MekoHold((s) => changes.push(s), () => now);
    const tick = (ms: number) => {
      now += ms;
      vi.advanceTimersByTime(ms);
    };
    return { h, changes, tick };
  }

  it('keeps a quick call shown for MEKO_MIN_SHOW_MS from its start, then reports the change once', () => {
    const { h, changes, tick } = hold();
    expect(h.active('s1')).toBe(false);
    h.started('s1');
    expect(h.active('s1')).toBe(true);
    tick(MEKO_MIN_SHOW_MS - 1);
    expect([h.active('s1'), changes]).toEqual([true, []]);
    tick(1);
    expect([h.active('s1'), changes]).toEqual([false, ['s1']]);
    tick(MEKO_MIN_SHOW_MS);
    expect(changes).toEqual(['s1']);
  });

  it('moves the end on for a later start, with one report, and per session', () => {
    const { h, changes, tick } = hold();
    h.started('s1');
    tick(1000);
    h.started('s1');
    h.started('s2');
    tick(MEKO_MIN_SHOW_MS - 1);
    expect([h.active('s1'), h.active('s2'), changes]).toEqual([true, true, []]);
    tick(1);
    expect([h.active('s1'), h.active('s2')]).toEqual([false, false]);
    expect(changes.sort()).toEqual(['s1', 's2']);
  });

  it('drops a hold at once on clear (the turn ended), without a report', () => {
    const { h, changes, tick } = hold();
    h.started('s1');
    h.clear('s1');
    h.clear('s1');
    expect(h.active('s1')).toBe(false);
    tick(MEKO_MIN_SHOW_MS * 2);
    expect(changes).toEqual([]);
  });
});
