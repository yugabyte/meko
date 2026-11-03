import { execFile } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdirSync, mkdtempSync, readFileSync, readdirSync, renameSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { join } from 'node:path';
import type { KiroPlan } from '../shared/types.js';
import type { Paths } from './paths.js';
import { writeScript } from './scriptfile.js';

/**
 * Kiro CLI (`kiro-cli`), what AgentK needs of it outside the adapter (src/server/harness/kiro.ts):
 * its home, the agent files AgentK writes there, the guard script, its session store, and its
 * version. Checked against kiro-cli 2.32.0, whose default engine runs only the selected agent's
 * inline hooks (`agentSpawn`, `userPromptSubmit`, `preToolUse`, `postToolUse`, `stop`), each given
 * the event as JSON on stdin with `hook_event_name`, `cwd` and `session_id`.
 */

/** Variables Kiro sets in its children's environment; an AgentK started inside Kiro must not pass them on. */
export const KIRO_SESSION_MARKERS = ['KIRO_SESSION_ID', 'KIRO_CLI_IS_CHILD_PROCESS', 'KIRO_CLI_ACP_CLIENT_NAME', 'KIRO_HOST_PID'];

/** The agent Kiro sessions run as (`--agent`), derived from the user's default agent with AgentK's hooks added. */
export const AGENTK_AGENT = 'agentk';
/** The agent Ask AgentK runs as: no tools, no MCP servers, no hooks. */
export const ASK_AGENT = 'agentk-ask';
/** The agent the plan usage read (`/usage`) runs as, likewise inert. */
export const USAGE_AGENT = 'agentk-usage';
/** In the description of every agent file AgentK writes, so it's never mistaken for the user's own. */
const MARK = 'Written by AgentK, which rewrites it: edit your default agent instead.';
/** The mark while the product was AgentK Local, on agent files written before the rename. */
const OLD_MARK = 'Written by AgentK Local, which rewrites it';

/** Kiro's home (`KIRO_HOME`, else `~/.kiro`), which every one of its global files is under. */
export function kiroHome(): string {
  return process.env.KIRO_HOME || join(homedir(), '.kiro');
}

/** The Design-stage guard for Kiro, which denies a tool call by exit code 2 with the reason on stderr. */
export function kiroGuardScriptPath(paths: Paths): string {
  return join(paths.run, 'kiro-guard.sh');
}

/**
 * Asks the server whether a file write may go ahead (the server answers a denial's reason as plain
 * text, nothing to allow it) and exits 2 with the reason for Kiro. Like Claude's guard, it lets the
 * write go ahead when the server can't be reached.
 */
const KIRO_GUARD_SCRIPT = `#!/bin/sh
# Written by AgentK. Asks the server whether a Kiro file write may go ahead.
[ -n "$AK_HOOK_TOKEN" ] && [ -S "$AK_HOOK_SOCK" ] || { cat >/dev/null; exit 0; }
reason=$(curl -sf --max-time 3 --unix-socket "$AK_HOOK_SOCK" \\
  -H "X-AK-Token: $AK_HOOK_TOKEN" -H "Content-Type: application/json" \\
  --data-binary @- http://localhost/guard 2>/dev/null)
[ -n "$reason" ] || exit 0
printf '%s\\n' "$reason" >&2
exit 2
`;

export function writeKiroGuardScript(paths: Paths): void {
  mkdirSync(paths.run, { recursive: true });
  writeScript(kiroGuardScriptPath(paths), KIRO_GUARD_SCRIPT);
}

type Json = Record<string, unknown>;
const isObject = (v: unknown): v is Json => !!v && typeof v === 'object' && !Array.isArray(v);

function readJson(file: string): Json | null {
  try {
    const v = JSON.parse(readFileSync(file, 'utf8')) as unknown;
    return isObject(v) ? v : null;
  } catch {
    return null;
  }
}

