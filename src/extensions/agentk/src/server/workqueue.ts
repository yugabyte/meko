// The work queue: the user's items across projects, their links and sessions, and the sync from GitHub and Slack.
// The items are kept in Meko Cloud (workstore.ts, found or made by workcloud.ts); this keeps the open ones in
// memory, and each item's session on this computer.
import { randomUUID } from 'node:crypto';
import { parseWorkLink, workLinkKey } from '../shared/worklinks.js';
import {
  WORK_LINK_MAX, WORK_LINKS_MAX, WORK_NOTES_MAX, WORK_TITLE_MAX,
  type CloseWorkItemsRequest, type CreateWorkItemRequest, type ReorderWorkItemsRequest, type Stage, type UpdateWorkItemRequest, type WorkItem, type WorkItemSource,
  type WorkLink, type WorkLinkMatch, type WorkSourceResult, type WorkSync,
} from '../shared/types.js';
import { all, get, kvDelete, kvGet, kvSet, run, tx, type Db } from './db.js';
import { REVIEW_REQUESTS_MAX, type ReviewRequest } from './github.js';
import { errMsg } from './log.js';
import { HttpError } from './projects.js';
import type { SlackListItem } from './slack.js';
import { isItemId, unreachable, type AmpWorkStore, type StoredItem } from './workstore.js';

/** kv: '1' while Slack is synced on the timer too, which a Sync that reached Slack turns on and any failure off. */
const SLACK_AUTO_KEY = 'work-slack-auto';
const CLOSED_MAX = 200;
const BY_LINK_MAX = 20;
const SYNCED_TITLE_MAX = 100;

export interface WorkQueueDeps {
  /** state.db: the session links (work_item_sessions), the closes waiting for Meko Cloud, and the items from before the move. */
  db: Db;
  changed: () => void;
  warn: (msg: string, detail?: string) => void;
  /** The open PRs asking for the user's review. */
  reviewRequests: () => Promise<ReviewRequest[]>;
  /** The messages on the user's Slack list; throws with what went wrong. */
  readSlack: () => Promise<SlackListItem[]>;
  /** Whether Claude Code can run, which the Slack read needs. */
  claudeAvailable: () => boolean;
  /** A GitHub PR's or issue's title, for an item added with a link and no title. */
  githubTitle: (repo: string, number: number) => Promise<string>;
  /** `[work_queue]` as it is now: the sources the sync reads and how often it runs by itself. */
  settings: () => { github: boolean; slack: boolean; syncMinutes: number };
  /** Why there's no database to use yet, as the 409 says it (WorkCloud: not linked, not set up…). */
  notReady: () => string;
  /** A query couldn't reach the database: WorkCloud reads the connection record again, which another computer may have changed. */
  unreachable: (e: unknown) => void;
  /** A read reached the database again after failing. */
  reached?: () => void;
}

/**
 * kv: how many of this computer's items from before the move were copied into Meko Cloud, and the newest's
 * time (copyLocal): a release before the move, after a rollback, may add more, which the next copy takes.
 */
export const LOCAL_COPIED_KEY = 'work-local-copied';
/** kv: the review requests this computer's GitHub sync has found, the only ones it marks Done on GitHub. */
const GITHUB_SEEN_KEY = 'work-github-seen';
const GITHUB_SEEN_MAX = 1000;

/**
 * Links a session to an item (`sessionId` null: unlinks it), one to one: the session leaves any other
 * item first, and the item's own session is replaced. On this computer only (work_item_sessions): a
 * session exists only where it runs. Call inside a transaction with the writes it goes with.
 */
export function setItemSession(db: Db, itemId: string, sessionId: string | null): void {
  run(db, 'DELETE FROM work_item_sessions WHERE item_id = ? OR session_id = ?', itemId, sessionId);
  if (sessionId) run(db, 'INSERT INTO work_item_sessions(item_id, session_id) VALUES (?, ?)', itemId, sessionId);
  // The same in the table from before the move, which a release before it reads after a rollback, and which
  // every start copies back from (WORK_LINKS_BACK in db.ts): kept in step, so a link removed here stays removed.
  run(db, 'UPDATE work_items SET session_id = NULL WHERE id = ? OR session_id = ?', itemId, sessionId);
  if (sessionId) run(db, 'UPDATE work_items SET session_id = ? WHERE id = ?', sessionId, itemId);
}

