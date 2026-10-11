import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { TUNE, noteTimes, typingKeyframes, typingLoopMs } from '../../src/web/typing';

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

  it.each([1, 2, 3])('press key %i for each of its notes in the tune, without moving the text', (key) => {
    // The tune is played from typing.ts (Dots skips it with reduced motion), never from the CSS.
    expect(STYLES.slice(STYLES.indexOf('.dots {'), STYLES.indexOf('.dot.starting {'))).not.toMatch(/animation/);
    const frames = typingKeyframes(TUNE, key);
    // Only transform and box-shadow move, so the label's width never changes.
    expect(new Set(frames.flatMap((f) => Object.keys(f)))).toEqual(new Set(['offset', 'transform', 'boxShadow']));
    const offsets = frames.map((f) => f.offset as number);
    expect(offsets).toEqual([...offsets].sort((a, b) => a - b));
    expect([offsets[0], offsets.at(-1)]).toEqual([0, 1]);
    expect([frames[0].transform, frames.at(-1)!.transform]).toEqual(['none', 'none']);
    // A press starts (at rest) at each of the key's notes and goes down; long notes stay down.
    const loop = typingLoopMs(TUNE);
    const notes = noteTimes(TUNE).filter((n) => n.key === key);
    const presses = frames.slice(1, -1);
    expect(presses.filter((f, i) => i % 4 === 0).map((f) => f.offset)).toEqual(notes.map((n) => n.at / loop));
    expect(presses.filter((f, i) => i % 4 === 1 || i % 4 === 2).every((f) => f.transform !== 'none')).toBe(true);
    expect(presses.filter((f, i) => i % 4 === 3).map((f) => f.offset)).toEqual(notes.map((n) => (n.at + n.ms) / loop));
  });
});
