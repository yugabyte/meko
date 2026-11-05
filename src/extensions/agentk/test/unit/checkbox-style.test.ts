import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

const css = readFileSync(join(import.meta.dirname, '../../src/web/styles.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, '');

/** The body of the `@media <query>` block that holds the keycap's rules, from its `{` to its closing `}`. */
function mediaBlock(query: string): string {
  const starts = [...css.matchAll(new RegExp(`@media ${query.replace(/[()]/g, '\\$&')}\\s*\\{`, 'g'))].map((m) => m.index! + m[0].length);
  const block = starts.map((start) => css.slice(start, css.indexOf('\n}\n', start))).find((b) => b.includes(KEYCAP));
  expect(block, `an @media ${query} block for the keycap`).toBeDefined();
  return block!;
}

/** The keycap's top-level rules (outside any @media block), as [selector, body]. */
function keycapRules(): [string, string][] {
  const outside = css.replace(/@media[^{]*\{(?:[^{}]*\{[^{}]*\})*[^{}]*\}/g, '');
  return [...outside.matchAll(/([^{}]+)\{([^{}]*)\}/g)].filter(([, sel]) => sel!.includes(KEYCAP)).map(([, sel, body]) => [sel!.trim(), body!]);
}

const KEYCAP = "input[type='checkbox']:not(.prose *)";

describe('checkboxes', () => {
  it('show checked by shape as well as color, and move only without reduced motion', () => {
    const rules = keycapRules();
    // State by shape as well as color (REVIEW.md): checked, the key moves down.
    const checked = rules.find(([sel]) => sel.endsWith(':checked'));
    expect(checked?.[1]).toMatch(/transform:/);
    // Movement only for those who haven't asked for less.
    expect(rules.map(([, body]) => body).join('')).not.toMatch(/transition/);
    expect(mediaBlock('(prefers-reduced-motion: no-preference)')).toMatch(/transition/);
  });

  it('fall back to the native box, held still, under forced colors', () => {
    const block = mediaBlock('(forced-colors: active)');
    expect(block).toMatch(/appearance:\s*auto/);
    expect(block).toMatch(/::after\s*\{\s*content:\s*none/);
    // Each rule that moves the key is reset with its own selector: a lower-specificity reset loses.
    const resets = [...block.matchAll(/([^{}]+)\{([^{}]*)\}/g)].filter(([, , body]) => /transform:\s*none/.test(body!)).map(([, sel]) => sel!);
    const moving = keycapRules().filter(([, body]) => /transform:/.test(body));
    expect(moving.length).toBeGreaterThan(0);
    for (const [sel] of moving) expect(resets.some((r) => r.split(/,\s*\n/).map((s) => s.trim()).includes(sel)), sel).toBe(true);
  });
});
