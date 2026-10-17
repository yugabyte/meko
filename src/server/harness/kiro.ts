import { execFile } from 'node:child_process';
import { existsSync } from 'node:fs';
import { HARNESS_CAPS } from '../../shared/harnesses.js';
import type { HarnessStatus } from '../../shared/types.js';
import { claudeVersion, contextScriptPath, hookScriptPath, versionAtLeast } from '../claude.js';
import type { Config } from '../config.js';
import type { HookPayload } from '../hooks.js';
import {
  AGENTK_AGENT, MIN_KIRO, agentkAgent, builtinKiroAgent, exportBuiltinKiroAgent, findKiroSession, kiroDefaultAgentName, kiroGuardScriptPath,
  kiroSessionFile, readKiroAgent, writeKiroAgent, writeKiroGuardScript, type KiroHooks,
} from '../kiro.js';
import { kiroWorkspaceConfig } from '../headtrust.js';
import { shq } from '../launcher.js';
import { errMsg, type Logger } from '../log.js';
import { claudePath, type Paths } from '../paths.js';
import { HttpError } from '../projects.js';
import type { SessionRow } from '../store.js';
import type { Harness } from './types.js';

/** How long a version read is used before it's read again (Kiro updates itself). */
const VERSION_TTL_MS = 10 * 60 * 1000;
/** How long `kiro-cli update` may run before it's stopped. */
const UPDATE_TIMEOUT_MS = 5 * 60 * 1000;
/** Kiro's file-writing tool, by the name its hook matchers take (Kiro 2.32 calls it `write` and keeps this alias). */
const WRITE_TOOLS = ['write', 'fs_write'];
/** Hook commands must never hold Kiro up for long. */
const HOOK_TIMEOUT_MS = 5000;

/** Kiro's hook events as AgentK's (Claude Code's hook names). */
const EVENTS: Record<string, string> = {
  agentSpawn: 'SessionStart', userPromptSubmit: 'UserPromptSubmit', preToolUse: 'PreToolUse', postToolUse: 'PostToolUse', stop: 'Stop',
};

/** AgentK's hooks in the `agentk` agent: the forwarder on every event it follows, the context at start, the guard on writes. */
export function agentkHooks(paths: Paths): KiroHooks {
  const cmd = (file: string) => shq(claudePath(file));
  const hook = { command: cmd(hookScriptPath(paths)), timeout_ms: HOOK_TIMEOUT_MS };
  return {
    agentSpawn: [hook, { command: cmd(contextScriptPath(paths)), timeout_ms: HOOK_TIMEOUT_MS }],
    userPromptSubmit: [hook],
    preToolUse: [{ matcher: 'fs_write', command: cmd(kiroGuardScriptPath(paths)), timeout_ms: HOOK_TIMEOUT_MS }],
    postToolUse: [{ matcher: '*', ...hook }],
    stop: [hook],
  };
}

/**
 * Writes the `agentk` agent from the user's default agent (Meko's `meko-default` when Meko's setup
 * for Kiro ran, else Kiro's built-in one) with AgentK's hooks added, and Meko's capture hooks only
 * with `[meko] capture_transcripts`. True when it changed.
 */
export function writeAgentkAgent(paths: Paths, cfg: Config): boolean {
  const name = kiroDefaultAgentName();
  const base = (name && name !== AGENTK_AGENT ? readKiroAgent(name) : null) ?? builtinKiroAgent(paths);
  return writeKiroAgent(agentkAgent(base, agentkHooks(paths), { mekoCapture: cfg.meko.captureTranscripts }));
}

/**
 * The command line of a Kiro session: as the `agentk` agent, its own conversation resumed by id
 * (never `--resume`, Kiro's newest in the folder, which can be an earlier session's in a reused
 * worktree). `trustTools` (`[kiro] trust_tools`) trusts every tool for this run only, so nothing in
 * Kiro's home grants it: `--trust-tools=*`, not `--trust-all-tools`, which makes Kiro 2.32 ask on
 * screen whether to go on with "No, exit" first (a typed prompt's Enter would pick it).
 */
export function kiroArgs(o: { conversation: string | null; trustTools: boolean; prompt: string | null }): string[] {
  const args = ['chat', '--agent', AGENTK_AGENT];
  if (o.trustTools) args.push('--trust-tools=*');
  if (o.conversation) args.push('--resume-id', o.conversation);
  // Not after "--": with --agent, Kiro 2.32 then drops the first prompt. A leading space keeps a
  // prompt that starts with "-" (such as "---" front matter) from being read as an option.
  if (o.prompt) args.push(o.prompt.startsWith('-') ? ` ${o.prompt}` : o.prompt);
  return args;
}

/** The session's own Kiro conversation, while Kiro still has it: the id a hook gave, or `findKiroConversation` found. */
export function kiroConversation(row: SessionRow): string | null {
  return row.agent_session_id && existsSync(kiroSessionFile(row.agent_session_id)) ? row.agent_session_id : null;
}

/**
 * The Kiro conversation a session that has none yet (no hook said its id) started in its worktree:
 * the newest Kiro started there since the session was created. Callers ask only while the session
 * owns the worktree, and keep what it finds, so a later session's conversation is never taken.
 */
