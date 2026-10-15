import type { MekoGap, MekoGapCounts, MekoGapPage, MekoGapQuality, MekoGapSession, MekoGapState } from '../shared/types.js';
import { all, run, type Db } from './db.js';

/**
 * What Meko doesn't know: a project's Meko searches that found nothing or nothing relevant
 * (`meko_calls.quality`, set by the transcript reader, and `meko_preloads.quality` for AgentK's
 * search at start), grouped by query (`query_key`), with what the user did about each
 * (`meko_gap_marks`), or a session's learn step did. A question whose latest ask found something
 * relevant is left out; one asked again after it was dismissed or taught is open again.
 */

/** Questions on one page. */
export const GAP_PAGE = 100;
/** Sessions named on a question. */
const GAP_SESSIONS = 20;
/** The most questions a learn step is offered (the session's latest), and how much of each. */
export const GAP_LEARN_MAX = 30;
export const GAP_LEARN_CHARS = 300;

interface AskRow {
  key: string;
  query: string;
  quality: MekoGapQuality;
  source: 'claude' | 'start';
  tool: string;
  results: number | null;
  top_score: number | null;
  top_raw: number | null;
  at: number;
  session_id: string;
  session_name: string;
  desired: string;
}

interface MarkRow {
  query_key: string;
  dismissed_at: number | null;
  taught_at: number | null;
  taught_text: string | null;
  taught_datapack: string | null;
  opened_at: number | null;
  taught_session: string | null;
  taught_session_name: string | null;
  taught_session_desired: string | null;
  dismissed_session: string | null;
  dismissed_session_name: string | null;
  dismissed_session_desired: string | null;
  dismissed_memory: string | null;
}

/** The session a mark names, or null when a person made it. */
function markSession(id: string | null, name: string | null, desired: string | null): MekoGapSession | null {
  return id ? { id, name: name ?? id, archived: desired !== 'running' } : null;
}

/** A question's state from its marks: the latest action at or after its last ask, else open. */
function stateOf(m: MarkRow | undefined, lastAt: number): { state: MekoGapState; markedAt: number | null } {
  if (!m) return { state: 'open', markedAt: null };
  const actions: [MekoGapState, number | null][] = [['dismissed', m.dismissed_at], ['taught', m.taught_at], ['open', m.opened_at]];
  let latest: [MekoGapState, number] | null = null;
  for (const [state, at] of actions) if (at !== null && (!latest || at > latest[1])) latest = [state, at];
  if (!latest || latest[1] < lastAt || latest[0] === 'open') return { state: 'open', markedAt: null };
  return { state: latest[0], markedAt: latest[1] };
}

