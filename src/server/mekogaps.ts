import type { MekoGap, MekoGapCounts, MekoGapPage, MekoGapQuality, MekoGapState } from '../shared/types.js';
import { all, run, type Db } from './db.js';

/**
 * What Meko doesn't know: a project's Meko searches that found nothing or nothing relevant
 * (`meko_calls.quality`, set by the transcript reader, and AgentK's searches at start that found
 * nothing), grouped by query (`query_key`), with what the user did about each (`meko_gap_marks`).
 * A question whose latest ask found something relevant is left out; one asked again after it was
 * dismissed or taught is open again.
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
  state: 'dismissed' | 'taught';
  text: string | null;
  datapack_name: string | null;
  at: number;
}

/** Every question of a project, latest asked first. */
export function projectGaps(db: Db, projectId: string): MekoGap[] {
  const asks = all<AskRow>(db,
    `SELECT c.query_key AS key, c.query, c.quality, 'claude' AS source, c.tool, c.results, c.top_score, c.top_raw, c.at,
       s.id AS session_id, s.name AS session_name, s.desired
     FROM meko_calls c JOIN sessions s ON s.id = c.session_id
     WHERE s.project_id = ? AND c.quality IS NOT NULL AND c.query_key IS NOT NULL
     UNION ALL
     SELECT p.query_key, p.query, 'empty', 'start', 'context_search', 0, NULL, NULL, p.at, s.id, s.name, s.desired
     FROM meko_preloads p JOIN sessions s ON s.id = p.session_id
     WHERE s.project_id = ? AND p.outcome = 'empty' AND p.query_key IS NOT NULL
     ORDER BY at DESC`, projectId, projectId);
  if (!asks.length) return [];
  // The latest ask of each question that found something relevant: one after the poor asks answers it.
  const answered = new Map(all<{ key: string; at: number }>(db,
    `SELECT key, MAX(at) AS at FROM (
       SELECT c.query_key AS key, c.at FROM meko_calls c JOIN sessions s ON s.id = c.session_id
       WHERE s.project_id = ? AND c.query_key IS NOT NULL AND c.outcome = 'found' AND c.quality IS NULL
       UNION ALL
       SELECT p.query_key, p.at FROM meko_preloads p JOIN sessions s ON s.id = p.session_id
       WHERE s.project_id = ? AND p.query_key IS NOT NULL AND p.outcome IN ('found', 'late')
     ) GROUP BY key`, projectId, projectId).map((r) => [r.key, r.at]));
  const marks = new Map(all<MarkRow>(db, 'SELECT query_key, state, text, datapack_name, at FROM meko_gap_marks WHERE project_id = ?', projectId)
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
    const marked = mark && mark.at >= latest.at ? mark : null;
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
      state: marked?.state ?? 'open',
      markedAt: marked?.at ?? null,
      taught: mark?.state === 'taught' ? { text: mark.text ?? '', datapack: mark.datapack_name, at: mark.at } : null,
    });
  }
  return out;
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

/** One page of a project's questions in a state (or all of them), latest asked before `before` first. */
export function gapPage(db: Db, projectId: string, state: MekoGapState | 'all', before?: number): MekoGapPage {
  const gaps = projectGaps(db, projectId);
  const counts: MekoGapPage['counts'] = { open: 0, dismissed: 0, taught: 0 };
  for (const g of gaps) counts[g.state]++;
  const matching = gaps.filter((g) => (state === 'all' || g.state === state) && (before === undefined || g.lastAt < before));
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

/** Marks a question dismissed or taught, or clears its mark (`null`: open again). */
export function markGap(db: Db, projectId: string, key: string, mark: { state: 'dismissed' } | { state: 'taught'; text: string; datapack: string } | null, at = Date.now()): void {
  if (!mark) {
    run(db, 'DELETE FROM meko_gap_marks WHERE project_id = ? AND query_key = ?', projectId, key);
    return;
  }
  const taught = mark.state === 'taught' ? mark : null;
  run(db, `INSERT INTO meko_gap_marks(project_id, query_key, state, text, datapack_name, at) VALUES (?, ?, ?, ?, ?, ?)
           ON CONFLICT(project_id, query_key) DO UPDATE SET state = excluded.state, text = excluded.text,
             datapack_name = excluded.datapack_name, at = excluded.at`,
    projectId, key, mark.state, taught?.text ?? null, taught?.datapack ?? null, at);
}

/** A question dismissed or taught (and still so), and when: what the usage report counts. */
export interface GapMark {
  at: number;
  state: 'dismissed' | 'taught';
}

export function gapMarks(db: Db): GapMark[] {
  return all<GapMark>(db, 'SELECT at, state FROM meko_gap_marks');
}
