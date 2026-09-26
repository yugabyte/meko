// Column sorting for tables. Pure, so it's unit-tested.

export type SortDir = 'asc' | 'desc';

export interface SortState<K extends string> {
  /** null keeps the rows in the order they came in. */
  key: K | null;
  dir: SortDir;
}

/**
 * The state after tapping a column: the same column flips direction; a new one starts with the
 * biggest numbers first, or A to Z for text.
 */
export function nextSort<K extends string>(cur: SortState<K>, key: K, text: boolean): SortState<K> {
  if (cur.key === key) return { key, dir: cur.dir === 'asc' ? 'desc' : 'asc' };
  return { key, dir: text ? 'asc' : 'desc' };
}

/** Sorted copy of `rows`. Ties keep their incoming order; text compares case-insensitively. */
export function sortRows<T, K extends string>(rows: T[], s: SortState<K>, value: (row: T, key: K) => number | string): T[] {
  const { key } = s;
  if (key === null) return rows;
  const sign = s.dir === 'asc' ? 1 : -1;
  return rows
    .map((row, i) => ({ row, i, v: value(row, key) }))
    .sort((a, b) => {
      const c =
        typeof a.v === 'string' && typeof b.v === 'string'
          ? a.v.localeCompare(b.v, undefined, { sensitivity: 'base', numeric: true })
          : Number(a.v) - Number(b.v);
      return c * sign || a.i - b.i;
    })
    .map((x) => x.row);
}
