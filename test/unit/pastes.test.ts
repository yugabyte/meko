import { existsSync, readFileSync, readdirSync, rmSync, statSync, utimesSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MAX_PASTE_BYTES, MAX_START_IMAGES } from '../../src/shared/types.js';
import {
  STAGED_MAX_AGE_MS, checkStaged, claimStaged, claimedPath, imageExt, pasteDir, removePastes, removeStaged, savePastedFile, savePastedImage, stageImage, stagedDir,
  sweepStaged,
} from '../../src/server/pastes.js';
import { tempDir } from '../helpers.js';

const PNG = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3]);
const JPEG = Buffer.from([0xff, 0xd8, 0xff, 0xe0, 0, 0x10]);
const GIF = Buffer.from('GIF89a\x01\x00', 'latin1');
const WEBP = Buffer.concat([Buffer.from('RIFF'), Buffer.from([0, 0, 0, 0]), Buffer.from('WEBPVP8 ')]);

describe('pasted images', () => {
  let root: string;
  beforeEach(() => {
    root = join(tempDir('ak-pastes-'), 'pastes');
  });
  afterEach(() => rmSync(dirname(root), { recursive: true, force: true }));

  it('knows the types Claude Code takes by their bytes, not their name', () => {
    expect([PNG, JPEG, GIF, WEBP].map(imageExt)).toEqual(['png', 'jpg', 'gif', 'webp']);
    expect(imageExt(Buffer.from('<svg xmlns="http://www.w3.org/2000/svg"/>'))).toBeNull();
    expect(imageExt(Buffer.alloc(0))).toBeNull();
  });

  it('saves an image as a one-word path in the session folder, readable only by the owner', () => {
    const path = savePastedImage(root, 's1', PNG, new Date('2026-10-05T12:34:56.789Z'));
    expect(dirname(path)).toBe(pasteDir(root, 's1'));
    expect(basename(path)).toMatch(/^20261005-123456-[0-9a-f]{8}\.png$/);
    expect(readFileSync(path)).toEqual(PNG);
    expect(statSync(path).mode & 0o777).toBe(0o600);
    expect(statSync(dirname(path)).mode & 0o777).toBe(0o700);
    // The same second again is a new file, never an overwrite.
    const again = savePastedImage(root, 's1', JPEG, new Date('2026-10-05T12:34:56.789Z'));
    expect(again).not.toBe(path);
    expect(again.endsWith('.jpg')).toBe(true);
    expect(readdirSync(pasteDir(root, 's1'))).toHaveLength(2);
  });

  it('refuses what is not an image Claude Code can take, with a reason', () => {
    const status = (data: unknown) => {
      try {
        savePastedImage(root, 's1', data);
        return 200;
      } catch (e) {
        return (e as { status: number }).status;
      }
    };
    expect(status({ a: 1 })).toBe(415);
    expect(status(Buffer.alloc(0))).toBe(400);
    expect(status(Buffer.from('not an image'))).toBe(415);
    expect(status(Buffer.concat([PNG, Buffer.alloc(MAX_PASTE_BYTES)]))).toBe(413);
    expect(existsSync(pasteDir(root, 's1'))).toBe(false);
  });

  it('saves a file of any type with its safe name kept, owner-only, and refuses what it must', () => {
    const path = savePastedFile(root, 's1', Buffer.from('%PDF-1'), '../My Spec (v2).pdf', new Date('2026-10-05T12:34:56.789Z'));
    expect(dirname(path)).toBe(pasteDir(root, 's1'));
    expect(basename(path)).toMatch(/^20261005-123456-[0-9a-f]{8}-My-Spec-v2-\.pdf$/);
    expect(statSync(path).mode & 0o777).toBe(0o600);
    expect(basename(savePastedFile(root, 's1', Buffer.from('x'), '...'))).toMatch(/^\d{8}-\d{6}-[0-9a-f]{8}$/);
    const status = (data: unknown) => {
      try {
        savePastedFile(root, 's2', data, 'a.md');
        return 200;
      } catch (e) {
        return (e as { status: number }).status;
      }
    };
    expect([status({ a: 1 }), status(Buffer.alloc(0)), status(Buffer.alloc(MAX_PASTE_BYTES + 1))]).toEqual([415, 400, 413]);
    expect(existsSync(pasteDir(root, 's2'))).toBe(false);
  });

  it("removes a session's images and leaves other sessions' alone", () => {
    const mine = savePastedImage(root, 's1', PNG);
    const theirs = savePastedImage(root, 's2', PNG);
    removePastes(root, 's1');
    expect(existsSync(mine)).toBe(false);
    expect(existsSync(pasteDir(root, 's1'))).toBe(false);
    expect(existsSync(theirs)).toBe(true);
    expect(() => removePastes(root, 'never-pasted')).not.toThrow();
  });

  describe('staged in the New session dialog', () => {
    const status = (fn: () => unknown) => {
      try {
        fn();
        return 200;
      } catch (e) {
        return (e as { status: number }).status;
      }
    };

    it("saves an image under its project, owner-only, and names it by an id that isn't a path", () => {
      const id = stageImage(root, 'demo', PNG, new Date('2026-10-07T10:15:00.000Z'));
      expect(id).toMatch(/^20261007-101500-[0-9a-f]{8}\.png$/);
      const path = join(stagedDir(root, 'demo'), id);
      expect(readFileSync(path)).toEqual(PNG);
      expect(statSync(path).mode & 0o777).toBe(0o600);
      for (const dir of [stagedDir(root, 'demo'), dirname(stagedDir(root, 'demo'))]) expect(statSync(dir).mode & 0o777).toBe(0o700);
      // The same checks as a terminal's paste.
      expect(status(() => stageImage(root, 'demo', Buffer.from('not an image')))).toBe(415);
      expect(status(() => stageImage(root, 'demo', Buffer.alloc(0)))).toBe(400);
    });

    it("takes only its own project's staged images, by their ids, each once", () => {
      const a = stageImage(root, 'demo', PNG);
      const b = stageImage(root, 'demo', JPEG);
      const theirs = stageImage(root, 'other', PNG);
      expect(checkStaged(root, 'demo', undefined)).toEqual([]);
      expect(checkStaged(root, 'demo', [a, b, a])).toEqual([a, b]);
      expect(status(() => checkStaged(root, 'demo', 'a.png'))).toBe(400);
      expect(status(() => checkStaged(root, 'demo', [1]))).toBe(400);
      expect(status(() => checkStaged(root, 'demo', [theirs]))).toBe(400);
      expect(status(() => checkStaged(root, 'demo', ['../other/' + theirs]))).toBe(400);
      expect(status(() => checkStaged(root, 'demo', ['/etc/passwd']))).toBe(400);
      expect(status(() => checkStaged(root, 'demo', ['20261007-101500-00000000.png']))).toBe(400);
      const many = Array.from({ length: MAX_START_IMAGES + 1 }, () => stageImage(root, 'demo', PNG));
      expect(status(() => checkStaged(root, 'demo', many))).toBe(400);
      expect(checkStaged(root, 'demo', many.slice(0, MAX_START_IMAGES))).toHaveLength(MAX_START_IMAGES);
    });

    it("moves claimed images into the session's folder, and names any that were gone", () => {
      const a = stageImage(root, 'demo', PNG);
      const b = stageImage(root, 'demo', GIF);
      rmSync(join(stagedDir(root, 'demo'), b));
      expect(claimStaged(root, 'demo', [a, b], 'demo--fix')).toEqual([b]);
      expect(readFileSync(claimedPath(root, 'demo--fix', a))).toEqual(PNG);
      expect(dirname(claimedPath(root, 'demo--fix', a))).toBe(pasteDir(root, 'demo--fix'));
      expect(statSync(pasteDir(root, 'demo--fix')).mode & 0o777).toBe(0o700);
      expect(existsSync(join(stagedDir(root, 'demo'), a))).toBe(false);
      expect(claimStaged(root, 'demo', [], 'demo--none')).toEqual([]);
      expect(existsSync(pasteDir(root, 'demo--none'))).toBe(false);
    });

    it('sweeps staged images older than a day, and removes a closed project\'s', () => {
      const now = Date.now();
      const old = stageImage(root, 'demo', PNG);
      const fresh = stageImage(root, 'demo', PNG);
      const gone = stageImage(root, 'stale', PNG);
      const age = (project: string, id: string) => {
        const t = (now - STAGED_MAX_AGE_MS - 60_000) / 1000;
        utimesSync(join(stagedDir(root, project), id), t, t);
      };
      age('demo', old);
      age('stale', gone);
      sweepStaged(root, STAGED_MAX_AGE_MS, now);
      expect(readdirSync(stagedDir(root, 'demo'))).toEqual([fresh]);
      expect(existsSync(stagedDir(root, 'stale'))).toBe(false);
      // A session's own pastes aren't staged ones.
      const pasted = savePastedImage(root, 's1', PNG);
      removeStaged(root, 'demo');
      expect(existsSync(stagedDir(root, 'demo'))).toBe(false);
      expect(existsSync(pasted)).toBe(true);
      expect(() => sweepStaged(join(root, 'nothing-here'))).not.toThrow();
    });
  });
});
