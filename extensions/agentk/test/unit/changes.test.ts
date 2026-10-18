import { describe, expect, it } from 'vitest';
import type { ChangedFile } from '../../src/shared/types.js';
import {
  FILE_PATCH_CAP, TOTAL_PATCH_CAP, capFiles, capPatches, matchPatches, newFilePatch, parseNameStatus, parseNumstat, quotePath, splitPatch,
} from '../../src/server/changes.js';
import { parsePatch } from '../../src/web/diff.js';

const file = (path: string, patch: string | null): ChangedFile => ({
  path, oldPath: null, kind: 'modified', added: 0, removed: 0, binary: false, uncommitted: false, patch, truncated: false,
});

describe('changes parsing', () => {
  it('splits a whole-tree diff per file, paths with spaces included', () => {
    const out = 'diff --git a/a b.txt b/a b.txt\n@@ -1 +1 @@\n-x\n+y\ndiff --git a/c b/c\n@@ -0,0 +1 @@\n+z\n';
    expect(splitPatch(out)).toEqual([
      'diff --git a/a b.txt b/a b.txt\n@@ -1 +1 @@\n-x\n+y',
      'diff --git a/c b/c\n@@ -0,0 +1 @@\n+z',
    ]);
    expect(splitPatch('')).toEqual([]);
  });

  it('reads -z name-status and numstat, renames and binaries included', () => {
    expect(parseNameStatus('M\0a b.txt\0R090\0old\0new\0D\0gone\0A\0add\0')).toEqual([
      { path: 'a b.txt', oldPath: null, kind: 'modified' },
      { path: 'new', oldPath: 'old', kind: 'renamed' },
      { path: 'gone', oldPath: null, kind: 'deleted' },
      { path: 'add', oldPath: null, kind: 'added' },
    ]);
    const n = parseNumstat('3\t1\ta b.txt\x001\t1\t\0old\0new\0-\t-\timg.png\0');
    expect(n.get('a b.txt')).toEqual({ added: 3, removed: 1, binary: false });
    expect(n.get('new')).toEqual({ added: 1, removed: 1, binary: false });
    expect(n.get('img.png')).toEqual({ added: 0, removed: 0, binary: true });
  });

  it('matches patches to files by header, joining the two halves of a type change', () => {
    const typeChange = [
      'diff --git a/f b/f\ndeleted file mode 100644\nindex 1..0\n--- a/f\n+++ /dev/null\n@@ -1 +0,0 @@\n-bye',
      'diff --git a/f b/f\nnew file mode 120000\nindex 0..2\n--- /dev/null\n+++ b/f\n@@ -0,0 +1 @@\n+target\n\\ No newline at end of file',
    ];
    const chunks = [
      'diff --git a/a b.txt b/a b.txt\n@@ -1 +1 @@\n-x\n+y',
      ...typeChange,
      'diff --git a/stray b/stray\n@@ -1 +1 @@\n-p\n+q', // changed after name-status ran
      'diff --git "a/t\\tab" "b/t\\tab"\n@@ -1 +1 @@\n-1\n+2',
      'diff --git a/old b/new\nsimilarity index 100%\nrename from old\nrename to new',
    ];
    const entries = [
      { path: 'a b.txt', oldPath: null }, { path: 'f', oldPath: null }, { path: 'late', oldPath: null },
      { path: 't\tab', oldPath: null }, { path: 'new', oldPath: 'old' },
    ];
    const got = matchPatches(entries, chunks);
    expect(got[0]).toBe(chunks[0]);
    expect(got[1]).toBe('diff --git a/f b/f\ndeleted file mode 100644\nindex 1..0\n--- a/f\n+++ /dev/null\nnew file mode 120000\n'
      + '@@ -1 +0,0 @@\n-bye\n@@ -0,0 +1 @@\n+target\n\\ No newline at end of file');
    expect(got[2]).toBeNull();
    expect(got[3]).toBe(chunks[4]);
    expect(got[4]).toBe(chunks[5]);
    expect(parsePatch(got[1]!).map((l) => [l.kind, l.old, l.new, l.text])).toEqual([
      ['meta', null, null, 'deleted file mode 100644'],
      ['meta', null, null, 'new file mode 120000'],
      ['hunk', null, null, '@@ -1 +0,0 @@'],
      ['del', 1, null, 'bye'],
      ['hunk', null, null, '@@ -0,0 +1 @@'],
      ['add', null, 1, 'target'],
      ['note', null, null, 'No newline at end of file'],
    ]);
  });

  it('quotes paths the way git does in diff headers', () => {
    expect(quotePath('a/plain name ü.txt')).toBe('a/plain name ü.txt');
    expect(quotePath('a/t\tn\nq"b\\\x01\x7f')).toBe('"a/t\\tn\\nq\\"b\\\\\\001\\177"');
  });

  it('cuts big patches and drops the rest past the total cap', () => {
    const line = '+' + 'x'.repeat(99) + '\n';
    const big = line.repeat(Math.ceil((FILE_PATCH_CAP * 1.5) / line.length));
    const files = [file('a', big), file('b', 'small')];
    expect(capPatches(files)).toBe(0);
    expect(files[0].truncated).toBe(true);
    expect(files[0].patch!.length).toBeLessThanOrEqual(FILE_PATCH_CAP);
    expect(files[0].patch!.endsWith('x')).toBe(true); // cut at a line boundary

    const many = Array.from({ length: Math.ceil(TOTAL_PATCH_CAP / FILE_PATCH_CAP) + 3 }, (_, i) => file(`f${i}`, big));
    const omitted = capPatches(many);
    expect(omitted).toBeGreaterThan(0);
    expect(many.filter((f) => f.patch === null)).toHaveLength(omitted);
    expect(many.reduce((t, f) => t + (f.patch?.length ?? 0), 0)).toBeLessThanOrEqual(TOTAL_PATCH_CAP);
  });

  it.each<[string, string[], string[], string[]]>([
    ['lists every file under the cap', ['a', 'b', 'c'], ['c'], ['a', 'b', 'c']],
    ['keeps the first files by path past it', ['a', 'b', 'c', 'd', 'e'], [], ['a', 'b', 'c']],
    ['keeps files with comments first, in path order', ['a', 'b', 'c', 'd', 'e'], ['e', 'gone', 'b'], ['a', 'b', 'e']],
  ])('caps the files listed: %s', (_, paths, keep, listed) => {
    expect(capFiles(paths.map((p) => file(p, null)), new Set(keep), 3).map((f) => f.path)).toEqual(listed);
  });

  it('builds a new-file patch', () => {
    expect(newFilePatch('a\nb\n')).toBe('@@ -0,0 +1,2 @@\n+a\n+b');
    expect(newFilePatch('')).toBe('');
  });
});
