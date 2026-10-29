import { execFile } from 'node:child_process';
import { HARNESS_CAPS } from '../../shared/harnesses.js';
import { openCodeAsker } from '../askagentk.js';
import { mekoConfigured, resolveMeko, type Config } from '../config.js';
import type { HookPayload } from '../hooks.js';
import { errMsg, type Logger } from '../log.js';
import {
  OPENCODE_NO_TRACKING_ENV, OPENCODE_SESSION_CLI_CONFIG, openCodeCanResume, openCodeCommand, openCodeHasOwnMeko, openCodeMessagesFile,
  openCodePluginPath, openCodeSessionConfig, openCodeSessionDir, removeOpenCodeMeko, removeOpenCodeSession, writeOpenCodeInstructions, writeOpenCodeMeko, writeOpenCodePlugin,
} from '../opencode.js';
import { serverMekoTool } from '../mekousage.js';
import { claudePath, type Paths } from '../paths.js';
import { openCodeTools } from '../tools.js';
import type { SessionRow } from '../store.js';
import { ingestOpenCodeUsage } from '../usage.js';
import type { Harness } from './types.js';
import { cliState } from './version.js';

/** How long `opencode upgrade` may run before it's stopped. */
const UPDATE_TIMEOUT_MS = 5 * 60 * 1000;
/** OpenCode's file-writing tools, as its plugin's `tool.execute.before` names them. */
const WRITE_TOOLS = ['edit', 'write'];
/** Its patch tool, whose files are named in the patch text. */
const PATCH_TOOLS = ['patch'];

/** The plugin's events (src/server/opencode.ts) as AgentK's (Claude Code's hook names). */
const EVENTS: Record<string, string> = {
  'agentk.start': 'SessionStart',
  'chat.message': 'UserPromptSubmit',
  'tool.execute.before': 'PreToolUse',
  'tool.execute.after': 'PostToolUse',
  'permission.asked': 'PermissionRequest',
  'permission.replied': 'PostToolUse',
  'session.idle': 'Stop',
};

/**
 * The command line of an OpenCode session: a server of its own (`--standalone`: OpenCode 2's shared
 * background service has the environment of whichever CLI started it, so it would never see the
 * session's variables, and would hand them to the user's own runs if AgentK started it), its own
 * session resumed by id (never `--continue`, OpenCode's newest in the folder, which can be an
 * earlier session's in a reused worktree), and the prompt to submit. `--opt=value`, so a prompt that
 * starts with "-" is never read as an option.
 */
export function openCodeArgs(o: { conversation: string | null; prompt: string | null }): string[] {
  const args = ['--standalone'];
  if (o.conversation) args.push(`--session=${o.conversation}`);
  if (o.prompt) args.push(`--prompt=${o.prompt}`);
  return args;
}

/** A plugin event as AgentK's: OpenCode's session id moves to `agent_session_id`, since `session_id` is Claude Code's. */
export function normalizeOpenCode(raw: HookPayload): HookPayload | null {
  const ev = EVENTS[String(raw.hook_event_name ?? '')];
  if (!ev) return null;
  const { session_id: id, tool, args, permission, ...rest } = raw;
  const name = typeof tool === 'string' ? tool : typeof permission === 'string' && permission ? permission : undefined;
  return {
    ...rest,
    hook_event_name: ev,
    ...(name ? { tool_name: name } : {}),
    ...(args && typeof args === 'object' ? { tool_input: args as Record<string, unknown> } : {}),
    ...(typeof id === 'string' ? { agent_session_id: id } : {}),
  };
}

/**
 * The key of the Meko call a tool event is about: the call's id and its tool (a call from code mode
 * can share its execute call's id), as the plugin sends them for a Meko server's tools.
 */
export function openCodeMekoTool(p: HookPayload): string | null {
  const tool = p.tool_name;
  return typeof tool === 'string' && serverMekoTool(tool) && typeof p.call_id === 'string' && p.call_id ? `${p.call_id}:${tool}` : null;
}

/** The files a patch (OpenCode's patch format) adds, changes, moves or deletes. */
export function patchPaths(text: string): string[] {
  const out: string[] = [];
  for (const m of text.matchAll(/^\*\*\* (?:Add|Update|Delete) File: (.+)$|^\*\*\* Move to: (.+)$/gm)) out.push((m[1] ?? m[2]).trim());
  return out;
}

/** The session's own OpenCode session, while OpenCode still has it to resume: the id the plugin gave. */
function conversation(paths: Paths, row: SessionRow): string | null {
  const id = row.agent_session_id;
  return id && openCodeCanResume(openCodeMessagesFile(paths, row.id), id) ? id : null;
}

