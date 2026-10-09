import { closeSync, existsSync, fstatSync, openSync, readSync, readdirSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { costOf, priceFor } from '../shared/pricing.js';
import type { DailyUsage, ModelUsage, ProjectUsage, ProjectUsageSummary, SessionUsage, TokenUsage, UsageCost, UsageOverview } from '../shared/types.js';
import { all, get, run, tx, type Db } from './db.js';
import {
  firstTurnMeko, learningOf, learnRuns, mekoBy, mekoCalls, parseMekoCalls, parseMekoResults, sessionMeko, summarizeMeko, type MekoCall, type MekoKind, type MekoResult,
} from './mekousage.js';
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
  /** The 1-hour share of cacheWrite. */
  cacheWrite1h: number;
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
  const cacheWrite = n(m.usage.cache_creation_input_tokens);
  // Without the split (older transcripts), count every write as 1-hour: Claude Code's cache.
  const split = m.usage.cache_creation as Record<string, unknown> | undefined;
  const cacheWrite1h = split && typeof split === 'object' ? Math.min(cacheWrite, n(split.ephemeral_1h_input_tokens)) : cacheWrite;
  return {
    messageId: m.id,
    model: m.model,
    at: Number.isFinite(at) ? at : Date.now(),
    input: n(m.usage.input_tokens),
    output: n(m.usage.output_tokens),
    cacheRead: n(m.usage.cache_read_input_tokens),
    cacheWrite,
    cacheWrite1h,
  };
}

/**
 * A line of the main transcript that tells where the first turn ended: a response that ended its
 * turn (`stop_reason: "end_turn"`) or an API error Claude Code wrote in the model's place
 * (`<synthetic>`), or something the user sent (a prompt, a command, "[Request interrupted by
 * user]"), which ends the turn before it once there's been a response. Meta lines (skill text,
 * caveats), compaction summaries and tool results don't count.
 */
export interface TurnMark {
  kind: 'end' | 'user';
  at: number;
}

export function parseTurnMark(line: string): TurnMark | null {
  if (!line.includes('"assistant"') && !line.includes('"user"')) return null;
  let d: {
    type?: string; timestamp?: string; isMeta?: unknown; isCompactSummary?: unknown;
    message?: { model?: unknown; stop_reason?: unknown; content?: unknown };
  };
  try {
    d = JSON.parse(line);
  } catch {
    return null;
  }
  const t = d.timestamp ? Date.parse(d.timestamp) : NaN;
  if (!Number.isFinite(t) || !d.message) return null;
  if (d.type === 'assistant') {
    return d.message.stop_reason === 'end_turn' || d.message.model === '<synthetic>' ? { kind: 'end', at: t } : null;
  }
  if (d.type !== 'user' || d.isMeta === true || d.isCompactSummary === true) return null;
  const c = d.message.content;
  const said = typeof c === 'string'
    ? !!c.trim()
    : Array.isArray(c) && c.length > 0 && !(c as { type?: unknown }[]).some((b) => b?.type === 'tool_result');
  return said ? { kind: 'user', at: t } : null;
}

/**
 * Sets when a session's first turn ended, once: the first mark that ends a turn, where a user mark
 * counts only after a response in the main transcript (the first prompt starts the turn).
 */
