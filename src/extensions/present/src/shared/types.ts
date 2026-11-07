// The API contract between Present's server and its pages: every route's request and reply, and
// every WebSocket message. Change it first.

import type { DatapackGroups, MekoAt } from './datapacks.js';
import type { Brief, Deck, DeckSources } from './deck.js';
import type { Op } from './ops.js';
import type { NavAction, RunState } from './run.js';

export const API_VERSION = 1;

/** GET /api/health */
export interface Health {
  ok: true;
  version: string;
  api: number;
  meko: 'ready' | 'none' | 'failed';
  /** Where decks are saved: Meko's Meko Cloud link, or the Meko on this computer; null with neither. */
  mekoAt: MekoAt | null;
  claude: boolean;
}

/** GET /api/datapacks */
export type DatapacksReply = DatapackGroups;

/** A deck in the list: its datapack, and whether this computer has a draft of it. */
export interface DeckSummary {
  id: string;
  name: string;
  title: string;
  savedAt: string | null;
  draft: boolean;
  /** A draft whose datapack is in another Meko than the one Present uses now: it can't be opened. */
  elsewhere?: MekoAt;
}

/** POST /api/decks */
export interface CreateDeckRequest {
  title: string;
  brief: Brief;
  sources: DeckSources;
}

/** A deck as the editor holds it: the working draft, and what Save would change. */
export interface DeckState {
  id: string;
  name: string;
  deck: Deck;
  /** The draft differs from the last save. */
  dirty: boolean;
  savedAt: string | null;
  canUndo: boolean;
  canRedo: boolean;
  /** A save in progress: pieces uploaded so far of all. */
  saving: { done: number; total: number } | null;
}

/** POST /api/decks/:id/ops: hand edits. */
export interface OpsRequest {
  ops: Op[];
}

/** GET /api/decks/:id/versions */
export interface DeckVersion {
  hash: string;
  savedAt: string;
}

export type JobKind = 'map' | 'build' | 'ask';
export type JobStatus = 'queued' | 'running' | 'done' | 'failed' | 'cancelled';

/** What an ask is about: the editor's selection. */
export interface AskScope {
  kind: 'block' | 'slide' | 'scene' | 'deck';
  scene?: string;
  slide?: string;
  block?: number;
  /** The variant whose blocks `block` counts, the one the editor shows. */
  variant?: string;
}

/** POST /api/decks/:id/jobs */
export type JobRequest =
  | { kind: 'map'; note?: string }
  | { kind: 'build'; scenes?: string[] }
  | { kind: 'ask'; instruction: string; scope: AskScope };

export interface Job {
  id: string;
  deckId: string;
  kind: JobKind;
  title: string;
  status: JobStatus;
  /** What Claude is doing, newest last ("reading ybdb/xcluster", "searching the web"). */
  activity: string[];
  error: string | null;
  changeSet: string | null;
  createdAt: number;
  endedAt: number | null;
}

/** Claude's proposed changes, previewed before any of them lands. */
export interface ChangeSet {
  id: string;
  deckId: string;
  jobId: string;
  title: string;
  summary: string;
  ops: Op[];
  /** The deck the ops were made against; applying checks them against the current one. */
  createdAt: number;
  status: 'open' | 'applied' | 'discarded';
}

/** POST /api/changesets/:id/apply: the indexes of the ops to apply. */
export interface ApplyRequest {
  take: number[];
}

/** POST /api/decks/:id/runs */
export interface StartRunRequest {
  variant?: string;
}

/** Messages on /ws/events: every open page hears every deck's changes and filters its own. */
export type ServerEvent =
  | { type: 'deck'; state: DeckState }
  | { type: 'job'; job: Job }
  | { type: 'changeset'; changeSet: ChangeSet }
  | { type: 'run'; run: RunState };

/** Messages a console or audience window sends on /ws/runs/:id. */
export type RunClientMessage = { type: 'nav'; nav: NavAction };

/** Messages on /ws/runs/:id: the run, and the deck it plays. */
export type RunServerMessage =
  | { type: 'run'; run: RunState }
  | { type: 'deck'; deck: Deck }
  | { type: 'error'; error: string };

/** An error reply from any route. */
export interface ApiError {
  error: string;
  problems?: string[];
}
