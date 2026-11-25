import { describe, expect, it } from 'vitest';
import { blueCode, spinRow } from '../../src/server/tty.js';

describe("a running step's row", () => {
  const doing = "Running Meko's setup for Claude Code (@yugabytedb/meko-mcp 2.5.1 from npm)";

  it('is the frame, the padded label and what it does, whole when it fits or the width is unknown', () => {
    expect(spinRow('⣾', 'Meko in Claude', 'Running', 80)).toBe('  ⣾ Meko in Claude Running');
    expect(spinRow('⣾', 'Meko', doing)).toBe(`  ⣾ Meko          ${doing}`);
  });

  it('is cut short of the last column, so it never wraps into a line per frame', () => {
    const row = spinRow('⣾', 'Meko in Claude', doing, 80);
    expect([...row].length).toBe(79);
    expect(row.endsWith('…')).toBe(true);
    expect(row.startsWith("  ⣾ Meko in Claude Running Meko's setup")).toBe(true);
    expect([...spinRow('⣾', 'Meko in Claude', 'x'.repeat(61), 80)].length).toBe(79);
    expect(spinRow('⣾', 'Meko in Claude', 'x'.repeat(60), 80)).toBe(`  ⣾ Meko in Claude ${'x'.repeat(60)}`);
  });

  it('has blue dots when colored, as Meko draws them, and is cut by what shows', () => {
    expect(spinRow('⣾', 'Meko', 'Running', 80, true)).toBe(`  \x1b[${blueCode()}m⣾\x1b[0m Meko          Running`);
    const row = spinRow('⣾', 'Meko in Claude', doing, 80, true);
    expect(row.startsWith(`  \x1b[${blueCode()}m⣾\x1b[0m Meko in Claude Running Meko's setup`)).toBe(true);
    expect([...row.replace(`\x1b[${blueCode()}m`, '').replace('\x1b[0m', '')].length).toBe(79);
  });
});

// The blue is Meko's, for the background Meko's installer passes on (MEKO_TERM_BG), else COLORFGBG,
// else dark, at the terminal's color depth.
describe('blueCode', () => {
  it('picks the blue for the background and the color depth', () => {
    expect(blueCode({ MEKO_TERM_BG: 'dark', COLORTERM: 'truecolor' })).toBe('38;2;121;174;255');
    expect(blueCode({ MEKO_TERM_BG: 'light', COLORTERM: '24bit' })).toBe('38;2;31;95;209');
    expect(blueCode({ MEKO_TERM_BG: 'light', TERM: 'xterm-256color' })).toBe('38;5;26');
    expect(blueCode({ COLORFGBG: '0;15', TERM: 'xterm-256color' })).toBe('38;5;26');
    expect(blueCode({ COLORFGBG: '15;0', TERM: 'xterm' })).toBe('38;5;111');
    expect(blueCode({ TERM: 'xterm' })).toBe('38;5;111');
    expect(blueCode({ TERM: 'linux' })).toBe('94');
    expect(blueCode({ MEKO_TERM_BG: 'light', TERM: 'vt100' })).toBe('34');
  });
});
