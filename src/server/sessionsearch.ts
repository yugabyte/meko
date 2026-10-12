// Finding a session by what it's about (GET /api/sessions/search): its name, task, PR title, branch and design.
import { SESSION_SEARCH_MAX, type SessionMatch, type SessionMatchField, type Stage } from '../shared/types.js';
import { all, type Db } from './db.js';
import { HttpError } from './projects.js';

/** What a session is searched by. */
export interface SessionDoc {
  id: string;
  projectId: string;
  name: string;
  stage: Stage;
  closed: boolean;
  prNumber: number | null;
  prUrl: string | null;
  createdAt: number;
  fields: Partial<Record<SessionMatchField, string>>;
}

const WEIGHTS: Record<SessionMatchField, number> = { name: 8, pr: 6, task: 5, branch: 3, design: 2 };
const FIELDS = Object.keys(WEIGHTS) as SessionMatchField[];
const MAX_RESULTS = 20;
const SNIPPET_CHARS = 120;

const STOP_WORDS = new Set(
  'a an and are as at be by did do does for from had has have how i in into is it its me my of on or our that the their this to was we were what when where which who why with you your about session sessions find'.split(' '),
);

/** The words searched for: lowercased, without stop words (unless that leaves none) or repeats. */
export function searchTerms(q: string): string[] {
  const words = q.toLowerCase().split(/[^\p{L}\p{N}]+/u).filter((w) => w.length > 1);
  const kept = words.filter((w) => !STOP_WORDS.has(w));
  return [...new Set(kept.length ? kept : words)];
}

/** A word's pattern: at the start of a word, with a long word's last two letters left out so "searching" finds "search". */
function termPattern(term: string): RegExp {
  const stem = term.length > 5 ? term.slice(0, -2) : term;
  return new RegExp(`(?<![\\p{L}\\p{N}])${stem.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}`, 'iu');
}

function snippetAt(text: string, at: number): string {
  const start = Math.max(0, at - SNIPPET_CHARS / 3);
  const cut = text.slice(start, start + SNIPPET_CHARS).replace(/\s+/g, ' ').trim();
  return `${start > 0 ? '…' : ''}${cut}${start + SNIPPET_CHARS < text.length ? '…' : ''}`;
}

/**
 * The sessions with at least half the words, best first: by how many words they have, then by where
 * (each word counts for the field that counts most among those having it), then open before closed,
 * then newest.
 */
export function rankSessions(docs: SessionDoc[], q: string, limit = MAX_RESULTS): SessionMatch[] {
  const terms = searchTerms(q);
  if (!terms.length) return [];
  const patterns = terms.map(termPattern);
  const need = Math.ceil(terms.length / 2);
  const scored = docs.flatMap((d) => {
    let covered = 0;
    let score = 0;
    let best: { field: SessionMatchField; at: number } | null = null;
    for (const re of patterns) {
      const field = FIELDS.find((f) => d.fields[f] && re.test(d.fields[f]));
      if (!field) continue;
      covered++;
      score += WEIGHTS[field];
      if (!best || WEIGHTS[field] > WEIGHTS[best.field]) best = { field, at: d.fields[field]!.search(re) };
    }
    if (!best || covered < need) return [];
    const match: SessionMatch = {
      sessionId: d.id, projectId: d.projectId, name: d.name, stage: d.stage, closed: d.closed, prNumber: d.prNumber, prUrl: d.prUrl,
      field: best.field, snippet: best.field === 'name' ? '' : snippetAt(d.fields[best.field]!, best.at),
    };
    return [{ match, covered, score, d }];
  });
  scored.sort((a, b) => b.covered - a.covered || b.score - a.score || Number(a.d.closed) - Number(b.d.closed) || b.d.createdAt - a.d.createdAt);
  return scored.slice(0, limit).map((s) => s.match);
}

interface Row {
  id: string;
  project_id: string;
  name: string;
  stage: Stage;
  desired: string;
  branch: string;
  initial_prompt: string | null;
  pr_number: number | null;
  pr_url: string | null;
  pr_title: string | null;
  created_at: number;
  markdown: string | null;
}

/** The sessions of the projects open (or only `projectId`) matching `q`; see rankSessions. */
export function searchSessions(db: Db, q: unknown, projectId?: string): SessionMatch[] {
  if (typeof q !== 'string' || q.length > SESSION_SEARCH_MAX) {
    throw new HttpError(400, `Search for at most ${SESSION_SEARCH_MAX} characters.`);
  }
  if (!searchTerms(q).length) return [];
  const rows = all<Row>(db,
    `SELECT s.id, s.project_id, s.name, s.stage, s.desired, s.branch, s.initial_prompt, s.pr_number, s.pr_url, s.pr_title,
            s.created_at, d.markdown
       FROM sessions s JOIN projects p ON p.id = s.project_id AND p.deleted_at IS NULL
       LEFT JOIN designs d ON d.session_id = s.id
      WHERE ? IS NULL OR s.project_id = ?`,
    projectId ?? null, projectId ?? null);
  return rankSessions(rows.map((r) => ({
    id: r.id, projectId: r.project_id, name: r.name, stage: r.stage, closed: r.desired === 'archived',
    prNumber: r.pr_number, prUrl: r.pr_url, createdAt: r.created_at,
    fields: {
      name: r.name,
      pr: [r.pr_number != null ? `#${r.pr_number}` : '', r.pr_title ?? ''].join(' ').trim(),
      task: r.initial_prompt ?? '',
      branch: r.branch,
      design: r.markdown ?? '',
    },
  })), q);
}