/** The session linked to an item on this computer, or null. */
export function sessionOfItem(db: Db, itemId: string): string | null {
  return get<{ session_id: string }>(db, 'SELECT session_id FROM work_item_sessions WHERE item_id = ?', itemId)?.session_id ?? null;
}

/** The item a session works on, or null. */
export function itemOfSession(db: Db, sessionId: string): string | null {
  return get<{ item_id: string }>(db, 'SELECT item_id FROM work_item_sessions WHERE session_id = ?', sessionId)?.item_id ?? null;
}

function text(v: unknown, what: string, max: number): string {
  if (typeof v !== 'string') throw new HttpError(400, `The ${what} must be text.`);
  const t = v.trim();
  if (t.length > max) throw new HttpError(400, `Keep the ${what} to ${max} characters.`);
  return t;
}

/** The links as typed, read and each kept once, or a 400 naming the first that isn't one. */
export function checkLinks(v: unknown): WorkLink[] {
  if (!Array.isArray(v) || !v.every((l) => typeof l === 'string')) throw new HttpError(400, 'Give the links as a list of text.');
  const typed = v.map((l) => l.trim()).filter(Boolean);
  if (typed.length > WORK_LINKS_MAX) throw new HttpError(400, `A work item takes at most ${WORK_LINKS_MAX} links.`);
  const out = new Map<string, WorkLink>();
  for (const t of typed) {
    if (t.length > WORK_LINK_MAX) throw new HttpError(400, `Keep each link to ${WORK_LINK_MAX} characters.`);
    const l = parseWorkLink(t);
    if (!l) throw new HttpError(400, `"${t.slice(0, 80)}" isn't a link Kode can read. Paste a GitHub, Jira or Slack link, or any https:// link.`);
    if (!out.has(l.key)) out.set(l.key, l);
  }
  return [...out.values()];
}

/** A synced message's title: its first line, cut to SYNCED_TITLE_MAX characters. */
/** A Slack list row's notes, started with its reporter the way Slack starts a message with its author. */
function slackNotes(m: SlackListItem): string {
  const notes = m.reporter && m.notes ? `${m.reporter}: ${m.notes}` : m.notes || (m.reporter && `From ${m.reporter}`);
  return notes.slice(0, WORK_NOTES_MAX);
}

function firstLine(s: string, fallback: string): string {
  const line = s.split('\n').find((l) => l.trim())?.trim() ?? '';
  if (!line) return fallback;
  return line.length > SYNCED_TITLE_MAX ? `${line.slice(0, SYNCED_TITLE_MAX - 1)}…` : line;
}

/** A sync source's read worked, and the database couldn't be reached to add what it found. */
class StoreLost extends Error {
  constructor(cause: unknown) {
    super(errMsg(cause), { cause });
  }
}

export class WorkQueue {
  private syncing = false;
  /** When the timer last synced (it starts counting at startup). */
  private lastAuto = Date.now();
  private last: { at: number | null; github: WorkSourceResult | null; slack: WorkSourceResult | null } = { at: null, github: null, slack: null };
  /** The database in Meko Cloud, once WorkCloud has one; null before set-up (or with Meko unlinked). */
  private store: AmpWorkStore | null = null;
  /** The database is a newer Kode's: read, never written, here. */
  private readOnly = false;
  /** Makes or updates the database's tables, until it has worked once. */
  private prepare: (() => Promise<boolean>) | null = null;
  /** The open items as last read (D2): what state() and the session starts read, so neither waits on the network. */
  private items: StoredItem[] = [];
  /**
   * Why the last query didn't reach the database, or why the first read failed (its tables, the copy); null
   * when it worked. The last list read stays meanwhile.
   */
  private reachError: string | null = null;
  /** Reads are numbered so one that started before a write and ended after it can't put the older list back. */
  private readSeq = 0;
  private shownSeq = 0;

  constructor(private readonly deps: WorkQueueDeps) {}

  private get db(): Db {
    return this.deps.db;
  }

