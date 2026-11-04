import { describe, expect, it } from 'vitest';
import { nextSort, sortRows, type SortState } from '../../src/web/sort.js';

type K = 'name' | 'n';
const rows = [
  { name: 'beta', n: 2 },
  { name: 'Alpha', n: 10 },
  { name: 'gamma', n: 2 },
  { name: 'alpha 2', n: 1 },
];
const value = (r: (typeof rows)[number], k: K) => r[k];

describe('table sorting', () => {
  it('keeps the incoming order until a column is picked', () => {
    expect(sortRows(rows, { key: null, dir: 'desc' }, value)).toBe(rows);
  });

  it('sorts numbers and text both ways, keeping ties in order', () => {
    const by = (s: SortState<K>) => sortRows(rows, s, value).map((r) => r.name);
    expect(by({ key: 'n', dir: 'desc' })).toEqual(['Alpha', 'beta', 'gamma', 'alpha 2']);
    expect(by({ key: 'n', dir: 'asc' })).toEqual(['alpha 2', 'beta', 'gamma', 'Alpha']);
    expect(by({ key: 'name', dir: 'asc' })).toEqual(['Alpha', 'alpha 2', 'beta', 'gamma']);
    expect(by({ key: 'name', dir: 'desc' })).toEqual(['gamma', 'beta', 'alpha 2', 'Alpha']);
  });

  it('starts a new column largest first (A to Z for text) and flips on a second tap', () => {
    const start: SortState<K> = { key: null, dir: 'desc' };
    expect(nextSort(start, 'n', false)).toEqual({ key: 'n', dir: 'desc' });
    expect(nextSort(start, 'name', true)).toEqual({ key: 'name', dir: 'asc' });
    expect(nextSort({ key: 'n', dir: 'desc' }, 'n', false)).toEqual({ key: 'n', dir: 'asc' });
    expect(nextSort({ key: 'n', dir: 'asc' }, 'name', true)).toEqual({ key: 'name', dir: 'asc' });
  });
});