/** The agent `kiro-cli chat` starts with: `chat.defaultAgent` in Kiro's settings, or null for its built-in one. */
export function kiroDefaultAgentName(home = kiroHome()): string | null {
  const name = readJson(join(home, 'settings', 'cli.json'))?.['chat.defaultAgent'];
  return typeof name === 'string' && name.trim() ? name.trim() : null;
}

/**
 * A global agent's config by its name: `<name>.json` in Kiro's agents folder, else the file there
 * whose `name` it is. Never one AgentK wrote.
 */
export function readKiroAgent(name: string, home = kiroHome()): Json | null {
  const dir = join(home, 'agents');
  const ours = (a: Json | null) =>
    !!a && typeof a.description === 'string' && (a.description.includes(MARK) || a.description.includes(OLD_MARK));
  const direct = readJson(join(dir, `${name}.json`));
  if (direct && !ours(direct)) return direct;
  let files: string[];
  try {
    files = readdirSync(dir).filter((f) => f.endsWith('.json'));
  } catch {
    return null;
  }
  for (const f of files) {
    const a = readJson(join(dir, f));
    if (a?.name === name && !ours(a)) return a;
  }
  return null;
}

/**
 * Kiro's built-in default agent as `kiro-cli agent create --from kiro_default` writes it (kept in
 * the run folder), or, before that has run, the parts of it AgentK's sessions need: every tool, the
 * user's MCP servers, and the project files it reads.
 */
export function builtinKiroAgent(paths: Paths): Json {
  return readJson(join(paths.run, 'kiro-default.json')) ?? {
    tools: ['*'],
    allowedTools: [],
    resources: ['file://AmazonQ.md', 'file://AGENTS.md', 'file://README.md', 'skill://.kiro/skills/*/SKILL.md'],
    includeMcpJson: true,
    mcpServers: {},
  };
}

/**
 * Saves Kiro's built-in default agent to the run folder (`builtinKiroAgent`), by having `kiro-cli`
 * export it into a temporary folder. Never throws: a Kiro that can't do it leaves the fallback.
 */
export function exportBuiltinKiroAgent(command: string, paths: Paths): Promise<boolean> {
  const dir = mkdtempSync(join(tmpdir(), 'akl-kiro-'));
  return new Promise<boolean>((resolve) => {
    execFile(command, ['agent', 'create', 'agentk-base', '--from', 'kiro_default', '--directory', dir],
      { timeout: 20_000, env: { ...process.env, NO_COLOR: '1', EDITOR: 'true', VISUAL: 'true' } }, () => {
        const a = readJson(join(dir, 'agentk-base.json'));
        if (a) {
          mkdirSync(paths.run, { recursive: true });
          writeAtomic(join(paths.run, 'kiro-default.json'), JSON.stringify(a, null, 2));
        }
        rmSync(dir, { recursive: true, force: true });
        resolve(!!a);
      }).stdin?.end();
  });
}

/** One hook entry in a Kiro agent's `hooks`. */
export interface KiroHook {
  command: string;
  matcher?: string;
  timeout_ms?: number;
}

/** AgentK's hooks for each Kiro event, added after the base agent's own. */
export type KiroHooks = Partial<Record<'agentSpawn' | 'userPromptSubmit' | 'preToolUse' | 'postToolUse' | 'stop', KiroHook[]>>;

/** AgentK's session tools in the `agentk` agent: their MCP server and their skill. */
export interface KiroTools {
  name: string;
  server: Json;
  skill: string;
}

/**
 * The agent Kiro sessions run as: the user's default agent (`base`, which is Meko's `meko-default`
 * when Meko's setup for Kiro ran, so its MCP server and capture hooks carry over) with AgentK's hooks
 * added to each event's list, its own kept first, and AgentK's session tools (`tools`: their MCP
 * server, their skill as a `skill://` resource, and their read-only `list` allowed). Kiro's upgrade
 * bookkeeping is left out. It grants nothing else the base doesn't: `[kiro] trust_tools` is a flag
 * on AgentK's launch (kiroArgs), so a chat someone starts as `agentk` outside AgentK asks as their
 * own agent would (and the tools there say it isn't an AgentK session).
 */