  /**
   * Starts using a database: `prepare` makes or updates its tables (true: it's a newer Kode's, so read-only
   * here), then this computer's items from before the move are copied in, then the queue is read. Each read
   * tries again what hasn't worked yet, and writes wait for it (503). Throws when the first read fails: it's
   * attached then, and the state says why (unreachable, or the database's error).
   */
  async attach(store: AmpWorkStore, prepare: () => Promise<boolean>): Promise<void> {
    this.store = store;
    this.prepare = prepare;
    this.readOnly = false;
    this.items = [];
    this.reachError = null;
    await this.read(true);
  }

  /** Stops using the database (Meko unlinked, or another being attached): the queue is empty here. Session links stay. */
  async detach(): Promise<void> {
    const s = this.store;
    this.store = null;
    this.prepare = null;
    this.readOnly = false;
    this.items = [];
    this.reachError = null;
    this.deps.changed();
    await s?.sql.close().catch(() => undefined);
  }

  /** There's a database, whether or not it answered last time. */
  get attached(): boolean {
    return !!this.store;
  }

  get isReadOnly(): boolean {
    return this.readOnly;
  }

  /** Why the last query didn't reach the database, or null. */
  get unreachableError(): string | null {
    return this.reachError;
  }

  private toItem(r: StoredItem): WorkItem {
    return {
      id: r.id, title: r.title, notes: r.notes, links: r.links, status: r.status, source: r.source, sessionId: sessionOfItem(this.db, r.id),
      unplaced: r.unplaced, githubDone: r.githubDoneAt != null, createdAt: r.createdAt, closedAt: r.closedAt,
    };
  }

  /** An item, open or closed, from the database. */
  async get(id: string): Promise<WorkItem> {
    const r = await this.query((s) => s.get(id));
    if (!r) throw new HttpError(404, 'That work item no longer exists.');
    return this.toItem(r);
  }

  /** The open items, in the queue's order, as last read: empty before set-up. */
  list(): WorkItem[] {
    return this.items.map((r) => this.toItem(r));
  }

  /** The closed items, last closed first: none before set-up. */
  async closed(): Promise<WorkItem[]> {
    if (!this.store) return [];
    return (await this.query((s) => s.closed(CLOSED_MAX))).map((r) => this.toItem(r));
  }

  syncState(): WorkSync {
    const { github, slack } = this.deps.settings();
    return { running: this.syncing, ...this.last, slackAuto: kvGet(this.db, SLACK_AUTO_KEY) === '1', sources: { github, slack } };
  }

  /** The database to read, or the 409 saying why there's none. */
  private reader(): AmpWorkStore {
    if (!this.store) throw new HttpError(409, this.deps.notReady());
    return this.store;
  }

  /** The database to write, or the 409 or 503 saying why it can't be written here. */
  private writer(): AmpWorkStore {
    const s = this.reader();
    if (this.prepare) {
      throw new HttpError(503, `Your work queue in Meko Cloud isn't open yet${this.reachError ? `: ${this.reachError}` : ''}. Try again in a minute, or check Settings › Work queue.`);
    }
    if (this.readOnly) throw new HttpError(503, 'Your work queue in Meko Cloud was set up by a newer Kode. Update Kode on this computer to change it.');
    return s;
  }

  /** A failed query as the route answers it: one that couldn't reach the database is a 503, and marks the queue unreachable. */
  private failed(e: unknown): unknown {
    if (!unreachable(e)) return e;
    this.markUnreachable(e);
    return new HttpError(503, `Can't reach your work queue in Meko Cloud: ${errMsg(e)}. Try again, or check Settings › Work queue.`);
  }

  private markUnreachable(e: unknown): void {
    const msg = errMsg(e);
    if (this.reachError !== msg) {
      this.reachError = msg;
      this.deps.changed();
    }
    this.deps.unreachable(e);
  }

  private async query<T>(fn: (s: AmpWorkStore) => Promise<T>): Promise<T> {
    const s = this.reader();
    try {
      return await fn(s);
    } catch (e) {
      throw this.failed(e);
    }
  }

  /** Runs a write, then reads the queue again for everyone's page. */
  private async write<T>(fn: (s: AmpWorkStore) => Promise<T>): Promise<T> {
    const s = this.writer();
    let r: T;
    try {
      r = await fn(s);
    } catch (e) {
      // The queue changed on another computer: the page gets it as it is now, for the retry.
      if (e instanceof HttpError && e.status === 409) await this.read(false);
      throw this.failed(e);
    }
    await this.read(false);
    return r;
  }

