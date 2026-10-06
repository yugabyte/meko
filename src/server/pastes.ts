// Images pasted into a session's terminal in the browser. Claude Code can't see the browser's
// clipboard, so the image is saved on the host and its path is pasted instead: Claude Code reads
// a pasted path to a .png, .jpg, .gif or .webp file as an attached image.

import { randomBytes } from 'node:crypto';
import { chmodSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { MAX_PASTE_BYTES } from '../shared/types.js';
import { HttpError } from './projects.js';

/** The image types Claude Code takes from a pasted path, by their first bytes. */
const KINDS: { ext: string; test: (b: Buffer) => boolean }[] = [
  { ext: 'png', test: (b) => b.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])) },
  { ext: 'jpg', test: (b) => b.length >= 3 && b[0] === 0xff && b[1] === 0xd8 && b[2] === 0xff },
  { ext: 'gif', test: (b) => ['GIF87a', 'GIF89a'].includes(b.subarray(0, 6).toString('latin1')) },
  { ext: 'webp', test: (b) => b.subarray(0, 4).toString('latin1') === 'RIFF' && b.subarray(8, 12).toString('latin1') === 'WEBP' },
];

/** The file extension for an image's bytes, or null when it isn't a type Claude Code takes. */
export function imageExt(data: Buffer): string | null {
  return KINDS.find((k) => k.test(data))?.ext ?? null;
}

/** A session's folder of pasted images. */
export function pasteDir(root: string, sessionId: string): string {
  return join(root, sessionId);
}

/**
 * Saves a pasted image for a session and returns its path. The type comes from the bytes, never
 * the browser's word for it. The folders and files are the owner's alone.
 */
export function savePastedImage(root: string, sessionId: string, data: unknown, now = new Date()): string {
  if (!Buffer.isBuffer(data)) throw new HttpError(415, 'Send the image itself as the body, with its image/* type as the Content-Type.');
  if (!data.length) throw new HttpError(400, 'The image was empty. Copy it again and paste.');
  if (data.length > MAX_PASTE_BYTES) throw new HttpError(413, `That image is over ${MAX_PASTE_BYTES / 1024 / 1024} MB. Paste a smaller one.`);
  const ext = imageExt(data);
  if (!ext) throw new HttpError(415, 'Only PNG, JPEG, GIF and WebP images can be pasted.');
  const dir = pasteDir(root, sessionId);
  mkdirSync(root, { recursive: true, mode: 0o700 });
  mkdirSync(dir, { recursive: true, mode: 0o700 });
  chmodSync(dir, 0o700);
  // Sortable by time, and no spaces or quotes, so the path pastes as one word.
  const stamp = now.toISOString().replace(/[-:]/g, '').replace('T', '-').slice(0, 15);
  const path = join(dir, `${stamp}-${randomBytes(4).toString('hex')}.${ext}`);
  writeFileSync(path, data, { mode: 0o600, flag: 'wx' });
  return path;
}

/** Forgets a session's pasted images (when it's archived or its project is closed). */
export function removePastes(root: string, sessionId: string): void {
  rmSync(pasteDir(root, sessionId), { recursive: true, force: true });
}
