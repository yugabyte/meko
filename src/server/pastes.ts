// Images pasted into a session's terminal in the browser. Claude Code can't see the browser's
// clipboard, so the image is saved on the host and its path is pasted instead: Claude Code reads
// a pasted path to a .png, .jpg, .gif or .webp file as an attached image.
//
// Images added in the New session dialog come before there's a session: they're staged under the
// project (`.new/<project>/`, which no session id can be) and moved into the session's folder when
// it's created. Staged images nobody started a session with are swept after a day.

import { randomBytes } from 'node:crypto';
import { chmodSync, existsSync, mkdirSync, readdirSync, renameSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { basename, join } from 'node:path';
import { MAX_PASTE_BYTES, MAX_START_IMAGES } from '../shared/types.js';
import { HttpError } from './projects.js';

/** The image types Claude Code takes from a pasted path, by their first bytes. */
const KINDS: { ext: string; test: (b: Buffer) => boolean }[] = [
  { ext: 'png', test: (b) => b.subarray(0, 8).equals(Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])) },
  { ext: 'jpg', test: (b) => b.length >= 3 && b[0] === 0xff && b[1] === 0xd8 && b[2] === 0xff },
  { ext: 'gif', test: (b) => ['GIF87a', 'GIF89a'].includes(b.subarray(0, 6).toString('latin1')) },
  { ext: 'webp', test: (b) => b.subarray(0, 4).toString('latin1') === 'RIFF' && b.subarray(8, 12).toString('latin1') === 'WEBP' },
];

/** The name every saved image gets: the only names a staged image's id may be. */
const IMAGE_NAME = /^\d{8}-\d{6}-[0-9a-f]{8}\.(?:png|jpg|gif|webp)$/;

/** The folder under the pastes root that holds staged images, one folder per project. */
const STAGED = '.new';

/** How long a staged image waits for a session to start with it. */
export const STAGED_MAX_AGE_MS = 24 * 60 * 60 * 1000;

/** The file extension for an image's bytes, or null when it isn't a type Claude Code takes. */
export function imageExt(data: Buffer): string | null {
  return KINDS.find((k) => k.test(data))?.ext ?? null;
}

/** A session's folder of pasted images. */
export function pasteDir(root: string, sessionId: string): string {
  return join(root, sessionId);
}

/** A project's folder of images staged in the New session dialog. */
export function stagedDir(root: string, projectId: string): string {
  return join(root, STAGED, projectId);
}

/** Makes `dirs` in order (each inside the one before), the owner's alone. */
function ownDirs(...dirs: string[]): void {
  for (const dir of dirs) {
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    chmodSync(dir, 0o700);
  }
}

/** Checks an image's bytes, and finds its file extension from them, never from the browser's word for its type. */
function checkImage(data: unknown): { data: Buffer; ext: string } {
  if (!Buffer.isBuffer(data)) throw new HttpError(415, 'Send the image itself as the body, with its image/* type as the Content-Type.');
  if (!data.length) throw new HttpError(400, 'The image was empty. Copy it again and paste.');
  if (data.length > MAX_PASTE_BYTES) throw new HttpError(413, `That image is over ${MAX_PASTE_BYTES / 1024 / 1024} MB. Paste a smaller one.`);
  const ext = imageExt(data);
  if (!ext) throw new HttpError(415, 'Only PNG, JPEG, GIF and WebP images can be pasted.');
  return { data, ext };
}

/** Writes a checked image into `dir` as a new file, the owner's alone, and returns its path. */
function writeImage(dir: string, image: { data: Buffer; ext: string }, now: Date): string {
  // Sortable by time, and no spaces or quotes, so the path pastes as one word.
  const stamp = now.toISOString().replace(/[-:]/g, '').replace('T', '-').slice(0, 15);
  const path = join(dir, `${stamp}-${randomBytes(4).toString('hex')}.${image.ext}`);
  writeFileSync(path, image.data, { mode: 0o600, flag: 'wx' });
  return path;
}

/** Saves a pasted image for a session and returns its path. The folders and files are the owner's alone. */
export function savePastedImage(root: string, sessionId: string, data: unknown, now = new Date()): string {
  const image = checkImage(data);
  const dir = pasteDir(root, sessionId);
  ownDirs(root, dir);
  return writeImage(dir, image, now);
}

/**
 * Saves an image added in the New session dialog under the project, and returns its id (the
 * file's name), which the dialog sends back when it creates the session.
 */
export function stageImage(root: string, projectId: string, data: unknown, now = new Date()): string {
  const image = checkImage(data);
  const dir = stagedDir(root, projectId);
  ownDirs(root, join(root, STAGED), dir);
  return basename(writeImage(dir, image, now));
}

/**
 * Checks the staged images a new session asks for and returns their ids, each once. Only names an
 * image is saved under are taken, and only from the project's own staging folder, so the browser
 * never names a path.
 */
export function checkStaged(root: string, projectId: string, ids: unknown): string[] {
  if (ids == null) return [];
  if (!Array.isArray(ids) || ids.some((i) => typeof i !== 'string')) throw new HttpError(400, 'images must be a list of the ids the image uploads returned.');
  const unique = [...new Set(ids as string[])];
  if (unique.length > MAX_START_IMAGES) throw new HttpError(400, `A session can start with at most ${MAX_START_IMAGES} images. Remove some and try again.`);
  for (const id of unique) {
    if (!IMAGE_NAME.test(id) || !existsSync(join(stagedDir(root, projectId), id))) {
      throw new HttpError(400, 'An image you added is no longer on the host. Remove it, paste it again, and start the session.');
    }
  }
  return unique;
}

/**
 * Moves checked staged images into a session's folder. Returns the ids that couldn't be moved
 * (gone since they were checked), so the caller can say so.
 */
export function claimStaged(root: string, projectId: string, ids: string[], sessionId: string): string[] {
  if (!ids.length) return [];
  const dir = pasteDir(root, sessionId);
  ownDirs(root, dir);
  const missed: string[] = [];
  for (const id of ids) {
    try {
      renameSync(join(stagedDir(root, projectId), id), join(dir, id));
    } catch {
      missed.push(id);
    }
  }
  return missed;
}

/** Where a staged image is once it's claimed for a session. */
export function claimedPath(root: string, sessionId: string, id: string): string {
  return join(pasteDir(root, sessionId), id);
}

/** Removes staged images older than `maxAgeMs`, and project folders left empty. */
export function sweepStaged(root: string, maxAgeMs = STAGED_MAX_AGE_MS, now = Date.now()): void {
  const staged = join(root, STAGED);
  if (!existsSync(staged)) return;
  for (const project of readdirSync(staged)) {
    const dir = join(staged, project);
    let left = 0;
    for (const name of readdirSync(dir)) {
      const path = join(dir, name);
      try {
        if (now - statSync(path).mtimeMs > maxAgeMs) rmSync(path, { recursive: true, force: true });
        else left++;
      } catch {
        // Claimed or removed meanwhile.
      }
    }
    if (!left) rmSync(dir, { recursive: true, force: true });
  }
}

/** Forgets a project's staged images (when it's closed). */
export function removeStaged(root: string, projectId: string): void {
  rmSync(stagedDir(root, projectId), { recursive: true, force: true });
}

/** Forgets a session's pasted images (when it's archived or its project is closed). */
export function removePastes(root: string, sessionId: string): void {
  rmSync(pasteDir(root, sessionId), { recursive: true, force: true });
}