  /**
   * Reads the open items again (D3: after each write, when a page shows the queue, and every minute while
   * one is open). First closes what Merge & Close dismissed while the database couldn't be reached. A
   * failure keeps the last list, says why, and throws only when `strict`.
   */
  private async read(strict: boolean): Promise<void> {
    const s = this.store;
    if (!s) return;
    const seq = ++this.readSeq;
    try {
      if (this.prepare) {
        const readOnly = await this.prepare();
        if (!readOnly) await this.copyLocal(s);
        if (s !== this.store) return;
        this.readOnly = readOnly;
        this.prepare = null;
      }
      const pending = all<{ item_id: string }>(this.db, 'SELECT item_id FROM work_pending_close').map((r) => r.item_id);
      if (pending.length && !this.readOnly) {
        await s.close(pending);
        for (const id of pending) run(this.db, 'DELETE FROM work_pending_close WHERE item_id = ?', id);
      }
      const items = await s.open();
      if (s !== this.store || seq < this.shownSeq) return;
      this.shownSeq = seq;
      this.items = items;
      const was = this.reachError;
      this.reachError = null;
      if (was) this.deps.reached?.();
      this.deps.changed();
    } catch (e) {
      if (s !== this.store) return;
      if (!unreachable(e)) {
        this.deps.warn('work queue: reading it failed', errMsg(e));
        // Before the first read worked (its tables, the copy), the queue can't be used: the state says why.
        if (this.prepare && this.reachError !== errMsg(e)) {
          this.reachError = errMsg(e);
          this.deps.changed();
        }
        if (strict) throw e;
        return;
      }
      this.markUnreachable(e);
      if (strict) throw e;
    }
  }

  /** Reads the queue again now, if there's a database; never throws (the failure is in the state). */
  async refresh(): Promise<void> {
    await this.read(false);
  }

  /** The links with the label a sync gave each one already on the item (a Slack message's channel), which reading it again loses. */
  private keepLabels(had: WorkLink[], links: WorkLink[]): WorkLink[] {
    const by = new Map(had.map((l) => [l.key, l]));
    return links.map((l) => {
      const old = by.get(l.key);
      return old && old.url === l.url ? { ...l, label: old.label } : l;
    });
  }

  async create(req: CreateWorkItemRequest): Promise<WorkItem> {
    const body = req && typeof req === 'object' ? req : {};
    let title = body.title == null ? '' : text(body.title, 'title', WORK_TITLE_MAX);
    const notes = body.notes == null ? '' : text(body.notes, 'notes', WORK_NOTES_MAX);
    const links = body.links == null ? [] : checkLinks(body.links);
    this.writer();
    if (!title) {
      const gh = links[0]?.kind === 'github' ? /^gh:(.+)#(\d+)$/.exec(links[0].key) : null;
      if (!gh) throw new HttpError(400, 'Give the work item a title, or put a GitHub PR or issue link first to use its title.');
      try {
        title = (await this.deps.githubTitle(gh[1], Number(gh[2]))).trim().slice(0, WORK_TITLE_MAX);
      } catch (e) {
        throw new HttpError(502, `GitHub didn't give the title of ${links[0].label}: ${errMsg(e)}. Type a title instead.`);
      }
      if (!title) throw new HttpError(400, `${links[0].label} has no title on GitHub. Type one.`);
    }
    const id = randomUUID();
    const createdAt = Date.now();
    await this.write((s) => s.insert({ id, title, notes, links, source: 'manual', sourceKey: null, unplaced: false, createdAt }));
    // From the list read after the write, or as written: a second query that failed would make a retry add it twice.
    const read = this.items.find((i) => i.id === id);
    return read
      ? this.toItem(read)
      : { id, title, notes, links, status: 'open', source: 'manual', sessionId: null, unplaced: false, githubDone: false, createdAt, closedAt: null };
  }

