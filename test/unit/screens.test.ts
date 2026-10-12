import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { ScreenBoard, type Screen } from '../../src/server/screens.js';
import type { ScreenState } from '../../src/shared/types.js';

type Fake = Screen & { told: ScreenState[] };
const screen = (id: string, browser = ''): Fake => {
  const told: ScreenState[] = [];
  return { id, browser, name: id.toUpperCase(), told, tell: (s) => void told.push(s) };
};
const wait = (ms: number) => vi.advanceTimersByTime(ms);

describe('one screen per session', () => {
  beforeEach(() => void vi.useFakeTimers());
  afterEach(() => void vi.useRealTimers());

  // [what happens, the steps, each screen's last state (the fields given)]
  it.each<[string, (b: ScreenBoard, s: Record<'a' | 'b' | 'c', Fake>) => void, Partial<Record<'a' | 'b' | 'c', Partial<ScreenState>>>]>([
    ['the first screen gets it; one that connects without showing it sees it elsewhere', (b, { a, b: s }) => {
      b.join(a, false);
      b.join(s, false);
    }, { a: { state: 'here' }, b: { state: 'elsewhere', other: { name: 'A', sameBrowser: false } } }],
    ['a reconnect from the same page keeps it', (b, { a, b: s, c }) => {
      b.join(a, true);
      b.join(s, false);
      b.leave(a);
      b.join(Object.assign(c, { id: 'a' }), false);
    }, { b: { state: 'elsewhere', other: { name: 'C', sameBrowser: false } }, c: { state: 'here' } }],
    ['asking while the other screen was used 3 s ago waits 7 s', (b, { a, b: s }) => {
      b.join(a, true);
      wait(20_000);
      b.used(a);
      wait(3_000);
      b.join(s, true);
      expect(s.told.at(-1)).toEqual({ state: 'waiting', other: { name: 'A', sameBrowser: false }, moveInMs: 7_000 });
      expect(a.told.at(-1)).toEqual({ state: 'here', waiting: { name: 'B', sameBrowser: false } });
      wait(6_999);
      expect(s.told.at(-1)?.state).toBe('waiting');
      wait(1);
    }, { a: { state: 'elsewhere', other: { name: 'B', sameBrowser: false } }, b: { state: 'here' } }],
    ['each use restarts the wait', (b, { a, b: s }) => {
      b.join(a, true);
      b.join(s, true);
      wait(5_000);
      b.used(a);
      wait(9_000);
      expect(s.told.at(-1)?.state).toBe('waiting');
      wait(1_000);
    }, { a: { state: 'elsewhere' }, b: { state: 'here' } }],
    ['the other screen idle 10 s: it moves at once', (b, { a, b: s }) => {
      b.join(a, true);
      wait(10_000);
      b.join(s, true);
    }, { a: { state: 'elsewhere' }, b: { state: 'here' } }],
    ['the other screen not showing it: it moves at once, and hiding hands it to a waiting screen', (b, { a, b: s, c }) => {
      b.join(a, true);
      b.hide(a);
      b.join(s, true);
      b.join(c, true);
      b.hide(s);
    }, { a: { state: 'elsewhere', other: { name: 'C', sameBrowser: false } }, b: { state: 'elsewhere' }, c: { state: 'here' } }],
    ['Move to this screen takes it at once', (b, { a, b: s }) => {
      b.join(a, true);
      b.used(a);
      b.join(s, false);
      b.move(s);
    }, { a: { state: 'elsewhere', other: { name: 'B', sameBrowser: false } }, b: { state: 'here' } }],
    ['a waiting screen that hides stops waiting', (b, { a, b: s }) => {
      b.join(a, true);
      b.join(s, true);
      b.hide(s);
      wait(30_000);
    }, { a: { state: 'here' }, b: { state: 'elsewhere', other: { name: 'A', sameBrowser: false } } }],
    ['a later request replaces an earlier one', (b, { a, b: s, c }) => {
      b.join(a, true);
      b.join(s, true);
      b.join(c, true);
    }, { a: { state: 'here', waiting: { name: 'C', sameBrowser: false } }, b: { state: 'elsewhere' }, c: { state: 'waiting' } }],
    ['the screen that has it closes: a waiting screen gets it at once', (b, { a, b: s }) => {
      b.join(a, true);
      b.join(s, true);
      b.leave(a);
    }, { b: { state: 'here' } }],
    ['it closes with no one waiting: the others hear it closed, and the next to ask gets it', (b, { a, b: s, c }) => {
      b.join(a, true);
      b.join(s, false);
      b.join(c, false);
      b.leave(a);
      expect(s.told.at(-1)).toEqual({ state: 'elsewhere', other: null });
      b.show(c);
    }, { b: { state: 'elsewhere', other: { name: 'C', sameBrowser: false } }, c: { state: 'here' } }],
    ['another tab in the same browser is named as one', (b, { a, b: s }) => {
      Object.assign(a, { browser: 'x' });
      Object.assign(s, { browser: 'x' });
      b.join(a, true);
      b.join(s, false);
    }, { b: { state: 'elsewhere', other: { name: 'A', sameBrowser: true } } }],
  ])('%s', (_what, steps, want) => {
    const s = { a: screen('a'), b: screen('b'), c: screen('c') };
    const board = new ScreenBoard();
    steps(board, s);
    for (const [k, state] of Object.entries(want)) expect(s[k as 'a'].told.at(-1), k).toMatchObject(state);
  });
});
