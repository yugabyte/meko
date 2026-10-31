import { describe, expect, it } from 'vitest';
import { anchorComment, defaultOpen, DESKTOP_BUDGET, keepUnchanged, parsePatch, pathFilter, PHONE_BUDGET, pickComment, pickRange, reparse, sameLine, showsLine, type RowBudget } from '../../src/web/diff.js';

const rows = (patch: string) => parsePatch(patch).map((l) => [l.kind, l.old, l.new, l.text]);

describe('parsing patches', () => {
  it('reads an empty line inside a hunk as an empty context line', () => {
    // With diff.suppressBlankEmpty, git writes a blank context line as "" instead of " ".
    const bare = rows(['@@ -1,4 +1,4 @@', ' a', '', '-b', '+B', ' c', ''].join('\n'));
    expect(bare).toEqual([
      ['hunk', null, null, '@@ -1,4 +1,4 @@'],
      ['ctx', 1, 1, 'a'],
      ['ctx', 2, 2, ''],
      ['del', 3, null, 'b'],
      ['add', null, 3, 'B'],
      ['ctx', 4, 4, 'c'],
    ]);
    expect(rows(['@@ -1,4 +1,4 @@', ' a', ' ', '-b', '+B', ' c', ''].join('\n'))).toEqual(bare);
  });

  it('numbers later hunks right after blank context lines', () => {
    const l = parsePatch(['@@ -1,2 +1,2 @@', '', '-x', '+y', '', '@@ -10 +10 @@', '-p', '+q', ''].join('\n'));
    expect(l.map((x) => [x.kind, x.old, x.new])).toEqual([
      ['hunk', null, null],
      ['ctx', 1, 1],
      ['del', 2, null],
      ['add', null, 2],
      ['hunk', null, null],
      ['del', 10, null],
      ['add', null, 10],
    ]);
  });

  it('keeps an empty line at the end of a hunk when the counts include it', () => {
    const l = parsePatch(['@@ -1,2 +1,3 @@', ' a', '+b', ''].join('\n'));
    expect(l.map((x) => [x.kind, x.old, x.new, x.text])).toEqual([
      ['hunk', null, null, '@@ -1,2 +1,3 @@'],
      ['ctx', 1, 1, 'a'],
      ['add', null, 2, 'b'],
      ['ctx', 2, 3, ''],
    ]);
  });
});

describe('matching excerpt lines', () => {
  const long = 'x'.repeat(150) + 'y'.repeat(4850);

  it('matches short lines exactly only', () => {
    expect(sameLine('}', '}')).toBe(true);
    expect(sameLine('  }', '}')).toBe(false);
    expect(sameLine('return a;', 'return')).toBe(false);
  });

  it('matches a long line the server cut short at either end', () => {
    expect(sameLine(long, long.slice(-4000))).toBe(true);
    expect(sameLine(long, long.slice(0, 4000))).toBe(true);
    expect(sameLine(long, `${long.slice(0, 3999)}…`)).toBe(true);
    expect(sameLine(long, `…${long.slice(-3999)}`)).toBe(true);
    expect(sameLine(long, 'z'.repeat(4000))).toBe(false);
  });

  it('finds a comment whose stored lines were cut short after its line moved', () => {
    const lines = parsePatch(['@@ -1,1 +1,2 @@', ' a', `+${long}`].join('\n'));
    expect(anchorComment(lines, { side: 'new', endLine: 9, excerpt: `a\n${long.slice(0, 4000)}` })).toBe(2);
  });
});

describe('telling changed lines from hidden ones', () => {
  const lines = parsePatch(['@@ -5,2 +5,2 @@', ' keep', '-old', '+new'].join('\n'));

  it('knows which line numbers the diff shows on each side', () => {
    expect(showsLine(lines, 'new', 6)).toBe(true);
    expect(showsLine(lines, 'old', 6)).toBe(true);
    expect(showsLine(lines, 'new', 5)).toBe(true);
    expect(showsLine(lines, 'old', 5)).toBe(false); // context lines are commented on the new side
    expect(showsLine(lines, 'new', 40)).toBe(false);
  });
});