export function findKiroConversation(row: SessionRow): string | null {
  return findKiroSession([row.worktree_path, claudePath(row.worktree_path)], row.created_at);
}

/** A Kiro hook payload as AgentK's event: Kiro's id moves to `agent_session_id`, since `session_id` is Claude Code's. */
export function normalizeKiro(raw: HookPayload): HookPayload | null {
  const ev = EVENTS[String(raw.hook_event_name ?? '')];
  if (!ev) return null;
  const { session_id: id, ...rest } = raw;
  return { ...rest, hook_event_name: ev, ...(typeof id === 'string' ? { agent_session_id: id } : {}) };
}

/** Kiro CLI (`kiro-cli`). */
export function kiroHarness(deps: { log: Logger; changed: () => void; readVersion?: (cmd: string) => Promise<string | null> }): Harness {
  const readVersion = deps.readVersion ?? claudeVersion;
  let ver: { command: string; at: number; read: Promise<string | null> } | null = null;
  let value: string | null = null;
  let updating = false;

  const version = (cfg: Config, force = false): Promise<string | null> => {
    const command = cfg.kiro.command;
    if (!cfg.kiro.enabled) return Promise.resolve(null);
    const now = Date.now();
    if (force || !ver || ver.command !== command || now - ver.at > VERSION_TTL_MS) {
      const read = readVersion(command).then((v) => {
        if (ver?.read === read && v !== value) {
          value = v;
          deps.changed();
        }
        return v;
      });
      ver = { command, at: now, read };
    }
    return ver.read;
  };
  const status = (cfg: Config): HarnessStatus => {
    const v = cfg.kiro.enabled ? value : null;
    return {
      id: 'kiro', enabled: cfg.kiro.enabled, version: v, minVersion: MIN_KIRO, available: !!v && versionAtLeast(v, MIN_KIRO),
      isDefault: cfg.agents.default === 'kiro', updating,
    };
  };
  const refreshAgent = (paths: Paths, cfg: Config) => {
    try {
      if (writeAgentkAgent(paths, cfg)) deps.log.info(`wrote Kiro's ${AGENTK_AGENT} agent`);
    } catch (e) {
      deps.log.warn(`couldn't write Kiro's ${AGENTK_AGENT} agent; Kiro sessions start without AgentK's hooks`, errMsg(e));
    }
  };

  return {
    id: 'kiro',
    caps: HARNESS_CAPS.kiro,
    command: (cfg) => cfg.kiro.command,
    enabled: (cfg) => cfg.kiro.enabled,
    systemPromptByContext: true,
    async prepare(paths, cfg) {
      writeKiroGuardScript(paths);
      // Only where Kiro is installed: AgentK never makes a ~/.kiro of its own.
      if (!(await version(cfg))) return;
      if (!kiroDefaultAgentName()) await exportBuiltinKiroAgent(cfg.kiro.command, paths);
      refreshAgent(paths, cfg);
    },
    beforeLaunch: (paths, cfg) => refreshAgent(paths, cfg),
    launcher(paths, cfg, l) {
      // Kiro has no flag to leave a workspace's agents and MCP servers out: refuse rather than run a PR's.
      const own = l.untrusted ? kiroWorkspaceConfig(l.row.worktree_path, AGENTK_AGENT) : [];
      if (own.length) {
        const what = own.map((f) => (f.endsWith('mcp.json') ? `the MCP servers in ${f}` : `the agent in ${f} in place of AgentK's`));
        throw new HttpError(400, `Kiro would run this PR's own config (${what.join(', and ')}), and anyone can have written `
          + "this PR's code. Review it on Claude Code instead, which can leave the repo's config out.");
      }
      return {
        sessionId: l.row.id,
        worktree: l.row.worktree_path,
        command: cfg.kiro.command,
        args: kiroArgs({ conversation: l.resume ? kiroConversation(l.row) : null, trustTools: cfg.kiro.trustTools, prompt: l.prompt }),
        env: l.env,
        unsetEnv: l.unsetEnv,
      };
    },
    normalize: normalizeKiro,
    // Kiro's guard script exits 2 with this on stderr.
    deny: (reason) => reason,
    // agentSpawn's stdout goes into the model's context as it is.
    context: (text) => text,
    editedPaths(p) {
      if (!WRITE_TOOLS.includes(p.tool_name ?? '')) return null;
      const path = p.tool_input?.path;
      return typeof path === 'string' && path ? [path] : [];
    },
    canResume: (row) => !!kiroConversation(row),
    status,
    version,
    async update(cfg) {
      if (updating) throw new HttpError(409, 'Kiro is already being updated.');
      updating = true;
      deps.changed();
      try {
        await new Promise<void>((done) => {
          execFile(cfg.kiro.command, ['update', '--non-interactive'], { timeout: UPDATE_TIMEOUT_MS, env: { ...process.env, NO_COLOR: '1' } }, () => done()).stdin?.end();
        });
        await version(cfg, true);
      } finally {
        updating = false;
        deps.changed();
      }
      return status(cfg);
    },
  };
}
