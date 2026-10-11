import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

const WEB = join(import.meta.dirname, '../../src/web');
const STYLES = readFileSync(join(WEB, 'styles.css'), 'utf8');
/** The UI's text: the components, and the terminal's connection messages. */
const SOURCES = [...readdirSync(join(WEB, 'components')).filter((f) => f.endsWith('.tsx')).map((f) => `components/${f}`), 'terminals.ts'];

describe('in-progress labels', () => {
  it('animate their dots instead of a static "…", which looks stuck', () => {
    // "Starting…", "Signing in…", "Loading datapacks…" and the like render as <Busy label="Signing in" />.
    // A "…" after a word ending in -ing is taken for one; "Open PR…" or a placeholder's "…" isn't.
    const found: string[] = [];
    for (const file of SOURCES) {
      readFileSync(join(WEB, file), 'utf8')
        .split('\n')
        .forEach((line, i) => {
          if (/[a-z]ing\b[^"`<>{}?:]*…/i.test(line) && !/^\s*(\/\/|\/?\*)/.test(line)) found.push(`${file}:${i + 1}: ${line.trim()}`);
        });
    }
    expect(found).toEqual([]);
  });

  it('press their keys in turn, only when motion is allowed and without moving the text', () => {
    const css = STYLES.slice(STYLES.indexOf('.dots {'), STYLES.indexOf('.dot.starting {'));
    const motion = '@media (prefers-reduced-motion: no-preference)';
    expect(css).toContain(motion);
    expect(css.slice(0, css.indexOf(motion))).not.toMatch(/animation/);
    // Typing, not a wave: key 1, then 3, then 2.
    const delay = (n: number) => Number(new RegExp(`\\.key:nth-child\\(${n}\\) \\{\\s*animation-delay: ([\\d.]+)s`).exec(css)?.[1]);
    expect(delay(3)).toBeGreaterThan(0);
    expect(delay(2)).toBeGreaterThan(delay(3));
    // Only transform and box-shadow move, so the label's width never changes.
    const frames = css.slice(css.indexOf('@keyframes key-press'));
    expect([...frames.matchAll(/([a-z-]+):/g)].map((m) => m[1]).filter((p) => p !== 'transform' && p !== 'box-shadow')).toEqual([]);
  });
});
