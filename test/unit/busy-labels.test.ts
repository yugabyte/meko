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

  it('cycle the dots with opacity, only when motion is allowed', () => {
    const css = STYLES.slice(STYLES.indexOf('.dots {'), STYLES.indexOf('.dot.starting {'));
    expect(css).toContain('@media (prefers-reduced-motion: no-preference)');
    expect(css).toMatch(/@keyframes dots-2 \{\s*0% \{ opacity: 0; \}\s*33\.333% \{ opacity: 1; \}/);
    expect(css).toMatch(/@keyframes dots-3 \{\s*0% \{ opacity: 0; \}\s*66\.667% \{ opacity: 1; \}/);
    // Safari doesn't animate `content`, so dots drawn that way never move on an iPhone.
    expect(css).not.toMatch(/content:/);
  });
});
