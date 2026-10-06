import { existsSync, readFileSync, readdirSync, rmSync, statSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { MAX_PASTE_BYTES } from '../../src/shared/types.js';
import { imageExt, pasteDir, removePastes, savePastedImage } from '../../src/server/pastes.js';
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
    expect(imageExt(Buffer.from('BM\0\0'))).toBeNull();
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

  it("removes a session's images and leaves other sessions' alone", () => {
    const mine = savePastedImage(root, 's1', PNG);
    const theirs = savePastedImage(root, 's2', PNG);
    removePastes(root, 's1');
    expect(existsSync(mine)).toBe(false);
    expect(existsSync(pasteDir(root, 's1'))).toBe(false);
    expect(existsSync(theirs)).toBe(true);
    expect(() => removePastes(root, 'never-pasted')).not.toThrow();
  });
});
