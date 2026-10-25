// A deck's pieces in Meko (src/shared/manifest.ts says what they are) and in the local cache: hashing,
// splitting into parts, uploading only what the datapack doesn't have yet with the manifest last, and
// fetching pieces back, checked against their hash.

import { createHash, randomUUID } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Deck } from '../shared/deck.js';
import {
  INDEX_BYTES, MANIFEST_FILE, PART_BYTES, POINTER_FORMAT, STORE_VERSION, assetFilename, bytesJson, fromManifest, jsonBytes, partFilename,
  readRoot, readManifest, readSceneFile, sceneFile, sceneFilename, splitBytes, toManifest, type Manifest, type Piece, type SceneFile,
} from '../shared/manifest.js';
import { MekoError, type MekoPort } from './meko.js';

export const sha256 = (b: Uint8Array) => createHash('sha256').update(b).digest('hex');

/** How many pieces go up, or come down, at once. */
const AT_ONCE = 4;

/**
 * Runs `fn` over `items`, `n` at a time, in order of starting. After a failure it starts no more,
 * waits for the ones under way (so none reports after the caller has moved on), and throws the first.
 */
async function pool<T>(items: T[], n: number, fn: (t: T) => Promise<void>): Promise<void> {
  let next = 0;
  let failure: { error: unknown } | null = null;
  const worker = async () => {
    while (!failure && next < items.length) {
      try {
        await fn(items[next++]!);
      } catch (error) {
        failure ??= { error };
      }
    }
  };
  await Promise.all(Array.from({ length: Math.min(n, items.length) }, worker));
  if (failure) throw (failure as { error: unknown }).error;
}

/** One artifact to upload. */
interface Upload {
  filename: string;
  bytes: Uint8Array;
  hash: string;
}

/** How bytes are stored: the piece, and the uploads that store it. */
export function storeAs(bytes: Uint8Array, filename: string): { piece: Piece; uploads: Upload[] } {
  const hash = sha256(bytes);
  const parts = splitBytes(bytes, PART_BYTES);
  if (parts.length === 1) return { piece: { hash, size: bytes.length }, uploads: [{ filename, bytes, hash }] };
  const uploads = parts.map((p, n) => ({ filename: partFilename(hash, n), bytes: p, hash: sha256(p) }));
  return { piece: { hash, size: bytes.length, parts: uploads.map((u) => u.hash) }, uploads };
}

/** Every hash a piece is kept under in Meko: its parts', or its own. */
export const pieceHashes = (p: Piece) => p.parts ?? [p.hash];

export interface SaveResult {
  manifestHash: string;
  savedAt: string;
  /** The hashes the saved version stands on, which the next save needn't upload again. */
  known: string[];
  assetPieces: Record<string, Piece>;
  uploaded: number;
}

export interface Loaded {
  deck: Deck;
  savedAt: string;
  manifestHash: string;
  known: string[];
  assetPieces: Record<string, Piece>;
}

export class PieceStore {
  constructor(private meko: MekoPort, private cacheDir: string) {
    mkdirSync(cacheDir, { recursive: true, mode: 0o700 });
  }

  private file(hash: string): string {
    return join(this.cacheDir, hash);
  }

  /** Whether the cache holds bytes under a hash (checked when they went in, by keep's own hashing or blob's check). */
  has(hash: string): boolean {
    return /^[0-9a-f]{64}$/.test(hash) && existsSync(this.file(hash));
  }

  /** The cached file of a hash the cache holds. */
  path(hash: string): string {
    if (!this.has(hash)) throw new Error(`The cache has no piece ${hash.slice(0, 12)}.`);
    return this.file(hash);
  }

  /** Keeps bytes in the cache and returns their hash. */
  keep(bytes: Uint8Array): string {
    const hash = sha256(bytes);
    const f = this.file(hash);
    if (!existsSync(f)) {
      const tmp = `${f}.${randomUUID()}.tmp`;
      writeFileSync(tmp, bytes, { mode: 0o600 });
      renameSync(tmp, f);
    }
    return hash;
  }

  /** Cached bytes, checked against their hash; null when they aren't here (or are damaged). */
  cached(hash: string): Uint8Array | null {
    try {
      const b = new Uint8Array(readFileSync(this.file(hash)));
      return sha256(b) === hash ? b : null;
    } catch {
      return null;
    }
  }

  /** One artifact's bytes: from the cache, else Meko, checked, and fetched again once when they don't match. */
  async blob(datapackId: string, hash: string): Promise<Uint8Array> {
    const here = this.cached(hash);
    if (here) return here;
    for (let attempt = 0; attempt < 2; attempt++) {
      const b = await this.meko.getArtifact(datapackId, hash);
      if (!b) throw new MekoError(`A piece of the deck (${hash.slice(0, 12)}) isn't in its datapack.`);
      if (sha256(b) === hash) {
        this.keep(b);
        return b;
      }
    }
    throw new MekoError(`A piece of the deck (${hash.slice(0, 12)}) came back damaged twice.`);
  }

