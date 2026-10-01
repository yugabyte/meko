import { randomUUID } from 'node:crypto';
import type { CommentSide, CommentState, NewComment, ReviewComment, SendCommentsResult, Stage } from '../shared/types.js';
import type { Core } from './core.js';
import { all, get, run, tx } from './db.js';
import { errMsg } from './log.js';
import { HttpError } from './projects.js';
import type { SessionRow } from './store.js';

/** Longest comment body, in characters. */
export const BODY_MAX = 10_000;
/** Most drafts a session can hold, and most comments pasted into Claude at once. */
export const BATCH_MAX = 50;
/** Excerpts keep this many lines, from the end of the range. */
export const EXCERPT_LINES = 20;
const EXCERPT_CHARS = 4000;
/** Longer excerpt lines are cut to this many characters plus '…'. */
export const EXCERPT_LINE_CHARS = 1000;
const PATH_MAX = 4096;
/** Stages in which Claude works on the diff (or helps review it), so queued comments can go to it. */
const COMMENT_STAGES: Stage[] = ['explore', 'code', 'review', 'reviewing-pr'];

interface CommentRow {
  id: string;
  session_id: string;
  path: string;
  side: CommentSide;
  line: number;
  end_line: number;
  excerpt: string;
  body: string;
  state: CommentState;
  created_at: number;
  updated_at: number;
  sent_at: number | null;
}

function toComment(r: CommentRow): ReviewComment {
  return {
    id: r.id, path: r.path, side: r.side, line: r.line, endLine: r.end_line, excerpt: r.excerpt, body: r.body,
    state: r.state, createdAt: r.created_at, updatedAt: r.updated_at, sentAt: r.sent_at,
  };
}

export function commentCounts(db: Core['db'], sessionId: string): { drafts: number; queued: number } {
  const rows = all<{ state: CommentState; n: number }>(
    db, `SELECT state, COUNT(*) AS n FROM review_comments WHERE session_id = ? AND state != 'sent' GROUP BY state`, sessionId);
  return {
    drafts: rows.find((r) => r.state === 'draft')?.n ?? 0,
    queued: rows.find((r) => r.state === 'queued')?.n ?? 0,
  };
}

function checkBody(body: unknown): string {
  if (typeof body !== 'string' || !body.trim()) throw new HttpError(400, 'Write something in the comment first.');
  const b = body.trim();
  if (b.length > BODY_MAX) throw new HttpError(400, `Comments can be at most ${BODY_MAX.toLocaleString('en-US')} characters; this one has ${b.length.toLocaleString('en-US')}.`);
  return b;
}

/**
 * An excerpt line as stored: cut to EXCERPT_LINE_CHARS plus '…' when longer. The diff view
 * clips its rows the same way when it re-anchors a comment, so long lines still match.
 */
export function clipExcerptLine(line: string): string {
  return line.length > EXCERPT_LINE_CHARS ? `${line.slice(0, EXCERPT_LINE_CHARS)}…` : line;
}

/** Checks a new comment from the API and normalizes it: trimmed body, excerpt cut to its last lines. */
export function checkNewComment(b: unknown): NewComment {
  const c = (b && typeof b === 'object' ? b : {}) as Partial<Record<keyof NewComment, unknown>>;
  if (typeof c.path !== 'string' || !c.path || c.path.length > PATH_MAX) throw new HttpError(400, 'The comment needs the path of a changed file.');
  if (c.side !== 'old' && c.side !== 'new') throw new HttpError(400, "The comment's side must be 'old' or 'new'.");
  const line = c.line, end = c.endLine;
  if (!Number.isInteger(line) || (line as number) < 1 || !Number.isInteger(end) || (end as number) < (line as number)) {
    throw new HttpError(400, 'The comment needs a line range: line and endLine are line numbers, with endLine ≥ line.');
  }
  if (typeof c.excerpt !== 'string') throw new HttpError(400, 'The comment needs the excerpt of the lines it is about.');
  // Whole lines only, dropped from the front: the last line is what re-anchors the comment.
  const lines = c.excerpt.split('\n').slice(-EXCERPT_LINES).map(clipExcerptLine);
  while (lines.length > 1 && lines.join('\n').length > EXCERPT_CHARS) lines.shift();
  return { path: c.path, side: c.side, line: line as number, endLine: end as number, excerpt: lines.join('\n'), body: checkBody(c.body) };
}