function setFirstTurnEnd(db: Db, sessionId: string, marks: TurnMark[]): void {
  const s = get<{ at: number | null }>(db, 'SELECT first_turn_end_at AS at FROM sessions WHERE id = ?', sessionId);
  if (!s || s.at !== null) return;
  for (const m of marks) {
    if (m.kind === 'user' && !get(db, 'SELECT 1 AS x FROM usage_messages WHERE session_id = ? AND agent IS NULL AND at < ? LIMIT 1', sessionId, m.at)) continue;
    run(db, 'UPDATE sessions SET first_turn_end_at = ? WHERE id = ? AND first_turn_end_at IS NULL', m.at, sessionId);
    return;
  }
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
 * seen) and records one row per model response, pushes to the base branch,
 * Meko calls with their results, and when the first turn ended (see
 * TurnMark; after a /clear, a transcript read again from the start is only
 * the current conversation's). A response spans several transcript
 * lines with the same message id, so rows are keyed by it.
 * Returns true if anything new was recorded.
 */
export function ingestSessionUsage(db: Db, sessionId: string, mainTranscript: string, baseBranch?: string): boolean {
  let changed = false;
  // Meko calls still waiting for their result, which may come in a later read.
  const pending = new Map<string, MekoKind>(
    all<{ tool_use_id: string; kind: MekoKind }>(db, 'SELECT tool_use_id, kind FROM meko_calls WHERE session_id = ? AND outcome IS NULL', sessionId)
      .map((r) => [r.tool_use_id, r.kind]));
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
      const calls: MekoCall[] = [];
      const results: MekoResult[] = [];
      const marks: TurnMark[] = [];
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
          if (agent === null) {
            const m = parseTurnMark(line);
            if (m) marks.push(m);
          }
          if (baseBranch) pushes.push(...parsePushLine(line, baseBranch));
          for (const c of parseMekoCalls(line)) {
            calls.push(c);
            pending.set(c.toolUseId, c.kind);
          }
          for (const res of parseMekoResults(line, pending)) {
            results.push(res);
            pending.delete(res.toolUseId);
          }
        }
        carry = data.subarray(lastNl + 1);
        consumed = pos - carry.length;
      }
      tx(db, () => {
        for (const p of pushes) {
          run(db, 'INSERT INTO pushes(session_id, range, ref, at) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING', sessionId, p.range, p.ref, p.at);
        }
        for (const r of rows) {
          run(db, `INSERT INTO usage_messages(session_id, message_id, agent, model, at, input, output, cache_read, cache_write, cache_write_1h)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                   ON CONFLICT(session_id, message_id) DO UPDATE SET
                     input = MAX(input, excluded.input), output = MAX(output, excluded.output),
                     cache_read = MAX(cache_read, excluded.cache_read), cache_write = MAX(cache_write, excluded.cache_write),
                     cache_write_1h = MAX(cache_write_1h, excluded.cache_write_1h)`,
            sessionId, r.messageId, agent, r.model, r.at, r.input, r.output, r.cacheRead, r.cacheWrite, r.cacheWrite1h);
        }
        for (const c of calls) {
          run(db, `INSERT INTO meko_calls(session_id, tool_use_id, agent, tool, kind, at, input_chars, model, context_tokens)
                   VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
            sessionId, c.toolUseId, agent, c.tool, c.kind, c.at, c.inputChars, c.model, c.contextTokens);
        }
        for (const r of results) {
          run(db, 'UPDATE meko_calls SET outcome = ?, results = ?, result_chars = ? WHERE session_id = ? AND tool_use_id = ?',
            r.outcome, r.results, r.resultChars, sessionId, r.toolUseId);
        }
        // After this read's responses are in: a user mark needs a response before it.
        if (marks.length) setFirstTurnEnd(db, sessionId, marks);
        run(db, `INSERT INTO usage_files(session_id, path, offset) VALUES (?, ?, ?)
                 ON CONFLICT(session_id, path) DO UPDATE SET offset = excluded.offset`, sessionId, path, consumed);
      });
      if (rows.length || pushes.length || calls.length || results.length) changed = true;
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

interface CostRow {
  model: string;
  input: number;
  output: number;
  cache_read: number;
  cache_write: number;
  cache_write_1h: number;
  messages: number;
}

const COST_SUMS = `u.model AS model, SUM(u.input) AS input, SUM(u.output) AS output, SUM(u.cache_read) AS cache_read,
  SUM(u.cache_write) AS cache_write, SUM(u.cache_write_1h) AS cache_write_1h, COUNT(*) AS messages`;

/** API-price cost of per-model sums. */
function toCost(rows: CostRow[]): UsageCost {
  const c: UsageCost = { usd: 0, unpricedMessages: 0 };
  for (const r of rows) {
    const p = priceFor(r.model);
    if (!p) c.unpricedMessages += r.messages;
    else c.usd += costOf({ input: r.input, output: r.output, cacheRead: r.cache_read, cacheWrite: r.cache_write, cacheWrite1h: r.cache_write_1h }, p);
  }
  return c;
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
  const calls = mekoCalls(db, projectId);
  const bySessionMeko = mekoBy(calls, (r) => r.session_id);
  const bySession: SessionUsage[] = all<SumRow & { id: string; name: string; stage: string; desired: string }>(db,
    `SELECT s.id, s.name, s.stage, s.desired, ${SUMS} FROM usage_messages u JOIN sessions s ON s.id = u.session_id
     WHERE s.project_id = ? GROUP BY s.id`, projectId)
    .map((r) => ({
      sessionId: r.id, name: r.name, stage: r.stage as SessionUsage['stage'], archived: r.desired === 'archived',
      usage: toUsage(r), meko: sessionMeko(bySessionMeko.get(r.id)),
    }))
    .sort((a, b) => totalTokens(b.usage) - totalTokens(a.usage));
  const cost = toCost(all<CostRow>(db,
    `SELECT ${COST_SUMS} FROM usage_messages u JOIN sessions s ON s.id = u.session_id WHERE s.project_id = ? GROUP BY u.model`, projectId));

  const daily = dailyUsage(db, days, now, projectId);
  return {
    total, cost, meko: summarizeMeko(calls), mekoFirstTurn: firstTurnMeko(db, projectId), mekoLearning: learningOf(learnRuns(db, projectId)),
    byModel, bySession, daily,
  };
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
/** `calls` is `mekoCalls(db)`, for a caller that has it already. */
export function usageOverview(db: Db, days = 14, now = Date.now(), calls = mekoCalls(db)): UsageOverview {
  const byUsage = usageByProject(db);
  const counts = <T extends { project_id: string }>(sql: string) => new Map(all<T>(db, sql).map((r) => [r.project_id, r]));
  const sessions = counts<{ project_id: string; n: number }>('SELECT project_id, COUNT(*) AS n FROM sessions GROUP BY project_id');
  // A restart mid-cleanup can log a merge twice; count sessions. Merge & Close ends in Done only
  // after a merge, so a Done session with a PR counts too (its event may predate a pruning release).
  // A Review a PR session ends in Done with someone else's PR, merged or not: it never counts.
  const merged = counts<{ project_id: string; n: number }>(
    `SELECT project_id, COUNT(DISTINCT session_id) AS n FROM (
       SELECT project_id, session_id FROM session_events WHERE kind = 'merged'
       UNION SELECT project_id, id FROM sessions WHERE stage = 'done' AND pr_number IS NOT NULL AND kind != 'review-pr'
     ) GROUP BY project_id`);
  // commits is -1 when git couldn't count a push's range: the push counts, its commits don't.
  const pushed = counts<{ project_id: string; pushes: number; commits: number }>(
    `SELECT s.project_id, COUNT(*) AS pushes, COALESCE(SUM(CASE WHEN p.commits > 0 THEN p.commits ELSE 0 END), 0) AS commits
     FROM pushes p JOIN sessions s ON s.id = p.session_id GROUP BY s.project_id`);
  const last = counts<{ project_id: string; at: number }>(
    'SELECT s.project_id, MAX(u.at) AS at FROM usage_messages u JOIN sessions s ON s.id = u.session_id GROUP BY s.project_id');
  const costRows = new Map<string, CostRow[]>();
  for (const r of all<CostRow & { project_id: string }>(db,
    `SELECT s.project_id, ${COST_SUMS} FROM usage_messages u JOIN sessions s ON s.id = u.session_id GROUP BY s.project_id, u.model`)) {
    costRows.set(r.project_id, [...(costRows.get(r.project_id) ?? []), r]);
  }
  const meko = mekoBy(calls, (r) => r.project_id);

  const byProject: ProjectUsageSummary[] = all<{ id: string; name: string; repo_path: string; deleted_at: number | null }>(db,
    'SELECT id, name, repo_path, deleted_at FROM projects')
    .map((p) => ({
      projectId: p.id,
      name: p.name,
      repoPath: p.repo_path,
      deleted: p.deleted_at != null,
      usage: byUsage.get(p.id) ?? { ...ZERO_USAGE },
      cost: toCost(costRows.get(p.id) ?? []),
      meko: meko.get(p.id) ?? summarizeMeko([]),
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
    cost: toCost(all<CostRow>(db, `SELECT ${COST_SUMS} FROM usage_messages u GROUP BY u.model`)),
    meko: summarizeMeko(calls),
    mekoFirstTurn: firstTurnMeko(db),
    mekoLearning: learningOf(learnRuns(db)),
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