export function agentkAgent(base: Json, hooks: KiroHooks, o: { mekoCapture?: boolean; tools?: KiroTools | null } = {}): Json {
  const { kiroUpgrade: _upgrade, ...rest } = base;
  void _upgrade;
  const merged: Record<string, unknown[]> = {};
  const own = isObject(base.hooks) ? base.hooks : {};
  // Meko's capture hooks send the session's turns to Meko: left out with `[meko] capture_transcripts`
  // off, as Claude's sessions run without Meko's plugin then.
  const keep = (h: unknown) => o.mekoCapture !== false || !(isObject(h) && typeof h.command === 'string' && MEKO_CAPTURE.test(h.command));
  for (const [event, list] of Object.entries(own)) if (Array.isArray(list)) merged[event] = list.filter(keep);
  for (const [event, list] of Object.entries(hooks)) merged[event] = [...(merged[event] ?? []), ...(list ?? [])];
  const from = typeof base.name === 'string' ? ` Based on ${base.name}.` : '';
  const agent: Json = { ...rest, name: AGENTK_AGENT, description: `${MARK}${from}`, hooks: merged };
  const t = o.tools;
  if (t) {
    const list = (v: unknown) => (Array.isArray(v) ? v : []);
    agent.mcpServers = { ...(isObject(base.mcpServers) ? base.mcpServers : {}), [t.name]: t.server };
    agent.resources = [...list(base.resources), `skill://${t.skill}`];
  }
  return agent;
}

/** Meko's hook handler for Kiro (its `meko-default` agent's hooks run it). */
const MEKO_CAPTURE = /kiro-handler\.js/;

/** The agent Ask AgentK runs as: its instructions, and nothing it could act with. */
export function askAgent(prompt: string, name = ASK_AGENT): Json {
  return {
    name, description: MARK, prompt, mcpServers: {}, tools: [], toolAliases: {}, allowedTools: [], resources: [],
    includeMcpJson: false, hooks: {},
  };
}

function writeAtomic(file: string, text: string): void {
  const tmp = `${file}.${randomBytes(4).toString('hex')}.tmp`;
  writeFileSync(tmp, text, { mode: 0o600 });
  renameSync(tmp, file);
}

/**
 * Writes a global agent file AgentK owns, only when its content changed (so a running Kiro isn't
 * made to reload it for nothing). True when it wrote. Throws when the folder can't be written.
 */
export function writeKiroAgent(agent: Json, home = kiroHome()): boolean {
  const dir = join(home, 'agents');
  const file = join(dir, `${String(agent.name)}.json`);
  const text = `${JSON.stringify(agent, null, 2)}\n`;
  try {
    if (readFileSync(file, 'utf8') === text) return false;
  } catch {
    /* not there yet */
  }
  mkdirSync(dir, { recursive: true });
  writeAtomic(file, text);
  return true;
}

/**
 * Removes the agent AgentK wrote in Kiro's home (uninstall). Ask AgentK's `agentk-ask` is a
 * workspace agent in AgentK's own run folder, which goes with the home.
 */
export function removeKiroAgent(home = kiroHome()): void {
  rmSync(join(home, 'agents', `${AGENTK_AGENT}.json`), { force: true });
}

/** Kiro's file for one of its sessions (`<session id>.json`, beside its `.jsonl` history). */
export function kiroSessionFile(id: string, home = kiroHome()): string {
  return join(home, 'sessions', 'cli', `${id}.json`);
}

/** Its session log beside it: each prompt, reply and tool result as a line (`parseKiroMekoCalls`). */
export function kiroSessionLog(id: string, home = kiroHome()): string {
  return join(home, 'sessions', 'cli', `${id}.jsonl`);
}

const SESSION_ID = /^[\w-]{1,100}$/;

/**
 * The newest Kiro session started in `cwd` (any of its spellings) at `since` (ms) or later, by when
 * Kiro says it started (its `created_at`; the file's time when it has none), in Kiro's session store;
 * null when there is none. What a Kiro session's id is found by when no hook said it. A worktree
 * outlives its sessions, so `since` keeps out the conversations of the sessions that used it before.
 */
