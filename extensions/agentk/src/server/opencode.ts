import { execFile } from 'node:child_process';
import { accessSync, constants, existsSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { delimiter, join } from 'node:path';
import { DatabaseSync } from 'node:sqlite';
import { writeScript } from './scriptfile.js';
import type { Paths } from './paths.js';

/**
 * OpenCode outside the adapter: AgentK's plugin (the hooks of its sessions), the config a session
 * starts with, a session's folder (its instructions file, and the usage log the plugin writes), the
 * Meko MCP server AgentK gives its sessions, its one-shot output (Ask AgentK), and deleting a session.
 * Nothing here writes to OpenCode's own config.
 */

/** What OpenCode puts in its children's environment. */
export const OPENCODE_SESSION_MARKERS = ['OPENCODE', 'OPENCODE_PID'];

/** The agent Ask AgentK runs as, defined in the ask's own config. */
export const OPENCODE_ASK_AGENT = 'agentk-ask';

/** AgentK's files for OpenCode, under the run folder. */
export function openCodeDir(paths: Paths): string {
  return join(paths.run, 'opencode');
}

export function openCodePluginPath(paths: Paths): string {
  return join(openCodeDir(paths), 'agentk-plugin.js');
}

/** A session's folder: `system.md` (its instructions) and `messages.jsonl` (its usage, from the plugin). */
export function openCodeSessionDir(paths: Paths, sessionId: string): string {
  return join(openCodeDir(paths), 'sessions', sessionId);
}

export function openCodeMessagesFile(paths: Paths, sessionId: string): string {
  return join(openCodeSessionDir(paths, sessionId), 'messages.jsonl');
}

/** The config file with AgentK's Meko server (owner-only: it holds the key). */
export function openCodeMekoFile(paths: Paths): string {
  return join(openCodeDir(paths), 'meko.json');
}

/**
 * The command OpenCode runs as: `command`, or for the default `opencode` when it isn't on the PATH,
 * `~/.opencode/bin/opencode`, where OpenCode's installer puts it (and adds it to a shell's rc file,
 * which a service doesn't read).
 */
export function openCodeCommand(command: string, home = homedir(), path = process.env.PATH ?? ''): string {
  if (command !== 'opencode') return command;
  const runnable = (f: string) => {
    try {
      accessSync(f, constants.X_OK);
      return true;
    } catch {
      return false;
    }
  };
  if (path.split(delimiter).some((d) => d && runnable(join(d, command)))) return command;
  const own = join(home, '.opencode', 'bin', 'opencode');
  return runnable(own) ? own : command;
}

/**
 * AgentK's plugin, loaded only into the sessions AgentK starts (`OPENCODE_CONFIG_CONTENT`). It
 * forwards OpenCode's events to the hook socket in their own shape (the adapter's
 * `normalizeOpenCode` maps them), asks the guard before a file edit and throws its reason, adds
 * AgentK's Meko recall to a new session's system prompt, and logs each finished response's tokens to
 * the session's `messages.jsonl`. It never holds OpenCode up for long, and without AgentK's session
 * variables it does nothing. A subagent's session (the task tool's) sends its events without its
 * id, so they never become the session's conversation, and not its `session.idle`, so it never ends
 * the turn; its tool calls and permission asks still count (one waiting on the user needs the user).
 */
const PLUGIN = `// Written by AgentK, which rewrites it. AgentK loads it into the OpenCode sessions it starts;
// without AgentK's session variables it does nothing.
import { appendFileSync } from 'node:fs';
import { request } from 'node:http';
import { join } from 'node:path';

const EDIT_TOOLS = new Set(['edit', 'write', 'apply_patch', 'patch', 'multiedit']);

export const AgentK = async (input) => {
  const sock = process.env.AK_HOOK_SOCK;
  const token = process.env.AK_HOOK_TOKEN;
  const dir = process.env.AK_OPENCODE_DIR;
  if (!sock || !token) return {};
  const cwd = (input && input.directory) || process.cwd();
  const headers = { 'X-AK-Token': token, 'Content-Type': 'application/json' };
  // The server's answer as text ('' when it has none or can't be reached).
  const post = (route, body, ms) => {
    const data = JSON.stringify({ cwd, ...body });
    if (globalThis.Bun) {
      return fetch('http://localhost/' + route, { unix: sock, method: 'POST', headers, body: data, signal: AbortSignal.timeout(ms) })
        .then((r) => (r.status === 200 ? r.text() : ''), () => '');
    }
    return new Promise((resolve) => {
      let req = null;
      const timer = setTimeout(() => { if (req) req.destroy(); resolve(''); }, ms);
      const done = (v) => { clearTimeout(timer); resolve(v); };
      req = request({ socketPath: sock, path: '/' + route, method: 'POST', headers: { ...headers, 'Content-Length': Buffer.byteLength(data) } }, (res) => {
        let text = '';
        res.setEncoding('utf8');
        res.on('data', (c) => { text += c; });
        res.on('end', () => done(res.statusCode === 200 ? text : ''));
        res.on('error', () => done(''));
      });
      req.on('error', () => done(''));
      req.end(data);
    });
  };
  // Events go one after another, in the order OpenCode fired them.
  let queue = Promise.resolve('');
  const send = (body) => {
    queue = queue.then(() => post('hook', body, 3000));
  };
  // The session the user last prompted (a resumed one's from the start); subagents' sessions apart.
  let main = process.env.AK_OPENCODE_SESSION || null;
  const children = new Set();
  const logged = new Set();
  const source = process.env.AK_OPENCODE_SOURCE || 'startup';
  const recall = source === 'startup' ? post('context', { hook_event_name: 'agentk.start', source }, 5000) : Promise.resolve('');
  send({ hook_event_name: 'agentk.start', source, ...(main ? { session_id: main } : {}) });
  const ids = (id) => (id && id === main ? { session_id: id } : {});
  const isMain = (id) => !!id && id === main;

  return {
    'chat.message': async (i) => {
      if (!i || !i.sessionID || children.has(i.sessionID)) return;
      main = i.sessionID;
      send({ hook_event_name: 'chat.message', session_id: main });
    },
    'tool.execute.before': async (i, o) => {
      if (!i || !EDIT_TOOLS.has(i.tool)) return;
      const reason = await post('guard', { hook_event_name: 'tool.execute.before', tool: i.tool, args: (o && o.args) || {}, ...ids(i.sessionID) }, 3000);
      if (reason.trim()) throw new Error(reason.trim());
    },
    'tool.execute.after': async (i) => {
      if (!i) return;
      send({ hook_event_name: 'tool.execute.after', tool: i.tool, ...ids(i.sessionID) });
    },
    'experimental.chat.system.transform': async (i, o) => {
      if (!i || !isMain(i.sessionID) || !o || !Array.isArray(o.system)) return;
      const text = (await recall).trim();
      if (text) o.system.push(text);
    },
    event: async ({ event }) => {
      const p = (event && event.properties) || {};
      switch (event && event.type) {
        case 'session.created':
          if (p.info && p.info.parentID) children.add(p.info.id);
          break;
        case 'session.idle':
          if (isMain(p.sessionID)) send({ hook_event_name: 'session.idle', session_id: p.sessionID });
          break;
        case 'permission.asked':
        case 'permission.updated':
          send({ hook_event_name: 'permission.asked', permission: p.permission || p.type || '', ...ids(p.sessionID) });
          break;
        case 'permission.replied':
          send({ hook_event_name: 'permission.replied', ...ids(p.sessionID) });
          break;
        case 'message.updated': {
          const m = p.info;
          if (!dir || !m || m.role !== 'assistant' || !m.time || !m.time.completed || logged.has(m.id)) break;
          logged.add(m.id);
          const t = m.tokens || {};
          const cache = t.cache || {};
          try {
            appendFileSync(join(dir, 'messages.jsonl'), JSON.stringify({
              id: m.id, session: m.sessionID, model: m.modelID, provider: m.providerID, at: m.time.completed,
              input: t.input, output: t.output, reasoning: t.reasoning, cacheRead: cache.read, cacheWrite: cache.write,
            }) + '\\n');
          } catch {
            // Usage is best effort: a session never fails over it.
          }
          break;
        }
      }
    },
  };
};
`;

/** Writes AgentK's plugin (renamed into place: running sessions keep the copy they loaded). */
export function writeOpenCodePlugin(paths: Paths): void {
  mkdirSync(openCodeDir(paths), { recursive: true });
  writeScript(openCodePluginPath(paths), PLUGIN);
}

/** The plugin's text, for tests that load it. */
export function openCodePluginSource(): string {
  return PLUGIN;
}

/**
 * The config a session starts with, as `OPENCODE_CONFIG_CONTENT`, merged by OpenCode over the user's
 * own (its `plugin` and `instructions` lists are added to theirs): AgentK's plugin, the session's
 * instructions file, and the folders outside the worktree it works in (its design folder),
 * writable without asking.
 */
export function openCodeSessionConfig(o: { plugin: string; instructions: string; dirs: string[] }): string {
  return JSON.stringify({
    plugin: [o.plugin],
    instructions: [o.instructions],
    permission: { external_directory: Object.fromEntries(o.dirs.map((d) => [join(d, '*'), 'allow'])) },
  });
}

/** What an Ask AgentK run keeps of the user's own OpenCode config: which model it answers on, and the providers that serve it. */
const ASK_KEPT_KEYS = ['model', 'small_model', 'provider', 'enabled_providers', 'disabled_providers'];

/**
 * The global config of one Ask AgentK run, which takes the place of the user's (`openCodeAskEnv`):
 * the `agentk-ask` agent, with AgentK's instructions and every tool denied, so it has nothing to act
 * with, and of the user's config only the model and providers (`own`), so their plugins (hooks) and
 * MCP servers are left out.
 */
export function openCodeAskConfig(prompt: string, own: Json = readOpenCodeConfig()): string {
  return JSON.stringify({
    ...Object.fromEntries(ASK_KEPT_KEYS.filter((k) => k in own).map((k) => [k, own[k]])),
    agent: { [OPENCODE_ASK_AGENT]: { description: "AgentK's Ask AgentK: answers in JSON, with no tools.", mode: 'primary', prompt, permission: 'deny' } },
  });
}

/**
 * Writes an Ask AgentK run's config into `cwd` and returns the variables the run (and deleting its
 * session) takes: OpenCode's global config read from there instead of the user's, no project config,
 * and no Claude Code files (CLAUDE.md, skills). Its sign-ins and sessions, in its data folder, stay.
 */
export function openCodeAskEnv(cwd: string, prompt: string): Record<string, string> {
  const vars = openCodeAskVars(cwd);
  mkdirSync(join(vars.XDG_CONFIG_HOME, 'opencode'), { recursive: true });
  writeFileSync(join(vars.XDG_CONFIG_HOME, 'opencode', 'opencode.json'), openCodeAskConfig(prompt));
  return vars;
}

/** The variables of an Ask AgentK run in `cwd` (`openCodeAskEnv`), without writing its config. */
export function openCodeAskVars(cwd: string): Record<string, string> & { XDG_CONFIG_HOME: string } {
  return { XDG_CONFIG_HOME: join(cwd, 'opencode-config'), OPENCODE_DISABLE_PROJECT_CONFIG: '1', OPENCODE_DISABLE_CLAUDE_CODE: '1' };
}

/** Writes a session's instructions (its system-prompt lines), read by OpenCode at the start. */
export function writeOpenCodeInstructions(paths: Paths, sessionId: string, text: string): string {
  const dir = openCodeSessionDir(paths, sessionId);
  mkdirSync(dir, { recursive: true });
  const file = join(dir, 'system.md');
  writeFileSync(file, text + '\n');
  return file;
}

/** Removes a session's folder (its project is gone, so it never resumes). */
export function removeOpenCodeSession(paths: Paths, sessionId: string): void {
  rmSync(openCodeSessionDir(paths, sessionId), { recursive: true, force: true });
}

type Json = Record<string, unknown>;
const isObject = (v: unknown): v is Json => !!v && typeof v === 'object' && !Array.isArray(v);

/** JSON with comments and trailing commas (OpenCode's `.jsonc`) as JSON. */
export function stripJsonc(text: string): string {
  let out = '';
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (c === '"') {
      let j = i + 1;
      while (j < text.length && text[j] !== '"') j += text[j] === '\\' ? 2 : 1;
      out += text.slice(i, j + 1);
      i = j;
    } else if (c === '/' && text[i + 1] === '/') {
      while (i < text.length && text[i] !== '\n') i++;
      out += '\n';
    } else if (c === '/' && text[i + 1] === '*') {
      const end = text.indexOf('*/', i + 2);
      i = end < 0 ? text.length : end + 1;
    } else {
      out += c;
    }
  }
  return out.replace(/,(\s*[}\]])/g, '$1');
}

