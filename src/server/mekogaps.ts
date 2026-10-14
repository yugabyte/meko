import type { MekoGap, MekoGapCounts, MekoGapPage, MekoGapQuality, MekoGapState } from '../shared/types.js';
import { all, run, type Db } from './db.js';

/**
 * What Meko doesn't know: a project's Meko searches that found nothing or nothing relevant
 * (`meko_calls.quality`, set by the transcript reader, and `meko_preloads.quality` for AgentK's
 * search at start), grouped by query (`query_key`), with what the user did about each
 * (`meko_gap_marks`). A question whose latest ask found something relevant is left out; one asked
 * again after it was dismissed or taught is open again.
 */

/** Questions on one page. */
export const GAP_PAGE = 100;
/** Sessions named on a question. */
const GAP_SESSIONS = 20;

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
    'SELECT query_key, dismissed_at, taught_at, taught_text, taught_datapack, opened_at FROM meko_gap_marks WHERE project_id = ?', projectId)
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
      ...stateOf(mark, latest.at),
      taught: mark?.taught_at != null ? { text: mark.taught_text ?? '', datapack: mark.taught_datapack, at: mark.taught_at } : null,
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
 * What the user did with a question: dismissed it, taught Meko (what was saved, and where), or
 * restored it (open again: a dismissal is cleared, what was taught is kept).
 */
export type GapAction = { kind: 'dismiss' } | { kind: 'teach'; text: string; datapack: string } | { kind: 'restore' };

export function markGap(db: Db, projectId: string, key: string, action: GapAction, at = Date.now()): void {
  run(db, 'INSERT INTO meko_gap_marks(project_id, query_key) VALUES (?, ?) ON CONFLICT DO NOTHING', projectId, key);
  if (action.kind === 'dismiss') {
    run(db, 'UPDATE meko_gap_marks SET dismissed_at = ? WHERE project_id = ? AND query_key = ?', at, projectId, key);
  } else if (action.kind === 'teach') {
    run(db, 'UPDATE meko_gap_marks SET taught_at = ?, taught_text = ?, taught_datapack = ? WHERE project_id = ? AND query_key = ?',
      at, action.text, action.datapack, projectId, key);
  } else {
    run(db, 'UPDATE meko_gap_marks SET dismissed_at = NULL, opened_at = ? WHERE project_id = ? AND query_key = ?', at, projectId, key);
  }
}

/** A dismissal (not restored since) or a teach, and when: what the usage report counts. */
export interface GapMark {
  at: number;
  state: 'dismissed' | 'taught';
}

/**
 * Each question's latest dismissal (unless restored since) and latest teach, by when. Asking again
 * doesn't undo either: these count what users did, not what's on the list now.
 */
export function gapMarks(db: Db): GapMark[] {
  const out: GapMark[] = [];
  for (const m of all<{ dismissed_at: number | null; taught_at: number | null }>(db, 'SELECT dismissed_at, taught_at FROM meko_gap_marks')) {
    if (m.dismissed_at !== null) out.push({ at: m.dismissed_at, state: 'dismissed' });
    if (m.taught_at !== null) out.push({ at: m.taught_at, state: 'taught' });
  }
  return out;
}
