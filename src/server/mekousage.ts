import { costOf, priceFor } from '../shared/pricing.js';
import type { CallHomeAgentkMeko, CallHomeLearn, CallHomeMekoTool, CallHomeTokens, MekoFirstTurn, MekoLearning, MekoUsage, SessionUsage } from '../shared/types.js';
import { all, get, type Db } from './db.js';
import { CARRY_TURNS, SEARCH_TOOL_NAMES, SEARCH_TOOLS, searchSaving } from './mekosavings.js';

export type MekoKind = 'lookup' | 'save' | 'other';
export type MekoOutcome = 'found' | 'empty' | 'error' | 'ok';

/** The Meko MCP server's tools, under its own name or a plugin's (`mcp__plugin_x_meko__…`). */
const MEKO_TOOL = /^mcp__(?:[\w-]+_)?meko__([a-z_]+)$/;
// Each one is a `CallHomeMekoTool`, so a tool added here must be added to the report's type too.
const LOOKUP_TOOLS = [
  ...SEARCH_TOOL_NAMES, 'memory_get_all', 'memory_get_by_id', 'conversation_get', 'artifact_get',
] as const satisfies readonly CallHomeMekoTool[];
const SAVE_TOOLS = [
  'memory_add', 'memory_update', 'memory_promote', 'artifact_put', 'conversation_add_message',
] as const satisfies readonly CallHomeMekoTool[];
const LOOKUPS: ReadonlySet<string> = new Set(LOOKUP_TOOLS);
const SAVES: ReadonlySet<string> = new Set(SAVE_TOOLS);
/** The tools the usage stats report names (`CallHomeMekoTool`): the lookups and saves. */
export const REPORTED_TOOLS: readonly CallHomeMekoTool[] = [...LOOKUP_TOOLS, ...SAVE_TOOLS];

/** The Meko tool a tool name calls, or null for any other tool. */
export function mekoTool(name: string): string | null {
  return MEKO_TOOL.exec(name)?.[1] ?? null;
}

export function mekoKind(tool: string): MekoKind {
  return LOOKUPS.has(tool) ? 'lookup' : SAVES.has(tool) ? 'save' : 'other';
}

export interface MekoCall {
  toolUseId: string;
  tool: string;
  kind: MekoKind;
  at: number;
  inputChars: number;
  /** Model of the response that made the call, and its context (input, cache read and write). */
  model: string | null;
  contextTokens: number | null;
}

export interface MekoResult {
  toolUseId: string;
  outcome: MekoOutcome;
  /** Items a lookup returned, when the result could be read. */
  results: number | null;
  resultChars: number;
}

type Json = Record<string, unknown>;
const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);

/** Meko calls in one transcript line (a model response). */
export function parseMekoCalls(line: string): MekoCall[] {
  if (!line.includes('meko__') || !line.includes('"tool_use"')) return [];
  let d: { type?: string; timestamp?: string; message?: { model?: string; usage?: Json; content?: unknown } };
  try {
    d = JSON.parse(line);
  } catch {
    return [];
  }
  const m = d.message;
  if (d.type !== 'assistant' || !Array.isArray(m?.content)) return [];
  const t = d.timestamp ? Date.parse(d.timestamp) : NaN;
  const at = Number.isFinite(t) ? t : Date.now();
  const u = m.usage;
  const contextTokens = u ? num(u.input_tokens) + num(u.cache_read_input_tokens) + num(u.cache_creation_input_tokens) : null;
  const out: MekoCall[] = [];
  for (const b of m.content as Json[]) {
    if (b?.type !== 'tool_use' || typeof b.id !== 'string' || typeof b.name !== 'string') continue;
    const tool = mekoTool(b.name);
    if (!tool) continue;
    out.push({
      toolUseId: b.id,
      tool,
      kind: mekoKind(tool),
      at,
      inputChars: JSON.stringify(b.input ?? {}).length,
      model: typeof m.model === 'string' && m.model !== '<synthetic>' ? m.model : null,
      contextTokens,
    });
  }
  return out;
}

