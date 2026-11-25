import { mkdirSync, mkdtempSync, readFileSync, rmSync, symlinkSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { OutsideRepoError, ReplaceError, replaceLines } from '../../src/server/changes.js';
import { excerptMatches, excerptWhole, hasSuggestion, splitSuggestions, suggestionBlock, suggestions } from '../../src/shared/suggestion.js';
import { Suggestion } from '../../src/web/components/ChangesPanel.js';
import { applyRange, parsePatch } from '../../src/web/diff.js';

describe('suggestion blocks', () => {
  it('wraps lines in a fence longer than any backticks in them', () => {
    expect(suggestionBlock('const a = 1;')).toBe('```suggestion\nconst a = 1;\n```');
    expect(suggestionBlock('a\r\nb\r')).toBe('```suggestion\na\nb\n```');
    expect(suggestionBlock('use ```js')).toBe('````suggestion\nuse ```js\n````');
    expect(suggestionBlock('')).toBe('```suggestion\n```');
  });

  it('reads the blocks back, between text, as their own lines', () => {
    const body = `Rename it.\n${suggestionBlock('const total = 1;\n  x();')}\nThanks.`;
    expect(splitSuggestions(body)).toEqual([
      { kind: 'text', text: 'Rename it.' },
      { kind: 'suggestion', lines: ['const total = 1;', '  x();'] },
      { kind: 'text', text: 'Thanks.' },
    ]);
    expect(suggestions(suggestionBlock('use ```js'))).toEqual([['use ```js']]);
    expect(suggestions('```suggestion\n```')).toEqual([[]]);
    expect(suggestions('```suggestion\r\na\r\n```\r\n')).toEqual([['a']]);
    expect(suggestions(`${suggestionBlock('a')}\n${suggestionBlock('b')}`)).toEqual([['a'], ['b']]);
  });

  it('leaves other code blocks, and an unclosed suggestion, as text', () => {
    expect(hasSuggestion('```ts\nconst a = 1;\n```')).toBe(false);
    expect(hasSuggestion('```suggestion\nnever closed')).toBe(false);
    // A shorter fence doesn't close a longer one.
    expect(hasSuggestion('````suggestion\na\n```')).toBe(false);
    expect(splitSuggestions('plain')).toEqual([{ kind: 'text', text: 'plain' }]);
  });
});

describe("a comment's stored excerpt, for a suggestion", () => {
  const cut = `${'x'.repeat(1000)}…`;

  it('is whole only with every line of the range, none cut short', () => {
    expect(excerptWhole('a\nb', 2)).toBe(true);
    expect(excerptWhole('l11\nl12', 12)).toBe(false); // the server kept the last lines only
    expect(excerptWhole(`a\n${cut}`, 2)).toBe(false);
  });

  it('matches the lines now while they end with it, a cut line by what is left of it', () => {
    expect(excerptMatches(['a', 'b'], 'a\nb')).toBe(true);
    expect(excerptMatches(['a\r', 'b'], 'a\nb\r')).toBe(true);
    expect(excerptMatches(['l1', 'l2', 'l3'], 'l2\nl3')).toBe(true);
    expect(excerptMatches(['a', `${'x'.repeat(1000)}yz`], `a\n${cut}`)).toBe(true);
    // A line added inside the range, or another line ending the same way, isn't the text it was on.
    expect(excerptMatches(['a', 'new', '}'], 'a\nb\n}')).toBe(false);
    expect(excerptMatches(['b', '}'], 'a\nb\n}')).toBe(false);
  });
});

describe('applying a suggestion to a worktree file', () => {
  let dir: string;
  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'kode-suggest-'));
  });
  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });
  const file = (text: string) => writeFileSync(join(dir, 'a.ts'), text);
  const read = () => readFileSync(join(dir, 'a.ts'), 'utf8');

  it('replaces the lines while they still read as shown', () => {
    file('one\ntwo\nthree\n');
    replaceLines(dir, 'a.ts', 2, 2, ['two'], ['TWO', 'two and a half']);
    expect(read()).toBe('one\nTWO\ntwo and a half\nthree\n');
    replaceLines(dir, 'a.ts', 2, 3, ['TWO', 'two and a half'], []);
    expect(read()).toBe('one\nthree\n');
  });

  it('keeps the file\'s line endings and its missing final newline', () => {
    file('one\r\ntwo\r\nthree');
    replaceLines(dir, 'a.ts', 2, 2, ['two\r'], ['2']);
    expect(read()).toBe('one\r\n2\r\nthree');
    replaceLines(dir, 'a.ts', 3, 3, ['three'], ['3', 'four']);
    expect(read()).toBe('one\r\n2\r\n3\r\nfour');
    replaceLines(dir, 'a.ts', 3, 4, ['3', 'four'], []);
    expect(read()).toBe('one\r\n2');
  });

  it('refuses lines that changed or don\'t exist, and leaves the file alone', () => {
    file('one\ntwo\n');
    expect(() => replaceLines(dir, 'a.ts', 2, 2, ['TWO'], ['x'])).toThrow(ReplaceError);
    expect(() => replaceLines(dir, 'a.ts', 2, 3, ['two', ''], ['x'])).toThrow(/have changed/);
    expect(() => replaceLines(dir, 'a.ts', 0, 1, ['one'], ['x'])).toThrow(/have changed/);
    expect(read()).toBe('one\ntwo\n');
    expect(() => replaceLines(dir, 'gone.ts', 1, 1, ['a'], ['b'])).toThrow(/isn't in the worktree/);
  });

  it('never writes outside the worktree', () => {
    expect(() => replaceLines(dir, '../a.ts', 1, 1, ['a'], ['b'])).toThrow(OutsideRepoError);
    expect(() => replaceLines(dir, '/etc/hosts', 1, 1, ['a'], ['b'])).toThrow(OutsideRepoError);
    expect(() => replaceLines(dir, '.git/hooks/pre-commit', 1, 1, ['a'], ['b'])).toThrow(OutsideRepoError);
    expect(() => replaceLines(dir, 'sub/.GIT/config', 1, 1, ['a'], ['b'])).toThrow(OutsideRepoError);
    const outside = mkdtempSync(join(tmpdir(), 'kode-outside-'));
    try {
      writeFileSync(join(outside, 'x.ts'), 'a\n');
      symlinkSync(join(outside, 'x.ts'), join(dir, 'link.ts'));
      expect(() => replaceLines(dir, 'link.ts', 1, 1, ['a'], ['b'])).toThrow(OutsideRepoError);
      expect(readFileSync(join(outside, 'x.ts'), 'utf8')).toBe('a\n');
    } finally {
      rmSync(outside, { recursive: true, force: true });
    }
    // A link inside the worktree into a nested repo's .git: refused by where it resolves.
    mkdirSync(join(dir, 'sub', '.git', 'hooks'), { recursive: true });
    writeFileSync(join(dir, 'sub', '.git', 'hooks', 'pre-commit'), 'a\n');
    symlinkSync(join(dir, 'sub', '.git', 'hooks', 'pre-commit'), join(dir, 'hook.ts'));
    expect(() => replaceLines(dir, 'hook.ts', 1, 1, ['a'], ['b'])).toThrow(OutsideRepoError);
    expect(readFileSync(join(dir, 'sub', '.git', 'hooks', 'pre-commit'), 'utf8')).toBe('a\n');
  });
});