describe('picking the lines a comment covers', () => {
  // New 1-5 and removed 3, 4 and 6 (an unchanged line 5 between them) in one hunk; new 20-21 and removed 21 in another.
  const lines = parsePatch(['@@ -1,6 +1,5 @@', ' a', ' b', '-c', '-d', '+C', '+D', ' e', '-f', '@@ -20,2 +20,2 @@', ' x', '-z', '+y'].join('\n'));

  it.each<[string, 'old' | 'new', number, number, [number, number, string] | null]>([
    ['one line', 'new', 2, 2, [2, 2, 'b']],
    ['the lines between two, picked downward', 'new', 1, 4, [1, 4, 'abCD']],
    ['the lines between two, picked upward', 'new', 4, 1, [1, 4, 'abCD']],
    ['added lines past removed ones', 'new', 2, 3, [2, 3, 'bC']],
    ['removed lines past an unchanged one, which they leave out', 'old', 3, 6, [3, 6, 'cdf']],
    ["new lines up to the hunk's edge", 'new', 2, 21, [2, 5, 'bCDe']],
    ["new lines back to the hunk's edge", 'new', 21, 1, [20, 21, 'xy']],
    ["removed lines up to the hunk's edge", 'old', 3, 21, [3, 6, 'cdf']],
    ["removed lines back to the hunk's edge", 'old', 21, 3, [21, 21, 'z']],
    ['nothing from a line the diff doesn\'t show', 'new', 10, 2, null],
    ['nothing from an unchanged line on the old side', 'old', 1, 1, null],
  ])('covers %s', (_, side, anchor, n, want) => {
    const r = pickRange(lines, side, anchor, n);
    expect(r && [r.from, r.to, r.rows.map((l) => l.text).join('')]).toEqual(want);
  });

  it.each<[string, number, number | null, [string, number, number, string, number] | null]>([
    ['its own line', 3, null, ['old', 3, 3, 'c', 3]],
    ['a range from the anchor, which a later shift-click extends from', 8, 3, ['old', 3, 6, 'c\nd\nf', 3]],
    ['its own line when the diff no longer shows the anchor', 2, 9, ['new', 2, 2, 'b', 2]],
    ['nothing on a hunk header', 0, null, null],
  ])('a + picks %s', (_, i, anchor, want) => {
    const c = pickComment(lines, i, anchor);
    expect(c && [c.side, c.line, c.endLine, c.excerpt, c.anchor]).toEqual(want);
  });
});

describe('opening diffs by default', () => {
  const images = (n: number): [string, number, boolean][] => Array.from({ length: n }, (_, i) => [`${i}.png`, 0, false]);
  // Each file as [path, rows, has comments]; the files named gone* are deleted.
  it.each<[string, RowBudget, [string, number, boolean][], string[]]>([
    ['keeps deleted files closed, and leaves their rows to the next files', DESKTOP_BUDGET, [['gone', 1000, false], ['a', 1000, false], ['b', 1000, false]], ['a', 'b']],
    ['opens a deleted file with comments, and counts its rows like any open file', DESKTOP_BUDGET, [['gone', 1000, true], ['a', 1000, false], ['b', 1000, false]], ['gone', 'a']],
    ['opens small files while they fit the budget', DESKTOP_BUDGET, [['a', 900, false], ['b', 900, false], ['c', 300, false], ['d', 100, false]], ['a', 'b', 'd']],
    ['never opens a large file, even first', DESKTOP_BUDGET, [['big', 1001, false], ['a', 10, false]], ['a']],
    ['opens files with comments, large ones included, and counts only small ones', DESKTOP_BUDGET, [['big', 50000, true], ['a', 1000, false], ['b', 1000, true], ['c', 1, false]], ['big', 'a', 'b']],
    ['lets files without rows take a slot, so a branch of images opens only the first ones', DESKTOP_BUDGET, [['a', 1000, false], ['b', 1000, false], ['img.png', 0, false]], ['a', 'b']],
    ['opens only as many files without rows as the budget has rows', DESKTOP_BUDGET, images(2001), images(2000).map(([p]) => p)],
    ['opens half as many rows on a phone', PHONE_BUDGET, [['a', 450, false], ['b', 450, false], ['c', 450, false], ['d', 100, false]], ['a', 'b', 'd']],
    ['counts a file over 500 rows as large on a phone', PHONE_BUDGET, [['a', 501, false], ['b', 500, false]], ['b']],
  ])('%s', (_, budget, files, open) => {
    const by = new Map(files.map(([path, n, c]) => [path, { n, c }]));
    const kinds = files.map(([path]) => ({ path, kind: path.startsWith('gone') ? 'deleted' as const : 'modified' as const }));
    expect([...defaultOpen(kinds, (p) => by.get(p)!.n, (p) => by.get(p)!.c, budget)]).toEqual(open);
  });
});

