import { closeSync, existsSync, fstatSync, openSync, readSync, readdirSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { DEFAULT_HARNESS, HARNESS_IDS, harnessOf } from '../shared/harnesses.js';
import { addCounts, noCounts, totalCount, type SeverityCounts } from '../shared/codereview.js';
import { costOf, priceFor } from '../shared/pricing.js';
import type {
  CodeReviewCounts, CodeReviewUsage, DailyUsage, ModelUsage, ProjectUsage, ProjectUsageSummary, SessionUsage, TokenUsage, UsageCost, UsageOverview,
} from '../shared/types.js';
import { all, get, run, tx, type Db } from './db.js';
import {
  firstTurnMeko, learningOf, learnRuns, mekoBy, mekoCallOf, mekoCalls, mekoResultOf, parseMekoCalls, parseMekoResults, queryKey, sessionMeko, summarizeMeko,
  type MekoCall, type MekoResult, type PendingCall,
} from './mekousage.js';
import { openGapCounts } from './mekogaps.js';
import { readSavedReplySync } from './savedreply.js';
import { parseKiroMekoCalls, type KiroTurn } from './kiro.js';
import type { AgentMekoCall } from './mekolog.js';
import { parseOpenCodeMekoCalls, parseOpenCodeMessages } from './opencode.js';
import type { SessionRow } from './store.js';

export const ZERO_USAGE: TokenUsage = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, messages: 0 };

export function totalTokens(u: TokenUsage): number {
  return u.input + u.output + u.cacheRead + u.cacheWrite;
}

export function addUsage(a: TokenUsage, b: TokenUsage): TokenUsage {
  const credits = (a.credits ?? 0) + (b.credits ?? 0);
  return {
    input: a.input + b.input, output: a.output + b.output, cacheRead: a.cacheRead + b.cacheRead, cacheWrite: a.cacheWrite + b.cacheWrite,
    messages: a.messages + b.messages, ...(credits ? { credits } : {}),
  };
}

