import { closeSync, existsSync, fstatSync, openSync, readSync, readdirSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import type { DailyUsage, ModelUsage, ProjectUsage, ProjectUsageSummary, SessionUsage, TokenUsage, UsageOverview } from '../shared/types.js';
import { all, get, run, tx, type Db } from './db.js';
import type { SessionRow } from './store.js';

export const ZERO_USAGE: TokenUsage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, messages: 0 };

export function totalTokens(u: TokenUsage): number {
  return u.input + u.output + u.cacheRead + u.cacheWrite;
}

export function addUsage(a: TokenUsage, b: TokenUsage): TokenUsage {
  return { input: a.input + b.input, output: a.output + b.output, cacheRead: a.cacheRead + b.cacheRead, cacheWrite: a.cacheWrite + b.cacheWrite, messages: a.messages + b.messages };
}

const CHUNK = 1024 * 1024;

/**
 * Transcript files for a session: the main conversation plus any subagent
 * transcripts Claude Code writes under <session-id>/subagents/.
 */
export function transcriptFiles(mainPath: string): { path: string; agent: string | null }[] {
  const out: { path: string; agent: string | null }[] = [];
  if (existsSync(mainPath)) out.push({ path: mainPath, agent: null });
  const subDir = join(dirname(mainPath), basename(mainPath, '.jsonl'), 'subagents');
  try {
    for (const f of readdirSync(subDir)) {
      if (f.endsWith('.jsonl')) out.push({ path: join(subDir, f), agent: f.replace(/^agent-/, '').replace(/\.jsonl$/, '') });
    }
  } catch {
    /* no subagents */
  }
  return out;
}

interface UsageRow {
  messageId: string;
  model: string;
  at: number;
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
}

/** Pulls the usage out of one transcript line, or null if it isn't a model response. */
export function parseUsageLine(line: string): UsageRow | null {
  if (!line.includes('"assistant"') || !line.includes('"usage"')) return null;
  let d: { type?: string; timestamp?: string; message?: { id?: string; model?: string; usage?: Record<string, unknown> } };
  try {
    d = JSON.parse(line);
  } catch {
    return null;
  }
  const m = d.message;
  if (d.type !== 'assistant' || !m?.id || !m.usage || !m.model || m.model === '<synthetic>') return null;
  const n = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
  const at = d.timestamp ? Date.parse(d.timestamp) : NaN;
  return {
    messageId: m.id,
    model: m.model,
    at: Number.isFinite(at) ? at : Date.now(),
    input: n(m.usage.input_tokens),
    output: n(m.usage.output_tokens),
    cacheRead: n(m.usage.cache_read_input_tokens),
    cacheWrite: n(m.usage.cache_creation_input_tokens),
  };
}

/** A push to a session's base branch: git's `old..new  src -> dst` line from a Bash tool result. */
export interface PushRow {
  /** `old..new`, what `git rev-list --count` takes. */
  range: string;
  ref: string;
  at: number;
}

// One ref update in `git push` output. Forced updates (`+ a...b`) and new refs (`* [new branch]`) don't match.
const PUSH_REF = /^\s*([0-9a-f]{7,40})\.\.([0-9a-f]{7,40})\s+\S+\s+->\s+(\S+)\s*$/gm;

/**
 * Pushes to `baseBranch` in one transcript line. Only a push counts: output with git push's
 * `To <remote>` line, or a result Claude Code marked as a push (`gitOperation.push`), which
 * catches `git push | tail -1` too. Fetch and pull output (`main -> origin/main`) never matches.
 */