  /** A stored thing's bytes: its parts joined, or the piece itself, checked. */
  async piece(datapackId: string, p: Piece): Promise<Uint8Array> {
    const whole = this.cached(p.hash);
    if (whole) return whole;
    if (!p.parts) return this.blob(datapackId, p.hash);
    const parts: Uint8Array[] = new Array(p.parts.length);
    await pool(p.parts.map((h, i) => [h, i] as const), AT_ONCE, async ([h, i]) => {
      parts[i] = await this.blob(datapackId, h);
    });
    const joined = new Uint8Array(p.size);
    let at = 0;
    for (const part of parts) {
      joined.set(part, at);
      at += part.length;
    }
    if (sha256(joined) !== p.hash) throw new MekoError(`A file of the deck (${p.hash.slice(0, 12)}) didn't join back to what was saved.`);
    this.keep(joined);
    return joined;
  }

  /**
   * Saves a deck: every scene and asset as pieces, uploading only hashes not in `known`, four at a
   * time, then deck.json last, so a save that stops partway leaves the previous version the newest.
   * Assets must be in the cache (where adding one puts it) unless their pieces are known already.
   */
  async save(datapackId: string, deck: Deck, known: Set<string>, assetPieces: Record<string, Piece>,
    progress: (done: number, total: number) => void = () => undefined, now: Date = new Date()): Promise<SaveResult> {
    const uploads: Upload[] = [];
    const want = (u: Upload[]) => uploads.push(...u.filter((x) => !known.has(x.hash) && !uploads.some((y) => y.hash === x.hash)));
    const scenePieces: Record<string, Piece> = {};
    for (const id of Object.keys(deck.scenes)) {
      const s = storeAs(jsonBytes(sceneFile(deck, id)), sceneFilename(id));
      scenePieces[id] = s.piece;
      want(s.uploads);
    }
    const pieces: Record<string, Piece> = {};
    for (const [name, a] of Object.entries(deck.assets)) {
      const prior = assetPieces[a.hash];
      if (prior && pieceHashes(prior).every((h) => known.has(h))) {
        pieces[a.hash] = prior;
        continue;
      }
      const bytes = this.cached(a.hash);
      if (!bytes) throw new MekoError(`Asset "${name}" isn't on this computer or in the deck's datapack. Add it again.`);
      const s = storeAs(bytes, assetFilename(name));
      pieces[a.hash] = s.piece;
      want(s.uploads);
    }
    const byName = Object.fromEntries(Object.entries(deck.assets).map(([n, a]) => [n, pieces[a.hash]!]));
    const savedAt = now.toISOString();
    const manifest = jsonBytes(toManifest(deck, scenePieces, byName, savedAt));
    let root = manifest;
    if (manifest.length > INDEX_BYTES) {
      const s = storeAs(manifest, MANIFEST_FILE);
      const piece: Piece = s.piece.parts ? s.piece : { ...s.piece, parts: [s.piece.hash] };
      want(s.piece.parts ? s.uploads : [{ filename: partFilename(s.piece.hash, 0), bytes: manifest, hash: s.piece.hash }]);
      root = jsonBytes({ format: POINTER_FORMAT, version: STORE_VERSION, savedAt, manifest: piece });
    }
    const total = uploads.length + 1;
    let done = 0;
    progress(done, total);
    await pool(uploads, AT_ONCE, async (u) => {
      const got = await this.meko.putArtifact(datapackId, u.filename, u.bytes, u.filename.endsWith('.json') ? 'application/json' : 'application/octet-stream');
      if (got !== u.hash) throw new MekoError(`Meko kept ${u.filename} under another hash than Presenter made.`);
      known.add(u.hash);
      progress(++done, total);
    });
    const manifestHash = await this.meko.putArtifact(datapackId, MANIFEST_FILE, root, 'application/json');
    this.keep(root);
    progress(total, total);
    const all = new Set(known);
    all.add(manifestHash);
    return { manifestHash, savedAt, known: [...all], assetPieces: { ...assetPieces, ...pieces }, uploaded: uploads.length + 1 };
  }

  /** The deck a saved deck.json describes, by that deck.json's hash. Assets stay in Meko until they're shown. */
  async load(datapackId: string, manifestHash: string): Promise<Loaded> {
    const root = readRoot(bytesJson(await this.blob(datapackId, manifestHash)));
    const known = new Set<string>([manifestHash]);
    let m: Manifest;
    if ('pointer' in root) {
      m = readManifest(bytesJson(await this.piece(datapackId, root.pointer.manifest)));
      for (const h of pieceHashes(root.pointer.manifest)) known.add(h);
    } else {
      m = root.manifest;
    }
    const scenes: Record<string, SceneFile> = {};
    await pool(Object.entries(m.scenes), AT_ONCE, async ([id, s]) => {
      scenes[id] = readSceneFile(bytesJson(await this.piece(datapackId, s.piece)), id);
      for (const h of pieceHashes(s.piece)) known.add(h);
    });
    const assetPieces: Record<string, Piece> = {};
    for (const a of Object.values(m.assets)) {
      assetPieces[a.hash] = a.piece;
      for (const h of pieceHashes(a.piece)) known.add(h);
    }
    return { deck: fromManifest(m, scenes), savedAt: m.savedAt, manifestHash, known: [...known], assetPieces };
  }

  /** Every saved version of a deck, newest first. */
  async versions(datapackId: string): Promise<{ hash: string; savedAt: string }[]> {
    const rows = await this.meko.listArtifacts(datapackId, MANIFEST_FILE);
    return rows.map((r) => ({ hash: r.hash, savedAt: r.createdAt }));
  }
}