  async update(id: string, req: UpdateWorkItemRequest): Promise<WorkItem> {
    const body = req && typeof req === 'object' ? req : {};
    if (body.status != null && body.status !== 'open' && body.status !== 'closed') throw new HttpError(400, 'A work item is open or closed.');
    if (body.move != null && !['up', 'down', 'top', 'bottom'].includes(body.move)) throw new HttpError(400, 'Move a work item up, down, or to the top or bottom.');
    if (body.placed != null && body.placed !== true) throw new HttpError(400, 'placed must be true.');
    if (body.sessionId !== undefined && body.sessionId !== null && (typeof body.sessionId !== 'string' || !body.sessionId)) throw new HttpError(400, 'The session must be its id.');
    const title = body.title == null ? null : text(body.title, 'title', WORK_TITLE_MAX);
    if (title === '') throw new HttpError(400, 'Give the work item a title.');
    const notes = body.notes == null ? null : text(body.notes, 'notes', WORK_NOTES_MAX);
    const links = body.links == null ? null : checkLinks(body.links);
    // Linking a session is this computer's alone, so it works from the list last read even while the database can't be reached.
    const onlySession = title == null && notes == null && !links && body.status == null && !body.move && !body.placed;
    if (onlySession && body.sessionId === null) {
      // Unlinking needs no item: it may be closed, or gone from the list (another account's, or not read yet since a restart).
      tx(this.db, () => setItemSession(this.db, id, null));
      this.deps.changed();
      const known = this.items.find((i) => i.id === id);
      return known ? this.toItem(known) : this.get(id).catch(() => this.unread(id));
    }
    if (onlySession && !this.store) throw new HttpError(409, this.deps.notReady());
    const cur = this.items.find((i) => i.id === id) ?? (onlySession ? null : await this.query((s) => s.get(id)));
    if (!cur) throw new HttpError(404, onlySession ? 'That work item is closed or no longer exists.' : 'That work item no longer exists.');
    const status = body.status ?? cur.status;
    if (status === 'closed' && (body.move || body.sessionId)) throw new HttpError(400, 'That work item is closed. Reopen it first.');
    if (body.sessionId && !get(this.db, 'SELECT 1 AS x FROM sessions WHERE id = ?', body.sessionId)) throw new HttpError(404, 'That session no longer exists.');
    if (!onlySession) {
      await this.write((s) => s.update(id, {
        title: title ?? undefined, notes: notes ?? undefined, links: links ? this.keepLabels(cur.links, links) : undefined,
        status: body.status ?? undefined, move: body.move ?? undefined, placed: body.placed ?? undefined,
      }));
    }
    if (body.sessionId !== undefined) {
      tx(this.db, () => setItemSession(this.db, id, body.sessionId ?? null));
      this.deps.changed();
    }
    const now = this.items.find((i) => i.id === id);
    return now ? this.toItem(now) : this.get(id);
  }

  /** An item that can't be read now, as far as this computer knows it: its id and its session here. */
  private unread(id: string): WorkItem {
    return {
      id, title: '', notes: '', links: [], status: 'closed', source: 'manual', sessionId: sessionOfItem(this.db, id), unplaced: false, githubDone: false,
      createdAt: 0, closedAt: null,
    };
  }

  /** Closes the open items among `ids` (Dismiss all, with the items the page showed); the others stay as they are. */
  async closeAll(req: CloseWorkItemsRequest): Promise<number> {
    const ids = req?.ids;
    if (!Array.isArray(ids) || !ids.every((x) => typeof x === 'string')) throw new HttpError(400, 'Give the items to dismiss as a list of ids.');
    return this.write((s) => s.close(ids));
  }

  /**
   * Dismisses the item a session is on, once its PR merged (D7). The close goes into work_pending_close,
   * whatever the list last read holds, and a read in the background applies it, so Merge & Close never
   * waits on Meko Cloud; one the database can't take yet (it can't be reached, or isn't set up here)
   * waits there for the next read that reaches it: nobody pressed anything, so nobody would see it fail.
   * The item's title when the list last read has it open, else null (closed already, or not read yet).
   * Nothing is queued when the database is a newer Kode's, which this one doesn't write.
   */
  closeForSession(sessionId: string): string | null {
    const id = itemOfSession(this.db, sessionId);
    if (!id) return null;
    if (this.readOnly) {
      this.deps.warn("work queue: not dismissing a merged session's item: the queue in Meko Cloud is a newer Kode's, which this one doesn't change");
      return null;
    }
    run(this.db, 'INSERT INTO work_pending_close(item_id, at) VALUES (?, ?) ON CONFLICT(item_id) DO NOTHING', id, Date.now());
    void this.read(false);
    return this.items.find((i) => i.id === id)?.title ?? null;
  }

