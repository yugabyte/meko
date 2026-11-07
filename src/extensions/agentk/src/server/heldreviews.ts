import type { HeldReview, HeldReviewAuthor, HeldReviewComment, HeldReviewsRequest, ReviewerMode } from '../shared/types.js';
import { all, get, run, type Db } from './db.js';
import { HttpError } from './projects.js';

/** Longest held review body kept, in characters; the rest is on GitHub. */
export const HELD_BODY_MAX = 10_000;
/** Inline comments kept per held review. */
export const HELD_COMMENTS_MAX = 50;
const COMMENT_BODY_MAX = 2000;

/** Who wrote a review, for routing it: a held review's author kinds, and the user (gh's login). */
export type ReviewWho = HeldReviewAuthor | 'user';

/**
 * Where a new review that asks for work (changes requested, or comments with a body) goes in a
 * reviewer mode: straight to Claude, or held for the user to send or dismiss. The user's own
 * reviews always go to Claude; someone with no role on the repo is always held.
 */
export function reviewRoute(mode: ReviewerMode, who: ReviewWho): 'claude' | 'hold' {
  switch (who) {
    case 'user':
      return 'claude';
    case 'outsider':
      return 'hold';
    case 'person':
      return mode === 'mine' ? 'hold' : 'claude';
    case 'bot':
      return mode === 'all' ? 'claude' : 'hold';
  }
}

/** What GitHub says about a review beyond `gh pr view`: its REST id, link and inline comments. */
export interface ReviewDetails {
  dbId: number | null;
  url: string;
  comments: HeldReviewComment[];
  totalComments: number;
}

/** A review to hold, as the poll found it. */
export interface NewHeld {
  id: string;
  author: string;
  kind: HeldReviewAuthor;
  state: string;
  body: string;
  submittedAt: string;
}

interface HeldRow {
  review_id: string;
  db_id: number | null;
  author: string;
  kind: HeldReviewAuthor;
  state: string;
  body: string;
  url: string;
  comments_json: string;
  more_comments: number;
  submitted_at: string;
}

const cut = (s: string, max: number) => (s.length > max ? `${s.slice(0, max)}…` : s);

