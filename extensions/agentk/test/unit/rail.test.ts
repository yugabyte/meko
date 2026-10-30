import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { railCollapsedSaved, railHiddenSaved, railSessionsSaved, railWidthSaved, saveRailSessions, sessionTabsShown } from '../../src/web/rail.js';
import { kiroPlanLines } from '../../src/web/util.js';
import type { KiroPlan } from '../../src/shared/types.js';

const STYLES = readFileSync(join(import.meta.dirname, '../../src/web/styles.css'), 'utf8');

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

describe("a rail session's name", () => {
  // A task with a local review (its row joined with the magnifier) once cut its name with an ellipsis.
  it('wraps in every row, never cut', () => {
    // Innermost rules only (no `{` in a body), so one nested in an @media block is checked too.
    const rules = [...STYLES.replace(/\/\*[\s\S]*?\*\//g, '').matchAll(/([^{}]+)\{([^{}]*)\}/g)].filter(([, sel]) => sel!.includes('.rail-sess-name'));
    expect(rules.map(([, sel]) => sel!.trim())).toContain('.rail-sess-name');
    for (const [, sel, body] of rules) expect(body, sel!.trim()).not.toMatch(/nowrap|ellipsis|overflow:\s*hidden/);
    expect(STYLES.slice(STYLES.indexOf('\n.rail-sess-name {')).split('}')[0]).toContain('overflow-wrap: anywhere;');
  });
});

describe("Kiro's plan lines", () => {
  const plan = (over: KiroPlan['overage'], bonus: KiroPlan['bonus'] = null): Pick<KiroPlan, 'bonus' | 'overage'> => ({ bonus, overage: over });

  it.each<[string, Pick<KiroPlan, 'bonus' | 'overage'>, { extra: string[]; said: string[] }]>([
    // The one form seen (`Overages: Disabled`): on/off read, no credits, so `extra` says it all and the quote goes.
    ['overage off: stated, not quoted', plan({ credits: null, enabled: false, line: 'Overages: Disabled' }), { extra: ['Overage off'], said: [] }],
    // Credits read too: `extra` shows only the credits, so the on/off and the rest stay in the quote (the format is unseen).
    ['overage with credits: stated and still quoted', plan({ credits: 14.2, enabled: true, line: 'Overages: 14.20 credits ($0.57), enabled' }), { extra: ['Overage 14.2 billed'], said: ['Overages: 14.20 credits ($0.57), enabled'] }],
    // Nothing read: only the quote carries it.
    ['overage unread: quoted only', plan({ credits: null, enabled: null, line: 'Overage: something new' }), { extra: [], said: ['Overage: something new'] }],
    // Bonus format is unseen, so it's always quoted.
    ['bonus: stated and quoted', plan(null, { used: 380, total: 500, expires: '2026-11-30', line: 'Bonus credits: 380.00/500 (expires 2026-11-30)' }), { extra: ['Bonus 120 of 500 left'], said: ['Bonus credits: 380.00/500 (expires 2026-11-30)'] }],
    ['neither: no lines', plan(null, null), { extra: [], said: [] }],
  ])('%s', (_, input, expected) => {
    expect(kiroPlanLines(input)).toEqual(expected);
  });
});
