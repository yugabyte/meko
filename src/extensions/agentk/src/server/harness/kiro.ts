import { execFile } from 'node:child_process';
import { existsSync } from 'node:fs';
import { join } from 'node:path';
import { HARNESS_CAPS, versionAtLeast } from '../../shared/harnesses.js';
import { kiroAsker } from '../askagentk.js';
import { contextScriptPath, hookScriptPath } from '../claude.js';
import type { Config } from '../config.js';
import { run } from '../db.js';
import type { HookPayload } from '../hooks.js';
import {
  AGENTK_AGENT, AGENTK_TRUSTED_AGENT, agentkAgent, builtinKiroAgent, exportBuiltinKiroAgent, findKiroSession, kiroDefaultAgentName, kiroGuardScriptPath,
  kiroHome, kiroSessionFile, kiroSessionLog, kiroTurns, ownKiroAgentExists, parseKiroModels, readKiroAgent, removeOwnKiroAgent, writeKiroAgent,
  writeKiroGuardScript, type KiroHooks,
} from '../kiro.js';
import { kiroMekoServerNames } from '../mekokiro.js';
import { kiroWorkspaceConfig } from '../headtrust.js';
import { shq } from '../launcher.js';
import { errMsg, type Logger } from '../log.js';
import { claudePath, type Paths } from '../paths.js';
import { HttpError } from '../projects.js';
import type { SessionRow } from '../store.js';
import { TOOLS_SERVER } from '../tooldefs.js';
import { kiroToolsServer, skillPath, toolsReady } from '../tools.js';
import { ingestKiroMekoCalls, recordKiroTurns } from '../usage.js';
import type { Harness } from './types.js';
import { cliState, cliVersion } from './version.js';

/** How long reading Kiro's sign-in or model list may take. */
const MODELS_TIMEOUT_MS = 15_000;
/** How long `kiro-cli update` may run before it's stopped. */
const UPDATE_TIMEOUT_MS = 5 * 60 * 1000;
/** Kiro's file-writing tool, by the name its hook matchers take (Kiro 2.32 calls it `write` and keeps this alias). */
const WRITE_TOOLS = ['write', 'fs_write'];
/** The copy of the `agentk` agent AgentK last wrote, in its run folder (`writeKiroAgent`'s `written`). */
const KIRO_AGENT_WRITTEN = 'kiro-agentk.json';
/** The same for `agentk-trusted`. */
const KIRO_TRUSTED_AGENT_WRITTEN = 'kiro-agentk-trusted.json';
/** The first Kiro whose engine ignores `--trust-tools`, so `[kiro] trust_tools` runs it as `agentk-trusted`. */
const TRUST_BY_AGENT_VERSION = '3.0.0';
/** Hook commands must never hold Kiro up for long. */
const HOOK_TIMEOUT_MS = 5000;

/**
 * Kiro's hook events as AgentK's (Claude Code's hook names). Kiro 2 sends its triggers' names
 * (`agentSpawn`); Kiro 3.0 sends Claude Code's (`SessionStart`) for the same triggers.
 */
const EVENTS: Record<string, string> = {
  agentSpawn: 'SessionStart', userPromptSubmit: 'UserPromptSubmit', preToolUse: 'PreToolUse', postToolUse: 'PostToolUse', stop: 'Stop',
  SessionStart: 'SessionStart', UserPromptSubmit: 'UserPromptSubmit', PreToolUse: 'PreToolUse', PostToolUse: 'PostToolUse', Stop: 'Stop',
};

/**
 * AgentK's hooks in the `agentk` agent: the forwarder on every event it follows, the context at
 * start, the guard on writes, and the forwarder before each call to a Meko server (`mekoServers`,
 * matched as `@<name>`, Kiro's matcher for every tool of an MCP server), for the dot's Meko m.
 */
export function agentkHooks(paths: Paths, mekoServers: string[] = ['meko']): KiroHooks {
  const cmd = (file: string) => shq(claudePath(file));
  const hook = { command: cmd(hookScriptPath(paths)), timeout_ms: HOOK_TIMEOUT_MS };
  return {
    agentSpawn: [hook, { command: cmd(contextScriptPath(paths)), timeout_ms: HOOK_TIMEOUT_MS }],
    userPromptSubmit: [hook],
    preToolUse: [
      { matcher: 'fs_write', command: cmd(kiroGuardScriptPath(paths)), timeout_ms: HOOK_TIMEOUT_MS },
      ...mekoServers.map((name) => ({ matcher: `@${name}`, ...hook })),
    ],
    postToolUse: [{ matcher: '*', ...hook }],
    stop: [hook],
  };
}