  async reorder(req: ReorderWorkItemsRequest): Promise<void> {
    const ids = req?.ids;
    if (!Array.isArray(ids) || !ids.every((x) => typeof x === 'string')) throw new HttpError(400, "Give the queue's order as a list of item ids.");
    if (req.moved != null && typeof req.moved !== 'string') throw new HttpError(400, 'The moved item must be its id.');
    await this.write((s) => s.reorder(ids, req.moved ?? null));
  }

  private matchSession(itemId: string): WorkLinkMatch['session'] {
    const sid = sessionOfItem(this.db, itemId);
    const s = sid
      ? get<{ id: string; project_id: string; name: string; stage: Stage; desired: string; pr_number: number | null; pr_url: string | null }>(this.db,
        'SELECT id, project_id, name, stage, desired, pr_number, pr_url FROM sessions WHERE id = ?', sid)
      : undefined;
    return s ? { id: s.id, projectId: s.project_id, name: s.name, stage: s.stage, closed: s.desired === 'archived', prNumber: s.pr_number, prUrl: s.pr_url } : null;
  }

  /**
   * The items holding a link (open first, then last closed), each with its session: none before set-up, and
   * only the open ones last read while the database can't be reached.
   */
  async byLink(link: unknown): Promise<WorkLinkMatch[]> {
    if (typeof link !== 'string' || link.length > WORK_LINK_MAX) throw new HttpError(400, 'Give one link as text.');
    const key = workLinkKey(link) ?? parseWorkLink(link)?.key;
    if (!key) throw new HttpError(400, "That isn't a link Kode can read.");
    let rows: StoredItem[];
    try {
      rows = this.store ? await this.store.byLink(key, BY_LINK_MAX) : [];
    } catch (e) {
      if (!unreachable(e)) throw e;
      this.markUnreachable(e);
      rows = this.items.filter((i) => i.links.some((l) => l.key === key)).slice(0, BY_LINK_MAX);
    }
    return rows.map((r) => ({ item: this.toItem(r), session: this.matchSession(r.id) }));
  }

  /**
   * The item a session started from a pasted link is for: the one open item holding the link, when it
   * has no session or only a closed one. Null when there's none, or more than one.
   */
  soleItemFor(link: string): string | null {
    const key = workLinkKey(link);
    if (!key) return null;
    const rows = this.items.filter((i) => i.links.some((l) => l.key === key));
    if (rows.length !== 1) return null;
    const sid = sessionOfItem(this.db, rows[0].id);
    const desired = sid ? get<{ desired: string }>(this.db, 'SELECT desired FROM sessions WHERE id = ?', sid)?.desired : undefined;
    return !sid || !desired || desired === 'archived' ? rows[0].id : null;
  }

  /** A new session's `workItemId`, checked: an open item's id (from the list last read), or null when none was given. */
  checkStartItem(id: unknown): string | null {
    if (id == null) return null;
    if (typeof id !== 'string') throw new HttpError(400, 'The work item must be its id.');
    if (!this.items.some((i) => i.id === id)) throw new HttpError(404, 'That work item is closed or no longer exists. Reopen it to start a session for it.');
    return id;
  }

  /**
   * The links on the item a session works on (the code review reads its issues): from the list last read,
   * else from the database (a closed item, or none read yet). Null when it has an item whose links can't be read.
   */
  async sessionLinks(sessionId: string): Promise<WorkLink[] | null> {
    const id = itemOfSession(this.db, sessionId);
    if (!id) return [];
    const known = this.items.find((i) => i.id === id);
    if (known) return known.links;
    if (!this.store) return null;
    try {
      return (await this.store.get(id))?.links ?? [];
    } catch (e) {
      if (unreachable(e)) this.markUnreachable(e);
      return null;
    }
  }

