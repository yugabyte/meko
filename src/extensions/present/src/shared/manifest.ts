// How a deck is stored in its Meko datapack: pieces that only Presenter puts back together, each a
// Meko artifact of PART_BYTES at most (Meko keeps an artifact to 5 MiB). deck.json is the manifest:
// everything the map needs, and every other piece by its SHA-256. Each scene is a piece; each asset
// is a piece; anything bigger than PART_BYTES is stored as parts. Pure: the server does the IO.

import { DECK_FORMAT, DECK_VERSION, deckProblems, DeckError, type AssetRef, type Brief, type Deck, type DeckSources, type Edge, type Scene, type Slide } from './deck.js';

/** The most one artifact Presenter writes holds: room under Meko's 5 MiB, and under ~5.4 MiB as base64. */
export const PART_BYTES = 4 * 1024 * 1024;
/** A manifest bigger than this goes up in parts, and deck.json points at them. */
export const INDEX_BYTES = 1024 * 1024;

export const MANIFEST_FILE = 'deck.json';
export const MANIFEST_FORMAT = 'present-manifest';
export const POINTER_FORMAT = 'present-manifest-parts';
export const SCENE_FORMAT = 'present-scene';
export const STORE_VERSION = 1;

/** How one stored thing is kept: whole when it fits in a part, else as parts in order. */
export interface Piece {
  hash: string;
  size: number;
  parts?: string[];
}

export interface Manifest {
  format: typeof MANIFEST_FORMAT;
  version: typeof STORE_VERSION;
  /** Makes every saved manifest's bytes differ, so the newest artifact is always the latest save. */
  savedAt: string;
  title: string;
  brief: Brief;
  sources: DeckSources;
  variants: string[];
  start: string;
  edges: Edge[];
  scenes: Record<string, Scene & { piece: Piece }>;
  assets: Record<string, AssetRef & { piece: Piece }>;
}

export interface ManifestPointer {
  format: typeof POINTER_FORMAT;
  version: typeof STORE_VERSION;
  savedAt: string;
  manifest: Piece;
}

export interface SceneFile {
  format: typeof SCENE_FORMAT;
  version: typeof STORE_VERSION;
  scene: string;
  slides: Slide[];
}

export const sceneFilename = (id: string) => `scenes/${id}.json`;
export const assetFilename = (name: string) => `assets/${name}`;
export const partFilename = (whole: string, n: number) => `parts/${whole}.${n}`;

/** Bytes cut into PART_BYTES pieces; one piece for anything that fits (or is empty). */
export function splitBytes(b: Uint8Array, size = PART_BYTES): Uint8Array[] {
  if (b.length <= size) return [b];
  const out: Uint8Array[] = [];
  for (let at = 0; at < b.length; at += size) out.push(b.subarray(at, Math.min(at + size, b.length)));
  return out;
}

export function joinBytes(parts: Uint8Array[]): Uint8Array {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.length;
  }
  return out;
}

const enc = new TextEncoder();
const dec = new TextDecoder();
export const jsonBytes = (v: unknown) => enc.encode(JSON.stringify(v));
export const bytesJson = (b: Uint8Array): unknown => JSON.parse(dec.decode(b));

/** A scene's piece: its slides, in order. */
export function sceneFile(deck: Deck, id: string): SceneFile {
  const scene = deck.scenes[id]!;
  return { format: SCENE_FORMAT, version: STORE_VERSION, scene: id, slides: scene.slides.map((s) => deck.slides[s]!) };
}

/** The manifest for a deck, given how each scene and asset was stored. */
export function toManifest(deck: Deck, scenePieces: Record<string, Piece>, assetPieces: Record<string, Piece>, savedAt: string): Manifest {
  const scenes: Manifest['scenes'] = {};
  for (const [id, s] of Object.entries(deck.scenes)) scenes[id] = { ...s, piece: scenePieces[id]! };
  const assets: Manifest['assets'] = {};
  for (const [name, a] of Object.entries(deck.assets)) assets[name] = { ...a, piece: assetPieces[name]! };
  return {
    format: MANIFEST_FORMAT, version: STORE_VERSION, savedAt,
    title: deck.title, brief: deck.brief, sources: deck.sources, variants: deck.variants, start: deck.start, edges: deck.edges,
    scenes, assets,
  };
}

const isObj = (v: unknown): v is Record<string, unknown> => !!v && typeof v === 'object' && !Array.isArray(v);
const HASH = /^[0-9a-f]{64}$/;

function isPiece(v: unknown): v is Piece {
  return isObj(v) && typeof v.hash === 'string' && HASH.test(v.hash) && typeof v.size === 'number'
    && (v.parts === undefined || (Array.isArray(v.parts) && v.parts.every((p) => typeof p === 'string' && HASH.test(p))));
}

/** What deck.json holds: the manifest, or a pointer to its parts. Throws when it's neither. */
export function readRoot(x: unknown): { manifest: Manifest } | { pointer: ManifestPointer } {
  if (isObj(x) && x.format === POINTER_FORMAT && isPiece(x.manifest)) return { pointer: x as unknown as ManifestPointer };
  return { manifest: readManifest(x) };
}

export function readManifest(x: unknown): Manifest {
  if (!isObj(x) || x.format !== MANIFEST_FORMAT || x.version !== STORE_VERSION || !isObj(x.scenes) || !isObj(x.assets)) {
    throw new DeckError([`deck.json isn't a ${MANIFEST_FORMAT} version ${STORE_VERSION}.`]);
  }
  const m = x as unknown as Manifest;
  for (const [id, s] of Object.entries(m.scenes)) if (!isPiece(s.piece)) throw new DeckError([`Scene "${id}" has no stored piece.`]);
  for (const [n, a] of Object.entries(m.assets)) if (!isPiece(a.piece)) throw new DeckError([`Asset "${n}" has no stored piece.`]);
  return m;
}

export function readSceneFile(x: unknown, id: string): SceneFile {
  if (!isObj(x) || x.format !== SCENE_FORMAT || x.scene !== id || !Array.isArray(x.slides)) throw new DeckError([`The stored scene "${id}" can't be read.`]);
  return x as unknown as SceneFile;
}

/** The deck a manifest and its scene files make, checked. */
export function fromManifest(m: Manifest, scenes: Record<string, SceneFile>): Deck {
  const deck: Deck = {
    format: DECK_FORMAT, version: DECK_VERSION, title: m.title, brief: m.brief, sources: m.sources, variants: m.variants,
    start: m.start, edges: m.edges, scenes: {}, slides: {}, assets: {},
  };
  for (const [id, s] of Object.entries(m.scenes)) {
    const { piece: _piece, ...scene } = s;
    const file = scenes[id];
    if (!file) throw new DeckError([`Scene "${scene.title}" wasn't loaded.`]);
    deck.scenes[id] = { ...scene, slides: file.slides.map((sl) => sl.id) };
    for (const sl of file.slides) deck.slides[sl.id] = sl;
  }
  for (const [name, a] of Object.entries(m.assets)) deck.assets[name] = { hash: a.hash, size: a.size, type: a.type };
  const p = deckProblems(deck);
  if (p.length) throw new DeckError(p);
  return deck;
}