/** OpenCode (`opencode`), its hooks from AgentK's plugin. */
export function openCodeHarness(deps: {
  paths: Paths; log: Logger; changed: () => void; readVersion?: (cmd: string) => Promise<string | null>;
  /** OpenCode has a `meko` MCP server of its own (`Harnesses.openCodeOwnMeko`); by default, one in its own config. */
  ownMeko?: () => boolean;
}): Harness {
  const ownMeko = deps.ownMeko ?? (() => openCodeHasOwnMeko());
  const command = (cfg: Config) => openCodeCommand(cfg.opencode.command);
  const cli = cliState({ id: 'opencode', changed: deps.changed, readVersion: deps.readVersion, command });
  const writePlugin = (paths: Paths) => {
    try {
      writeOpenCodePlugin(paths);
    } catch (e) {
      deps.log.warn("couldn't write AgentK's plugin for OpenCode; OpenCode sessions start without AgentK's hooks", errMsg(e));
    }
  };
  /**
   * AgentK's Meko for a launch: the config file with its MCP server, while Meko is on and OpenCode
   * has no `meko` server of its own (`ownMeko`); else null (and the file removed).
   */
  const mekoConfig = (paths: Paths, cfg: Config): string | null => {
    try {
      if (mekoConfigured(cfg) && !ownMeko()) {
        const { url, apiKey } = resolveMeko(cfg);
        return writeOpenCodeMeko(paths, url, apiKey);
      }
      removeOpenCodeMeko(paths);
    } catch (e) {
      deps.log.warn("couldn't write AgentK's Meko server for OpenCode; OpenCode sessions start without it", errMsg(e));
    }
    return null;
  };

  return {
    id: 'opencode',
    caps: HARNESS_CAPS.opencode,
    systemPromptByContext: false,
    async prepare(paths, cfg) {
      writePlugin(paths);
      await cli.version(cfg);
    },
    beforeLaunch: async ({ paths }) => writePlugin(paths),
    launcher(paths, cfg, l) {
      const conv = l.resume ? conversation(paths, l.row) : null;
      const instructions = writeOpenCodeInstructions(paths, l.row.id, l.systemPrompt);
      const meko = mekoConfig(paths, cfg);
      return {
        sessionId: l.row.id,
        worktree: l.row.worktree_path,
        command: command(cfg),
        args: openCodeArgs({ conversation: conv, prompt: l.prompt }),
        env: {
          ...l.env,
          AK_OPENCODE_DIR: claudePath(openCodeSessionDir(paths, l.row.id)),
          AK_OPENCODE_SOURCE: conv ? 'resume' : 'startup',
          ...(conv ? { AK_OPENCODE_SESSION: conv } : {}),
          OPENCODE_CONFIG_CONTENT: openCodeSessionConfig({
            plugin: claudePath(openCodePluginPath(paths)), instructions: claudePath(instructions), dirs: l.addDirs.map((d) => claudePath(d)),
            tools: l.tools ? openCodeTools(paths) : null,
            agents: l.review?.agents ?? null,
          }),
          OPENCODE_CLI_CONFIG_CONTENT: OPENCODE_SESSION_CLI_CONFIG,
          ...(meko ? { OPENCODE_CONFIG: claudePath(meko) } : {}),
          // A PR's own opencode.json and .opencode folders (plugins: code that would run) are left out.
          ...(l.untrusted ? { OPENCODE_DISABLE_PROJECT_CONFIG: '1' } : {}),
          // No telemetry, update check or free models ([opencode] disable_tracking).
          ...(cfg.opencode.disableTracking ? OPENCODE_NO_TRACKING_ENV : {}),
        },
        unsetEnv: l.unsetEnv,
      };
    },
    normalize: normalizeOpenCode,
    // The plugin throws this as the tool call's error.
    deny: (reason) => reason,
    // Recall comes back as text for the plugin to add to the system prompt.
    context: (text) => text,
    mekoTool: openCodeMekoTool,
    editedPaths(p) {
      const tool = p.tool_name ?? '';
      if (WRITE_TOOLS.includes(tool)) {
        // Relative to the worktree, or absolute.
        const path = p.tool_input?.path;
        return typeof path === 'string' && path ? [path] : [];
      }
      if (PATCH_TOOLS.includes(tool)) {
        const text = p.tool_input?.patchText;
        return typeof text === 'string' ? patchPaths(text) : [];
      }
      return null;
    },
    canResume: (row) => !!conversation(deps.paths, row),
    activityFiles: (row) => [openCodeMessagesFile(deps.paths, row.id)],
    forget: (row) => removeOpenCodeSession(deps.paths, row.id),
    mekoChanged: (paths) => removeOpenCodeMeko(paths),
    readUsage: (db, row) => ingestOpenCodeUsage(db, row.id, openCodeMessagesFile(deps.paths, row.id), row.agent_session_id),
    ask: openCodeAsker,
    status: cli.status,
    version: cli.version,
    update: (cfg) => cli.update(cfg, (cmd) => new Promise<void>((done) => {
      execFile(cmd, ['upgrade'], { timeout: UPDATE_TIMEOUT_MS, env: { ...process.env, NO_COLOR: '1' } }, () => done()).stdin?.end();
    })),
  };
}