  /**
   * Copies this computer's items from before the move (state.db's work_items, open ones in their order
   * and then closed ones) into the database (D8): at the bottom, the open ones marked New, skipping any whose
   * id or source is there already, so a copy cut short can run again. It runs again when there are more
   * (LOCAL_COPIED_KEY): a rollback's release adds them there. The local rows stay as they are. A synced item
   * another computer has already keeps its session here: the link moves to that computer's item.
   */
  private async copyLocal(s: AmpWorkStore): Promise<void> {
    const had = get<{ n: number; last: number | null }>(this.db, 'SELECT COUNT(*) AS n, MAX(created_at) AS last FROM work_items');
    const mark = `${had?.n ?? 0}:${had?.last ?? 0}`;
    if (kvGet(this.db, LOCAL_COPIED_KEY) === mark) return;
    const rows = all<{
      id: string; title: string; notes: string; status: 'open' | 'closed'; source: WorkItemSource; source_key: string | null;
      github_done_at: number | null; created_at: number; closed_at: number | null;
    }>(this.db, "SELECT * FROM work_items ORDER BY status = 'closed', position, closed_at, created_at");
    for (const r of rows) {
      if (!isItemId(r.id)) continue;
      const links = all<{ key: string; kind: WorkLink['kind']; url: string | null; label: string }>(this.db,
        'SELECT key, kind, url, label FROM work_item_links WHERE item_id = ? ORDER BY ord', r.id);
      const added = await s.insert({
        id: r.id, title: r.title, notes: r.notes, links, source: r.source, sourceKey: r.source_key, unplaced: r.status === 'open',
        createdAt: r.created_at, status: r.status, closedAt: r.closed_at, githubDoneAt: r.github_done_at,
      });
      const sid = added || !r.source_key ? null : sessionOfItem(this.db, r.id);
      if (sid) {
        const there = await s.idBySource(r.source_key!);
        if (there && there !== r.id && !sessionOfItem(this.db, there)) tx(this.db, () => setItemSession(this.db, there, sid));
      }
    }
    kvSet(this.db, LOCAL_COPIED_KEY, mark);
  }

  private async syncGithub(s: AmpWorkStore): Promise<WorkSourceResult> {
    const found = await this.deps.reviewRequests();
    let added = 0;
    const keys = new Set<string>();
    for (const pr of found) {
      const link = parseWorkLink(pr.url) ?? parseWorkLink(`${pr.repo}#${pr.number}`);
      if (!link || link.kind !== 'github') continue;
      keys.add(link.key);
      if (await s.seen(link.key)) continue;
      // Another computer syncing at the same time may add it first: the insert then adds nothing.
      if (await s.insert({ id: randomUUID(), title: `Review ${pr.repo}#${pr.number}: ${pr.title}`.slice(0, WORK_TITLE_MAX), notes: '', links: [link],
        source: 'github', sourceKey: link.key, unplaced: true, createdAt: Date.now() })) added++;
    }
    // Done on GitHub: a review request it no longer finds (merged, closed, or not asked of you any more); back if it's asked again.
    // A full page may have left some out, and those aren't done, so nothing is marked then.
    // Only the requests this computer's sync has found: another computer's gh may be another account, whose requests this one never sees.
    let seen: string[] = [];
    try {
      const v: unknown = JSON.parse(kvGet(this.db, GITHUB_SEEN_KEY) ?? '[]');
      if (Array.isArray(v)) seen = v.filter((x): x is string => typeof x === 'string');
    } catch {
      /* none kept */
    }
    seen = [...seen.filter((k) => !keys.has(k)), ...keys].slice(-GITHUB_SEEN_MAX);
    kvSet(this.db, GITHUB_SEEN_KEY, JSON.stringify(seen));
    if (found.length >= REVIEW_REQUESTS_MAX) return { ok: true, added, error: null };
    const mine = new Set(seen);
    const now = Date.now();
    for (const r of await s.githubOpen()) {
      if (!mine.has(r.sourceKey)) continue;
      const done = !keys.has(r.sourceKey);
      if (done !== r.done) await s.setGithubDone(r.id, done ? now : null);
    }
    return { ok: true, added, error: null };
  }

