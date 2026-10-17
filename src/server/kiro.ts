import { execFile } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { chmodSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, renameSync, rmSync, statSync, writeFileSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { join } from 'node:path';
import type { Paths } from './paths.js';

/**
 * Kiro CLI (`kiro-cli`), what AgentK needs of it outside the adapter (src/server/harness/kiro.ts):
 * its home, the agent files AgentK writes there, the guard script, its session store, and its
 * version. Checked against kiro-cli 2.32.0, whose default engine runs only the selected agent's
 * inline hooks (`agentSpawn`, `userPromptSubmit`, `preToolUse`, `postToolUse`, `stop`), each given
 * the event as JSON on stdin with `hook_event_name`, `cwd` and `session_id`.
 */

/** Variables Kiro sets in its children's environment; an AgentK started inside Kiro must not pass them on. */
export const KIRO_SESSION_MARKERS = ['KIRO_SESSION_ID', 'KIRO_CLI_IS_CHILD_PROCESS', 'KIRO_CLI_ACP_CLIENT_NAME', 'KIRO_HOST_PID'];

/** The oldest Kiro CLI AgentK runs sessions on (the one its hooks and session store were checked against). */
export const MIN_KIRO = '2.32.0';

/** The agent Kiro sessions run as (`--agent`), derived from the user's default agent with AgentK's hooks added. */
export const AGENTK_AGENT = 'agentk';
/** The agent Ask AgentK runs as: no tools, no MCP servers, no hooks. */
export const ASK_AGENT = 'agentk-ask';
/** In the description of every agent file AgentK writes, so it's never mistaken for the user's own. */
const MARK = 'Written by AgentK Local, which rewrites it: edit your default agent instead.';

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
# Written by agent-k-local. Asks the server whether a Kiro file write may go ahead.
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
  const file = kiroGuardScriptPath(paths);
  writeFileSync(file, KIRO_GUARD_SCRIPT);
  chmodSync(file, 0o755);
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
  const ours = (a: Json | null) => !!a && typeof a.description === 'string' && a.description.includes(MARK);
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

/**
 * The agent Kiro sessions run as: the user's default agent (`base`, which is Meko's `meko-default`
 * when Meko's setup for Kiro ran, so its MCP server and capture hooks carry over) with AgentK's hooks
 * added to each event's list, its own kept first. Kiro's upgrade bookkeeping is left out. With
 * `trustTools` every tool is allowed without asking: in the agent, since `--trust-all-tools` makes
 * Kiro 2.32 ask on screen whether to go on, with "No, exit" first (a typed prompt's Enter picks it).
 */
export function agentkAgent(base: Json, hooks: KiroHooks, o: { trustTools?: boolean; mekoCapture?: boolean } = {}): Json {
  const { kiroUpgrade: _upgrade, ...rest } = base;
  void _upgrade;
  const trustTools = !!o.trustTools;
  const merged: Record<string, unknown[]> = {};
  const own = isObject(base.hooks) ? base.hooks : {};
  // Meko's capture hooks send the session's turns to Meko: left out with `[meko] capture_transcripts`
  // off, as Claude's sessions run without Meko's plugin then.
  const keep = (h: unknown) => o.mekoCapture !== false || !(isObject(h) && typeof h.command === 'string' && MEKO_CAPTURE.test(h.command));
  for (const [event, list] of Object.entries(own)) if (Array.isArray(list)) merged[event] = list.filter(keep);
  for (const [event, list] of Object.entries(hooks)) merged[event] = [...(merged[event] ?? []), ...(list ?? [])];
  const from = typeof base.name === 'string' ? ` Based on ${base.name}.` : '';
  return { ...rest, ...(trustTools ? { allowedTools: ['*'] } : {}), name: AGENTK_AGENT, description: `${MARK}${from}`, hooks: merged };
}

/** Meko's hook handler for Kiro (its `meko-default` agent's hooks run it). */
const MEKO_CAPTURE = /kiro-handler\.js/;

/** The agent Ask AgentK runs as: its instructions, and nothing it could act with. */
export function askAgent(prompt: string): Json {
  return {
    name: ASK_AGENT, description: MARK, prompt, mcpServers: {}, tools: [], toolAliases: {}, allowedTools: [], resources: [],
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

/** Removes the agent files AgentK wrote (uninstall). */
export function removeKiroAgents(home = kiroHome()): void {
  for (const name of [AGENTK_AGENT, ASK_AGENT]) rmSync(join(home, 'agents', `${name}.json`), { force: true });
}

/** Kiro's file for one of its sessions (`<session id>.json`, beside its `.jsonl` history). */
export function kiroSessionFile(id: string, home = kiroHome()): string {
  return join(home, 'sessions', 'cli', `${id}.json`);
}

const SESSION_ID = /^[\w-]{1,100}$/;

/**
 * The newest Kiro session started in `cwd` (any of its spellings) since `since` (ms), by Kiro's
 * session store; null when there is none. What a Kiro session's id is found by when no hook said it.
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
    let at: number;
    try {
      at = statSync(join(dir, f)).mtimeMs;
    } catch {
      continue;
    }
    if (at < since || (best && at <= best.at)) continue;
    const s = readJson(join(dir, f));
    if (s && typeof s.cwd === 'string' && cwds.includes(s.cwd)) best = { id, at };
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

/** Deletes one of Kiro's saved sessions (an Ask AgentK run's), in the background; never throws. */
export function deleteKiroSession(command: string, id: string, cwd: string): void {
  if (!SESSION_ID.test(id)) return;
  execFile(command, ['chat', '--delete-session', id], { cwd, timeout: 15_000 }, () => undefined).stdin?.end();
}