/** Every question of a project, latest asked first (ties by key). */
export function projectGaps(db: Db, projectId: string): MekoGap[] {
  const asks = all<AskRow>(db,
    `SELECT c.query_key AS key, c.query, c.quality, 'claude' AS source, c.tool, c.results, c.top_score, c.top_raw, c.at,
       s.id AS session_id, s.name AS session_name, s.desired
     FROM meko_calls c JOIN sessions s ON s.id = c.session_id
     WHERE s.project_id = ? AND c.quality IS NOT NULL AND c.query_key IS NOT NULL
     UNION ALL
     SELECT p.query_key, p.query, p.quality, 'start', 'context_search', NULL, p.top_score, NULL, p.at, s.id, s.name, s.desired
     FROM meko_preloads p JOIN sessions s ON s.id = p.session_id
     WHERE s.project_id = ? AND p.quality IS NOT NULL AND p.query_key IS NOT NULL
     ORDER BY at DESC`, projectId, projectId);
  if (!asks.length) return [];
  // The latest ask of each question that found something relevant: one after the poor asks answers it.
  const answered = new Map(all<{ key: string; at: number }>(db,
    `SELECT key, MAX(at) AS at FROM (
       SELECT c.query_key AS key, c.at FROM meko_calls c JOIN sessions s ON s.id = c.session_id
       WHERE s.project_id = ? AND c.query_key IS NOT NULL AND c.outcome = 'found' AND c.quality IS NULL
       UNION ALL
       SELECT p.query_key, p.at FROM meko_preloads p JOIN sessions s ON s.id = p.session_id
       WHERE s.project_id = ? AND p.query_key IS NOT NULL AND p.outcome != 'error' AND p.quality IS NULL
     ) GROUP BY key`, projectId, projectId).map((r) => [r.key, r.at]));
  const marks = new Map(all<MarkRow>(db,
    `SELECT m.query_key, m.dismissed_at, m.taught_at, m.taught_text, m.taught_datapack, m.opened_at,
       m.taught_session, ts.name AS taught_session_name, ts.desired AS taught_session_desired,
       m.dismissed_session, ds.name AS dismissed_session_name, ds.desired AS dismissed_session_desired, m.dismissed_memory
     FROM meko_gap_marks m LEFT JOIN sessions ts ON ts.id = m.taught_session LEFT JOIN sessions ds ON ds.id = m.dismissed_session
     WHERE m.project_id = ?`, projectId)
    .map((m) => [m.query_key, m]));
  const groups = new Map<string, AskRow[]>();
  for (const a of asks) {
    const g = groups.get(a.key);
    if (g) g.push(a);
    else groups.set(a.key, [a]);
  }
  const out: MekoGap[] = [];
  for (const [key, g] of groups) {
    const latest = g[0];
    if ((answered.get(key) ?? -Infinity) > latest.at) continue;
    const sessions: MekoGap['sessions'] = [];
    for (const a of g) {
      if (sessions.length === GAP_SESSIONS) break;
      if (!sessions.some((s) => s.id === a.session_id)) sessions.push({ id: a.session_id, name: a.session_name, archived: a.desired === 'archived' });
    }
    const mark = marks.get(key);
    const state = stateOf(mark, latest.at);
    out.push({
      key,
      query: latest.query,
      quality: latest.quality,
      source: latest.source,
      tool: latest.tool,
      results: latest.results,
      topScore: latest.top_score,
      topRaw: latest.top_raw,
      asks: g.length,
      sessions,
      firstAt: g[g.length - 1].at,
      lastAt: latest.at,
      ...state,
      taught: mark?.taught_at != null
        ? { text: mark.taught_text ?? '', datapack: mark.taught_datapack, at: mark.taught_at, session: markSession(mark.taught_session, mark.taught_session_name, mark.taught_session_desired) }
        : null,
      dismissal: mark && state.state === 'dismissed'
        ? { session: markSession(mark.dismissed_session, mark.dismissed_session_name, mark.dismissed_session_desired), memory: mark.dismissed_memory }
        : null,
    });
  }
  return out.sort((a, b) => b.lastAt - a.lastAt || (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
}

/** The open questions by how their latest ask went, for the line under the Meko stats. */
export function openGapCounts(db: Db, projectId: string): MekoGapCounts {
  return countOpen(projectGaps(db, projectId));
}

function countOpen(gaps: MekoGap[]): MekoGapCounts {
  const c: MekoGapCounts = { empty: 0, weak: 0 };
  for (const g of gaps) if (g.state === 'open') c[g.quality]++;
  return c;
}

/** Where a page starts: after the question with this `lastAt` and `key` (the last one shown). */
export interface GapCursor {
  lastAt: number;
  key: string;
}

/** One page of a project's questions in a state (or all of them), in `projectGaps` order, after `after`. */
export function gapPage(db: Db, projectId: string, state: MekoGapState | 'all', after?: GapCursor): MekoGapPage {
  const gaps = projectGaps(db, projectId);
  const counts: MekoGapPage['counts'] = { open: 0, dismissed: 0, taught: 0 };
  for (const g of gaps) counts[g.state]++;
  const matching = gaps.filter((g) => (state === 'all' || g.state === state) &&
    (!after || g.lastAt < after.lastAt || (g.lastAt === after.lastAt && g.key > after.key)));
  const recorded = all<{ n: number; since: number | null }>(db,
    `SELECT COUNT(*) AS n, MIN(at) AS since FROM (
       SELECT c.at FROM meko_calls c JOIN sessions s ON s.id = c.session_id WHERE s.project_id = ? AND c.query_key IS NOT NULL
       UNION ALL
       SELECT p.at FROM meko_preloads p JOIN sessions s ON s.id = p.session_id WHERE s.project_id = ? AND p.query_key IS NOT NULL
     )`, projectId, projectId)[0];
  return {
    gaps: matching.slice(0, GAP_PAGE),
    more: matching.length > GAP_PAGE,
    counts,
    open: countOpen(gaps),
    searches: recorded?.n ?? 0,
    since: recorded?.since ?? null,
  };
}

/** A project's question by its key, or null when it has none. */
export function findGap(db: Db, projectId: string, key: string): MekoGap | null {
  return projectGaps(db, projectId).find((g) => g.key === key) ?? null;
}

/**
 * The project's open questions a session asked in Claude's own searches (AgentK's search at start
 * doesn't count: its query is the task), latest first, for its learn step to answer: at most
 * `GAP_LEARN_MAX`, each query as last asked, cut to `GAP_LEARN_CHARS`.
 */
export function sessionGaps(db: Db, projectId: string, sessionId: string): { key: string; query: string }[] {
  const asked = new Set(all<{ key: string }>(db,
    'SELECT DISTINCT query_key AS key FROM meko_calls WHERE session_id = ? AND quality IS NOT NULL AND query_key IS NOT NULL', sessionId).map((r) => r.key));
  if (!asked.size) return [];
  return projectGaps(db, projectId).filter((g) => g.state === 'open' && asked.has(g.key)).slice(0, GAP_LEARN_MAX)
    .map((g) => ({ key: g.key, query: g.query.replace(/\s+/g, ' ').trim().slice(0, GAP_LEARN_CHARS) }));
}

/**
 * What was done with a question: dismissed (by a session's learn step, `session`, when Meko already
 * had the answer, the memory that said it), taught (what was saved, where, and the session whose
 * learn step saved it), or restored (open again: a dismissal is cleared, what was taught is kept).
 * Without `session`, a person did it.
 */
export type GapAction =
  | { kind: 'dismiss'; session?: string; memory?: string | null }
  | { kind: 'teach'; text: string; datapack: string; session?: string }
  | { kind: 'restore' };

export function markGap(db: Db, projectId: string, key: string, action: GapAction, at = Date.now()): void {
  run(db, 'INSERT INTO meko_gap_marks(project_id, query_key) VALUES (?, ?) ON CONFLICT DO NOTHING', projectId, key);
  if (action.kind === 'dismiss') {
    run(db, 'UPDATE meko_gap_marks SET dismissed_at = ?, dismissed_session = ?, dismissed_memory = ? WHERE project_id = ? AND query_key = ?',
      at, action.session ?? null, action.memory ?? null, projectId, key);
  } else if (action.kind === 'teach') {
    run(db, 'UPDATE meko_gap_marks SET taught_at = ?, taught_text = ?, taught_datapack = ?, taught_session = ? WHERE project_id = ? AND query_key = ?',
      at, action.text, action.datapack, action.session ?? null, projectId, key);
  } else {
    run(db, 'UPDATE meko_gap_marks SET dismissed_at = NULL, dismissed_session = NULL, dismissed_memory = NULL, opened_at = ? WHERE project_id = ? AND query_key = ?',
      at, projectId, key);
  }
}

/** A dismissal (not restored since) or a teach, and when: what the usage report counts. */
export interface GapMark {
  at: number;
  state: 'dismissed' | 'taught';
}

/**
 * Each question's latest dismissal (unless restored since) and latest teach, by when, when a person
 * made it. Asking again doesn't undo either: these count what users did, not what's on the list
 * now. A learn step's marks are counted with the learn step (`meko_learns`).
 */
export function gapMarks(db: Db): GapMark[] {
  const out: GapMark[] = [];
  for (const m of all<{ dismissed_at: number | null; taught_at: number | null; dismissed_session: string | null; taught_session: string | null }>(db,
    'SELECT dismissed_at, taught_at, dismissed_session, taught_session FROM meko_gap_marks')) {
    if (m.dismissed_at !== null && m.dismissed_session === null) out.push({ at: m.dismissed_at, state: 'dismissed' });
    if (m.taught_at !== null && m.taught_session === null) out.push({ at: m.taught_at, state: 'taught' });
  }
  return out;
}
