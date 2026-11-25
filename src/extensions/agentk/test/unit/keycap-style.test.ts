import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

const css = readFileSync(join(import.meta.dirname, '../../src/web/styles.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');

/** The keys: checkboxes, and the buttons drawn like them (.btn and the stage bar's .gate). */
const KEYS = [
  {
    name: 'checkboxes',
    is: (sel: string) => sel.includes("input[type='checkbox']:not(.prose *)"),
    on: (sel: string) => sel.endsWith(':checked'),
  },
  {
    name: 'buttons',
    is: (sel: string) => /\.(btn|gate)(?![\w-])/.test(sel),
    on: (sel: string) => sel === ".btn:is(.on, [aria-pressed='true']):not(.ghost)",
  },
];
type Key = (typeof KEYS)[number];

/** The body of the `@media <query>` block that holds the keys' rules, from its `{` to its closing `}`. */
function mediaBlock(query: string, key: Key): string {
  const starts = [...css.matchAll(new RegExp(`@media ${query.replace(/[()]/g, '\\$&')}\\s*\\{`, 'g'))].map((m) => m.index! + m[0].length);
  const block = starts.map((start) => css.slice(start, css.indexOf('\n}\n', start))).find((b) => rulesOf(b).some(([sel]) => key.is(sel)));
  expect(block, `an @media ${query} block for the ${key.name}`).toBeDefined();
  return block!;
}

/** Each rule in some CSS, as [selector, body]. */
function rulesOf(text: string): [string, string][] {
  return [...text.matchAll(/([^{}]+)\{([^{}]*)\}/g)].map(([, sel, body]) => [sel!.trim(), body!]);
}

/** A key's top-level rules (outside any @media block). */
function keyRules(key: Key): [string, string][] {
  return rulesOf(css.replace(/@media[^{]*\{(?:[^{}]*\{[^{}]*\})*[^{}]*\}/g, '')).filter(([sel]) => key.is(sel));
}

/** The selectors of a rule's list. */
const selectors = (sel: string) => sel.split(/,\s*\n/).map((s) => s.trim());
const transformOf = (body: string) => /transform:\s*([^;]+);/.exec(body)?.[1]?.trim();
/** The rules that move the key: a transform other than none. */
const moving = (key: Key) => keyRules(key).filter(([, body]) => (transformOf(body) ?? 'none') !== 'none');

describe.each(KEYS)('$name drawn as keys', (key) => {
  it('show on by shape as well as color, and a press is one move down to that depth', () => {
    const rules = keyRules(key);
    // State by shape as well as color (REVIEW.md): on or checked, the key is down.
    const on = rules.find(([sel]) => key.on(sel));
    expect(on && transformOf(on[1]), 'the on rule moves the key').toBeDefined();
    // Pressed goes straight to the on depth, so a press that turns the key on doesn't move it twice.
    const pressed = rules.filter(([sel]) => sel.includes(':active') && !sel.includes('::'));
    expect(pressed.length).toBeGreaterThan(0);
    for (const [sel, body] of pressed) expect(transformOf(body), sel).toBe(transformOf(on![1]));
  });

  it('keep the spot they left while down, so the pointer never slips off the key', () => {
    const hitAreas = keyRules(key).filter(([, body]) => /content:/.test(body) && /position:\s*absolute/.test(body));
    const covered = hitAreas.flatMap(([sel]) => selectors(sel));
    for (const [sel] of moving(key)) expect(covered, sel).toContain(`${sel}::before`);
  });

  it('move only without reduced motion', () => {
    expect(
      keyRules(key)
        .map(([, body]) => body)
        .join(''),
    ).not.toMatch(/transition/);
    expect(
      rulesOf(mediaBlock('(prefers-reduced-motion: no-preference)', key))
        .filter(([sel]) => selectors(sel).some(key.is))
        .map(([, body]) => body)
        .join(''),
    ).toMatch(/transition/);
  });

  it('hold still under forced colors', () => {
    const block = mediaBlock('(forced-colors: active)', key);
    // Each rule that moves the key is reset with its own selector: a lower-specificity reset loses.
    const resets = rulesOf(block)
      .filter(([, body]) => /transform:\s*none/.test(body))
      .flatMap(([sel]) => selectors(sel));
    expect(moving(key).length).toBeGreaterThan(0);
    for (const [sel] of moving(key)) expect(resets, sel).toContain(sel);
  });
});

describe('checkboxes under forced colors', () => {
  it('fall back to the native box', () => {
    const block = mediaBlock('(forced-colors: active)', KEYS[0]!);
    expect(block).toMatch(/appearance:\s*auto/);
    expect(block).toMatch(/::after\s*\{\s*content:\s*none/);
  });
});