const CHUNK = 1024 * 1024;
/** The largest reply Claude Code saved to a file that's read for a Meko search's quality. */
export const SAVED_REPLY_MAX = 8 * 1024 * 1024;

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
  const pending = new Map<string, PendingCall>(
    all<{ tool_use_id: string } & PendingCall>(db, 'SELECT tool_use_id, kind, tool FROM meko_calls WHERE session_id = ? AND outcome IS NULL', sessionId)
      .map((r) => [r.tool_use_id, { kind: r.kind, tool: r.tool }]));
  // A search's reply too large for Claude is read from the file Claude Code saved, beside the main transcript.
  const readSaved = (notice: string) => readSavedReplySync(notice, mainTranscript, SAVED_REPLY_MAX);
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
            pending.set(c.toolUseId, { kind: c.kind, tool: c.tool });
          }
          for (const res of parseMekoResults(line, pending, readSaved)) {
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
        recordMekoCalls(db, sessionId, agent, calls, results);
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

/**
 * Writes Meko calls and their results into `meko_calls` (inside the caller's transaction): a call
 * already there is left as it is, and a result fills in its call's outcome. The same for every
 * agent's calls, so they're counted and judged alike. Returns the ids of the calls that were new.
 */
export function recordMekoCalls(db: Db, sessionId: string, agent: string | null, calls: MekoCall[], results: MekoResult[]): Set<string> {
  const added = new Set<string>();
  for (const c of calls) {
    const r = run(db, `INSERT INTO meko_calls(session_id, tool_use_id, agent, tool, kind, at, input_chars, model, context_tokens, query, query_key)
             VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
      sessionId, c.toolUseId, agent, c.tool, c.kind, c.at, c.inputChars, c.model, c.contextTokens, c.query, c.query === null ? null : queryKey(c.query));
    if (Number(r.changes)) added.add(c.toolUseId);
  }
  for (const r of results) {
    run(db, `UPDATE meko_calls SET outcome = ?, results = ?, result_chars = ?, top_score = ?, top_raw = ?, quality = ?
             WHERE session_id = ? AND tool_use_id = ?`,
      r.outcome, r.results, r.resultChars, r.topScore, r.topRaw, r.quality, sessionId, r.toolUseId);
  }
  return added;
}

interface SumRow {
  input: number | null;
  output: number | null;
  cache_read: number | null;
  cache_write: number | null;
  messages: number;
  credits: number | null;
}

const SUMS = `COALESCE(SUM(input), 0) AS input, COALESCE(SUM(output), 0) AS output,
  COALESCE(SUM(cache_read), 0) AS cache_read, COALESCE(SUM(cache_write), 0) AS cache_write, COUNT(*) AS messages,
  COALESCE(SUM(credits), 0) AS credits`;

function toUsage(r: SumRow | undefined): TokenUsage {
  if (!r) return { ...ZERO_USAGE };
  return {
    input: r.input ?? 0, output: r.output ?? 0, cacheRead: r.cache_read ?? 0, cacheWrite: r.cache_write ?? 0, messages: r.messages,
    // Only Kiro's turns bill credits: Claude Code's usage leaves the field out.
    ...(r.credits ? { credits: r.credits } : {}),
  };
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

/** Rows priced from their tokens: Claude Code's and OpenCode's responses. Kiro's turns carry the credits it billed instead. */
const PRICED = 'u.credits IS NULL';

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

/**
 * Totals per agent and model, most tokens first, for one project or all of them. The same model id
 * under two agents is two rows. The agent is read as `harnessOf` reads it before grouping: a row
 * whose session is gone, or whose agent this version doesn't know (a newer install's), counts as
 * the default's, in one row with the default's own.
 */
function usageByModel(db: Db, projectId?: string): ModelUsage[] {
  const known = HARNESS_IDS.map(() => '?').join(', ');
  return all<SumRow & { harness: string; model: string }>(db,
    `SELECT CASE WHEN s.harness IN (${known}) THEN s.harness ELSE ? END AS harness, u.model, ${SUMS}
     FROM usage_messages u LEFT JOIN sessions s ON s.id = u.session_id
     ${projectId ? 'WHERE s.project_id = ?' : ''} GROUP BY 1, u.model`, ...HARNESS_IDS, DEFAULT_HARNESS, ...(projectId ? [projectId] : []))
    .map((r) => ({ harness: harnessOf(r.harness), model: r.model, usage: toUsage(r) }))
    .sort((a, b) => totalTokens(b.usage) - totalTokens(a.usage));
}

/** A local review as the usage counts read it from `code_reviews`. */
export interface ReviewRun {
  /** When it started. */
  at: number;
  state: string;
  /** Rounds that reached their passes. */
  round: number;
  /** Its PR prompt went. */
  sent: boolean;
  counts: SeverityCounts;
  candidates: number;
  fixed: number;
  declined: number;
}

const parsed = <T>(s: string | null, fallback: T): T => {
  try {
    return s ? ((JSON.parse(s) as T | null) ?? fallback) : fallback;
  } catch {
    return fallback;
  }
};

/** Every local review, or one project's (its task's project), oldest first. A row that doesn't parse counts as finding nothing. */
export function reviewRuns(db: Db, projectId?: string): ReviewRun[] {
  // `reported`: a step of the review's current round (r<round>s…, r<round>c…) has reported.
  return all<{
    created_at: number; state: string; round: number; finished_at: number | null; counts_json: string; findings_json: string; candidates_json: string; reported: number;
  }>(db,
    `SELECT r.created_at, r.state, r.round, r.finished_at, r.counts_json, r.findings_json, r.candidates_json,
            EXISTS (SELECT 1 FROM code_review_steps s WHERE s.review_id = r.id
                    AND (s.step LIKE 'r' || r.round || 's%' OR s.step LIKE 'r' || r.round || 'c%')) AS reported
     FROM code_reviews r ${projectId ? 'JOIN sessions t ON t.id = r.task_id WHERE t.project_id = ?' : ''} ORDER BY r.created_at`, ...(projectId ? [projectId] : []))
    .map((r) => {
      const findings = parsed<{ round?: unknown; kept?: unknown; outcome?: { status?: unknown } | null }[]>(r.findings_json, []);
      const list = Array.isArray(findings) ? findings : [];
      const kept = list.filter((f) => f?.kept === true);
      // The current round reached its passes: it's sweeping, scoring or fixing, or a step of it
      // reported. One that stopped (or found nothing changed) while preparing never ran. nextRound
      // bumps `round` and leaves the last round's candidates_json until beginRound resets it, so
      // its candidates_json is the earlier round's, already in findings_json.
      const ran = !!r.reported || r.state === 'sweeping' || r.state === 'scoring' || r.state === 'fixing';
      // A round's candidates move into findings_json only when it finishes; until then (or for good,
      // once it failed or was cancelled) they're in candidates_json.
      const open = ran && !list.some((f) => f?.round === r.round) ? parsed<unknown[]>(r.candidates_json, []) : [];
      return {
        at: r.created_at, state: r.state, round: Math.max(0, r.round - (ran ? 0 : 1)), sent: r.finished_at != null,
        counts: { ...noCounts(), ...parsed<Partial<SeverityCounts>>(r.counts_json, {}) },
        candidates: list.length + (Array.isArray(open) ? open.length : 0),
        fixed: kept.filter((f) => f.outcome?.status === 'fixed').length,
        declined: kept.filter((f) => f.outcome?.status === 'declined').length,
      };
    });
}

/** Reviews added up: each by its state now, and what their rounds found and the task did with them. */
export function countReviews(runs: ReviewRun[]): CodeReviewCounts {
  const c: CodeReviewCounts = {
    runs: runs.length, byOutcome: { done: 0, failed: 0, cancelled: 0, running: 0 }, lgtm: 0, prs: 0, rounds: 0, candidates: 0,
    findings: noCounts(), fixed: 0, declined: 0,
  };
  for (const r of runs) {
    c.byOutcome[r.state === 'done' || r.state === 'failed' || r.state === 'cancelled' ? r.state : 'running']++;
    if (r.state === 'done' && !totalCount(r.counts)) c.lgtm++;
    if (r.sent) c.prs++;
    c.rounds += Math.max(0, r.round);
    c.candidates += r.candidates;
    c.findings = addCounts(c.findings, r.counts);
    c.fixed += r.fixed;
    c.declined += r.declined;
  }
  return c;
}

/** Local reviews' counts, and their sessions' tokens, for every project or one. */
export function reviewUsage(db: Db, projectId?: string): CodeReviewUsage {
  const where = `s.kind = 'code-review'${projectId ? ' AND s.project_id = ?' : ''}`;
  const args = projectId ? [projectId] : [];
  return {
    ...countReviews(reviewRuns(db, projectId)),
    usage: toUsage(get<SumRow>(db, `SELECT ${SUMS} FROM usage_messages u JOIN sessions s ON s.id = u.session_id WHERE ${where}`, ...args)),
  };
}

/** Local reviews' sessions' tokens and cost per UTC day from `since`, for the usage report. Days with none are left out. */
export function reviewSpendByDay(db: Db, since: number): Map<string, { usage: TokenUsage; cost: UsageCost }> {
  const day = "strftime('%Y-%m-%d', u.at / 1000, 'unixepoch')";
  const from = `FROM usage_messages u JOIN sessions s ON s.id = u.session_id WHERE s.kind = 'code-review' AND u.at >= ?`;
  const out = new Map<string, { usage: TokenUsage; cost: UsageCost }>();
  for (const r of all<SumRow & { day: string }>(db, `SELECT ${day} AS day, ${SUMS} ${from} GROUP BY day`, since)) {
    out.set(r.day, { usage: toUsage(r), cost: { usd: 0, unpricedMessages: 0 } });
  }
  const costs = new Map<string, CostRow[]>();
  for (const r of all<CostRow & { day: string }>(db, `SELECT ${day} AS day, ${COST_SUMS} ${from} AND ${PRICED} GROUP BY day, u.model`, since)) {
    costs.set(r.day, [...(costs.get(r.day) ?? []), r]);
  }
  for (const [d, rows] of costs) {
    const o = out.get(d);
    if (o) o.cost = toCost(rows);
  }
  return out;
}

/** Usage breakdown for the project home: totals, by model, by session, and the last `days` days. */
export function projectUsage(db: Db, projectId: string, days = 14, now = Date.now()): ProjectUsage {
  const total = toUsage(get<SumRow>(db,
    `SELECT ${SUMS} FROM usage_messages u JOIN sessions s ON s.id = u.session_id WHERE s.project_id = ?`, projectId));
  const byModel = usageByModel(db, projectId);
  const calls = mekoCalls(db, projectId);
  const bySessionMeko = mekoBy(calls, (r) => r.session_id);
  // The most recently used first, the largest first among ties.
  const bySession: SessionUsage[] = all<SumRow & { id: string; name: string; stage: string; desired: string; last_at: number }>(db,
    `SELECT s.id, s.name, s.stage, s.desired, MAX(u.at) AS last_at, ${SUMS} FROM usage_messages u JOIN sessions s ON s.id = u.session_id
     WHERE s.project_id = ? GROUP BY s.id`, projectId)
    .map((r) => ({
      lastAt: r.last_at,
      row: {
        sessionId: r.id, name: r.name, stage: r.stage as SessionUsage['stage'], archived: r.desired === 'archived',
        usage: toUsage(r), meko: sessionMeko(bySessionMeko.get(r.id)),
      },
    }))
    .sort((a, b) => b.lastAt - a.lastAt || totalTokens(b.row.usage) - totalTokens(a.row.usage))
    .map((x) => x.row);
  const cost = toCost(all<CostRow>(db,
    `SELECT ${COST_SUMS} FROM usage_messages u JOIN sessions s ON s.id = u.session_id WHERE s.project_id = ? AND ${PRICED} GROUP BY u.model`, projectId));

  const daily = dailyUsage(db, days, now, projectId);
  return {
    total, cost, meko: summarizeMeko(calls), mekoGaps: openGapCounts(db, projectId), mekoFirstTurn: firstTurnMeko(db, projectId),
    mekoLearning: learningOf(learnRuns(db, projectId)), codeReviews: reviewUsage(db, projectId), byModel, bySession, daily,
  };
}

/** Daily totals for the last `days` days (in the server's time zone), for one project or all of them. */
function dailyUsage(db: Db, days: number, now: number, projectId?: string): DailyUsage[] {
  const start = new Date(now);
  start.setHours(0, 0, 0, 0);
  start.setDate(start.getDate() - (days - 1));
  const buckets = new Map<string, DailyUsage>();
  for (let i = 0; i < days; i++) {
    const d = new Date(start);
    d.setDate(start.getDate() + i);
    const day = localDay(d.getTime());
    buckets.set(day, { day, usage: { ...ZERO_USAGE }, reviewTokens: 0 });
  }
  const rows = all<{ at: number; kind: string; input: number; output: number; cache_read: number; cache_write: number; credits: number | null }>(db,
    `SELECT u.at, s.kind, u.input, u.output, u.cache_read, u.cache_write, u.credits FROM usage_messages u JOIN sessions s ON s.id = u.session_id
     WHERE u.at >= ?${projectId ? ' AND s.project_id = ?' : ''}`, ...(projectId ? [start.getTime(), projectId] : [start.getTime()]));
  for (const r of rows) {
    const d = buckets.get(localDay(r.at));
    if (!d) continue;
    const b = d.usage;
    b.input += r.input;
    b.output += r.output;
    b.cacheRead += r.cache_read;
    b.cacheWrite += r.cache_write;
    b.messages += 1;
    if (r.credits) b.credits = (b.credits ?? 0) + r.credits;
    if (r.kind === 'code-review') d.reviewTokens += r.input + r.output + r.cache_read + r.cache_write;
  }
  return [...buckets.values()];
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
    `SELECT s.project_id, ${COST_SUMS} FROM usage_messages u JOIN sessions s ON s.id = u.session_id WHERE ${PRICED} GROUP BY s.project_id, u.model`)) {
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
    // Current projects first, then deleted ones; in each, the most recently used first (none yet last).
    .sort((a, b) =>
      Number(a.deleted) - Number(b.deleted) || (b.lastActiveAt ?? -1) - (a.lastActiveAt ?? -1) || totalTokens(b.usage) - totalTokens(a.usage));

  const byModel = usageByModel(db);
  const sum = (f: (p: ProjectUsageSummary) => number) => byProject.reduce((n, p) => n + f(p), 0);
  return {
    total: toUsage(get<SumRow>(db, `SELECT ${SUMS} FROM usage_messages`)),
    cost: toCost(all<CostRow>(db, `SELECT ${COST_SUMS} FROM usage_messages u WHERE ${PRICED} GROUP BY u.model`)),
    meko: summarizeMeko(calls),
    mekoFirstTurn: firstTurnMeko(db),
    mekoLearning: learningOf(learnRuns(db)),
    codeReviews: reviewUsage(db),
    mergedPrs: sum((p) => p.mergedPrs),
    directPushes: sum((p) => p.directPushes),
    directCommits: sum((p) => p.directCommits),
    byProject,
    byModel,
    daily: dailyUsage(db, days, now),
  };
}

/**
 * Records a Kiro session's finished turns as usage rows (one per turn, keyed by the conversation and
 * when the turn ended, so reading the file again changes nothing), with the credits Kiro billed.
 * True when a turn was new.
 */
export function recordKiroTurns(db: Db, sessionId: string, conversation: string, turns: KiroTurn[]): boolean {
  let added = 0;
  tx(db, () => {
    for (const t of turns) {
      const r = run(db, `INSERT INTO usage_messages(session_id, message_id, agent, model, at, input, output, cache_read, cache_write, credits)
        VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(session_id, message_id) DO NOTHING`,
      sessionId, `kiro:${conversation}:${t.key}`, t.model, t.at, t.input, t.output, t.cacheRead, t.cacheWrite, t.credits);
      added += Number(r.changes);
    }
  });
  return added > 0;
}

/**
 * The whole lines a file gained since the last read (by the offset kept in `usage_files`), and the
 * offset to keep once they're in; null when there's nothing new. A line still being written is read
 * next time, and a file that shrank is read again from the start (rows already in are skipped by id).
 */
function newLines(db: Db, sessionId: string, file: string): { text: string; offset: number } | null {
  const prev = get<{ offset: number }>(db, 'SELECT offset FROM usage_files WHERE session_id = ? AND path = ?', sessionId, file);
  let fd: number;
  try {
    fd = openSync(file, 'r');
  } catch {
    return null;
  }
  try {
    let offset = prev?.offset ?? 0;
    const size = fstatSync(fd).size;
    if (size < offset) offset = 0;
    if (size === offset) return null;
    const buf = Buffer.alloc(size - offset);
    readSync(fd, buf, 0, buf.length, offset);
    const end = buf.lastIndexOf(0x0a) + 1;
    if (!end) return null;
    return { text: buf.subarray(0, end).toString('utf8'), offset: offset + end };
  } finally {
    closeSync(fd);
  }
}

function keepOffset(db: Db, sessionId: string, file: string, offset: number): void {
  run(db, `INSERT INTO usage_files(session_id, path, offset) VALUES (?, ?, ?)
           ON CONFLICT(session_id, path) DO UPDATE SET offset = excluded.offset`, sessionId, file, offset);
}

/** What a read of an agent's own files added: whether anything did, and the Meko calls new to `meko_calls`, for the call log. */
export interface AgentUsageRead {
  changed: boolean;
  mekoCalls: AgentMekoCall[];
}

/**
 * Reads an OpenCode session's `messages.jsonl` (each finished response's tokens, and each Meko call
 * with its reply, written by AgentK's plugin) from where the last read stopped: responses into
 * `usage_messages`, priced like Claude Code's, and Meko calls into `meko_calls`. Its subagents'
 * count too, under their own OpenCode session as the agent (`main`'s have none).
 */
export function ingestOpenCodeUsage(db: Db, sessionId: string, file: string, main: string | null): AgentUsageRead {
  const read = newLines(db, sessionId, file);
  if (!read) return { changed: false, mekoCalls: [] };
  const agentOf = (session: string) => (session && session !== main ? session : null);
  let added = 0;
  const logged: AgentMekoCall[] = [];
  tx(db, () => {
    for (const m of parseOpenCodeMessages(read.text)) {
      const r = run(db, `INSERT INTO usage_messages(session_id, message_id, agent, model, at, input, output, cache_read, cache_write, cache_write_1h)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0) ON CONFLICT(session_id, message_id) DO NOTHING`,
      sessionId, `opencode:${m.id}`, agentOf(m.session), m.model, m.at, m.input, m.output, m.cacheRead, m.cacheWrite);
      added += Number(r.changes);
    }
    for (const c of parseOpenCodeMekoCalls(read.text)) {
      const id = `opencode:${c.id}`;
      const call = mekoCallOf({ toolUseId: id, tool: c.tool, at: c.at, input: c.input, model: c.model, contextTokens: null });
      // A reply too long to log counts as found, as one too large for Claude does.
      const res = mekoResultOf(id, call, c.big ? null : c.text ?? '', !c.ok);
      if (!recordMekoCalls(db, sessionId, agentOf(c.session), [call], [res]).size) continue;
      added++;
      logged.push({ tool: c.tool, at: c.at, ms: c.ms, ok: c.ok, error: c.error, args: c.input, text: c.text, notice: null, subagent: !!agentOf(c.session) });
    }
    keepOffset(db, sessionId, file, read.offset);
  });
  return { changed: added > 0, mekoCalls: logged };
}

/**
 * Reads a Kiro session's log (`<conversation>.jsonl`) from where the last read stopped, into
 * `meko_calls`: the Meko calls whose results are in (`parseKiroMekoCalls`). Kiro says neither the
 * model nor the context of a call, nor how long it took.
 */
export function ingestKiroMekoCalls(db: Db, sessionId: string, file: string, now = Date.now()): AgentUsageRead {
  const read = newLines(db, sessionId, file);
  if (!read) return { changed: false, mekoCalls: [] };
  const logged: AgentMekoCall[] = [];
  tx(db, () => {
    for (const c of parseKiroMekoCalls(read.text, now)) {
      const call = mekoCallOf({ toolUseId: c.id, tool: c.tool, at: c.at, input: c.input, model: null, contextTokens: null });
      if (!recordMekoCalls(db, sessionId, null, [call], [mekoResultOf(c.id, call, c.text, c.isError)]).size) continue;
      logged.push({ tool: c.tool, at: c.at, ms: null, ok: !c.isError, error: c.isError ? c.text.slice(0, 300) || 'failed' : null, args: c.input, text: c.text, notice: null, subagent: false });
    }
    keepOffset(db, sessionId, file, read.offset);
  });
  return { changed: logged.length > 0, mekoCalls: logged };
}

/** Where a session's main transcript is, preferring the path Claude reported. */
export function sessionTranscript(row: SessionRow, fallback: (cwd: string, id: string) => string): string {
  return row.transcript_path && existsSync(row.transcript_path) ? row.transcript_path : fallback(row.worktree_path, row.claude_session_id);
}
