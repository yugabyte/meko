import { randomUUID } from 'node:crypto';
import { existsSync } from 'node:fs';
import { EXCERPT_LINE_CHARS, excerptMatches, fence, hasSuggestion, suggestions } from '../shared/suggestion.js';
import type { CommentSide, CommentState, NewComment, ReviewComment, SendCommentsResult, SessionStatus, Stage } from '../shared/types.js';
import { OutsideRepoError, ReplaceError, replaceLines } from './changes.js';
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
export { EXCERPT_LINE_CHARS };
const PATH_MAX = 4096;
/** Stages in which Claude works on the diff (or helps review it), so queued comments can go to it. */
const COMMENT_STAGES: Stage[] = ['explore', 'code', 'review', 'reviewing-pr'];
/** While the agent is in one of these, Apply waits: it would write under the agent's edit. */
const MID_TURN: SessionStatus[] = ['starting', 'working', 'needs_input'];

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

/** A body for a comment on `side`: a suggestion only on added or unchanged lines, as on GitHub. */
function checkSide(side: CommentSide, body: string): string {
  if (side === 'old' && hasSuggestion(body)) throw new HttpError(400, 'Suggestions go on added or unchanged lines, not removed ones.');
  return body;
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
  const body = checkSide(c.side, checkBody(c.body));
  // Whole lines only, dropped from the front: the last line is what re-anchors the comment.
  const lines = c.excerpt.split('\n').slice(-EXCERPT_LINES).map(clipExcerptLine);
  while (lines.length > 1 && lines.join('\n').length > EXCERPT_CHARS) lines.shift();
  return { path: c.path, side: c.side, line: line as number, endLine: end as number, excerpt: lines.join('\n'), body };
}

function lineRange(c: ReviewComment): string {
  return c.line === c.endLine ? `line ${c.line}` : `lines ${c.line}–${c.endLine}`;
}

function where(c: ReviewComment): string {
  const lines = lineRange(c);
  return c.side === 'new' ? lines[0].toUpperCase() + lines.slice(1) : `Removed ${lines} (numbered as in the base)`;
}

/**
 * What a suggestion block in a comment means, said after the comment, so every prompt override
 * carries it. In a PR review (`review`) it's the user's suggestion for the PR's author, which
 * GitHub shows as one when it's posted as is over the same lines.
 */
export function suggestionNote(c: ReviewComment, review = false): string {
  if (review) {
    const span = c.line === c.endLine ? `"line" ${c.line}` : `"start_line" ${c.line} and "line" ${c.endLine}`;
    return `Keep the suggestion block above as it is in this review comment's body, on ${span} (side RIGHT), so GitHub shows it as a suggested change.`;
  }
  return `The suggestion block above is the exact text the user wants in place of ${lineRange(c)}. Apply it as written unless it's wrong, and say so if it is.`;
}

