import { describe, expect, it } from 'vitest';
import { spinRow } from '../../src/server/tty.js';

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
});