/** OpenCode's global config folder: `$XDG_CONFIG_HOME/opencode`, else `~/.config/opencode`. */
export function openCodeConfigDir(env: NodeJS.ProcessEnv = process.env, home = homedir()): string {
  return join(env.XDG_CONFIG_HOME || join(home, '.config'), 'opencode');
}

/** The user's own global OpenCode config: its files' top-level keys, later files over earlier ones (unreadable ones left out). */
export function readOpenCodeConfig(dir = openCodeConfigDir()): Json {
  let out: Json = {};
  for (const name of ['config.json', 'opencode.json', 'opencode.jsonc']) {
    const file = join(dir, name);
    if (!existsSync(file)) continue;
    try {
      const cfg = JSON.parse(stripJsonc(readFileSync(file, 'utf8'))) as unknown;
      if (isObject(cfg)) out = { ...out, ...cfg, ...(isObject(out.mcp) && isObject(cfg.mcp) ? { mcp: { ...out.mcp, ...cfg.mcp } } : {}) };
    } catch {
      // A file OpenCode can't read either.
    }
  }
  return out;
}

/** Whether the user's own OpenCode config has an MCP server named `meko`, which AgentK then leaves alone. */
export function openCodeHasOwnMeko(dir = openCodeConfigDir()): boolean {
  const cfg = readOpenCodeConfig(dir);
  return isObject(cfg.mcp) && 'meko' in cfg.mcp;
}

