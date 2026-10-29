// The work queue: the user's items across projects, their links and sessions, and the sync from GitHub and Slack.
import { randomUUID } from 'node:crypto';
import { moveItem } from '../shared/workorder.js';
import { parseWorkLink, workLinkKey } from '../shared/worklinks.js';
import {
  WORK_LINK_MAX, WORK_LINKS_MAX, WORK_NOTES_MAX, WORK_TITLE_MAX,
  type CloseWorkItemsRequest, type CreateWorkItemRequest, type ReorderWorkItemsRequest, type Stage, type UpdateWorkItemRequest, type WorkItem, type WorkItemSource,
  type WorkLink, type WorkLinkMatch, type WorkMove, type WorkSourceResult, type WorkSync,
} from '../shared/types.js';
import { all, get, kvDelete, kvGet, kvSet, run, tx, type Db } from './db.js';
import { REVIEW_REQUESTS_MAX, type ReviewRequest } from './github.js';
import { errMsg } from './log.js';
import { HttpError } from './projects.js';
import type { SlackTodo } from './slack.js';

/** kv: '1' while Slack is synced on the timer too, which a Sync that reached Slack turns on and any failure off. */
const SLACK_AUTO_KEY = 'work-slack-auto';
const CLOSED_MAX = 200;
const BY_LINK_MAX = 20;
const SYNCED_TITLE_MAX = 100;

export interface WorkQueueDeps {
  db: Db;
  changed: () => void;
  warn: (msg: string, detail?: string) => void;
  /** The open PRs asking for the user's review. */
  reviewRequests: () => Promise<ReviewRequest[]>;
  /** The messages on the user's Slack To-dos; throws with what went wrong. */
  readSlack: () => Promise<SlackTodo[]>;
  /** Whether Claude Code can run, which the Slack read needs. */
  claudeAvailable: () => boolean;
  /** A GitHub PR's or issue's title, for an item added with a link and no title. */
  githubTitle: (repo: string, number: number) => Promise<string>;
  /** `[work_queue]` as it is now: the sources the sync reads and how often it runs by itself. */
  settings: () => { github: boolean; slack: boolean; syncMinutes: number };
}

interface ItemRow {
  id: string;
  title: string;
  notes: string;
  position: number;
  status: 'open' | 'closed';
  source: WorkItemSource;
  source_key: string | null;
  session_id: string | null;
  unplaced: number;
  github_done_at: number | null;
  created_at: number;
  closed_at: number | null;
}

/**
 * Links a session to an item (`sessionId` null: unlinks it), one to one: the session leaves any other
 * item first, and the item's own session is replaced. Call inside a transaction with the writes it goes with.
 */
export function setItemSession(db: Db, itemId: string, sessionId: string | null): void {
  if (sessionId) run(db, 'UPDATE work_items SET session_id = NULL WHERE session_id = ? AND id != ?', sessionId, itemId);
  run(db, 'UPDATE work_items SET session_id = ? WHERE id = ?', sessionId, itemId);
}

/** A new session's `workItemId`, checked: an open item's id, or null when none was given. */
export function checkStartItem(db: Db, id: unknown): string | null {
  if (id == null) return null;
  if (typeof id !== 'string') throw new HttpError(400, 'The work item must be its id.');
  const r = get<{ status: string }>(db, 'SELECT status FROM work_items WHERE id = ?', id);
  if (!r) throw new HttpError(404, 'That work item no longer exists.');
  if (r.status !== 'open') throw new HttpError(400, 'That work item is closed. Reopen it to start a session for it.');
  return id;
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
    if (!l) throw new HttpError(400, `"${t.slice(0, 80)}" isn't a link AgentK can read. Paste a GitHub, Jira or Slack link, or any https:// link.`);
    if (!out.has(l.key)) out.set(l.key, l);
  }
  return [...out.values()];
}