/**
 * Whether Kiro sessions run as `agentk-trusted`: true with `[kiro] trust_tools` on a Kiro whose
 * engine ignores `--trust-tools` (3.0 or later, by `version`, the last one read); false with it off
 * or on an older Kiro; null with it on and no version read yet (or every read failed), when the
 * trusted agent is neither written nor removed, and a launch runs as it while it's there.
 */
export function kiroTrustByAgent(cfg: Config, version: string | null): boolean | null {
  if (!cfg.kiro.trustTools) return false;
  return version ? versionAtLeast(version, TRUST_BY_AGENT_VERSION) : null;
}

/**
 * Writes the `agentk` agent from the user's default agent (Meko's `meko-default` when Meko's setup
 * for Kiro ran, else Kiro's built-in one) with AgentK's hooks added, Meko's capture hooks only
 * with `[meko] capture_transcripts`, and AgentK's session tools while `[tools] enabled` (one file for
 * every session, so a session on an untrusted PR head has them too; the tools' route refuses it).
 * With `trusted` (`kiroTrustByAgent`) true, also `agentk-trusted`, the same with every tool allowed;
 * false removes it, so a session already running as it falls back to Kiro's default agent and asks
 * again; null leaves it as it is. Returns what changed, as lines for the log.
 */
export function writeAgentkAgent(paths: Paths, cfg: Config, trusted: boolean | null = false): string[] {
  const name = kiroDefaultAgentName();
  const base = (name && name !== AGENTK_AGENT && name !== AGENTK_TRUSTED_AGENT ? readKiroAgent(name) : null) ?? builtinKiroAgent(paths);
  const tools = toolsReady(paths, cfg.tools.enabled)
    ? { name: TOOLS_SERVER, server: kiroToolsServer(paths), skill: claudePath(skillPath(paths)) }
    : null;
  const hooks = agentkHooks(paths, kiroMekoServerNames(base.mcpServers));
  const o = { mekoCapture: cfg.meko.captureTranscripts, tools };
  const changes: string[] = [];
  if (writeKiroAgent(agentkAgent(base, hooks, o), kiroHome(), join(paths.run, KIRO_AGENT_WRITTEN))) changes.push(`wrote Kiro's ${AGENTK_AGENT} agent`);
  if (trusted && writeKiroAgent(agentkAgent(base, hooks, { ...o, trustAll: true }), kiroHome(), join(paths.run, KIRO_TRUSTED_AGENT_WRITTEN))) {
    changes.push(`wrote Kiro's ${AGENTK_TRUSTED_AGENT} agent`);
  }
  if (trusted === false && dropTrustedAgent(paths)) changes.push(`removed Kiro's ${AGENTK_TRUSTED_AGENT} agent`);
  return changes;
}

/** Removes `agentk-trusted` while it's AgentK's. True when it removed it. */
function dropTrustedAgent(paths: Paths): boolean {
  return removeOwnKiroAgent(AGENTK_TRUSTED_AGENT, kiroHome(), join(paths.run, KIRO_TRUSTED_AGENT_WRITTEN));
}

/**
 * The command line of a Kiro session: as `agent` (`agentk` by default, `agentk-trusted` by `kiroTrustByAgent`), its own
 * conversation resumed by id (never `--resume`, Kiro's newest in the folder, which can be an earlier
 * session's in a reused worktree). `trustTools` (`[kiro] trust_tools`) trusts every tool for this
 * run only, so nothing in Kiro's home grants it to `agentk`: `--trust-tools=*`, not
 * `--trust-all-tools`, which makes Kiro ask on screen whether to go on with "No, exit" first (a
 * typed prompt's Enter would pick it). Kiro 3.0's engine ignores `--trust-tools`, so there the
 * launch is also as `agentk-trusted`; the flag stays for Kiro 3.0 on its older engine.
 */
export function kiroArgs(o: { agent?: string; conversation: string | null; trustTools: boolean; prompt: string | null; model?: string | null }): string[] {
  const args = ['chat', '--agent', o.agent ?? AGENTK_AGENT];
  if (o.trustTools) args.push('--trust-tools=*');
  // Every launch, a resume's too, so the session stays on the model picked at its start.
  if (o.model) args.push('--model', o.model);
  if (o.conversation) args.push('--resume-id', o.conversation);
  // Not after "--": with --agent, Kiro 2.32 then drops the first prompt. A leading space keeps a
  // prompt that starts with "-" (such as "---" front matter) from being read as an option.
  if (o.prompt) args.push(o.prompt.startsWith('-') ? ` ${o.prompt}` : o.prompt);
  return args;
}