  private async syncSlack(s: AmpWorkStore): Promise<WorkSourceResult> {
    if (!this.deps.claudeAvailable()) throw new Error('Reading Slack needs Claude Code, with a Slack connector, and it isn\'t available.');
    const found = await this.deps.readSlack();
    let added = 0;
    try {
      for (const m of found) {
        const link = parseWorkLink(m.url);
        if (!link || link.kind !== 'slack' || await s.seen(link.key)) continue;
        if (m.channel) link.label = `Slack #${m.channel.replace(/^#/, '')}`.slice(0, 80);
        if (await s.insert({ id: randomUUID(), title: firstLine(m.text, 'Slack message'), notes: slackNotes(m), links: [link],
          source: 'slack', sourceKey: link.key, unplaced: true, createdAt: Date.now() })) added++;
      }
    } catch (e) {
      // Slack was read; Meko Cloud's database is what failed.
      throw unreachable(e) ? new StoreLost(e) : e;
    }
    return { ok: true, added, error: null };
  }

  /**
   * Adds the open PRs asking for your review and the messages on the user's Slack list to the bottom of
   * the queue, marked as new, skipping any already on an item. Slack is read when `manual`, or on the
   * timer while the last Slack read worked: each one is a model call. Each source fails on its own.
   * Nothing runs before the queue is set up, while it can't be written here, or while the database can't be
   * reached (read again first): a pressed Sync gets the 409 or 503. A failure to reach it mid-sync leaves
   * Slack on the timer.
   */
  async sync(manual: boolean): Promise<WorkSync> {
    const on = this.deps.settings();
    if (!on.github && !on.slack) {
      if (manual) throw new HttpError(409, 'Sync is off: turn on GitHub or Slack in Settings › Work queue.');
      return this.syncState();
    }
    let s: AmpWorkStore;
    try {
      if (this.reachError) await this.read(false);
      s = this.writer();
      if (this.reachError) {
        throw new HttpError(503, `Can't reach your work queue in Meko Cloud: ${this.reachError}. Sync again once it's back, or check Settings › Work queue.`);
      }
    } catch (e) {
      if (manual) throw e;
      return this.syncState();
    }
    if (this.syncing) {
      if (manual) throw new HttpError(409, 'The work queue is syncing already.');
      return this.syncState();
    }
    this.syncing = true;
    this.deps.changed();
    try {
      const lost = new Set<string>();
      const attempt = async (name: string, fn: () => Promise<WorkSourceResult>): Promise<WorkSourceResult> => {
        try {
          return await fn();
        } catch (e) {
          // A database that stopped answering shows in the read after the sync, which marks it.
          if (e instanceof StoreLost) lost.add(name);
          this.deps.warn(`work queue: syncing ${name} failed`, errMsg(e));
          return { ok: false, added: 0, error: errMsg(e) };
        }
      };
      const github = on.github ? await attempt('GitHub', () => this.syncGithub(s)) : null;
      const slackOn = on.slack && (manual || kvGet(this.db, SLACK_AUTO_KEY) === '1');
      const slack = slackOn ? await attempt('Slack', () => this.syncSlack(s)) : null;
      if (slack?.ok && manual) kvSet(this.db, SLACK_AUTO_KEY, '1');
      // Slack's own failure turns the timer's Slack read off; Meko Cloud's doesn't (the read was fine).
      if (slack && !slack.ok && !lost.has('Slack')) kvDelete(this.db, SLACK_AUTO_KEY);
      this.last = { at: Date.now(), github, slack };
    } finally {
      this.syncing = false;
      await this.read(false);
      this.deps.changed();
    }
    return this.syncState();
  }

  /**
   * A new Slack list link: the last Slack result was about the old one (often "Set the link…"), so
   * it's dropped, and the sync line says Slack waits for a Sync press, which reads the new list and
   * puts it back on the timer.
   */
  slackListChanged(): void {
    if (!this.last.slack) return;
    this.last = { ...this.last, slack: null };
    this.deps.changed();
  }

  /** The timer's tick (every minute): a sync once `[work_queue] sync_minutes` have passed since the last, so a change in Settings applies at once. */
  async tick(now = Date.now()): Promise<void> {
    const minutes = this.deps.settings().syncMinutes;
    if (minutes <= 0 || now - this.lastAuto < minutes * 60_000) return;
    this.lastAuto = now;
    await this.sync(false);
  }
}