/** A synced message's title: its first line, cut to SYNCED_TITLE_MAX characters. */
function firstLine(s: string, fallback: string): string {
  const line = s.split('\n').find((l) => l.trim())?.trim() ?? '';
  if (!line) return fallback;
  return line.length > SYNCED_TITLE_MAX ? `${line.slice(0, SYNCED_TITLE_MAX - 1)}…` : line;
}

export class WorkQueue {
  private syncing = false;
  /** When the timer last synced (it starts counting at startup). */
  private lastAuto = Date.now();
  private last: { at: number | null; github: WorkSourceResult | null; slack: WorkSourceResult | null } = { at: null, github: null, slack: null };

  constructor(private readonly deps: WorkQueueDeps) {}

  private get db(): Db {
    return this.deps.db;
  }

  private links(id: string): WorkLink[] {
    return all<{ key: string; kind: WorkLink['kind']; url: string | null; label: string }>(
      this.db, 'SELECT key, kind, url, label FROM work_item_links WHERE item_id = ? ORDER BY ord', id,
    ).map((l) => ({ kind: l.kind, key: l.key, url: l.url, label: l.label }));
  }

  private toItem(r: ItemRow): WorkItem {
    return {
      id: r.id, title: r.title, notes: r.notes, links: this.links(r.id), status: r.status, source: r.source, sessionId: r.session_id,
      unplaced: !!r.unplaced, githubDone: r.github_done_at != null, createdAt: r.created_at, closedAt: r.closed_at,
    };
  }

  private row(id: string): ItemRow {
    const r = get<ItemRow>(this.db, 'SELECT * FROM work_items WHERE id = ?', id);
    if (!r) throw new HttpError(404, 'That work item no longer exists.');
    return r;
  }

  get(id: string): WorkItem {
    return this.toItem(this.row(id));
  }

  /** The open items, in the queue's order. */
  list(): WorkItem[] {
    return all<ItemRow>(this.db, "SELECT * FROM work_items WHERE status = 'open' ORDER BY position, created_at").map((r) => this.toItem(r));
  }

  /** The closed items, last closed first. */
  closed(): WorkItem[] {
    return all<ItemRow>(this.db, "SELECT * FROM work_items WHERE status = 'closed' ORDER BY closed_at DESC LIMIT ?", CLOSED_MAX).map((r) => this.toItem(r));
  }

  syncState(): WorkSync {
    const { github, slack } = this.deps.settings();
    return { running: this.syncing, ...this.last, slackAuto: kvGet(this.db, SLACK_AUTO_KEY) === '1', sources: { github, slack } };
  }

  private writeLinks(id: string, links: WorkLink[]): void {
    run(this.db, 'DELETE FROM work_item_links WHERE item_id = ?', id);
    links.forEach((l, i) => run(this.db, 'INSERT INTO work_item_links(item_id, ord, key, kind, url, label) VALUES (?, ?, ?, ?, ?, ?)', id, i, l.key, l.kind, l.url, l.label));
  }

  /** The links with the label a sync gave each one already on the item (a Slack message's channel), which reading it again loses. */
  private keepLabels(id: string, links: WorkLink[]): WorkLink[] {
    const had = new Map(this.links(id).map((l) => [l.key, l]));
    return links.map((l) => {
      const old = had.get(l.key);
      return old && old.url === l.url ? { ...l, label: old.label } : l;
    });
  }

  /** Writes a new item at the bottom of the queue. */
  private insert(o: { title: string; notes?: string; links: WorkLink[]; source: WorkItemSource; sourceKey?: string }): string {
    const id = randomUUID();
    tx(this.db, () => {
      run(this.db, `INSERT INTO work_items(id, title, notes, position, source, source_key, unplaced, created_at)
                    VALUES (?, ?, ?, (SELECT COALESCE(MAX(position), 0) + 1 FROM work_items WHERE status = 'open'), ?, ?, ?, ?)`,
        id, o.title, o.notes ?? '', o.source, o.sourceKey ?? null, o.source === 'manual' ? 0 : 1, Date.now());
      this.writeLinks(id, o.links);
    });
    return id;
  }