/**
 * Writes the config file with AgentK's Meko as a remote MCP server named `meko` (owner-only, since
 * it holds the key), for `OPENCODE_CONFIG`: in a file, not the session's environment, so the
 * commands OpenCode runs never see the key. Rewritten only when it changes.
 */
export function writeOpenCodeMeko(paths: Paths, url: string, apiKey: string): string {
  const file = openCodeMekoFile(paths);
  const text = JSON.stringify({ mcp: { meko: { type: 'remote', url, headers: { Authorization: `Bearer ${apiKey}` }, oauth: false } } }, null, 2) + '\n';
  mkdirSync(openCodeDir(paths), { recursive: true });
  let old: string | null = null;
  try {
    old = readFileSync(file, 'utf8');
  } catch {
    // Not written yet.
  }
  if (old !== text) writeFileSync(file, text, { mode: 0o600 });
  return file;
}

export function removeOpenCodeMeko(paths: Paths): void {
  rmSync(openCodeMekoFile(paths), { force: true });
}

/** One finished response of an OpenCode session, from its `messages.jsonl`. */
export interface OpenCodeMessage {
  id: string;
  /** Its OpenCode session: the main one, or a subagent's. */
  session: string;
  model: string;
  at: number;
  input: number;
  /** Output and reasoning tokens, both billed as output. */
  output: number;
  cacheRead: number;
  cacheWrite: number;
}