/** Results, in one transcript line, of the calls still waiting for one (`pending`: tool_use_id → kind). */
export function parseMekoResults(line: string, pending: ReadonlyMap<string, MekoKind>): MekoResult[] {
  if (pending.size === 0 || !line.includes('"tool_result"')) return [];
  let mentioned = false;
  for (const id of pending.keys()) {
    if (line.includes(id)) {
      mentioned = true;
      break;
    }
  }
  if (!mentioned) return [];
  let d: { type?: string; message?: { content?: unknown } };
  try {
    d = JSON.parse(line);
  } catch {
    return [];
  }
  if (d.type !== 'user' || !Array.isArray(d.message?.content)) return [];
  const out: MekoResult[] = [];
  for (const b of d.message.content as Json[]) {
    if (b?.type !== 'tool_result' || typeof b.tool_use_id !== 'string') continue;
    const kind = pending.get(b.tool_use_id);
    if (!kind) continue;
    out.push({ toolUseId: b.tool_use_id, ...classifyResult(kind, resultText(b.content), b.is_error === true) });
  }
  return out;
}

function resultText(content: unknown): string {
  if (typeof content === 'string') return content;
  if (!Array.isArray(content)) return '';
  return (content as Json[]).map((c) => (c?.type === 'text' && typeof c.text === 'string' ? c.text : '')).join('');
}

/**
 * Claude Code's notice for a result too large to show, which it saved to a file instead. Only the
 * notice enters the context; reading the file is a tool call of Claude's own, counted as usual.
 */
const TOO_LARGE = /^Error: result \(([\d,]+) characters\) exceeds maximum allowed tokens/;
/** Claude Code's notice for a call it moved to the background: no result yet. */
const BACKGROUND = /^MCP tool "[^"]*" is still running after/;
/**
 * Where a lookup's items are: memory and KB `results`, `memories`, and context_search's buckets
 * (`memory`, `knowledge_base`, `conversation`), wrapped in `results` or not.
 */
const ITEM_KEYS = ['results', 'memories', 'hits', 'chunks', 'documents', 'items', 'messages', 'memory', 'knowledge_base', 'conversation'];
/** A plain-text reply that says nothing was found. */
const NOTHING = /^(?:no|nothing|0)\b|\bnot found\b|\bno (?:results|memories|matches|documents)\b/i;

/** Found, empty or failed, from a result's text. Meko wraps its JSON in `{"result": "<json>"}`. */
export function classifyResult(kind: MekoKind, text: string, isError: boolean): Omit<MekoResult, 'toolUseId'> {
  const chars = text.length;
  if (isError) return { outcome: 'error', results: null, resultChars: chars };
  const big = TOO_LARGE.exec(text);
  if (big) return { outcome: kind === 'lookup' ? 'found' : 'ok', results: null, resultChars: chars };
  if (BACKGROUND.test(text)) return { outcome: kind === 'lookup' ? 'empty' : 'ok', results: null, resultChars: chars };
  if (/^Error( calling tool|:)/.test(text)) return { outcome: 'error', results: null, resultChars: chars };
  const v = unwrap(text);
  if (v && typeof v === 'object' && !Array.isArray(v) && hasError((v as Json).error)) return { outcome: 'error', results: null, resultChars: chars };
  if (kind !== 'lookup') return { outcome: 'ok', results: null, resultChars: chars };
  const n = countItems(v);
  return { outcome: n > 0 ? 'found' : 'empty', results: n, resultChars: chars };
}

function unwrap(text: string): unknown {
  let v: unknown;
  try {
    v = JSON.parse(text);
  } catch {
    return text.trim();
  }
  if (v && typeof v === 'object' && !Array.isArray(v) && typeof (v as Json).result === 'string' && Object.keys(v).length === 1) {
    try {
      return JSON.parse((v as Json).result as string);
    } catch {
      return ((v as Json).result as string).trim();
    }
  }
  return v;
}

/** An `error` field that says something (`"error": null` or `""` in a good reply doesn't). */
function hasError(e: unknown): boolean {
  if (e == null || e === false || e === '') return false;
  if (typeof e === 'object') return Object.keys(e).length > 0;
  return true;
}

function countItems(v: unknown): number {
  if (Array.isArray(v)) return v.length;
  if (typeof v === 'string') return v && v !== '[]' && v !== '{}' && !NOTHING.test(v) ? 1 : 0;
  if (!v || typeof v !== 'object') return 0;
  const keys = ITEM_KEYS.filter((k) => k in v);
  // A single record (a memory by id, an artifact, a conversation) counts as one.
  if (keys.length === 0) return Object.keys(v).length > 0 ? 1 : 0;
  return keys.reduce((n, k) => n + itemCount((v as Json)[k]), 0);
}

/** Items under an item key: a list, the lists inside an object (context_search's buckets), or one text. */
function itemCount(v: unknown): number {
  if (typeof v === 'string') return v.trim() ? 1 : 0;
  return deepCount(v);
}