export function parsePushLine(line: string, baseBranch: string): PushRow[] {
  if (!line.includes('"toolUseResult"') || !line.includes(' -> ')) return [];
  let d: { type?: string; timestamp?: string; toolUseResult?: unknown };
  try {
    d = JSON.parse(line);
  } catch {
    return [];
  }
  const r = d.toolUseResult as { stdout?: unknown; stderr?: unknown; gitOperation?: unknown } | null | undefined;
  if (d.type !== 'user' || !r || typeof r !== 'object') return [];
  const out = [r.stdout, r.stderr].filter((x): x is string => typeof x === 'string').join('\n');
  const markedPush = !!r.gitOperation && typeof r.gitOperation === 'object' && 'push' in r.gitOperation;
  if (!markedPush && !/^To \S/m.test(out)) return [];
  const t = d.timestamp ? Date.parse(d.timestamp) : NaN;
  const at = Number.isFinite(t) ? t : Date.now();
  const rows: PushRow[] = [];
  for (const m of out.matchAll(PUSH_REF)) {
    const ref = m[3].replace(/^refs\/heads\//, '');
    if (ref === baseBranch) rows.push({ range: `${m[1]}..${m[2]}`, ref, at });
  }
  return rows;
}

/**
 * Reads new lines from a session's transcripts (from the last byte offset
 * seen) and records one row per model response. A response spans several
 * transcript lines with the same message id, so rows are keyed by it.
 * Returns true if anything new was recorded.
 */
export function ingestSessionUsage(db: Db, sessionId: string, mainTranscript: string, baseBranch?: string): boolean {
  let changed = false;
  for (const { path, agent } of transcriptFiles(mainTranscript)) {
    const prev = get<{ offset: number }>(db, 'SELECT offset FROM usage_files WHERE session_id = ? AND path = ?', sessionId, path);
    let offset = prev?.offset ?? 0;
    let fd: number;
    try {
      fd = openSync(path, 'r');
    } catch {
      continue;
    }
    try {
      const size = fstatSync(fd).size;
      if (size < offset) offset = 0; // rewritten or truncated: start over (rows are upserts)
      if (size === offset) continue;
      const rows: UsageRow[] = [];
      const pushes: PushRow[] = [];
      // Split on newline bytes and decode whole lines only, so a multi-byte
      // character across a chunk boundary is never cut in half.
      let carry: Buffer = Buffer.alloc(0);
      let pos = offset;
      let consumed = offset;
      const buf = Buffer.alloc(CHUNK);
      while (pos < size) {
        const n = readSync(fd, buf, 0, Math.min(CHUNK, size - pos), pos);
        if (n <= 0) break;
        pos += n;
        const data = carry.length ? Buffer.concat([carry, buf.subarray(0, n)]) : Buffer.from(buf.subarray(0, n));
        const lastNl = data.lastIndexOf(0x0a);
        if (lastNl < 0) {
          carry = data;
          continue;
        }
        for (const line of data.subarray(0, lastNl).toString('utf8').split('\n')) {
          const r = parseUsageLine(line);
          if (r) rows.push(r);
          if (baseBranch) pushes.push(...parsePushLine(line, baseBranch));
        }
        carry = data.subarray(lastNl + 1);
        consumed = pos - carry.length;
      }
      tx(db, () => {
        for (const p of pushes) {
          run(db, 'INSERT INTO pushes(session_id, range, ref, at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING', sessionId, p.range, p.ref, p.at);
        }
        for (const r of rows) {
          run(db, `INSERT INTO usage_messages(session_id, message_id, agent, model, at, input, output, cache_read, cache_write)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                   ON CONFLICT(session_id, message_id) DO UPDATE SET
                     input = MAX(input, excluded.input), output = MAX(output, excluded.output),
                     cache_read = MAX(cache_read, excluded.cache_read), cache_write = MAX(cache_write, excluded.cache_write)`,
            sessionId, r.messageId, agent, r.model, r.at, r.input, r.output, r.cacheRead, r.cacheWrite);
        }
        run(db, `INSERT INTO usage_files(session_id, path, offset) VALUES (?, ?, ?)
                 ON CONFLICT(session_id, path) DO UPDATE SET offset = excluded.offset`, sessionId, path, consumed);
      });
      if (rows.length || pushes.length) changed = true;
    } finally {
      closeSync(fd);
    }
  }
  return changed;
}

interface SumRow {
  input: number | null;
  output: number | null;
  cache_read: number | null;
  cache_write: number | null;
  messages: number;
}

const SUMS = `COALESCE(SUM(input), 0) AS input, COALESCE(SUM(output), 0) AS output,
  COALESCE(SUM(cache_read), 0) AS cache_read, COALESCE(SUM(cache_write), 0) AS cache_write, COUNT(*) AS messages`;

function toUsage(r: SumRow | undefined): TokenUsage {
  if (!r) return { ...ZERO_USAGE };
  return { input: r.input ?? 0, output: r.output ?? 0, cacheRead: r.cache_read ?? 0, cacheWrite: r.cache_write ?? 0, messages: r.messages };
}

/** Totals for every session in one query (for the state snapshot). */
export function usageBySession(db: Db): Map<string, TokenUsage> {
  const rows = all<SumRow & { session_id: string }>(db, `SELECT session_id, ${SUMS} FROM usage_messages GROUP BY session_id`);
  return new Map(rows.map((r) => [r.session_id, toUsage(r)]));
}

/** Totals for one session, read through the primary key rather than a scan of every row. */
export function sessionUsage(db: Db, sessionId: string): TokenUsage {
  return toUsage(get<SumRow>(db, `SELECT ${SUMS} FROM usage_messages WHERE session_id = ?`, sessionId));
}

/** Totals per project, including archived sessions. */
export function usageByProject(db: Db): Map<string, TokenUsage> {
  const rows = all<SumRow & { project_id: string }>(db,
    `SELECT s.project_id, ${SUMS} FROM usage_messages u JOIN sessions s ON s.id = u.session_id GROUP BY s.project_id`);
  return new Map(rows.map((r) => [r.project_id, toUsage(r)]));
}

function localDay(ms: number): string {
  const d = new Date(ms);
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

/** Usage breakdown for the project home: totals, by model, by session, and the last `days` days. */
export function projectUsage(db: Db, projectId: string, days = 14, now = Date.now()): ProjectUsage {
  const total = toUsage(get<SumRow>(db,
    `SELECT ${SUMS} FROM usage_messages u JOIN sessions s ON s.id = u.session_id WHERE s.project_id = ?`, projectId));
  const byModel: ModelUsage[] = all<SumRow & { model: string }>(db,
    `SELECT u.model, ${SUMS} FROM usage_messages u JOIN sessions s ON s.id = u.session_id
     WHERE s.project_id = ? GROUP BY u.model`, projectId)
    .map((r) => ({ model: r.model, usage: toUsage(r) }))
    .sort((a, b) => totalTokens(b.usage) - totalTokens(a.usage));
  const bySession: SessionUsage[] = all<SumRow & { id: string; name: string; stage: string; desired: string }>(db,
    `SELECT s.id, s.name, s.stage, s.desired, ${SUMS} FROM usage_messages u JOIN sessions s ON s.id = u.session_id
     WHERE s.project_id = ? GROUP BY s.id`, projectId)
    .map((r) => ({ sessionId: r.id, name: r.name, stage: r.stage as SessionUsage['stage'], archived: r.desired === 'archived', usage: toUsage(r) }))
    .sort((a, b) => totalTokens(b.usage) - totalTokens(a.usage));

  const daily = dailyUsage(db, days, now, projectId);
  return { total, byModel, bySession, daily };
}

/** Daily totals for the last `days` days (in the server's time zone), for one project or all of them. */
function dailyUsage(db: Db, days: number, now: number, projectId?: string): DailyUsage[] {
  const start = new Date(now);
  start.setHours(0, 0, 0, 0);
  start.setDate(start.getDate() - (days - 1));
  const buckets = new Map<string, TokenUsage>();
  for (let i = 0; i < days; i++) {
    const d = new Date(start);
    d.setDate(start.getDate() + i);
    buckets.set(localDay(d.getTime()), { ...ZERO_USAGE });
  }
  const rows = all<{ at: number; input: number; output: number; cache_read: number; cache_write: number }>(db,
    `SELECT u.at, u.input, u.output, u.cache_read, u.cache_write FROM usage_messages u JOIN sessions s ON s.id = u.session_id
     WHERE u.at >= ?${projectId ? ' AND s.project_id = ?' : ''}`, ...(projectId ? [start.getTime(), projectId] : [start.getTime()]));
  for (const r of rows) {
    const b = buckets.get(localDay(r.at));
    if (!b) continue;
    b.input += r.input;
    b.output += r.output;
    b.cacheRead += r.cache_read;
    b.cacheWrite += r.cache_write;
    b.messages += 1;
  }
  return [...buckets].map(([day, usage]) => ({ day, usage }));
}

/** Usage across every project, deleted ones included: tokens, merged PRs and direct pushes. */
export function usageOverview(db: Db, days = 14, now = Date.now()): UsageOverview {
  const byUsage = usageByProject(db);
  const counts = <T extends { project_id: string }>(sql: string) => new Map(all<T>(db, sql).map((r) => [r.project_id, r]));
  const sessions = counts<{ project_id: string; n: number }>('SELECT project_id, COUNT(*) AS n FROM sessions GROUP BY project_id');
  // A restart mid-cleanup can log a merge twice; count sessions. Merge & Close ends in Done only
  // after a merge, so a Done session with a PR counts too (its event may predate a pruning release).
  const merged = counts<{ project_id: string; n: number }>(
    `SELECT project_id, COUNT(DISTINCT session_id) AS n FROM (
       SELECT project_id, session_id FROM session_events WHERE kind = 'merged'
       UNION SELECT project_id, id FROM sessions WHERE stage = 'done' AND pr_number IS NOT NULL
     ) GROUP BY project_id`);
  // commits is -1 when git couldn't count a push's range: the push counts, its commits don't.
  const pushed = counts<{ project_id: string; pushes: number; commits: number }>(
    `SELECT s.project_id, COUNT(*) AS pushes, COALESCE(SUM(CASE WHEN p.commits > 0 THEN p.commits ELSE 0 END), 0) AS commits
     FROM pushes p JOIN sessions s ON s.id = p.session_id GROUP BY s.project_id`);
  const last = counts<{ project_id: string; at: number }>(
    'SELECT s.project_id, MAX(u.at) AS at FROM usage_messages u JOIN sessions s ON s.id = u.session_id GROUP BY s.project_id');

  const byProject: ProjectUsageSummary[] = all<{ id: string; name: string; repo_path: string; deleted_at: number | null }>(db,
    'SELECT id, name, repo_path, deleted_at FROM projects')
    .map((p) => ({
      projectId: p.id,
      name: p.name,
      repoPath: p.repo_path,
      deleted: p.deleted_at != null,
      usage: byUsage.get(p.id) ?? { ...ZERO_USAGE },
      sessions: sessions.get(p.id)?.n ?? 0,
      mergedPrs: merged.get(p.id)?.n ?? 0,
      directPushes: pushed.get(p.id)?.pushes ?? 0,
      directCommits: pushed.get(p.id)?.commits ?? 0,
      lastActiveAt: last.get(p.id)?.at ?? null,
    }))
    // A deleted project that never ran a session has nothing to show.
    .filter((p) => !p.deleted || p.sessions > 0)
    .sort((a, b) => Number(a.deleted) - Number(b.deleted) || totalTokens(b.usage) - totalTokens(a.usage));

  const byModel: ModelUsage[] = all<SumRow & { model: string }>(db, `SELECT model, ${SUMS} FROM usage_messages GROUP BY model`)
    .map((r) => ({ model: r.model, usage: toUsage(r) }))
    .sort((a, b) => totalTokens(b.usage) - totalTokens(a.usage));
  const sum = (f: (p: ProjectUsageSummary) => number) => byProject.reduce((n, p) => n + f(p), 0);
  return {
    total: toUsage(get<SumRow>(db, `SELECT ${SUMS} FROM usage_messages`)),
    mergedPrs: sum((p) => p.mergedPrs),
    directPushes: sum((p) => p.directPushes),
    directCommits: sum((p) => p.directCommits),
    byProject,
    byModel,
    daily: dailyUsage(db, days, now),
  };
}

/** Where a session's main transcript is, preferring the path Claude reported. */
export function sessionTranscript(row: SessionRow, fallback: (cwd: string, id: string) => string): string {
  return row.transcript_path && existsSync(row.transcript_path) ? row.transcript_path : fallback(row.worktree_path, row.claude_session_id);
}