  async create(req: CreateWorkItemRequest): Promise<WorkItem> {
    const body = req && typeof req === 'object' ? req : {};
    let title = body.title == null ? '' : text(body.title, 'title', WORK_TITLE_MAX);
    const notes = body.notes == null ? '' : text(body.notes, 'notes', WORK_NOTES_MAX);
    const links = body.links == null ? [] : checkLinks(body.links);
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
    const id = this.insert({ title, notes, links, source: 'manual' });
    this.deps.changed();
    return this.get(id);
  }

  update(id: string, req: UpdateWorkItemRequest): WorkItem {
    const body = req && typeof req === 'object' ? req : {};
    const r = this.row(id);
    if (body.status != null && body.status !== 'open' && body.status !== 'closed') throw new HttpError(400, 'A work item is open or closed.');
    if (body.move != null && !['up', 'down', 'top', 'bottom'].includes(body.move)) throw new HttpError(400, 'Move a work item up, down, or to the top or bottom.');
    if (body.placed != null && body.placed !== true) throw new HttpError(400, 'placed must be true.');
    if (body.sessionId !== undefined && body.sessionId !== null && (typeof body.sessionId !== 'string' || !body.sessionId)) throw new HttpError(400, 'The session must be its id.');
    const title = body.title == null ? null : text(body.title, 'title', WORK_TITLE_MAX);
    if (title === '') throw new HttpError(400, 'Give the work item a title.');
    const notes = body.notes == null ? null : text(body.notes, 'notes', WORK_NOTES_MAX);
    const links = body.links == null ? null : checkLinks(body.links);
    const status = body.status ?? r.status;
    if (status === 'closed' && (body.move || body.sessionId)) throw new HttpError(400, 'That work item is closed. Reopen it first.');
    if (body.sessionId && !get(this.db, 'SELECT 1 AS x FROM sessions WHERE id = ?', body.sessionId)) throw new HttpError(404, 'That session no longer exists.');
    tx(this.db, () => {
      if (title != null) run(this.db, 'UPDATE work_items SET title = ? WHERE id = ?', title, id);
      if (notes != null) run(this.db, 'UPDATE work_items SET notes = ? WHERE id = ?', notes, id);
      if (links) this.writeLinks(id, this.keepLabels(id, links));
      if (status !== r.status) {
        if (status === 'closed') run(this.db, 'UPDATE work_items SET status = ?, closed_at = ?, unplaced = 0 WHERE id = ?', status, Date.now(), id);
        else run(this.db, `UPDATE work_items SET status = 'open', closed_at = NULL,
                           position = (SELECT COALESCE(MAX(position), 0) + 1 FROM work_items WHERE status = 'open') WHERE id = ?`, id);
      }
      if (body.move) this.move(id, body.move);
      if (body.move || body.placed) run(this.db, 'UPDATE work_items SET unplaced = 0 WHERE id = ?', id);
      if (body.sessionId !== undefined) setItemSession(this.db, id, body.sessionId);
    });
    this.deps.changed();
    return this.get(id);
  }

  /** Moves an open item one place up or down, or to the top or bottom; the queue keeps its order otherwise. */
  private move(id: string, to: WorkMove): void {
    const ids = this.list().map((i) => i.id);
    if (!ids.includes(id)) return;
    this.writeOrder(moveItem(ids, id, to));
  }

  private writeOrder(ids: string[]): void {
    ids.forEach((x, i) => run(this.db, 'UPDATE work_items SET position = ? WHERE id = ?', i + 1, x));
  }