function deepCount(v: unknown): number {
  if (Array.isArray(v)) return v.length;
  if (v && typeof v === 'object') return Object.values(v).reduce<number>((n, x) => n + deepCount(x), 0);
  return 0;
}

interface CallRow {
  project_id: string;
  session_id: string;
  tool: string;
  /** `seed`: what AgentK's search at session start passed to Claude, which counts only toward the savings. */
  kind: MekoKind | 'seed';
  outcome: MekoOutcome | null;
  result_chars: number | null;
  /** What the call sent (its input JSON); 0 for a seed. */
  input_chars: number;
  model: string | null;
  context_tokens: number | null;
  /** For searches that found something, and seeds: responses after the call (or the first response), up to CARRY_TURNS. */
  later: number | null;
  /** When the call was made, or the search at start ran. */
  at: number;
}

const SEARCH_LIST = [...SEARCH_TOOLS].map((t) => `'${t}'`).join(', ');

/**
 * Every Meko call, for one project or all of them, plus a `seed` row for each session that AgentK's
 * search at start passed something to. A seed is credited like a search made by the session's first
 * response: its context (less the seed itself, which a search's context doesn't hold yet), model, and
 * the responses after it. A seed waits for that first response.
 */
export function mekoCalls(db: Db, projectId?: string): CallRow[] {
  const where = projectId ? ' WHERE s.project_id = ?' : '';
  return all<CallRow>(db,
    `SELECT s.project_id, c.session_id, c.tool, c.kind, c.outcome, c.result_chars, c.input_chars, c.model, c.context_tokens, c.at,
       CASE WHEN c.outcome = 'found' AND c.tool IN (${SEARCH_LIST}) THEN
         (SELECT COUNT(*) FROM (SELECT 1 FROM usage_messages u
          WHERE u.session_id = c.session_id AND u.agent IS c.agent AND u.at > c.at LIMIT ?))
       END AS later
     FROM meko_calls c JOIN sessions s ON s.id = c.session_id${where}
     UNION ALL
     SELECT s.project_id, p.session_id, 'seed', 'seed', 'found', p.chars, 0, f.model,
       MAX(0, f.input + f.cache_read + f.cache_write - ROUND(p.chars / 4.0)), p.at,
       (SELECT COUNT(*) FROM (SELECT 1 FROM usage_messages u
        WHERE u.session_id = p.session_id AND u.agent IS NULL AND u.at > f.at LIMIT ?))
     FROM meko_preloads p JOIN sessions s ON s.id = p.session_id
       JOIN usage_messages f ON f.session_id = p.session_id AND f.agent IS NULL
         AND f.message_id = (SELECT m.message_id FROM usage_messages m WHERE m.session_id = p.session_id AND m.agent IS NULL
                             ORDER BY m.at, m.message_id LIMIT 1)
     ${where ? `${where} AND` : 'WHERE'} p.outcome = 'found'`,
    ...(projectId ? [CARRY_TURNS, projectId, CARRY_TURNS, projectId] : [CARRY_TURNS, CARRY_TURNS]));
}

const tokensOf = (chars: number | null) => Math.round((chars ?? 0) / 4);

/** Counts and the savings estimate for a set of calls (seeds count only toward the savings). */
export function summarizeMeko(rows: CallRow[]): MekoUsage {
  const u: MekoUsage = {
    calls: 0, lookups: 0, found: 0, empty: 0, errors: 0, saves: 0, foundTokens: 0, sentTokens: 0,
    savings: { searches: 0, seeds: 0, tokens: 0, usd: null, unpricedSearches: 0, unpricedSeeds: 0 },
  };
  let usd = 0;
  let priced = 0;
  const credit = (r: CallRow) => {
    const s = searchSaving({ contextTokens: r.context_tokens ?? 0, resultTokens: tokensOf(r.result_chars), laterResponses: r.later ?? 0, model: r.model });
    u.savings.tokens += s.tokens;
    if (s.usd !== null) {
      usd += s.usd;
      priced++;
    } else if (r.kind === 'seed') {
      u.savings.unpricedSeeds++;
    } else {
      u.savings.unpricedSearches++;
    }
  };
  for (const r of rows) {
    if (r.kind === 'seed') {
      u.savings.seeds++;
      credit(r);
      continue;
    }
    u.calls++;
    u.sentTokens += tokensOf(r.input_chars);
    if (r.outcome === 'error') u.errors++;
    if (r.kind === 'save') u.saves++;
    if (r.kind !== 'lookup') continue;
    u.lookups++;
    if (r.outcome === 'empty') u.empty++;
    if (r.outcome !== 'found') continue;
    u.found++;
    u.foundTokens += tokensOf(r.result_chars);
    if (r.later === null) continue; // not a search
    u.savings.searches++;
    credit(r);
  }
  u.savings.usd = priced > 0 || u.savings.searches + u.savings.seeds === 0 ? usd : null;
  return u;
}