/** A one-shot Kiro command's output, or null when it failed or ran out of time. */
function kiroOutput(command: string, args: string[]): Promise<string | null> {
  return new Promise((resolve) => {
    execFile(command, args, { timeout: MODELS_TIMEOUT_MS, env: { ...process.env, NO_COLOR: '1' } }, (err, stdout) => resolve(err ? null : String(stdout)))
      .stdin?.end();
  });
}

/**
 * The models Kiro offers this sign-in (`chat --list-models`). Asked only once `whoami` says someone
 * is signed in: signed out, the list starts Kiro's sign-in and opens a browser.
 */
export async function kiroModels(command: string): Promise<string[]> {
  if ((await kiroOutput(command, ['whoami'])) === null) return [];
  const out = await kiroOutput(command, ['chat', '--list-models', '-f', 'json']);
  return out === null ? [] : parseKiroModels(out);
}

/** The session's own Kiro conversation, while Kiro still has it: the id a hook gave, or `findConversation` found. */
function kiroConversation(row: SessionRow): string | null {
  return row.agent_session_id && existsSync(kiroSessionFile(row.agent_session_id)) ? row.agent_session_id : null;
}

/** A Meko tool as Kiro names it: `@<server>/<tool>`, from a server named `meko` or starting with it. */
const MEKO_TOOL = /^@meko[\w-]*\/[\w-]+$/i;

/**
 * The key of the Meko call a tool event is about: its tool name. Kiro gives a call no id, so two
 * calls of one tool share it, and `MekoCalls` counts them.
 */
