import type { MekoFirstTurn, MekoUsage, SessionUsage } from '../shared/types.js';
import { all, get, type Db } from './db.js';
import { CARRY_TURNS, SEARCH_TOOLS, searchSaving } from './mekosavings.js';

export type MekoKind = 'lookup' | 'save' | 'other';
export type MekoOutcome = 'found' | 'empty' | 'error' | 'ok';

/** The Meko MCP server's tools, under its own name or a plugin's (`mcp__plugin_x_meko__…`). */
const MEKO_TOOL = /^mcp__(?:[\w-]+_)?meko__([a-z_]+)$/;
const LOOKUPS = new Set([...SEARCH_TOOLS, 'memory_get_all', 'memory_get_by_id', 'conversation_get', 'artifact_get']);
const SAVES = new Set(['memory_add', 'memory_update', 'memory_promote', 'artifact_put', 'conversation_add_message']);

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
  model: string | null;
  context_tokens: number | null;
  /** For searches that found something, and seeds: responses after the call (or the first response), up to CARRY_TURNS. */
  later: number | null;
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
    `SELECT s.project_id, c.session_id, c.tool, c.kind, c.outcome, c.result_chars, c.model, c.context_tokens,
       CASE WHEN c.outcome = 'found' AND c.tool IN (${SEARCH_LIST}) THEN
         (SELECT COUNT(*) FROM (SELECT 1 FROM usage_messages u
          WHERE u.session_id = c.session_id AND u.agent IS c.agent AND u.at > c.at LIMIT ?))
       END AS later
     FROM meko_calls c JOIN sessions s ON s.id = c.session_id${where}
     UNION ALL
     SELECT s.project_id, p.session_id, 'seed', 'seed', 'found', p.chars, f.model,
       MAX(0, f.input + f.cache_read + f.cache_write - ROUND(p.chars / 4.0)),
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
    calls: 0, lookups: 0, found: 0, empty: 0, errors: 0, saves: 0, foundTokens: 0,
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