/**
 * Sessions whose first turn has ended, for one project or all of them, and about how many tokens
 * Meko brought into it: Claude's own lookups (from any agent) that found something up to the
 * turn's end, and what AgentK's search at start passed over. Also how many had a search at start at
 * all, since sessions from before AgentK searched at start had none. Only sessions in projects with
 * datapacks count, since the others had no Meko to search.
 */
export function firstTurnMeko(db: Db, projectId?: string): MekoFirstTurn {
  const r = get<{ [K in keyof MekoFirstTurn]: number | null }>(db,
    `SELECT COUNT(*) AS sessions, SUM(started) AS startSearches, SUM(searched) AS searchedTokens, SUM(startup) AS seedTokens
     FROM (SELECT
             (SELECT SUM(ROUND(COALESCE(c.result_chars, 0) / 4.0)) FROM meko_calls c
              WHERE c.session_id = s.id AND c.kind = 'lookup' AND c.outcome = 'found' AND c.at <= s.first_turn_end_at) AS searched,
             EXISTS (SELECT 1 FROM meko_preloads p WHERE p.session_id = s.id) AS started,
             (SELECT ROUND(p.chars / 4.0) FROM meko_preloads p WHERE p.session_id = s.id AND p.outcome = 'found') AS startup
           FROM sessions s
           WHERE s.first_turn_end_at IS NOT NULL
             AND EXISTS (SELECT 1 FROM project_datapacks d WHERE d.project_id = s.project_id)${projectId ? ' AND s.project_id = ?' : ''})`,
    ...(projectId ? [projectId] : []));
  return {
    sessions: r?.sessions ?? 0, startSearches: r?.startSearches ?? 0, searchedTokens: r?.searchedTokens ?? 0, seedTokens: r?.seedTokens ?? 0,
  };
}

/**
 * Calls by tool, as the usage stats report names them; tools with no calls are left out, and seeds
 * aren't calls. A call counts by the kind stored with it, as in summarizeMeko, so the lookups and
 * saves here add up to its counts even for a call stored before its tool was a lookup or a save.
 */
export function toolCounts(rows: CallRow[]): Partial<Record<CallHomeMekoTool, number>> {
  const out: Partial<Record<CallHomeMekoTool, number>> = {};
  for (const r of rows) {
    if (r.kind === 'seed') continue;
    const t = r.kind !== 'other' && REPORTED_TOOLS.includes(r.tool as CallHomeMekoTool) ? (r.tool as CallHomeMekoTool) : 'other';
    out[t] = (out[t] ?? 0) + 1;
  }
  return out;
}

/** About how many tokens the calls sent to Meko, by tool as toolCounts names them; seeds sent nothing. */
export function toolSent(rows: CallRow[]): Partial<Record<CallHomeMekoTool, number>> {
  const out: Partial<Record<CallHomeMekoTool, number>> = {};
  for (const r of rows) {
    if (r.kind === 'seed') continue;
    const t = r.kind !== 'other' && REPORTED_TOOLS.includes(r.tool as CallHomeMekoTool) ? (r.tool as CallHomeMekoTool) : 'other';
    out[t] = (out[t] ?? 0) + tokensOf(r.input_chars);
  }
  return out;
}

/** `usage_messages.agent` of a learn step's own model calls (the fork and the duplicate check, learn.ts). */
export const LEARN_AGENT = 'agentk:learn';

/** One model call's tokens that a learn step spent, and which part of the step made it. */
interface LearnTokens {
  phase: 'extract' | 'dedup' | 'turn';
  model: string;
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  cacheWrite1h: number;
}

/** A learn step (meko_learns) as the usage report counts it, with what its model calls cost. */
export interface LearnRun {
  at: number;
  trigger: 'merge' | 'manual';
  path: 'mod' | 'turn' | 'none';
  outcome: 'done' | 'failed' | 'interrupted' | 'skipped' | null;
  candidates: number;
  saves: number;
  updates: number;
  skips: number;
  lookups: number;
  errors: number;
  sentChars: number;
  tokens: LearnTokens[];
}

