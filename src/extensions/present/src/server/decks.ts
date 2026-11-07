// The decks Present works on: each one's working draft (kept in its folder after every change, so a
// crash loses nothing), one undo history for hand edits and Claude's, and saving to and loading from
// the deck's datapack through the piece store.

import { existsSync, mkdirSync, readdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { DECK_PREFIX, deckDatapackName, LOCAL_MEKO, mekoLabel, reopenHint, sameMeko, type DatapackRow, type MekoAt } from '../shared/datapacks.js';
import { ASSET_NAME, assetFits, checkDeck, DeckError, newDeck, own, type Deck } from '../shared/deck.js';
import type { Piece } from '../shared/manifest.js';
import { applyOps, type Op } from '../shared/ops.js';
import type { CreateDeckRequest, DeckState, DeckSummary, DeckVersion } from '../shared/types.js';
import { MekoError, type MekoPort } from './meko.js';
import type { Paths } from './paths.js';
import { PieceStore, sha256 } from './pieces.js';

/** How many steps Undo can go back. */
const UNDO_MAX = 100;

export class HttpError extends Error {
  constructor(readonly status: number, message: string, readonly problems?: string[]) {
    super(message);
  }
}

/** What's kept on disk for a deck: decks/<id>/state.json. */
interface Stored {
  id: string;
  name: string;
  deck: Deck;
  /** The deck as last saved (or loaded), to tell whether there's anything to save. */
  savedJson: string | null;
  savedAt: string | null;
  manifestHash: string | null;
  known: string[];
  assetPieces: Record<string, Piece>;
  /**
   * The Meko its datapack is in. A draft from before Present kept it is the current Meko's when that
   * Meko has its datapack, else the Meko on this computer's (`untagged`). While the Meko on this
   * computer links Meko Cloud, it serves both, and a deck in one of its own datapacks
   * is its own (`localIds`).
   */
  meko?: MekoAt;
}

interface Entry extends Stored {
  undo: Deck[];
  redo: Deck[];
  saving: { done: number; total: number } | null;
}

function writeJson(file: string, v: unknown): void {
  const tmp = `${file}.tmp`;
  writeFileSync(tmp, JSON.stringify(v), { mode: 0o600 });
  renameSync(tmp, file);
}

/**
 * Whether Present, using `at`, serves a deck saved in `m`: the same Meko, or the Meko on this
 * computer while it links Meko Cloud and serves its own datapacks (`here`, from `localIds`), this
 * deck's among them.
 */
function serves(at: MekoAt, m: MekoAt, id: string, here: Set<string> | null): boolean {
  return sameMeko(m, at) || (m.kind === 'local' && !!here?.has(id));
}

export class DeckStore {
  private open = new Map<string, Entry>();
  /** First loads under way, so two requests for a deck not open yet share one Entry. */
  private loading = new Map<string, Promise<Entry>>();
  private listeners: ((s: DeckState) => void)[] = [];
  readonly pieces: PieceStore;

  constructor(private paths: Paths, private meko: MekoPort, private emit: (s: DeckState) => void = () => undefined) {
    mkdirSync(paths.decks, { recursive: true, mode: 0o700 });
    this.pieces = new PieceStore(meko, paths.cache);
  }

  /** Hears every change to any deck: hand edits, Claude's, undo, restore and saves alike. */
  listen(fn: (s: DeckState) => void): void {
    this.listeners.push(fn);
  }

  /** A deck's own folder: its draft, and its Claude session's working folder (work/). */
  dir(id: string): string {
    if (!/^[A-Za-z0-9_-]+$/.test(id)) throw new HttpError(400, 'That isn\'t a deck id.');
    return join(this.paths.decks, id);
  }

  private stateFile(id: string): string {
    return join(this.dir(id), 'state.json');
  }

  private persist(e: Entry): void {
    mkdirSync(this.dir(e.id), { recursive: true, mode: 0o700 });
    const { undo: _u, redo: _r, saving: _s, ...stored } = e;
    writeJson(this.stateFile(e.id), stored);
  }

  stateOf(e: Entry): DeckState {
    return {
      id: e.id, name: e.name, deck: e.deck, dirty: JSON.stringify(e.deck) !== e.savedJson, savedAt: e.savedAt,
      canUndo: e.undo.length > 0, canRedo: e.redo.length > 0, saving: e.saving,
    };
  }

  private changed(e: Entry): DeckState {
    const s = this.stateOf(e);
    this.emit(s);
    for (const fn of this.listeners) fn(s);
    return s;
  }

  /** The decks: every present/deck/* datapack, and drafts here whose datapack Meko didn't list. */
  async list(): Promise<DeckSummary[]> {
    const local = new Map<string, Stored>();
    for (const id of existsSync(this.paths.decks) ? readdirSync(this.paths.decks) : []) {
      const s = this.readStored(id);
      if (s) local.set(id, s);
    }
    let rows: DatapackRow[] = [];
    let listed = false;
    try {
      rows = (await this.meko.listDatapacks()).filter((d) => d.name.startsWith(DECK_PREFIX));
      listed = true;
    } catch {
      /* drafts alone, when Meko can't be reached */
    }
    const at = await this.meko.at();
    // Without the Meko on this computer's own list, its drafts aren't called another Meko's: opening
    // one says why it can't be told (mine).
    let here: Set<string> | null = null;
    let unsure = false;
    if (at?.kind === 'cloud') {
      try {
        here = await this.meko.localIds();
      } catch {
        unsure = true;
      }
    }
    const out: DeckSummary[] = rows.map((d) => {
      const s = this.open.get(d.id) ?? local.get(d.id);
      return { id: d.id, name: d.name, title: s?.deck.title ?? d.name.slice(DECK_PREFIX.length), savedAt: s?.savedAt ?? null, draft: !!s };
    });
    for (const [id, s] of local) {
      if (out.some((d) => d.id === id)) continue;
      // The current Meko didn't list it: an untagged draft is the Meko on this computer's.
      const m = s.meko ?? (listed ? LOCAL_MEKO : undefined);
      const elsewhere = at && m && !serves(at, m, id, here) && !(unsure && m.kind === 'local') ? { elsewhere: m } : {};
      out.push({ id, name: s.name, title: s.deck.title, savedAt: s.savedAt, draft: true, ...elsewhere });
    }
    return out.sort((a, b) => a.title.localeCompare(b.title));
  }

  private readStored(id: string): Stored | null {
    try {
      const s = JSON.parse(readFileSync(this.stateFile(id), 'utf8')) as Stored;
      return s.id === id ? s : null;
    } catch {
      return null;
    }
  }

  /** Makes a deck: its datapack in Meko (present/deck/<slug>, -2, -3… when taken) and its first draft. */
  async create(req: CreateDeckRequest): Promise<DeckState> {
    const title = req.title.trim();
    if (!title) throw new HttpError(400, 'Give the deck a title.');
    // Read before Meko is asked anything: the datapack is made in the Meko Present uses now.
    const meko = (await this.meko.at()) ?? undefined;
    const deck = checkDeck(newDeck(title, req.brief, req.sources));
    const base = deckDatapackName(title);
    let row: DatapackRow | null = null;
    for (let n = 1; n <= 20 && !row; n++) {
      const name = n === 1 ? base : `${base.slice(0, 60 - String(n).length - 1)}-${n}`;
      try {
        row = await this.meko.createDatapack(name, `Present deck "${title.slice(0, 200)}"`);
      } catch (e) {
        if (!(e instanceof MekoError && /conflict|taken|exists/i.test(e.message))) throw e;
      }
    }
    if (!row) throw new HttpError(409, `Every name like ${base} is taken. Try another title.`);
    const e: Entry = { id: row.id, name: row.name, deck, savedJson: null, savedAt: null, manifestHash: null, known: [], assetPieces: {}, meko, undo: [], redo: [], saving: null };
    this.open.set(e.id, e);
    this.persist(e);
    return this.changed(e);
  }

  /**
   * A deck to edit, save or hand to Claude: refused when its draft belongs to another Meko than the
   * one Present uses now (Meko's link changed), whose datapack this Meko doesn't have. Playing it
   * (`deck()`) still works, so a talk under way outlives a change of the link. With no Meko found,
   * the draft can still be edited here.
   */
  private async mine(id: string): Promise<Entry> {
    const e = await this.entry(id);
    const at = await this.meko.at();
    if (!at) return e;
    if (!e.meko) {
      const m = await this.untagged(e.id, at);
      if (!m) return e;
      e.meko = m;
      this.persist(e);
    }
    // While the Meko on this computer links Meko Cloud, it serves its own datapacks too. A deck in one
    // is that Meko's, even one tagged as Meko Cloud's, which an earlier Meko led Present to do, so it
    // still opens once Meko Cloud is unlinked.
    // Unlinked, that Meko's own listing tells such a deck too.
    let here: Set<string> | null = null;
    if (at.kind === 'cloud') {
      try {
        here = await this.meko.localIds();
      } catch (err) {
        // Without the list, a deck of the Meko on this computer can't be told from one elsewhere.
        if (e.meko.kind === 'local') throw err;
      }
    }
    if (e.meko.kind === 'cloud' && (here?.has(e.id) || (at.kind === 'local' && (await this.listsHere(e.id))))) {
      e.meko = LOCAL_MEKO;
      this.persist(e);
    }
    // The same Meko Cloud may be named by another host: its MCP one when Present held the link's key,
    // its web app's now. The datapack is in one Meko only, so the Meko that lists it is the draft's.
    if (!sameMeko(e.meko, at) && e.meko.kind === 'cloud' && at.kind === 'cloud' && (await this.untagged(e.id, at)) === at) {
      e.meko = at;
      this.persist(e);
    }
    if (!serves(at, e.meko, e.id, here)) {
      throw new HttpError(409, `This deck is saved in ${mekoLabel(e.meko)}, and Present now uses ${mekoLabel(at)}. ${reopenHint(e.meko)}`);
    }
    return e;
  }

  /**
   * The Meko a draft from before Present kept it belongs to: the one Present uses now when it has
   * the deck's datapack, else the Meko on this computer (the only one an older Present used, short of
   * a hand-written config.json). A datapack the Meko on this computer serves itself while it links
   * Meko Cloud is its own, though it lists it with Meko Cloud's. Null while Meko can't be asked.
   */
  private async untagged(id: string, at: MekoAt): Promise<MekoAt | null> {
    try {
      if (at.kind === 'cloud' && (await this.meko.localIds())?.has(id)) return LOCAL_MEKO;
      return (await this.meko.listDatapacks()).some((d) => d.id === id) ? at : LOCAL_MEKO;
    } catch {
      return null;
    }
  }

  /** A deck to work on: its draft here, else its newest save in Meko. */
  async entry(id: string): Promise<Entry> {
    const open = this.open.get(id);
    if (open) return open;
    const stored = this.readStored(id);
    if (stored) {
      const e: Entry = { ...stored, undo: [], redo: [], saving: null };
      this.open.set(id, e);
      return e;
    }
    // One load per deck: a second one finishing later would replace the Entry, and edits made on the first.
    let loading = this.loading.get(id);
    if (!loading) {
      loading = this.load(id).finally(() => this.loading.delete(id));
      this.loading.set(id, loading);
    }
    return loading;
  }

  /** Whether the Meko on this computer, used unlinked, lists the datapack; false while it can't say. */
  private async listsHere(id: string): Promise<boolean> {
    try {
      return (await this.meko.listDatapacks()).some((d) => d.id === id);
    } catch {
      return false;
    }
  }

  /** A deck with no draft here, from its newest save in its datapack. */
  private async load(id: string): Promise<Entry> {
    // Read before Meko is asked anything: the deck comes from the Meko Present uses now, or the Meko
    // on this computer's own datapacks while it serves them beside Meko Cloud's.
    const at = (await this.meko.at()) ?? undefined;
    const meko = at?.kind === 'cloud' && (await this.meko.localIds())?.has(id) ? LOCAL_MEKO : at;
    const row = (await this.meko.listDatapacks()).find((d) => d.id === id);
    if (!row) throw new HttpError(404, 'There\'s no such deck.');
    const versions = await this.pieces.versions(id);
    if (!versions.length) throw new HttpError(404, `${row.name} has no saved deck yet.`);
    const loaded = await this.pieces.load(id, versions[0]!.hash);
    const e: Entry = {
      id, name: row.name, deck: loaded.deck, savedJson: JSON.stringify(loaded.deck), savedAt: loaded.savedAt,
      manifestHash: loaded.manifestHash, known: loaded.known, assetPieces: loaded.assetPieces, meko, undo: [], redo: [], saving: null,
    };
    this.open.set(id, e);
    this.persist(e);
    return e;
  }

  async state(id: string): Promise<DeckState> {
    return this.stateOf(await this.mine(id));
  }

  /** The deck to play: drafts of another Meko too. */
  async deck(id: string): Promise<Deck> {
    return (await this.entry(id)).deck;
  }

  /** Applies edits, checked; the deck before them goes on the undo history. */
  async apply(id: string, ops: Op[]): Promise<DeckState> {
    const e = await this.mine(id);
    // An asset whose bytes Present doesn't hold would make every save fail: files come in through addAsset().
    for (const o of ops) {
      if (o?.op === 'addAsset' && !(this.pieces.has(String(o.asset?.hash)) || Object.hasOwn(e.assetPieces, String(o.asset?.hash)))) {
        throw new HttpError(400, `The file for asset "${o.name}" isn't on this computer or in the deck's datapack. Add the file itself.`);
      }
    }
    let next: Deck;
    try {
      next = applyOps(e.deck, ops);
    } catch (err) {
      const problems = err instanceof DeckError ? err.problems : [(err as Error).message];
      throw new HttpError(400, `That change would break the deck: ${problems[0]}`, problems);
    }
    e.undo = [...e.undo, e.deck].slice(-UNDO_MAX);
    e.redo = [];
    e.deck = next;
    this.persist(e);
    return this.changed(e);
  }

  async undo(id: string): Promise<DeckState> {
    const e = await this.mine(id);
    const prev = e.undo.pop();
    if (prev) {
      e.redo.push(e.deck);
      e.deck = prev;
      this.persist(e);
    }
    return this.changed(e);
  }

  async redo(id: string): Promise<DeckState> {
    const e = await this.mine(id);
    const next = e.redo.pop();
    if (next) {
      e.undo.push(e.deck);
      e.deck = next;
      this.persist(e);
    }
    return this.changed(e);
  }

  /** Adds a file as an asset, refused here (not at save time) when it's over the limits. */
  async addAsset(id: string, name: string, type: string, bytes: Uint8Array): Promise<DeckState> {
    const e = await this.mine(id);
    if (!ASSET_NAME.test(name)) throw new HttpError(400, 'Name the file with letters, digits, dots, dashes and underscores.');
    const why = assetFits(e.deck, bytes.length, name);
    if (why) throw new HttpError(413, why);
    const hash = this.pieces.keep(bytes);
    return this.apply(id, [{ op: 'addAsset', name, asset: { hash, size: bytes.length, type: type || 'application/octet-stream' } }]);
  }

  /**
   * An asset's file in the cache, fetched from the deck's datapack first (checked against its hash)
   * when it isn't there, for the asset route to send as it is or in ranges.
   */
  async assetFile(id: string, name: string): Promise<{ file: string; hash: string; type: string }> {
    const e = await this.entry(id);
    const a = own(e.deck.assets, name);
    if (!a) throw new HttpError(404, 'The deck has no such asset.');
    if (!this.pieces.has(a.hash)) {
      const piece = Object.hasOwn(e.assetPieces, a.hash) ? e.assetPieces[a.hash] : undefined;
      if (!piece) throw new HttpError(404, 'The asset isn\'t on this computer or in the deck\'s datapack.');
      await this.pieces.piece(id, piece);
    }
    return { file: this.pieces.path(a.hash), hash: a.hash, type: a.type };
  }

  /** Saves the draft to the deck's datapack: only the pieces that changed, then a new deck.json. */
  async save(id: string): Promise<DeckState> {
    const e = await this.mine(id);
    if (e.saving) throw new HttpError(409, 'The deck is being saved already.');
    const deck = e.deck;
    e.saving = { done: 0, total: 1 };
    this.changed(e);
    // Pieces that went up stay known even when the save stops partway, so a retry skips them.
    const known = new Set(e.known);
    try {
      const r = await this.pieces.save(id, deck, known, e.assetPieces, (done, total) => {
        e.saving = { done, total };
        this.changed(e);
      });
      e.known = r.known;
      e.assetPieces = r.assetPieces;
      e.savedJson = JSON.stringify(deck);
      e.savedAt = r.savedAt;
      e.manifestHash = r.manifestHash;
    } finally {
      e.known = [...new Set([...e.known, ...known])];
      e.saving = null;
      this.persist(e);
      // Every page hears that the save ended, failed or not, so none keeps showing it as running.
      this.changed(e);
    }
    return this.stateOf(e);
  }

  async versions(id: string): Promise<DeckVersion[]> {
    await this.mine(id);
    return this.pieces.versions(id);
  }

  /** Makes a saved version the draft (Undo goes back); Save makes it the newest. */
  async restore(id: string, hash: string): Promise<DeckState> {
    const e = await this.mine(id);
    if (!/^[0-9a-f]{64}$/.test(hash)) throw new HttpError(400, 'That isn\'t a version.');
    const loaded = await this.pieces.load(id, hash);
    for (const h of loaded.known) if (!e.known.includes(h)) e.known.push(h);
    e.assetPieces = { ...e.assetPieces, ...loaded.assetPieces };
    e.undo = [...e.undo, e.deck].slice(-UNDO_MAX);
    e.redo = [];
    e.deck = loaded.deck;
    this.persist(e);
    return this.changed(e);
  }

  /** The hash a deck's saved form would have, for tests and the version list. */
  static fingerprint(deck: Deck): string {
    return sha256(new TextEncoder().encode(JSON.stringify(deck)));
  }
}