describe("Apply's lines from the diff shown", () => {
  // New lines: 1 a, 2 B, 3 C, 4 d; then a hunk edge, 10 x.
  const rows = parsePatch(['@@ -1,3 +1,4 @@', ' a', '-b', '+B', '+C', ' d', '@@ -9,1 +10,1 @@', ' x'].join('\n'));
  const at = (text: string) => rows.find((l) => l.text === text && l.kind !== 'del')!;

  it('reads the comment\'s lines where the diff shows them now', () => {
    expect(applyRange(rows, at('C'), { side: 'new', line: 2, endLine: 3 })).toEqual({ line: 2, endLine: 3, current: ['B', 'C'] });
    // Moved down two lines since it was written: the lines it sits on now.
    expect(applyRange(rows, at('d'), { side: 'new', line: 1, endLine: 2 })).toEqual({ line: 3, endLine: 4, current: ['C', 'd'] });
  });

  it('offers nothing for lines the diff doesn\'t show in one run, or removed lines', () => {
    expect(applyRange(rows, at('x'), { side: 'new', line: 9, endLine: 10 })).toBeNull();
    expect(applyRange(rows, rows.find((l) => l.kind === 'del')!, { side: 'old', line: 2, endLine: 2 })).toBeNull();
    expect(applyRange(rows, at('a'), { side: 'new', line: 1, endLine: 2 })).toBeNull();
  });
});

describe('a suggested change on its card', () => {
  const html = (excerpt: string, line: number, endLine: number, lines: string[]) =>
    renderToStaticMarkup(createElement(Suggestion, { c: { line, endLine, excerpt }, lines }));

  it('shows the lines it was written on, then the suggested ones', () => {
    const out = html('const n = 1;', 4, 4, ['const total = 1;']);
    expect(out).toContain('Suggested change');
    expect(out).toMatch(/dl-del.*const n = 1;.*dl-add.*const total = 1;/);
    expect(out).not.toContain('not shown');
  });

  it('says how many earlier lines a cut excerpt left out, and shows a deletion as removed lines only', () => {
    const out = html('k\nl', 1, 12, []);
    expect(out).toContain('10 earlier lines not shown');
    expect(out).not.toContain('dl-add');
  });
});