/** The comments as markdown for Claude: grouped by file in path order, then by line; `review` for a PR review's. */
export function formatComments(comments: ReviewComment[], review = false): string {
  const byPath = new Map<string, ReviewComment[]>();
  for (const c of comments) byPath.set(c.path, [...(byPath.get(c.path) ?? []), c]);
  const out: string[] = [];
  for (const path of [...byPath.keys()].sort()) {
    out.push(`## ${path}`);
    const cs = byPath.get(path)!.sort((a, b) => a.endLine - b.endLine || (a.side === b.side ? 0 : a.side === 'old' ? -1 : 1) || a.createdAt - b.createdAt);
    for (const c of cs) {
      const f = fence(c.excerpt);
      const body = c.side === 'new' && hasSuggestion(c.body) ? `${c.body}\n${suggestionNote(c, review)}` : c.body;
      out.push(c.excerpt ? `${where(c)}:\n${f}\n${c.excerpt}\n${f}\n${body}` : `${where(c)}:\n${body}`);
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
    if (r.state !== 'draft') throw new HttpError(409, 'This comment was already sent to Kode, so it can no longer be changed.');
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
      throw new HttpError(409, `You already have ${BATCH_MAX} draft comments, the most that can go to Kode at once. Send them first.`);
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
    const c = this.draft(sessionId, id);
    run(this.db, 'UPDATE review_comments SET body = ?, updated_at = ? WHERE id = ?', checkSide(c.side, checkBody(body)), Date.now(), id);
    return toComment(this.one(sessionId, id));
  }

  remove(sessionId: string, id: string): void {
    this.running(sessionId);
    this.draft(sessionId, id);
    run(this.db, 'DELETE FROM review_comments WHERE id = ?', id);
    this.core.changed();
  }

  /**
   * Writes a draft's suggested change into the worktree, then deletes the draft: the Changes
   * panel's Apply. Only while the agent is between turns with nothing left running in the
   * background, so it never writes under the agent's edit, and only while the lines are still the
   * text the panel shows (`current`), which still ends with the text the suggestion was written on.
   */
  apply(sessionId: string, id: string, body: unknown): void {
    const row = this.running(sessionId);
    const c = this.draft(sessionId, id);
    if (row.kind === 'review-pr') throw new HttpError(409, 'In a PR review, suggestions are review comments for the PR\'s author. Send them to Kode to draft.');
    if (c.side !== 'new') throw new HttpError(409, 'Suggestions go on added or unchanged lines, not removed ones.');
    const found = suggestions(c.body);
    if (found.length !== 1) throw new HttpError(409, found.length ? 'This comment has more than one suggestion. Send it to Kode, or keep one.' : 'This comment has no suggestion to apply.');
    const r = (body && typeof body === 'object' ? body : {}) as Record<string, unknown>;
    const { line, endLine, current } = r;
    if (!Number.isInteger(line) || !Number.isInteger(endLine) || (line as number) < 1 || (endLine as number) < (line as number)
      || !Array.isArray(current) || current.length !== (endLine as number) - (line as number) + 1 || current.some((l) => typeof l !== 'string')) {
      throw new HttpError(400, 'Applying a suggestion needs the lines it replaces: line, endLine, and their text in current.');
    }
    // The lines it would replace must still end with the text the suggestion was written against:
    // the panel draws a comment where its last line is now, which needn't be its whole range.
    if (c.end_line - c.line !== (endLine as number) - (line as number) || !excerptMatches(current as string[], c.excerpt)) {
      throw new HttpError(409, 'The suggestion\'s lines have changed since the comment was written. Send it to Kode instead.');
    }
    // Background subagents and shells a turn left running may be editing files too.
    const rt = this.core.sessions.runtime(sessionId);
    if (MID_TURN.includes(row.status) || rt.busy || rt.background.length) {
      throw new HttpError(409, 'Kode is working. Apply the suggestion once it\'s idle, or send it to Kode.');
    }
    if (!existsSync(row.worktree_path) || !this.core.sessions.ownsWorktree(row)) throw new HttpError(409, 'This session\'s worktree is gone, so there\'s nothing to apply the suggestion to.');
    try {
      replaceLines(row.worktree_path, c.path, line as number, endLine as number, current as string[], found[0]);
    } catch (e) {
      if (e instanceof ReplaceError) throw new HttpError(409, e.message);
      if (e instanceof OutsideRepoError) throw new HttpError(400, 'Only files inside this session\'s repo can be changed.');
      const code = (e as NodeJS.ErrnoException).code;
      throw new HttpError(500, `Couldn't write the suggestion into ${c.path}${code ? ` (${code})` : ''}: ${errMsg(e)}`);
    }
    run(this.db, 'DELETE FROM review_comments WHERE id = ?', id);
    this.core.sessions.event(sessionId, 'suggestion', `Applied a suggestion to ${c.path}`);
    this.core.changed();
    // The footer's diff size, now rather than at the next periodic refresh.
    void this.core.sessions.refreshDiff(sessionId).catch(() => undefined);
  }

  /** Queues every draft, then pastes the queue into Claude if it's between turns. */
  async send(sessionId: string): Promise<SendCommentsResult> {
    const row = this.running(sessionId);
    // An agent stopped for sitting idle starts again for them (deliver).
    if (row.status === 'exited' && !row.idle_stopped_at) throw new HttpError(409, 'Claude has exited. Resume the session to send comments.');
    const moved = run(this.db, `UPDATE review_comments SET state = 'queued', updated_at = ? WHERE session_id = ? AND state = 'draft'`,
      Date.now(), sessionId).changes;
    if (!moved && !commentCounts(this.db, sessionId).queued) throw new HttpError(409, 'There are no draft comments to send.');
    this.core.changed();
    let sent: number;
    try {
      sent = await this.deliver(sessionId);
    } catch (e) {
      throw new HttpError(502, `Couldn't send the comments to Claude: ${errMsg(e)}. They stay queued and are retried when Claude finishes a turn.`);
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
    // An agent stopped for sitting idle starts again for queued comments; they go once it's up.
    if (row.idle_stopped_at && COMMENT_STAGES.includes(row.stage)
      && get(this.db, "SELECT 1 AS x FROM review_comments WHERE session_id = ? AND state = 'queued'", sessionId)) {
      this.core.sessions.wakeSoon(sessionId, 'for your review comments');
      return 0;
    }
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
      await this.core.sessions.submit(sessionId, this.core.sessions.prompt(row, prompt, { count, comments: formatComments(queued, row.kind === 'review-pr') }));
      const now = Date.now();
      tx(this.db, () => {
        for (const c of queued) run(this.db, `UPDATE review_comments SET state = 'sent', sent_at = ?, updated_at = ? WHERE id = ?`, now, now, c.id);
      });
      this.core.sessions.event(sessionId, 'comments', `Sent ${count} to Kode`);
      this.core.changed();
      return queued.length;
    } finally {
      this.delivering.delete(sessionId);
    }
  }
}