describe('parsing again after a refetch', () => {
  it('keeps the rows of patches that did not change and parses the rest', () => {
    const a = '@@ -1 +1 @@\n-a\n+A', b = '@@ -1 +1 @@\n-b\n+B';
    const first = reparse(new Map(), [{ patch: a }, { patch: b }, { patch: null }]);
    const next = reparse(first, [{ patch: a }, { patch: `${b}\n+C` }]);
    expect(next.get(a)).toBe(first.get(a));
    expect(next.get(`${b}\n+C`)?.map((l) => l.text)).toEqual(['@@ -1 +1 @@', 'b', 'B', 'C']);
    expect(next.has(b)).toBe(false);
  });

  it('keeps the objects of files whose fields are all the same', () => {
    const prev = [{ path: 'a', added: 1, patch: 'x' }, { path: 'b', added: 1, patch: 'y' }];
    const next = keepUnchanged(prev, [{ path: 'a', added: 1, patch: 'x' }, { path: 'b', added: 2, patch: 'y' }, { path: 'c', added: 1, patch: 'z' }], (f) => f.path);
    expect(next[0]).toBe(prev[0]);
    expect(next.slice(1)).toEqual([{ path: 'b', added: 2, patch: 'y' }, { path: 'c', added: 1, patch: 'z' }]);
    expect(next[1]).not.toBe(prev[1]);
  });
});

describe('filtering the Files menu', () => {
  const files = [
    { path: 'src/web/diff.ts', oldPath: null },
    { path: 'src/web/App.tsx', oldPath: null },
    { path: 'docs/c++/notes.md', oldPath: null },
    { path: 'src/pages/[id].tsx', oldPath: null },
    { path: 'test/unit/new.test.ts', oldPath: 'test/old.test.ts' },
  ];
  const shown = (q: string) => files.filter(pathFilter(q)).map((f) => f.path);

  it('takes a regular expression, ignoring case', () => {
    expect(shown('\\.tsx?$')).toEqual(['src/web/diff.ts', 'src/web/App.tsx', 'src/pages/[id].tsx', 'test/unit/new.test.ts']);
    expect(shown('^src/.*app')).toEqual(['src/web/App.tsx']);
    expect(shown('diff|notes')).toEqual(['src/web/diff.ts', 'docs/c++/notes.md']);
  });

  it('finds a name with regex characters in it as typed', () => {
    expect(shown('[ID].tsx')).toEqual(['src/pages/[id].tsx']);
  });

  it('matches a renamed file by its old path too', () => {
    expect(shown('^test/old')).toEqual(['test/unit/new.test.ts']);
  });

  it('filters by the longest start that is a valid expression while it is not one', () => {
    expect(shown('WEB/(')).toEqual(['src/web/diff.ts', 'src/web/App.tsx']);
    expect(shown('(diff')).toEqual(files.map((f) => f.path));
    expect(shown('c++')).toContain('docs/c++/notes.md');
  });
});
