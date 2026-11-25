// Live runs: one room per talk, which the console and the audience window both join. The server
// holds the position, so every window shows the same slide, and keeps it on disk, so a restart
// resumes where the talk was.

import { randomUUID } from 'node:crypto';
import { mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import type { Deck } from '../shared/deck.js';
import { fitRun, navigate, startRun, type NavAction, type RunState } from '../shared/run.js';
import type { RunServerMessage } from '../shared/types.js';
import { HttpError, type DeckStore } from './decks.js';

/** How long after its last move a run counts as live with no window open: a window reconnecting. */
export const LIVE_AFTER_MOVE_MS = 5 * 60_000;

export interface RunPeer {
  send(m: RunServerMessage): void;
}

export class Runs {
  private runs = new Map<string, RunState>();
  private peers = new Map<string, Set<RunPeer>>();
  /** Each run's moves, one after another: two presses in one packet both move it. */
  private navs = new Map<string, Promise<unknown>>();
  /** The deck each deck's runs last heard, so a save's progress (the same deck) isn't sent again. */
  private sent = new Map<string, Deck>();
  /** When each run last moved. */
  private moved = new Map<string, number>();

  constructor(private decks: DeckStore, private dir: string) {
    mkdirSync(dir, { recursive: true, mode: 0o700 });
  }

  private file(id: string): string {
    if (!/^[0-9a-f-]{36}$/.test(id)) throw new HttpError(404, 'There\'s no such run.');
    return join(this.dir, `${id}.json`);
  }

  private persist(r: RunState): void {
    const f = this.file(r.id);
    writeFileSync(`${f}.tmp`, JSON.stringify(r), { mode: 0o600 });
    renameSync(`${f}.tmp`, f);
  }

  async start(deckId: string, variant?: string): Promise<RunState> {
    const deck = await this.decks.deck(deckId);
    const r = startRun(deck, randomUUID(), deckId, variant ?? deck.variants[0]!, Date.now());
    this.runs.set(r.id, r);
    this.persist(r);
    return r;
  }

  /** A run, from memory or from disk after a restart, fitted to the deck as it is now. */
  async get(id: string): Promise<{ run: RunState; deck: Deck }> {
    let r = this.runs.get(id);
    if (!r) {
      try {
        r = JSON.parse(readFileSync(this.file(id), 'utf8')) as RunState;
      } catch {
        throw new HttpError(404, 'There\'s no such run.');
      }
    }
    const deck = await this.decks.deck(r.deckId);
    r = fitRun(deck, r);
    this.runs.set(id, r);
    return { run: r, deck };
  }

  async join(id: string, peer: RunPeer): Promise<() => void> {
    const { run, deck } = await this.get(id);
    let set = this.peers.get(id);
    if (!set) this.peers.set(id, (set = new Set()));
    set.add(peer);
    peer.send({ type: 'deck', deck });
    peer.send({ type: 'run', run });
    return () => set!.delete(peer);
  }

  nav(id: string, a: NavAction): Promise<RunState> {
    const step = async () => {
      const { run, deck } = await this.get(id);
      const next = navigate(deck, run, a);
      this.runs.set(id, next);
      this.persist(next);
      this.moved.set(id, Date.now());
      for (const p of this.peers.get(id) ?? []) p.send({ type: 'run', run: next });
      return next;
    };
    const done = (this.navs.get(id) ?? Promise.resolve()).then(step, step);
    this.navs.set(id, done.catch(() => undefined));
    return done;
  }

  /**
   * Whether a talk is live, which a restart would interrupt: a run with a window open (the console
   * or the audience's), or one that moved in the last LIVE_AFTER_MOVE_MS (its windows reconnecting).
   */
  live(now = Date.now()): boolean {
    for (const set of this.peers.values()) if (set.size) return true;
    for (const at of this.moved.values()) if (now - at < LIVE_AFTER_MOVE_MS) return true;
    return false;
  }

  /** Tells a deck's open runs that it changed: the windows redraw, and a position that's gone moves. */
  deckChanged(deckId: string, deck: Deck): void {
    if (this.sent.get(deckId) === deck) return;
    this.sent.set(deckId, deck);
    for (const [id, r] of this.runs) {
      if (r.deckId !== deckId) continue;
      try {
        const fitted = fitRun(deck, r);
        this.runs.set(id, fitted);
        this.persist(fitted);
        for (const p of this.peers.get(id) ?? []) {
          p.send({ type: 'deck', deck });
          p.send({ type: 'run', run: fitted });
        }
      } catch {
        /* one run's trouble doesn't keep the others from the new deck */
      }
    }
  }
}
