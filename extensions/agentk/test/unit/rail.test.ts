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

describe("a rail task's local review", () => {
  // Every rule in the file, in order, with the @media it sits in ('' outside one).
  const css = STYLES.replace(/\/\*[\s\S]*?\*\//g, '');
  const rules: { media: string; sel: string; body: string; at: number }[] = [];
  for (const m of css.matchAll(/@media([^{]+)\{((?:[^{}]*\{[^{}]*\})*[^{}]*)\}|@[^{]+\{(?:[^{}]*\{[^{}]*\})*[^{}]*\}|([^{}@]+)\{([^{}]*)\}/g)) {
    if (m[1]) for (const r of m[2]!.matchAll(/([^{}]+)\{([^{}]*)\}/g)) rules.push({ media: m[1].trim(), sel: r[1]!.trim(), body: r[2]!, at: m.index + r.index });
    else if (m[3]) rules.push({ media: '', sel: m[3].trim(), body: m[4]!, at: m.index });
  }
  // A property's value on an element with these classes, where the given media match: the last
  // declaration of the most specific selector whose subject has no other class (states left out).
  const value = (classes: string[], prop: string, media: string[]) => {
    let best: { spec: number; at: number; v: string } | null = null;
    for (const r of rules) {
      if (r.media && !media.includes(r.media)) continue;
      const v = r.body.match(new RegExp(`(?:^|;)\\s*${prop}:\\s*([^;]+)`))?.[1]?.trim();
      if (!v) continue;
      for (const part of r.sel.split(',')) {
        if (/:(hover|focus|active|disabled)|\.(sel|stopped|running|waiting|meko)\b/.test(part)) continue;
        const subject = part.trim().split(/\s*[\s>+~]\s*/).pop()!;
        const own = [...subject.matchAll(/\.([\w-]+)/g)].map((c) => c[1]!);
        if (!own.length || !own.every((c) => classes.includes(c)) || !/^\.[\w.-]+$/.test(subject)) continue;
        const spec = (part.match(/\./g) ?? []).length;
        if (!best || spec > best.spec || (spec === best.spec && r.at >= best.at)) best = { spec, at: r.at, v };
      }
    }
    return best?.v;
  };
  const px = (v: string | undefined) => (v ? parseFloat(v) : 0);
  // The box's height: its line or its glass, whichever is taller, and its padding, at least its min-height.
  const boxHeight = (media: string[]) => {
    const box = ['review-seg', 'rail-review'];
    const pad = (value(box, 'padding', media) ?? '0').split(/\s+/).map(px);
    const line = value(box, 'line-height', media);
    const font = px(value(box, 'font-size', media));
    const lineHeight = line?.endsWith('px') ? px(line) : font * (line ? parseFloat(line) : 1.45);
    // The glass is 16 px (the SVG's own size) times any zoom a rule on .rail-review .review-glass gives it.
    const zoom = rules.filter((r) => !r.media && r.sel === '.rail-review .review-glass').map((r) => parseFloat(r.body.match(/zoom:\s*([\d.]+)/)?.[1] ?? '1'))[0] ?? 1;
    const height = Math.max(lineHeight, 16 * zoom) + pad[0]! + (pad[2] ?? pad[0]!);
    return Math.max(height, px(value(box, 'min-height', media)));
  };

  // Its box once took 23.4 px (a 17.4 px line, 12 px at 1.45, and 6 px of padding) under a 4 px gap.
  it('is no taller than its 13 px magnifier', () => {
    expect(boxHeight([])).toBe(13);
  });

  it("keeps that height on a touch screen, where a tab's part is a finger's size", () => {
    expect(boxHeight(['(pointer: coarse)'])).toBe(13);
    expect(value(['review-seg', 'tab-review'], 'min-height', ['(pointer: coarse)'])).toBe('36px');
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