export function findKiroSession(cwds: string[], since = 0, home = kiroHome()): string | null {
  const dir = join(home, 'sessions', 'cli');
  let best: { id: string; at: number } | null = null;
  let files: string[];
  try {
    files = readdirSync(dir).filter((f) => f.endsWith('.json'));
  } catch {
    return null;
  }
  for (const f of files) {
    const id = f.slice(0, -'.json'.length);
    if (!SESSION_ID.test(id)) continue;
    const s = readJson(join(dir, f));
    if (!s || typeof s.cwd !== 'string' || !cwds.includes(s.cwd)) continue;
    let at = typeof s.created_at === 'string' ? Date.parse(s.created_at) : NaN;
    if (!Number.isFinite(at)) {
      try {
        at = statSync(join(dir, f)).mtimeMs;
      } catch {
        continue;
      }
    }
    if (at < since || (best && at <= best.at)) continue;
    best = { id, at };
  }
  return best?.id ?? null;
}

/** One finished turn of a Kiro session, as its session file records it. */
export interface KiroTurn {
  /** Unique within the session: when the turn ended, as Kiro wrote it. */
  key: string;
  at: number;
  model: string;
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  credits: number;
  /** How full the context was after the turn, in percent; null when Kiro didn't say. */
  contextPercent: number | null;
}

const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : 0);

/** The finished turns in a Kiro session file's text, oldest first; none when it can't be read. */
export function parseKiroTurns(text: string): KiroTurn[] {
  let s: unknown;
  try {
    s = JSON.parse(text);
  } catch {
    return [];
  }
  const meta = isObject(s) && isObject(s.session_state) && isObject(s.session_state.conversation_metadata) ? s.session_state.conversation_metadata : null;
  const turns = Array.isArray(meta?.user_turn_metadatas) ? meta.user_turn_metadatas : [];
  const out: KiroTurn[] = [];
  for (const t of turns) {
    if (!isObject(t) || typeof t.end_timestamp !== 'string') continue;
    const at = Date.parse(t.end_timestamp);
    if (!Number.isFinite(at)) continue;
    const credits = (Array.isArray(t.metering_usage) ? t.metering_usage : [])
      .filter((m): m is Json => isObject(m) && m.unit === 'credit')
      .reduce((n, m) => n + num(m.value), 0);
    const pct = t.final_context_usage_percentage ?? t.context_usage_percentage;
    out.push({
      key: t.end_timestamp, at, model: typeof t.model === 'string' && t.model ? t.model : 'auto',
      input: num(t.input_token_count), output: num(t.output_token_count),
      cacheRead: num(t.cache_read_input_token_count), cacheWrite: num(t.cache_write_input_token_count),
      credits, contextPercent: typeof pct === 'number' && Number.isFinite(pct) ? pct : null,
    });
  }
  return out;
}

/** A Meko call of a Kiro session with its result, from its session log (`<conversation>.jsonl`). */
export interface KiroMekoCall {
  /** Kiro's tool use id. */
  id: string;
  /** The Meko tool (`context_search`). */
  tool: string;
  /** When its turn started (its prompt), or when AgentK read it when the read didn't see the prompt. */
  at: number;
  input: Json;
  text: string;
  isError: boolean;
}

/** The text of an MCP reply's content items (`[{ type: 'text', text }]`). */
function mcpText(content: unknown): string {
  return Array.isArray(content) ? content.map((c) => (isObject(c) && c.type === 'text' && typeof c.text === 'string' ? c.text : '')).join('') : '';
}

/**
 * The Meko calls whose results are in a Kiro session log's whole lines, checked against kiro-cli
 * 2.34.0. Only a `ToolResults` line names a call's MCP server (`data.results[<id>].tool.kind.Mcp`),
 * so a call is taken from there, from a server whose name starts with `meko`, with its reply from
 * the line's `toolResult` content. Only `Prompt` lines carry a time (`data.meta.timestamp`, in
 * seconds), so a call is dated by its turn's prompt, or `now` when the text read didn't hold it.
 * A line it doesn't know is skipped.
 */