  /** Closes the open items among `ids` (Dismiss all, with the items the page showed); the others stay as they are. */
  closeAll(req: CloseWorkItemsRequest): number {
    const ids = req?.ids;
    if (!Array.isArray(ids) || !ids.every((x) => typeof x === 'string')) throw new HttpError(400, 'Give the items to dismiss as a list of ids.');
    const now = Date.now();
    let closed = 0;
    tx(this.db, () => {
      for (const id of new Set(ids)) closed += run(this.db, "UPDATE work_items SET status = 'closed', closed_at = ?, unplaced = 0 WHERE id = ? AND status = 'open'", now, id).changes;
    });
    if (closed) this.deps.changed();
    return closed;
  }

  reorder(req: ReorderWorkItemsRequest): void {
    const ids = req?.ids;
    if (!Array.isArray(ids) || !ids.every((x) => typeof x === 'string')) throw new HttpError(400, "Give the queue's order as a list of item ids.");
    if (req.moved != null && typeof req.moved !== 'string') throw new HttpError(400, 'The moved item must be its id.');
    const open = new Set(this.list().map((i) => i.id));
    if (ids.length !== open.size || new Set(ids).size !== ids.length || !ids.every((x) => open.has(x))) {
      throw new HttpError(409, 'The work queue changed meanwhile. Try again on the queue as it is now.');
    }
    tx(this.db, () => {
      this.writeOrder(ids);
      if (req.moved) run(this.db, 'UPDATE work_items SET unplaced = 0 WHERE id = ?', req.moved);
    });
    this.deps.changed();
  }

  /** The items holding a link (open first, then last closed), each with its session. */
  byLink(link: unknown): WorkLinkMatch[] {
    if (typeof link !== 'string' || link.length > WORK_LINK_MAX) throw new HttpError(400, 'Give one link as text.');
    const key = workLinkKey(link) ?? parseWorkLink(link)?.key;
    if (!key) throw new HttpError(400, "That isn't a link AgentK can read.");
    const rows = all<ItemRow>(this.db,
      `SELECT w.* FROM work_items w JOIN work_item_links l ON l.item_id = w.id WHERE l.key = ?
        ORDER BY w.status = 'closed', w.closed_at DESC, w.position LIMIT ?`, key, BY_LINK_MAX);
    return rows.map((r) => {
      const s = r.session_id
        ? get<{ id: string; project_id: string; name: string; stage: Stage; desired: string; pr_number: number | null; pr_url: string | null }>(this.db,
          'SELECT id, project_id, name, stage, desired, pr_number, pr_url FROM sessions WHERE id = ?', r.session_id)
        : undefined;
      return {
        item: this.toItem(r),
        session: s ? { id: s.id, projectId: s.project_id, name: s.name, stage: s.stage, closed: s.desired === 'archived', prNumber: s.pr_number, prUrl: s.pr_url } : null,
      };
    });
  }

  /**
   * The item a session started from a pasted link is for: the one open item holding the link, when it
   * has no session or only a closed one. Null when there's none, or more than one.
   */
  soleItemFor(link: string): string | null {
    const key = workLinkKey(link);
    if (!key) return null;
    const rows = all<{ id: string; desired: string | null; session_id: string | null }>(this.db,
      `SELECT w.id, w.session_id, s.desired FROM work_items w JOIN work_item_links l ON l.item_id = w.id
         LEFT JOIN sessions s ON s.id = w.session_id WHERE l.key = ? AND w.status = 'open'`, key);
    if (rows.length !== 1) return null;
    return !rows[0].session_id || rows[0].desired === 'archived' ? rows[0].id : null;
  }

  /** Whether an item (open or closed) already has this key: synced from it, or holding it as a link. */
  private seen(key: string): boolean {
    return !!get(this.db, 'SELECT 1 AS x FROM work_items WHERE source_key = ? UNION SELECT 1 FROM work_item_links WHERE key = ?', key, key);
  }

