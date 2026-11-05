import { describe, expect, it } from 'vitest';
import type { ReviewComment } from '../../src/shared/types.js';
import { BODY_MAX, EXCERPT_LINE_CHARS, EXCERPT_LINES, checkNewComment, clipExcerptLine, formatComments } from '../../src/server/comments.js';
import { anchorComment, parsePatch } from '../../src/web/diff.js';

const comment = (over: Partial<ReviewComment>): ReviewComment => ({
  id: 'c', path: 'a.ts', side: 'new', line: 1, endLine: 1, excerpt: '', body: 'fix', state: 'queued',
  createdAt: 0, updatedAt: 0, sentAt: null, ...over,
});

describe('anchoring comments to diff rows', () => {
  const patch = ['@@ -1,4 +1,5 @@', ' one', '-two', '+TWO', '+three', ' }', ' }'].join('\n');
  const lines = parsePatch(patch);
  const row = (text: string, kind: string) => lines.findIndex((l) => l.text === text && l.kind === kind);

  it('keeps a comment on its line while the text matches', () => {
    expect(anchorComment(lines, { side: 'new', endLine: 3, excerpt: 'three' })).toBe(row('three', 'add'));
    expect(anchorComment(lines, { side: 'new', endLine: 3, excerpt: 'TWO\nthree' })).toBe(row('three', 'add'));
  });

  it('points old-side comments at removed lines only', () => {
    expect(anchorComment(lines, { side: 'old', endLine: 2, excerpt: 'two' })).toBe(row('two', 'del'));
    // Old line 1 is unchanged context, which comments address on the new side.
    expect(anchorComment(lines, { side: 'old', endLine: 1, excerpt: 'one' })).toBeNull();
  });

  it('follows lines that moved, to the nearest match', () => {
    const moved = parsePatch(['@@ -1,2 +1,4 @@', '+new a', '+new b', ' one', '+TWO', '+three'].join('\n'));
    expect(anchorComment(moved, { side: 'new', endLine: 3, excerpt: 'three' })).toBe(moved.findIndex((l) => l.text === 'three'));
    // Repeated text: the nearest `}` to the stored line wins.
    expect(anchorComment(lines, { side: 'new', endLine: 7, excerpt: '}' })).toBe(lines.findIndex((l) => l.text === '}' && l.new === 5));
  });

  it('prefers a match of the whole excerpt over a nearer last line', () => {
    const l = parsePatch(['@@ -0,0 +1,6 @@', '+a', '+}', '+x', '+b', '+}', '+y'].join('\n'));
    const at = anchorComment(l, { side: 'new', endLine: 3, excerpt: 'b\n}' });
    expect(l[at!].new).toBe(5);
  });

  it('marks a comment outdated when its lines are gone', () => {
    expect(anchorComment(lines, { side: 'new', endLine: 3, excerpt: 'gone' })).toBeNull();
    expect(anchorComment([], { side: 'new', endLine: 1, excerpt: 'x' })).toBeNull();
  });
});

describe('formatting comments for Claude', () => {
  it('groups by file and line, and fences excerpts safely', () => {
    const out = formatComments([
      comment({ path: 'b.ts', line: 9, endLine: 9, excerpt: 'const b = 1;', body: 'Rename b.' }),
      comment({ path: 'a.md', line: 4, endLine: 6, excerpt: 'use ```js\nfoo\n```', body: 'Drop the fence.' }),
      comment({ path: 'b.ts', side: 'old', line: 2, endLine: 2, excerpt: 'old();', body: 'Why remove this?' }),
      comment({ path: 'b.ts', line: 1, endLine: 1, excerpt: '', body: 'No excerpt.' }),
    ]);
    expect(out).toBe([
      '## a.md',
      'Lines 4–6:\n````\nuse ```js\nfoo\n```\n````\nDrop the fence.',
      '## b.ts',
      'Line 1:\nNo excerpt.',
      'Removed line 2 (numbered as in the base):\n```\nold();\n```\nWhy remove this?',
      'Line 9:\n```\nconst b = 1;\n```\nRename b.',
    ].join('\n\n'));
  });
});

describe('checking new comments', () => {
  const ok = { path: 'a.ts', side: 'new', line: 2, endLine: 3, excerpt: 'x\ny', body: '  looks off  ' };

  it('trims the body and keeps the last lines of a long excerpt', () => {
    expect(checkNewComment(ok)).toEqual({ ...ok, body: 'looks off' });
    const long = Array.from({ length: 30 }, (_, i) => `l${i}`).join('\n');
    const c = checkNewComment({ ...ok, line: 1, endLine: 30, excerpt: long });
    expect(c.excerpt.split('\n')).toHaveLength(EXCERPT_LINES);
    expect(c.excerpt.endsWith('l29')).toBe(true);
  });

  it('clips long lines and drops whole lines from the front, keeping the last line matchable', () => {
    const long = 'x'.repeat(5000);
    const c = checkNewComment({ ...ok, excerpt: `a\n${long}` });
    expect(c.excerpt).toBe(`a\n${clipExcerptLine(long)}`);
    expect(clipExcerptLine(long)).toBe(`${'x'.repeat(EXCERPT_LINE_CHARS)}…`);
    expect(clipExcerptLine('short')).toBe('short');
    const many = Array.from({ length: 8 }, (_, i) => `${i}${'y'.repeat(900)}`);
    const lines = checkNewComment({ ...ok, excerpt: many.join('\n') }).excerpt.split('\n');
    expect(lines.length).toBeLessThan(8);
    expect(lines).toEqual(many.slice(-lines.length)); // whole lines, never a partial first one
    // The diff view still finds a comment on the long line.
    const diff = parsePatch(['@@ -0,0 +1,2 @@', '+a', `+${long}`].join('\n'));
    expect(anchorComment(diff, { side: 'new', endLine: 2, excerpt: c.excerpt })).toBe(2);
  });

  it('refuses bad input with a clear reason', () => {
    expect(() => checkNewComment({ ...ok, body: '   ' })).toThrow(/Write something/);
    expect(() => checkNewComment({ ...ok, body: 'x'.repeat(BODY_MAX + 1) })).toThrow(/at most 10,000 characters/);
    expect(() => checkNewComment({ ...ok, side: 'left' })).toThrow(/'old' or 'new'/);
    expect(() => checkNewComment({ ...ok, line: 3, endLine: 2 })).toThrow(/endLine ≥ line/);
    expect(() => checkNewComment({ ...ok, line: 0 })).toThrow(/line range/);
    expect(() => checkNewComment({ ...ok, path: '' })).toThrow(/path/);
    expect(() => checkNewComment(null)).toThrow(/path/);
  });
});
