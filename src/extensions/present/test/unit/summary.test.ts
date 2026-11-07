import { describe, expect, it } from 'vitest';
import { forMekoSummary, summaryRow } from '../../src/server/summary.js';

describe('summaryRow', () => {
  it("prints the row in Meko's format: two spaces, the mark, the title padded to 15, the text", () => {
    expect(summaryRow('✓', 'Presenter', 'started', false)).toBe('  ✓ Presenter       started');
    expect(summaryRow('✗', 'Kode', "didn't start", false)).toBe("  ✗ Kode            didn't start");
  });

  it('colors only the mark', () => {
    expect(summaryRow('!', 'Kode', 'starting', true)).toBe('  \x1b[1;33m!\x1b[0m Kode            starting');
  });

  it('is asked for only by MEKO_SUMMARY=1', () => {
    expect(forMekoSummary({ MEKO_SUMMARY: '1' })).toBe(true);
    expect(forMekoSummary({})).toBe(false);
    expect(forMekoSummary({ MEKO_SUMMARY: '0' })).toBe(false);
  });
});