/**
 * Every learn step, for one project or all of them, with its model calls: through the mod, the
 * rows learn.ts recorded for its fork and duplicate check; as a typed turn, the session's
 * responses while it ran.
 */
export function learnRuns(db: Db, projectId?: string): LearnRun[] {
  const runs = all<{ id: string; at: number; trigger: LearnRun['trigger']; path: LearnRun['path']; outcome: LearnRun['outcome']; candidates: number; saves: number; updates: number; skips: number; lookups: number; errors: number; sent_chars: number }>(db,
    `SELECT l.id, l.started_at AS at, l.trigger, l.path, l.outcome, l.candidates, l.saves, l.updates, l.skips, l.lookups, l.errors, l.sent_chars
     FROM meko_learns l JOIN sessions s ON s.id = l.session_id${projectId ? ' WHERE s.project_id = ?' : ''}`, ...(projectId ? [projectId] : []));
  if (!runs.length) return [];
  const tokens = new Map<string, LearnTokens[]>();
  for (const u of all<{ learn_id: string; phase: string; model: string; input: number; output: number; cache_read: number; cache_write: number; cache_write_1h: number }>(db,
    `SELECT l.id AS learn_id, CASE WHEN u.agent = ? THEN substr(u.message_id, length(l.id) + 8) ELSE 'turn' END AS phase,
       u.model, u.input, u.output, u.cache_read, u.cache_write, u.cache_write_1h
     FROM meko_learns l JOIN usage_messages u ON u.session_id = l.session_id
     WHERE (l.path = 'mod' AND u.agent = ? AND u.message_id LIKE 'learn:' || l.id || ':%')
        OR (l.path = 'turn' AND u.agent IS NOT ? AND u.at >= l.started_at AND u.at <= COALESCE(l.ended_at, u.at))`,
    LEARN_AGENT, LEARN_AGENT, LEARN_AGENT)) {
    const phase = u.phase === 'extract' || u.phase === 'dedup' ? u.phase : 'turn';
    const list = tokens.get(u.learn_id) ?? [];
    list.push({ phase, model: u.model, input: u.input, output: u.output, cacheRead: u.cache_read, cacheWrite: u.cache_write, cacheWrite1h: u.cache_write_1h });
    tokens.set(u.learn_id, list);
  }
  return runs.map((r) => ({
    at: r.at, trigger: r.trigger, path: r.path, outcome: r.outcome, candidates: r.candidates, saves: r.saves, updates: r.updates,
    skips: r.skips, lookups: r.lookups, errors: r.errors, sentChars: r.sent_chars, tokens: tokens.get(r.id) ?? [],
  }));
}

const noTokens = (): CallHomeTokens => ({ input: 0, output: 0, cacheRead: 0, cacheWrite: 0, messages: 0 });

/** Learn steps as the usage report has them: counts, tokens by part, and their price in whole cents. */
export function summarizeLearn(runs: LearnRun[]): CallHomeLearn {
  const l: CallHomeLearn = {
    runs: 0, byTrigger: { merge: 0, manual: 0 }, byPath: { mod: 0, turn: 0, none: 0 },
    byOutcome: { done: 0, failed: 0, interrupted: 0, skipped: 0 },
    candidates: 0, saves: 0, updates: 0, skips: 0, empty: 0, lookups: 0, errors: 0, sentTokens: 0,
    tokens: { extract: noTokens(), dedup: noTokens(), turn: noTokens() }, usdCents: 0,
  };
  let usd = 0;
  for (const r of runs) {
    l.runs++;
    l.byTrigger[r.trigger]++;
    l.byPath[r.path]++;
    if (r.outcome) l.byOutcome[r.outcome]++;
    l.candidates += r.candidates;
    l.saves += r.saves;
    l.updates += r.updates;
    l.skips += r.skips;
    if (r.outcome === 'done' && r.saves + r.updates === 0) l.empty++;
    l.lookups += r.lookups;
    l.errors += r.errors;
    l.sentTokens += tokensOf(r.sentChars);
    for (const t of r.tokens) {
      const sum = l.tokens[t.phase];
      sum.input += t.input;
      sum.output += t.output;
      sum.cacheRead += t.cacheRead;
      sum.cacheWrite += t.cacheWrite;
      sum.messages++;
      const price = priceFor(t.model);
      if (price) usd += costOf(t, price);
    }
  }
  l.usdCents = Math.round(usd * 100);
  return l;
}