  private async syncGithub(): Promise<WorkSourceResult> {
    const found = await this.deps.reviewRequests();
    let added = 0;
    const keys = new Set<string>();
    for (const pr of found) {
      const link = parseWorkLink(pr.url) ?? parseWorkLink(`${pr.repo}#${pr.number}`);
      if (!link || link.kind !== 'github') continue;
      keys.add(link.key);
      if (this.seen(link.key)) continue;
      this.insert({ title: `Review ${pr.repo}#${pr.number}: ${pr.title}`.slice(0, WORK_TITLE_MAX), links: [link], source: 'github', sourceKey: link.key });
      added++;
    }
    // Done on GitHub: a review request it no longer finds (merged, closed, or not asked of you any more); back if it's asked again.
    // A full page may have left some out, and those aren't done, so nothing is marked then.
    if (found.length >= REVIEW_REQUESTS_MAX) return { ok: true, added, error: null };
    const now = Date.now();
    for (const r of all<{ id: string; source_key: string; github_done_at: number | null }>(this.db,
      "SELECT id, source_key, github_done_at FROM work_items WHERE source = 'github' AND status = 'open'")) {
      const done = !keys.has(r.source_key);
      if (done !== (r.github_done_at != null)) run(this.db, 'UPDATE work_items SET github_done_at = ? WHERE id = ?', done ? now : null, r.id);
    }
    return { ok: true, added, error: null };
  }

  private async syncSlack(): Promise<WorkSourceResult> {
    if (!this.deps.claudeAvailable()) throw new Error('Reading Slack needs Claude Code, with a Slack connector, and it isn\'t available.');
    const found = await this.deps.readSlack();
    let added = 0;
    for (const m of found) {
      const link = parseWorkLink(m.url);
      if (!link || link.kind !== 'slack' || this.seen(link.key)) continue;
      if (m.channel) link.label = `Slack #${m.channel.replace(/^#/, '')}`.slice(0, 80);
      this.insert({ title: firstLine(m.text, 'Slack message'), links: [link], source: 'slack', sourceKey: link.key });
      added++;
    }
    return { ok: true, added, error: null };
  }

  /**
   * Adds the open PRs asking for your review and the messages on Slack's To-dos to the bottom of
   * the queue, marked as new, skipping any already on an item. Slack is read when `manual`, or on the
   * timer while the last Slack read worked: each one is a model call. Each source fails on its own.
   */
  async sync(manual: boolean): Promise<WorkSync> {
    const on = this.deps.settings();
    if (!on.github && !on.slack) {
      if (manual) throw new HttpError(409, 'Sync is off: turn on GitHub or Slack in Settings › Work queue.');
      return this.syncState();
    }
    if (this.syncing) {
      if (manual) throw new HttpError(409, 'The work queue is syncing already.');
      return this.syncState();
    }
    this.syncing = true;
    this.deps.changed();
    try {
      const attempt = async (name: string, fn: () => Promise<WorkSourceResult>): Promise<WorkSourceResult> => {
        try {
          return await fn();
        } catch (e) {
          this.deps.warn(`work queue: syncing ${name} failed`, errMsg(e));
          return { ok: false, added: 0, error: errMsg(e) };
        }
      };
      const github = on.github ? await attempt('GitHub', () => this.syncGithub()) : null;
      const slackOn = on.slack && (manual || kvGet(this.db, SLACK_AUTO_KEY) === '1');
      const slack = slackOn ? await attempt('Slack', () => this.syncSlack()) : null;
      if (slack?.ok && manual) kvSet(this.db, SLACK_AUTO_KEY, '1');
      if (slack && !slack.ok) kvDelete(this.db, SLACK_AUTO_KEY);
      this.last = { at: Date.now(), github, slack };
    } finally {
      this.syncing = false;
      this.deps.changed();
    }
    return this.syncState();
  }

  /** The timer's tick (every minute): a sync once `[work_queue] sync_minutes` have passed since the last, so a change in Settings applies at once. */
  async tick(now = Date.now()): Promise<void> {
    const minutes = this.deps.settings().syncMinutes;
    if (minutes <= 0 || now - this.lastAuto < minutes * 60_000) return;
    this.lastAuto = now;
    await this.sync(false);
  }
}