export function kiroMekoTool(p: HookPayload): string | null {
  return typeof p.tool_name === 'string' && MEKO_TOOL.test(p.tool_name) ? p.tool_name : null;
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
  const cli = cliState({ id: 'kiro', changed: deps.changed, readVersion: deps.readVersion });
  const version = cli.version;
  const readVersion = deps.readVersion ?? cliVersion;
  /**
   * The last version any read gave, for the command it was read for: kept through a read that
   * fails, and through Kiro being turned off for new sessions (its sessions still resume).
   */
  let known: { command: string; version: string } | null = null;
  const remember = (cfg: Config, v: string | null) => {
    if (v) known = { command: cfg.kiro.command, version: v };
    return v;
  };
  const knownVersion = (cfg: Config) => (known?.command === cfg.kiro.command ? known.version : null);
  const trustByAgent = (cfg: Config) => kiroTrustByAgent(cfg, knownVersion(cfg));
  /** The agent a launch now runs as: `agentk-trusted` while it's there when the version isn't known. */
  const launchAgent = (cfg: Config) => ((trustByAgent(cfg) ?? ownKiroAgentExists(AGENTK_TRUSTED_AGENT)) ? AGENTK_TRUSTED_AGENT : AGENTK_AGENT);
  const refreshAgent = (paths: Paths, cfg: Config) => {
    try {
      for (const line of writeAgentkAgent(paths, cfg, trustByAgent(cfg))) deps.log.info(line);
    } catch (e) {
      deps.log.warn(`couldn't write Kiro's ${AGENTK_AGENT} agent; Kiro sessions start without AgentK's hooks`, errMsg(e));
    }
  };

  return {
    id: 'kiro',
    caps: HARNESS_CAPS.kiro,
    systemPromptByContext: true,
    async prepare(paths, cfg) {
      writeKiroGuardScript(paths);
      // With trust_tools off (Settings runs this when it changes), nothing runs as agentk-trusted any
      // more, a chat started by hand included, whether Kiro is on for new sessions or not.
      try {
        if (!cfg.kiro.trustTools && dropTrustedAgent(paths)) deps.log.info(`removed Kiro's ${AGENTK_TRUSTED_AGENT} agent`);
      } catch (e) {
        deps.log.warn(`couldn't remove Kiro's ${AGENTK_TRUSTED_AGENT} agent`, errMsg(e));
      }
      // Only where Kiro is installed: AgentK never makes a ~/.kiro of its own.
      if (!remember(cfg, await version(cfg))) return;
      if (!kiroDefaultAgentName()) await exportBuiltinKiroAgent(cfg.kiro.command, paths);
      refreshAgent(paths, cfg);
    },
    // The version first (kept 10 minutes), so the launcher picks the agent by the Kiro it'll run.
    // Turned off for new sessions, Kiro still runs its sessions' resumes: read it all the same.
    beforeLaunch: async ({ paths, cfg }) => {
      remember(cfg, cfg.kiro.enabled ? await version(cfg) : knownVersion(cfg) ?? await readVersion(cfg.kiro.command));
      refreshAgent(paths, cfg);
    },
    launcher(paths, cfg, l) {
      // Kiro has no flag to leave a workspace's agents and MCP servers out: refuse rather than run a PR's.
      const agent = launchAgent(cfg);
      const own = l.untrusted ? kiroWorkspaceConfig(l.row.worktree_path, agent) : [];
      if (own.length) {
        const what = own.map((f) => (f.endsWith('mcp.json') ? `the MCP servers in ${f}` : `the agent in ${f} in place of AgentK's`));
        throw new HttpError(400, `Kiro would run this PR's own config (${what.join(', and ')}), and anyone can have written `
          + "this PR's code. Review it on Claude Code instead, which can leave the repo's config out.");
      }
      return {
        sessionId: l.row.id,
        worktree: l.row.worktree_path,
        command: cfg.kiro.command,
        args: kiroArgs({ agent, conversation: l.resume ? kiroConversation(l.row) : null, trustTools: cfg.kiro.trustTools, prompt: l.prompt, model: l.model }),
        // Kiro picks its theme once, at its start, from this before its guesses (COLORFGBG, then
        // asking the terminal, then the terminal's variables, then the OS). Its own /theme still wins.
        env: { ...l.env, KIRO_TERMINAL_THEME: l.termTheme },
        unsetEnv: l.unsetEnv,
      };
    },
    models: (cfg) => kiroModels(cfg.kiro.command),
    normalize: normalizeKiro,
    // Kiro's guard script exits 2 with this on stderr.
    deny: (reason) => reason,
    // agentSpawn's stdout goes into the model's context as it is.
    context: (text) => text,
    mekoTool: kiroMekoTool,
    editedPaths(p) {
      if (!WRITE_TOOLS.includes(p.tool_name ?? '')) return null;
      const path = p.tool_input?.path;
      return typeof path === 'string' && path ? [path] : [];
    },
    // The newest Kiro started in the worktree since the session was created: the session manager
    // asks only while the session owns the worktree, so a later session's conversation is never taken.
    findConversation: (row) => findKiroSession([row.worktree_path, claudePath(row.worktree_path)], row.created_at),
    canResume: (row) => !!kiroConversation(row),
    activityFiles(row) {
      const conv = kiroConversation(row);
      return conv ? [kiroSessionFile(conv), kiroSessionLog(conv)] : [];
    },
    // Its finished turns (credits, tokens) and its context fill after the last one, from its session
    // file, and its Meko calls with their results, from its session log.
    readUsage(db, row) {
      const conv = kiroConversation(row);
      if (!conv) return { changed: false };
      const turns = kiroTurns(conv);
      let changed = false;
      try {
        changed = recordKiroTurns(db, row.id, conv, turns);
        // Marks the session as read, so backfillUsage leaves it alone at the next start.
        run(db, `INSERT INTO usage_files(session_id, path, offset) VALUES (?, ?, ?)
                 ON CONFLICT(session_id, path) DO UPDATE SET offset = excluded.offset`, row.id, kiroSessionFile(conv), turns.length);
      } catch (e) {
        // The context fill from the same read still counts.
        deps.log.warn(`reading Kiro's usage for ${row.id} failed`, errMsg(e));
      }
      try {
        // Not `changed ||= …`, which would skip the read once the turns changed something.
        if (ingestKiroMekoCalls(db, row.id, kiroSessionLog(conv)).changed) changed = true;
      } catch (e) {
        deps.log.warn(`reading Kiro's Meko calls for ${row.id} failed`, errMsg(e));
      }
      return { changed, contextPercent: turns.at(-1)?.contextPercent };
    },
    ask: kiroAsker,
    status: cli.status,
    version,
    update: (cfg) => cli.update(cfg, (command) => new Promise<void>((done) => {
      execFile(command, ['update', '--non-interactive'], { timeout: UPDATE_TIMEOUT_MS, env: { ...process.env, NO_COLOR: '1' } }, () => done()).stdin?.end();
    })),
  };
}
