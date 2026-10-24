import { afterEach, describe, expect, it, vi } from 'vitest';
import { railCollapsedSaved, railHiddenSaved, railSessionsSaved, railWidthSaved, saveRailSessions, sessionTabsShown } from '../../src/web/rail.js';
import { kiroPlanLines } from '../../src/web/util.js';
import type { KiroPlan } from '../../src/shared/types.js';

describe('projects pane', () => {
  afterEach(() => vi.unstubAllGlobals());

  it.each([
    [null, null],
    ['', null],
    ['junk', null],
    ['0', null],
    ['300', 300],
    ['300.6', 301],
    ['90', 180],
    ['2000', 480],
  ])('reads a stored %s as %s', (stored, width) => {
    vi.stubGlobal('localStorage', { getItem: () => stored });
    expect(railWidthSaved()).toBe(width);
  });

  it.each([
    [null, []],
    ['junk', []],
    ['{"a":1}', []],
    ['["p1", 7, "p2"]', ['p1', 'p2']],
  ])('reads stored folded projects %s as %o', (stored, ids) => {
    vi.stubGlobal('localStorage', { getItem: () => stored });
    expect([...railCollapsedSaved()]).toEqual(ids);
  });

  it.each([
    [null, false],
    ['0', false],
    ['1', true],
  ])('lists sessions in the rail for a stored %s: %s (off by default)', (stored, on) => {
    vi.stubGlobal('localStorage', { getItem: () => stored });
    expect(railSessionsSaved()).toBe(on);
  });

  it.each([
    [null, false],
    ['1', true],
  ])('hides the pane for a stored %s: %s (shown by default)', (stored, hidden) => {
    vi.stubGlobal('localStorage', { getItem: () => stored });
    expect(railHiddenSaved()).toBe(hidden);
  });

  it.each([
    [false, false, false, true],
    [false, false, true, true],
    [false, true, false, true],
    [false, true, true, true],
    [true, false, false, false],
    [true, true, false, true],
    [true, false, true, false],
    [true, true, true, false],
  ])('session tabs with rail sessions %s, pane hidden %s, phone %s: %s', (railSessions, railHidden, mobile, shown) => {
    expect(sessionTabsShown(railSessions, railHidden, mobile)).toBe(shown);
  });

  // Last: the save is kept in the module for the rest of the file.
  it('keeps the setting for this page when storage refuses it', () => {
    const refuse = () => {
      throw new Error('blocked');
    };
    vi.stubGlobal('localStorage', { getItem: refuse, setItem: refuse, removeItem: refuse });
    vi.stubGlobal('window', { dispatchEvent: () => true });
    saveRailSessions(true);
    expect(railSessionsSaved()).toBe(true);
  });
});

describe("Kiro's plan lines", () => {
  const plan = (over: KiroPlan['overage'], bonus: KiroPlan['bonus'] = null): Pick<KiroPlan, 'bonus' | 'overage'> => ({ bonus, overage: over });

  it('states overage off without quoting Kiro, since it read the line', () => {
    expect(kiroPlanLines(plan({ credits: null, enabled: false, line: 'Overages: Disabled' }))).toEqual({
      extra: ['Overage off'], said: [],
    });
  });

  it('states overage billed without quoting Kiro', () => {
    expect(kiroPlanLines(plan({ credits: 14.2, enabled: true, line: 'Overages: 14.20 credits ($0.57), enabled' }))).toEqual({
      extra: ['Overage 14.2 billed'], said: [],
    });
  });

  it("quotes Kiro's overage line only when it read nothing from it", () => {
    expect(kiroPlanLines(plan({ credits: null, enabled: null, line: 'Overage: something new' }))).toEqual({
      extra: [], said: ['Overage: something new'],
    });
  });

  it('keeps quoting the bonus line, whose format is still a guess', () => {
    expect(kiroPlanLines(plan(null, { used: 380, total: 500, expires: '2026-11-30', line: 'Bonus credits: 380.00/500 (expires 2026-11-30)' }))).toEqual({
      extra: ['Bonus 120 of 500 left'], said: ['Bonus credits: 380.00/500 (expires 2026-11-30)'],
    });
  });

  it('has no lines without a bonus or overage', () => {
    expect(kiroPlanLines(plan(null, null))).toEqual({ extra: [], said: [] });
  });
});
