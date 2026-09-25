import { describe, expect, it } from 'vitest';
import type { ChangedFile } from '../../src/shared/types.js';
import {
  FILE_PATCH_CAP, TOTAL_PATCH_CAP, capPatches, newFilePatch, parseNameStatus, parseNumstat, splitPatch,
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

  it('builds a new-file patch', () => {
    expect(newFilePatch('a\nb\n')).toBe('@@ -0,0 +1,2 @@\n+a\n+b');
    expect(newFilePatch('')).toBe('');
  });

  it('turns a patch into numbered display lines', () => {
    const lines = parsePatch('diff --git a/x b/x\nindex 1..2 100644\n--- a/x\n+++ b/x\n@@ -3,3 +3,3 @@ fn\n keep\n-old\n+new\n--dashes\n\\ No newline at end of file');
    expect(lines.map((l) => [l.kind, l.old, l.new, l.text])).toEqual([
      ['hunk', null, null, '@@ -3,3 +3,3 @@ fn'],
      ['ctx', 3, 3, 'keep'],
      ['del', 4, null, 'old'],
      ['add', null, 4, 'new'],
      ['del', 5, null, '-dashes'],
      ['note', null, null, 'No newline at end of file'],
    ]);
    expect(parsePatch('diff --git a/m b/m\nold mode 100644\nnew mode 100755')).toEqual([
      { kind: 'meta', text: 'old mode 100644', old: null, new: null },
      { kind: 'meta', text: 'new mode 100755', old: null, new: null },
    ]);
  });
});
