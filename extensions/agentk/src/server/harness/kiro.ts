import { execFile } from 'node:child_process';
import { existsSync } from 'node:fs';
import { HARNESS_CAPS } from '../../shared/harnesses.js';
import { kiroAsker } from '../askagentk.js';
import { contextScriptPath, hookScriptPath } from '../claude.js';
import type { Config } from '../config.js';
import { run } from '../db.js';
import type { HookPayload } from '../hooks.js';
import {
  AGENTK_AGENT, agentkAgent, builtinKiroAgent, exportBuiltinKiroAgent, findKiroSession, kiroDefaultAgentName, kiroGuardScriptPath,
  kiroSessionFile, kiroSessionLog, kiroTurns, readKiroAgent, writeKiroAgent, writeKiroGuardScript, type KiroHooks,
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
import { cliState } from './version.js';

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
 * Writes the `agentk` agent from the user's default agent (Meko's `meko-default` when Meko's setup
 * for Kiro ran, else Kiro's built-in one) with AgentK's hooks added, Meko's capture hooks only
 * with `[meko] capture_transcripts`, and AgentK's session tools while `[tools] enabled` (one file for
 * every session, so a session on an untrusted PR head has them too; the tools' route refuses it).
 * True when it changed.
 */
export function writeAgentkAgent(paths: Paths, cfg: Config): boolean {
  const name = kiroDefaultAgentName();
  const base = (name && name !== AGENTK_AGENT ? readKiroAgent(name) : null) ?? builtinKiroAgent(paths);
  const tools = toolsReady(paths, cfg.tools.enabled)
    ? { name: TOOLS_SERVER, server: kiroToolsServer(paths), skill: claudePath(skillPath(paths)) }
    : null;
  const hooks = agentkHooks(paths, kiroMekoServerNames(base.mcpServers));
  return writeKiroAgent(agentkAgent(base, hooks, { mekoCapture: cfg.meko.captureTranscripts, tools }));
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
    systemPromptByContext: true,
    async prepare(paths, cfg) {
      writeKiroGuardScript(paths);
      // Only where Kiro is installed: AgentK never makes a ~/.kiro of its own.
      if (!(await version(cfg))) return;
      if (!kiroDefaultAgentName()) await exportBuiltinKiroAgent(cfg.kiro.command, paths);
      refreshAgent(paths, cfg);
    },
    beforeLaunch: async ({ paths, cfg }) => refreshAgent(paths, cfg),
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
