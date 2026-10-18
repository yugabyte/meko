import { describe, expect, it } from 'vitest';
import { anchorComment, parsePatch, sameLine, showsLine } from '../../src/web/diff.js';

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