export function parseKiroMekoCalls(text: string, now: number): KiroMekoCall[] {
  const out: KiroMekoCall[] = [];
  let turnAt: number | null = null;
  for (const line of text.split('\n')) {
    const prompt = line.includes('"kind":"Prompt"');
    if (!prompt && !(line.includes('"kind":"ToolResults"') && /"serverName":"meko/i.test(line))) continue;
    let d: unknown;
    try {
      d = JSON.parse(line);
    } catch {
      continue;
    }
    if (!isObject(d) || !isObject(d.data)) continue;
    if (d.kind === 'Prompt') {
      const ts = isObject(d.data.meta) ? d.data.meta.timestamp : null;
      if (typeof ts === 'number' && Number.isFinite(ts) && ts > 0) turnAt = ts * 1000;
      continue;
    }
    if (d.kind !== 'ToolResults' || !isObject(d.data.results)) continue;
    const replies = new Map<string, { text: string; isError: boolean }>();
    for (const c of Array.isArray(d.data.content) ? d.data.content : []) {
      if (!isObject(c) || c.kind !== 'toolResult' || !isObject(c.data) || typeof c.data.toolUseId !== 'string') continue;
      // An MCP reply is a `json` part; Kiro's own message (a refused call's) is a `text` part.
      const parts = Array.isArray(c.data.content) ? c.data.content : [];
      const json = parts.find((x): x is Json => isObject(x) && isObject(x.data));
      const plain = parts.map((x) => (isObject(x) && x.kind === 'text' && typeof x.data === 'string' ? x.data : '')).join('');
      replies.set(c.data.toolUseId, {
        text: json ? mcpText((json.data as Json).content) : plain,
        isError: c.data.status !== 'success' || (!!json && (json.data as Json).isError === true),
      });
    }
    for (const [id, r] of Object.entries(d.data.results)) {
      const mcp = isObject(r) && isObject(r.tool) && isObject(r.tool.kind) && isObject(r.tool.kind.Mcp) ? r.tool.kind.Mcp : null;
      if (!mcp || typeof mcp.serverName !== 'string' || !/^meko/i.test(mcp.serverName) || typeof mcp.toolName !== 'string') continue;
      if (!/^[a-z_]+$/.test(mcp.toolName) || !id || id.length > 200) continue;
      const result = isObject(r) && isObject(r.result) ? r.result : {};
      const success = isObject(result.Success) ? result.Success : null;
      const items = success && Array.isArray(success.items) ? success.items : [];
      const first = items.find((x): x is Json => isObject(x) && isObject(x.Json));
      const reply = replies.get(id) ?? {
        text: first ? mcpText((first.Json as Json).content) : typeof result.Error === 'string' ? result.Error : '',
        isError: !success || (!!first && (first.Json as Json).isError === true),
      };
      const { __tool_use_purpose: _purpose, ...input } = isObject(mcp.params) ? mcp.params : {};
      out.push({ id, tool: mcp.toolName, at: turnAt ?? now, input, ...reply });
    }
  }
  return out;
}

/** The finished turns of a Kiro session, from its session file. */
export function kiroTurns(id: string, home = kiroHome()): KiroTurn[] {
  try {
    return parseKiroTurns(readFileSync(kiroSessionFile(id, home), 'utf8'));
  } catch {
    return [];
  }
}

/**
 * The answer in what `kiro-cli chat --no-interactive --output-format stream-json` printed: the
 * `runFinished` event's final text, whether the run succeeded, and its session id (to delete it).
 */
export function readKiroRun(stdout: string): { text: string; ok: boolean; sessionId: string | null; error: string | null } {
  let finished: Json | null = null;
  let error: string | null = null;
  for (const line of stdout.split('\n')) {
    if (!line.trim()) continue;
    let e: unknown;
    try {
      e = JSON.parse(line);
    } catch {
      continue;
    }
    if (!isObject(e) || !isObject(e.data)) continue;
    if (e.type === 'runFinished') finished = e.data;
    if (e.type === 'error' && typeof e.data.message === 'string') error = e.data.message;
  }
  const sessionId = typeof finished?.sessionId === 'string' && SESSION_ID.test(finished.sessionId) ? finished.sessionId : null;
  return {
    text: typeof finished?.finalText === 'string' ? finished.finalText : '',
    ok: finished?.status === 'success',
    sessionId,
    error: error ?? (finished && typeof finished.status === 'string' && finished.status !== 'success' ? `the run ended with ${finished.status}` : null),
  };
}

/**
 * The arguments for reading Kiro's plan allowance: its `/usage` command in a one-shot run, which
 * bills nothing, as the inert `agentk-usage` agent (written in the run's folder), so the user's
 * default agent, its MCP servers and its hooks aren't loaded for it.
 */
export function kiroUsageArgs(): string[] {
  return ['chat', '--no-interactive', '--output-format', 'stream-json', '--agent', USAGE_AGENT, '/usage'];
}

/**
 * Points the sessions in Kiro's store that ran in a folder `map` moves at its new path (their
 * `cwd`, which `findKiroSession` and `kiroSessionsIn` match on). Returns how many it changed.
 */
export function moveKiroSessions(map: (dir: string) => string | null, home = kiroHome()): number {
  const dir = join(home, 'sessions', 'cli');
  let files: string[];
  try {
    files = readdirSync(dir).filter((f) => f.endsWith('.json'));
  } catch {
    return 0;
  }
  let n = 0;
  for (const f of files) {
    const s = readJson(join(dir, f));
    const to = s && typeof s.cwd === 'string' ? map(s.cwd) : null;
    if (!s || !to) continue;
    writeAtomic(join(dir, f), JSON.stringify({ ...s, cwd: to }));
    n++;
  }
  return n;
}

/** The ids of the sessions in Kiro's store that ran in one of `cwds` (any of its spellings). */
export function kiroSessionsIn(cwds: string[], home = kiroHome()): string[] {
  const dir = join(home, 'sessions', 'cli');
  let files: string[];
  try {
    files = readdirSync(dir).filter((f) => f.endsWith('.json'));
  } catch {
    return [];
  }
  return files
    .map((f) => f.slice(0, -'.json'.length))
    .filter((id) => {
      if (!SESSION_ID.test(id)) return false;
      const s = readJson(join(dir, `${id}.json`));
      return !!s && typeof s.cwd === 'string' && cwds.includes(s.cwd);
    });
}

/** A number as Kiro prints one: `36.02`, `1,000`. */
const NUM = String.raw`\d[\d,]*(?:\.\d+)?`;
const toNum = (s: string) => Number(s.replace(/,/g, ''));
const DAY = /\b(\d{4}-\d{2}-\d{2})\b/;

/**
 * Kiro's plan allowance from what its `/usage` command printed, checked against kiro-cli 2.32.0 on
 * the free plan:
 *
 *     Estimated Usage | resets on 2026-11-01 | KIRO FREE
 *     Credits (36.02 of 50 covered in plan), 72.0%
 *
 * Null without the credits line. The overage line's only seen form is `Overages: Disabled`
 * (kiro-cli 2.34.0, Power plan); its credit-bearing form and the bonus line haven't been seen, so
 * all three are matched loosely (a `bonus` line with two numbers, an `overage` line), each left
 * null when absent.
 */
export function parseKiroPlan(text: string, now: number): KiroPlan | null {
  // eslint-disable-next-line no-control-regex
  const lines = text.replace(/\x1b\[[0-9;]*[A-Za-z]/g, '').split('\n').map((l) => l.trim()).filter(Boolean);
  const credits = new RegExp(String.raw`credits\s*\(\s*(${NUM})\s+of\s+(${NUM})\s+covered`, 'i');
  const main = lines.map((l) => (/bonus/i.test(l) ? null : credits.exec(l))).find((m) => m);
  if (!main) return null;
  const head = lines.find((l) => /resets on/i.test(l));
  const plan = head?.split('|').at(-1)?.trim().replace(/^kiro\s+/i, '') ?? '';
  const bonusLine = lines.find((l) => /bonus/i.test(l));
  const pair = bonusLine && new RegExp(String.raw`(${NUM})\s*(?:/|of)\s*(${NUM})`, 'i').exec(bonusLine);
  const overLine = lines.find((l) => /overage/i.test(l) && !/bonus/i.test(l));
  return {
    plan: plan && !/resets on|usage/i.test(plan) ? plan.toLowerCase().replace(/\b[a-z]/g, (c) => c.toUpperCase()) : null,
    used: toNum(main[1]),
    covered: toNum(main[2]),
    resetsOn: (head && /resets on\s+(\d{4}-\d{2}-\d{2})/i.exec(head)?.[1]) ?? null,
    bonus: bonusLine && pair ? {
      // "120 of 500 remaining" counts what's left; anything else, what's used.
      used: /remaining|left/i.test(bonusLine) ? Math.max(0, toNum(pair[2]) - toNum(pair[1])) : toNum(pair[1]),
      total: toNum(pair[2]),
      expires: DAY.exec(bonusLine)?.[1] ?? inDays(/expires? in\s+(\d+)\s+days?/i.exec(bonusLine)?.[1], now),
      line: bonusLine.slice(0, 200),
    } : null,
    overage: overLine ? {
      credits: firstAmount(overLine),
      enabled: /\benabled\b|:\s*on\b/i.test(overLine) ? true : /\bdisabled\b|:\s*off\b/i.test(overLine) ? false : null,
      line: overLine.slice(0, 200),
    } : null,
    updatedAt: now,
  };
}

/** The first number on a line that isn't money or a percentage: 14.2 in "Overages: 14.20 credits ($0.57)". */
function firstAmount(line: string): number | null {
  for (const m of line.matchAll(new RegExp(NUM, 'g'))) {
    if (line[m.index - 1] !== '$' && line[m.index + m[0].length] !== '%') return toNum(m[0]);
  }
  return null;
}

/** The day `n` days from `now` (YYYY-MM-DD, UTC), or null without `n`. */
function inDays(n: string | undefined, now: number): string | null {
  return n ? new Date(now + Number(n) * 86_400_000).toISOString().slice(0, 10) : null;
}

/** The session deletes still running, for `kiroDeletesSettled`. */
const deleting = new Set<Promise<void>>();

/** Deletes one of Kiro's saved sessions (an Ask AgentK run's), in the background; never throws. */
export function deleteKiroSession(command: string, id: string, cwd: string): void {
  if (!SESSION_ID.test(id)) return;
  const done = new Promise<void>((resolve) => {
    execFile(command, ['chat', '--delete-session', id], { cwd, timeout: 15_000 }, () => resolve()).stdin?.end();
  });
  deleting.add(done);
  void done.then(() => deleting.delete(done));
}

/** Resolves once no session delete is running (including ones started meanwhile). Never throws. */
export async function kiroDeletesSettled(): Promise<void> {
  while (deleting.size) await Promise.all(deleting);
}

/**
 * The model ids in `kiro-cli chat --list-models -f json` (a `ModelListResult`: `{ models, default_model }`,
 * each model an object with its `model_id`, or a plain id), in its order, without repeats. Anything
 * else reads as none.
 */
export function parseKiroModels(stdout: string): string[] {
  let data: unknown;
  try {
    data = JSON.parse(stdout);
  } catch {
    return [];
  }
  const list = Array.isArray(data) ? data : isObject(data) && Array.isArray(data.models) ? data.models : [];
  const ids = list.map((m: unknown) => {
    if (typeof m === 'string') return m;
    if (!isObject(m)) return '';
    const id = m.model_id ?? m.modelId ?? m.id ?? m.model_name;
    return typeof id === 'string' ? id.trim() : '';
  });
  return [...new Set(ids.filter(Boolean))];
}