const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : 0);
const ID = /^[\w-]{1,100}$/;

/** The responses in a `messages.jsonl` text; a line still being written, or one that isn't one, is skipped. */
export function parseOpenCodeMessages(text: string): OpenCodeMessage[] {
  const out: OpenCodeMessage[] = [];
  for (const line of text.split('\n')) {
    if (!line.trim()) continue;
    let m: unknown;
    try {
      m = JSON.parse(line);
    } catch {
      continue;
    }
    if (!isObject(m) || typeof m.id !== 'string' || !ID.test(m.id) || typeof m.model !== 'string' || !m.model) continue;
    out.push({
      id: m.id, session: typeof m.session === 'string' ? m.session : '', model: m.model, at: num(m.at) || Date.now(),
      input: num(m.input), output: num(m.output) + num(m.reasoning), cacheRead: num(m.cacheRead), cacheWrite: num(m.cacheWrite),
    });
  }
  return out;
}

/** Whether a session's usage log has a response of OpenCode session `id`. */
export function openCodeLoggedSession(file: string, id: string): boolean {
  if (!ID.test(id)) return false;
  try {
    return readFileSync(file, 'utf8').includes(`"session":${JSON.stringify(id)}`);
  } catch {
    return false;
  }
}

/** OpenCode's data folder (its sign-ins and session store): `$XDG_DATA_HOME/opencode`, else `~/.local/share/opencode`. */
export function openCodeDataDir(env: NodeJS.ProcessEnv = process.env, home = homedir()): string {
  return join(env.XDG_DATA_HOME || join(home, '.local', 'share'), 'opencode');
}

