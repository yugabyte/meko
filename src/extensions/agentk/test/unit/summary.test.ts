import { describe, expect, it } from 'vitest';
import { forMekoSummary, stopRowText, summaryRow } from '../../src/server/summary.js';

describe('summaryRow', () => {
  it("prints the row in Meko's format: two spaces, the mark, the title padded to 15, the text", () => {
    expect(summaryRow('✓', 'Presenter', 'started', false)).toBe('  ✓ Presenter       started');
    expect(summaryRow('✗', 'Kode', "didn't start", false)).toBe("  ✗ Kode            didn't start");
  });

  it('colors only the mark', () => {
    expect(summaryRow('!', 'Kode', 'starting', true)).toBe('  \x1b[1;33m!\x1b[0m Kode            starting');
  });

  it.each([
    [false, 0, true, "wasn't running"],
    [true, 0, true, 'stopped'],
    [true, 2, true, 'stopped; 2 session(s) resume at meko start'],
    [true, 1, false, 'stopped; 1 session(s) resume at meko kode serve'],
  ])('says where stopped sessions resume (stopped %s, %i sessions, services %s)', (stopped, n, services, text) => {
    expect(stopRowText(stopped, n, services)).toBe(text);
  });

  it('is asked for only by MEKO_SUMMARY=1', () => {
    expect(forMekoSummary({ MEKO_SUMMARY: '1' })).toBe(true);
    expect(forMekoSummary({})).toBe(false);
    expect(forMekoSummary({ MEKO_SUMMARY: '0' })).toBe(false);
  });
});