/** Learn steps as the project home and the Usage page show them (`MekoLearning`). */
export function learningOf(runs: LearnRun[]): MekoLearning {
  const l: MekoLearning = { runs: 0, failed: 0, skipped: 0, saves: 0, updates: 0, skips: 0, tokens: 0, usd: null };
  let usd = 0;
  let priced = false;
  for (const r of runs) {
    if (r.outcome === 'skipped') {
      l.skipped++;
      continue;
    }
    l.runs++;
    if (r.outcome === 'failed' || r.outcome === 'interrupted') l.failed++;
    l.saves += r.saves;
    l.updates += r.updates;
    l.skips += r.skips;
    for (const t of r.tokens) {
      l.tokens += t.input + t.output + t.cacheRead + t.cacheWrite;
      const price = priceFor(t.model);
      if (!price) continue;
      usd += costOf(t, price);
      priced = true;
    }
  }
  l.usd = priced ? usd : null;
  return l;
}

/** One of AgentK's own Meko calls (meko_agentk_calls). */
export interface AgentkCall {
  at: number;
  tool: string;
  input_chars: number;
  ok: number;
  ms: number;
}

export function agentkCalls(db: Db): AgentkCall[] {
  return all<AgentkCall>(db, 'SELECT at, tool, input_chars, ok, ms FROM meko_agentk_calls');
}

/** How many tools AgentK's own calls are counted by: the rest are summed as `other`. */
const AGENTK_TOOLS = 10;

/**
 * AgentK's own Meko calls as the usage report has them: a tool name that isn't a plain one counts
 * as `other`, and so do all but the AGENTK_TOOLS - 1 most called tools.
 */
export function summarizeAgentk(rows: AgentkCall[]): CallHomeAgentkMeko {
  const out: CallHomeAgentkMeko = { calls: 0, errors: 0, sentTokens: 0, ms: 0, byTool: {} };
  const by = new Map<string, number>();
  for (const r of rows) {
    out.calls++;
    if (!r.ok) out.errors++;
    out.sentTokens += tokensOf(r.input_chars);
    out.ms += Math.max(0, r.ms);
    const t = /^[a-z][a-z_]{0,39}$/.test(r.tool) ? r.tool : 'other';
    by.set(t, (by.get(t) ?? 0) + 1);
  }
  const sorted = [...by].sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : 1));
  const keep = sorted.length <= AGENTK_TOOLS ? sorted : sorted.filter(([t]) => t !== 'other').slice(0, AGENTK_TOOLS - 1);
  for (const [t, n] of keep) out.byTool[t] = n;
  const rest = rows.length - keep.reduce((n, [, c]) => n + c, 0);
  if (rest > 0) out.byTool.other = (out.byTool.other ?? 0) + rest;
  return out;
}

/** One run of the Meko plugin's transcript capture (meko_capture_runs). */
export interface CaptureRun {
  at: number;
  ok: number;
  ms: number;
}

export function captureRuns(db: Db): CaptureRun[] {
  return all<CaptureRun>(db, 'SELECT at, ok, ms FROM meko_capture_runs');
}

export function summarizeCapture(rows: CaptureRun[]): { runs: number; failures: number; ms: number } {
  return { runs: rows.length, failures: rows.filter((r) => !r.ok).length, ms: rows.reduce((n, r) => n + Math.max(0, r.ms), 0) };
}

export interface MekoStart {
  at: number;
  outcome: 'found' | 'empty' | 'error' | 'late';
  ms: number;
}

/** AgentK's searches at session start (`src/server/recall.ts`). */
export function mekoStarts(db: Db): MekoStart[] {
  return all<MekoStart>(db, 'SELECT at, outcome, ms FROM meko_preloads');
}

/** Calls grouped by a key (session or project), each summarized. */
export function mekoBy(rows: CallRow[], key: (r: CallRow) => string): Map<string, MekoUsage> {
  const groups = new Map<string, CallRow[]>();
  for (const r of rows) {
    const k = key(r);
    const g = groups.get(k);
    if (g) g.push(r);
    else groups.set(k, [r]);
  }
  return new Map([...groups].map(([k, g]) => [k, summarizeMeko(g)]));
}

/** The per-session counts shown in the project home's session table. */
export function sessionMeko(u: MekoUsage | undefined): SessionUsage['meko'] {
  return { lookups: u?.lookups ?? 0, found: u?.found ?? 0, saves: u?.saves ?? 0 };
}