/** Keeps a review for the user to decide on; one already kept (sent, dismissed or held) stays as it is. */
export function holdReview(db: Db, sessionId: string, pr: number, r: NewHeld, d: ReviewDetails, prUrl: string): void {
  const comments = d.comments.slice(0, HELD_COMMENTS_MAX).map((c) => ({ ...c, body: cut(c.body, COMMENT_BODY_MAX) }));
  const more = Math.max(0, d.totalComments - comments.length);
  run(db, `INSERT OR IGNORE INTO held_reviews(session_id, review_id, pr_number, db_id, author, kind, state, body, url, comments_json, more_comments, submitted_at, held_at)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
  sessionId, r.id, pr, d.dbId, r.author, r.kind, r.state, cut(r.body, HELD_BODY_MAX), d.url || prUrl,
  JSON.stringify(comments), more, r.submittedAt, Date.now());
}

function toHeld(r: HeldRow): HeldReview {
  let comments: HeldReviewComment[] = [];
  try {
    comments = JSON.parse(r.comments_json) as HeldReviewComment[];
  } catch {
    // Unreadable: the review's own page still has them.
  }
  return { id: r.review_id, author: r.author, kind: r.kind, state: r.state, body: r.body, url: r.url, submittedAt: r.submitted_at, comments, moreComments: r.more_comments };
}

const HELD_COLUMNS = 'review_id, db_id, author, kind, state, body, url, comments_json, more_comments, submitted_at';

/** The reviews on PR <pr> still held for the user, oldest first. */
export function heldReviews(db: Db, sessionId: string, pr: number | null): HeldReview[] {
  if (!pr) return [];
  return all<HeldRow>(db, `SELECT ${HELD_COLUMNS} FROM held_reviews WHERE session_id = ? AND pr_number = ? AND status = 'held' ORDER BY held_at, rowid`,
    sessionId, pr).map(toHeld);
}

/** How many reviews on PR <pr> are held, and their authors (each once, oldest first). */
export function heldSummary(db: Db, sessionId: string, pr: number | null): { count: number; authors: string[] } {
  if (!pr) return { count: 0, authors: [] };
  const rows = all<{ author: string }>(db, `SELECT author FROM held_reviews WHERE session_id = ? AND pr_number = ? AND status = 'held' ORDER BY held_at, rowid`,
    sessionId, pr);
  return { count: rows.length, authors: [...new Set(rows.map((r) => r.author))] };
}

/** Most reviews one decision can name. */
const DECIDE_MAX = 100;

/** A send or dismiss request from the browser, checked. */
export function checkHeldRequest(body: unknown): HeldReviewsRequest {
  const b = (body ?? {}) as Partial<Record<keyof HeldReviewsRequest, unknown>>;
  if (b.action !== 'send' && b.action !== 'dismiss') throw new HttpError(400, 'Say whether to send the reviews or dismiss them.');
  if (!Array.isArray(b.ids) || !b.ids.length || b.ids.length > DECIDE_MAX || !b.ids.every((id) => typeof id === 'string' && id.length <= 200)) {
    throw new HttpError(400, `Pick between 1 and ${DECIDE_MAX} reviews.`);
  }
  return { action: b.action, ids: b.ids as string[] };
}

/** A held review the user decided on: what the prompt needs to name it. */
export interface DecidedReview {
  id: string;
  dbId: number | null;
  author: string;
  kind: HeldReviewAuthor;
  state: string;
  url: string;
}

/** Marks the held reviews among <ids> on PR <pr> as sent or dismissed, and returns them. */
export function decideHeld(db: Db, sessionId: string, pr: number, ids: string[], status: 'sent' | 'dismissed'): DecidedReview[] {
  const out: DecidedReview[] = [];
  for (const id of new Set(ids)) {
    const r = get<HeldRow>(db, `SELECT ${HELD_COLUMNS} FROM held_reviews WHERE session_id = ? AND pr_number = ? AND review_id = ? AND status = 'held'`, sessionId, pr, id);
    if (!r) continue;
    run(db, 'UPDATE held_reviews SET status = ?, decided_at = ? WHERE session_id = ? AND review_id = ?', status, Date.now(), sessionId, id);
    out.push({ id: r.review_id, dbId: r.db_id, author: r.author, kind: r.kind, state: r.state, url: r.url });
  }
  return out;
}

/** The session works on another PR now: the old PR's reviews no longer wait for anyone. */
export function dropOtherPrs(db: Db, sessionId: string, pr: number): void {
  run(db, `DELETE FROM held_reviews WHERE session_id = ? AND pr_number != ? AND status = 'held'`, sessionId, pr);
}

/** `a`, `a and b`, `a, b and c`. */
export function andList(names: string[]): string {
  return names.length <= 1 ? (names[0] ?? '') : `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}`;
}

/** The merge blocker while reviews are held: the user decides on each first. */
export function heldBlocker(authors: string[]): string {
  const one = authors.length === 1;
  return `Send the held ${one ? 'review' : 'reviews'} from ${andList(authors)} to Kode or dismiss ${one ? 'it' : 'them'}.`;
}

const STATE_WORDS: Record<string, string> = { CHANGES_REQUESTED: 'requested changes', COMMENTED: 'commented' };

/**
 * The `review-held` prompt's {{reviews}}: a line per review, each naming its REST id for the
 * prompt's `gh api` calls, and marking reviews from someone with no role on the repo.
 */
export function heldPromptLines(reviews: DecidedReview[]): string[] {
  return reviews.map((r) => {
    const id = r.dbId !== null ? `review ${r.dbId}` : 'review';
    const outside = r.kind === 'outsider' ? ' (not a collaborator)' : '';
    return `- @${r.author}${outside} (${STATE_WORDS[r.state] ?? r.state.toLowerCase()}), ${id}: ${r.url}`;
  });
}