/** A code fence longer than any run of backticks in the text. */
function fence(text: string): string {
  const longest = Math.max(0, ...(text.match(/`+/g) ?? []).map((m) => m.length));
  return '`'.repeat(Math.max(3, longest + 1));
}

function where(c: ReviewComment): string {
  const lines = c.line === c.endLine ? `line ${c.line}` : `lines ${c.line}–${c.endLine}`;
  return c.side === 'new' ? lines[0].toUpperCase() + lines.slice(1) : `Removed ${lines} (numbered as in the base)`;
}

/** The comments as markdown for Claude: grouped by file in path order, then by line. */
export function formatComments(comments: ReviewComment[]): string {
  const byPath = new Map<string, ReviewComment[]>();
  for (const c of comments) byPath.set(c.path, [...(byPath.get(c.path) ?? []), c]);
  const out: string[] = [];
  for (const path of [...byPath.keys()].sort()) {
    out.push(`## ${path}`);
    const cs = byPath.get(path)!.sort((a, b) => a.endLine - b.endLine || (a.side === b.side ? 0 : a.side === 'old' ? -1 : 1) || a.createdAt - b.createdAt);
    for (const c of cs) {
      const f = fence(c.excerpt);
      out.push(c.excerpt ? `${where(c)}:\n${f}\n${c.excerpt}\n${f}\n${c.body}` : `${where(c)}:\n${c.body}`);
    }
  }
  return out.join('\n\n');
}

/**
 * Review comments on a session's diff. Drafts are kept until the user sends them; sent
 * comments are pasted into Claude as one prompt when it's between turns, or queued until
 * its turn ends (Lifecycle.onTurnEnd, and the periodic poll after a restart).
 */
export class CommentService {
  private delivering = new Set<string>();

  constructor(private core: Core) {}

  private get db() {
    return this.core.db;
  }

  private running(sessionId: string): SessionRow {
    const row = this.core.sessions.row(sessionId);
    if (row.desired !== 'running') throw new HttpError(409, 'This session is archived.');
    return row;
  }

  private one(sessionId: string, id: string): CommentRow {
    const r = get<CommentRow>(this.db, 'SELECT * FROM review_comments WHERE id = ? AND session_id = ?', id, sessionId);
    if (!r) throw new HttpError(404, 'That comment no longer exists.');
    return r;
  }

  private draft(sessionId: string, id: string): CommentRow {
    const r = this.one(sessionId, id);
    if (r.state !== 'draft') throw new HttpError(409, 'This comment was already sent to AgentK, so it can no longer be changed.');
    return r;
  }

  list(sessionId: string): ReviewComment[] {
    this.core.sessions.row(sessionId);
    return all<CommentRow>(this.db, 'SELECT * FROM review_comments WHERE session_id = ? ORDER BY created_at, id', sessionId).map(toComment);
  }

  add(sessionId: string, body: unknown): ReviewComment {
    this.running(sessionId);
    const c = checkNewComment(body);
    if (commentCounts(this.db, sessionId).drafts >= BATCH_MAX) {
      throw new HttpError(409, `You already have ${BATCH_MAX} draft comments, the most that can go to AgentK at once. Send them first.`);
    }
    const id = randomUUID();
    const now = Date.now();
    run(this.db, `INSERT INTO review_comments(id, session_id, path, side, line, end_line, excerpt, body, state, created_at, updated_at)
                  VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'draft', ?, ?)`,
      id, sessionId, c.path, c.side, c.line, c.endLine, c.excerpt, c.body, now, now);
    this.core.changed();
    return toComment(this.one(sessionId, id));
  }

  edit(sessionId: string, id: string, body: unknown): ReviewComment {
    this.running(sessionId);
    this.draft(sessionId, id);
    run(this.db, 'UPDATE review_comments SET body = ?, updated_at = ? WHERE id = ?', checkBody(body), Date.now(), id);
    return toComment(this.one(sessionId, id));
  }

  remove(sessionId: string, id: string): void {
    this.running(sessionId);
    this.draft(sessionId, id);
    run(this.db, 'DELETE FROM review_comments WHERE id = ?', id);
    this.core.changed();
  }

  /** Queues every draft, then pastes the queue into Claude if it's between turns. */
  async send(sessionId: string): Promise<SendCommentsResult> {
    const row = this.running(sessionId);
    if (row.status === 'exited') throw new HttpError(409, 'Claude has exited. Resume the session to send comments.');
    const moved = run(this.db, `UPDATE review_comments SET state = 'queued', updated_at = ? WHERE session_id = ? AND state = 'draft'`,
      Date.now(), sessionId).changes;
    if (!moved && !commentCounts(this.db, sessionId).queued) throw new HttpError(409, 'There are no draft comments to send.');
    this.core.changed();
    let sent: number;
    try {
      sent = await this.deliver(sessionId);
    } catch (e) {
      throw new HttpError(502, `Couldn't paste the comments into Claude's terminal: ${errMsg(e)}. They stay queued and are retried when Claude finishes a turn.`);
    }
    return { sent, queued: commentCounts(this.db, sessionId).queued };
  }

  /**
   * Pastes queued comments into Claude when it's waiting at its prompt in the Explore, Code,
   * Review or Reviewing PR stage (never while it shows a permission or question prompt). Returns how many went out.
   */
  async deliver(sessionId: string): Promise<number> {
    if (this.delivering.has(sessionId)) return 0;
    const row = this.core.sessions.row(sessionId);
    if (row.desired !== 'running' || (row.status !== 'idle' && row.status !== 'recovered')) return 0;
    if (!COMMENT_STAGES.includes(row.stage) || this.core.sessions.runtime(sessionId).busy) return 0;
    const queued = all<CommentRow>(this.db,
      `SELECT * FROM review_comments WHERE session_id = ? AND state = 'queued' ORDER BY created_at, id LIMIT ?`, sessionId, BATCH_MAX).map(toComment);
    if (!queued.length) return 0;
    this.delivering.add(sessionId);
    try {
      const count = `${queued.length} comment${queued.length === 1 ? '' : 's'}`;
      // In a Review a PR session they're the user's review comments on someone else's PR, not changes to make.
      const prompt = row.kind === 'review-pr' ? 'review-pr-comments' : 'comments';
      await this.core.sessions.submit(sessionId, this.core.sessions.prompt(row, prompt, { count, comments: formatComments(queued) }));
      const now = Date.now();
      tx(this.db, () => {
        for (const c of queued) run(this.db, `UPDATE review_comments SET state = 'sent', sent_at = ?, updated_at = ? WHERE id = ?`, now, now, c.id);
      });
      this.core.sessions.event(sessionId, 'comments', `Sent ${count} to AgentK`);
      this.core.changed();
      return queued.length;
    } finally {
      this.delivering.delete(sessionId);
    }
  }
}
