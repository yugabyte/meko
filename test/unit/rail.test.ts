import { afterEach, describe, expect, it, vi } from 'vitest';
import { railCollapsedSaved, railSessionsSaved, railWidthSaved, saveRailSessions } from '../../src/web/rail.js';

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