/**
 * Whether OpenCode still has session `id`: a row in its SQLite store (`opencode.db`, or a release
 * channel's `opencode-<channel>.db`), read only. Null when no store could be read (none there, or a
 * schema this doesn't know), for the caller to decide.
 */
export function openCodeStoredSession(id: string, dir = openCodeDataDir()): boolean | null {
  if (!ID.test(id)) return false;
  let files: string[];
  try {
    files = readdirSync(dir).filter((f) => /^opencode(?:-[\w.-]+)?\.db$/.test(f));
  } catch {
    return null;
  }
  let read = false;
  for (const f of files) {
    let db: DatabaseSync | null = null;
    try {
      db = new DatabaseSync(join(dir, f), { readOnly: true });
      const row = db.prepare('SELECT 1 AS ok FROM session WHERE id = ?').get(id);
      read = true;
      if (row) return true;
    } catch {
      // Another schema, or a store being replaced: another file may still say.
    } finally {
      db?.close();
    }
  }
  return read ? false : null;
}

/**
 * Whether OpenCode can resume session `id` (`--session` exits 1 on one it doesn't have): its store
 * has it, or, when the store can't be read, the session's usage log has a response of it.
 */
export function openCodeCanResume(log: string, id: string, dataDir = openCodeDataDir()): boolean {
  return openCodeStoredSession(id, dataDir) ?? openCodeLoggedSession(log, id);
}

/**
 * The answer in what `opencode run --format json` printed: its text parts joined, its session id
 * (to delete it), and the first error it reported.
 */
export function readOpenCodeRun(stdout: string): { text: string; sessionId: string | null; error: string | null } {
  const texts: string[] = [];
  let sessionId: string | null = null;
  let error: string | null = null;
  for (const line of stdout.split('\n')) {
    if (!line.trim()) continue;
    let e: unknown;
    try {
      e = JSON.parse(line);
    } catch {
      continue;
    }
    if (!isObject(e)) continue;
    if (!sessionId && typeof e.sessionID === 'string' && ID.test(e.sessionID)) sessionId = e.sessionID;
    if (e.type === 'text' && isObject(e.part) && typeof e.part.text === 'string') texts.push(e.part.text);
    if (e.type === 'error' && !error && isObject(e.error)) {
      const data = isObject(e.error.data) ? e.error.data : {};
      error = typeof data.message === 'string' ? data.message : typeof e.error.name === 'string' ? e.error.name : 'an error';
    }
  }
  return { text: texts.join('\n'), sessionId, error };
}

/** The session deletes still running, for `openCodeDeletesSettled`. */
const deleting = new Set<Promise<void>>();

/** Deletes one of OpenCode's sessions (an Ask AgentK run's, with the run's variables), in the background; never throws. */
export function deleteOpenCodeSession(command: string, id: string, cwd: string, env: Record<string, string> = {}): void {
  if (!ID.test(id)) return;
  const done = new Promise<void>((resolve) => {
    execFile(command, ['session', 'delete', id], { cwd, timeout: 15_000, env: { ...process.env, ...env } }, () => resolve()).stdin?.end();
  });
  deleting.add(done);
  void done.then(() => deleting.delete(done));
}

/** Resolves once no session delete is running (including ones started meanwhile). Never throws. */
export async function openCodeDeletesSettled(): Promise<void> {
  while (deleting.size) await Promise.all(deleting);
}
