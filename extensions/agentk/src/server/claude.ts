import { execFile } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import type { LauncherSpec } from './launcher.js';
import { claudePath, type Paths } from './paths.js';
import { writeScript } from './scriptfile.js';

/**
 * Markers a running Claude Code session puts in its children's environment.
 * If AgentK itself was started from inside Claude Code, these must not
 * reach the sessions it launches (e.g. CLAUDE_CODE_CHILD_SESSION turns off
 * transcript saving, which breaks --resume). User settings such as
 * CLAUDE_CODE_USE_BEDROCK are deliberately kept.
 */
export const CLAUDE_SESSION_MARKERS = [
  'CLAUDECODE', 'CLAUDE_CODE_ENTRYPOINT', 'CLAUDE_CODE_SSE_PORT', 'CLAUDE_CODE_CHILD_SESSION', 'CLAUDE_CODE_BRIDGE_SESSION_ID',
  'CLAUDE_CODE_EXECPATH', 'CLAUDE_CODE_MESSAGING_SOCKET', 'CLAUDE_CODE_MESSAGING_TOKEN', 'CLAUDE_CODE_SESSION_ATTENDED',
  'CLAUDE_CODE_SESSION_ID', 'CLAUDE_PID', 'CLAUDE_EFFORT',
];

/**
 * Hook script: forwards the hook payload (stdin) to the server over its unix
 * socket, tagged with the session's token. It must never block or fail Claude,
 * so it always exits 0 and gives up after 3 seconds.
 */
const HOOK_SCRIPT = `#!/bin/sh
# Written by AgentK. Forwards Claude Code hook events to the server.
[ -n "$AK_HOOK_TOKEN" ] && [ -S "$AK_HOOK_SOCK" ] || { cat >/dev/null; exit 0; }
curl -s -o /dev/null --max-time 3 --unix-socket "$AK_HOOK_SOCK" \\
  -H "X-AK-Token: $AK_HOOK_TOKEN" -H "Content-Type: application/json" \\
  --data-binary @- http://localhost/hook >/dev/null 2>&1
exit 0
`;

/**
 * Guard hook for file edits: asks the server whether the edit may go ahead and prints
 * its decision (Claude Code reads it from stdout). During Design the server denies edits
 * inside the repo. If the server can't be reached, it prints nothing and the edit goes
 * ahead: the guard must never leave Claude stuck.
 */
const GUARD_SCRIPT = `#!/bin/sh
# Written by AgentK. Asks the server whether a file edit may go ahead.
[ -n "$AK_HOOK_TOKEN" ] && [ -S "$AK_HOOK_SOCK" ] || { cat >/dev/null; exit 0; }
curl -sf --max-time 3 --unix-socket "$AK_HOOK_SOCK" \\
  -H "X-AK-Token: $AK_HOOK_TOKEN" -H "Content-Type: application/json" \\
  --data-binary @- http://localhost/guard 2>/dev/null
exit 0
`;

/**
 * SessionStart hook: asks the server for context to start the session with (what AgentK's Meko
 * search found, see recall.ts) and prints it for Claude Code. Like the guard, it prints nothing
 * when the server can't be reached or has nothing, and never fails or holds up Claude for long.
 */
const CONTEXT_SCRIPT = `#!/bin/sh
# Written by AgentK. Asks the server for context to start the session with.
[ -n "$AK_HOOK_TOKEN" ] && [ -S "$AK_HOOK_SOCK" ] || { cat >/dev/null; exit 0; }
curl -sf --max-time 4 --unix-socket "$AK_HOOK_SOCK" \\
  -H "X-AK-Token: $AK_HOOK_TOKEN" -H "Content-Type: application/json" \\
  --data-binary @- http://localhost/context 2>/dev/null
exit 0
`;

/**
 * Status line: Claude Code runs it with a JSON description of the session, which includes
 * the plan usage limits (5-hour and weekly) on Pro and Max plans. It passes those to the
 * server in the background, then runs the user's own status line command with the same
 * input, so the terminal looks as it would without AgentK.
 */
const STATUSLINE_SCRIPT = `#!/bin/sh
# Written by AgentK. Reports plan usage limits, then prints the user's status line.
input=$(cat)
if [ -n "$AK_HOOK_TOKEN" ] && [ -S "$AK_HOOK_SOCK" ]; then
  case $input in
    *'"rate_limits"'*)
      printf '%s' "$input" | curl -s -o /dev/null --max-time 2 --unix-socket "$AK_HOOK_SOCK" \\
        -H "X-AK-Token: $AK_HOOK_TOKEN" -H "Content-Type: application/json" \\
        --data-binary @- http://localhost/statusline >/dev/null 2>&1 &
      ;;
  esac
fi
# Sessions launched before this script existed don't have AK_USER_STATUSLINE; use the saved user one.
if [ -n "\${AK_USER_STATUSLINE+x}" ]; then cmd=$AK_USER_STATUSLINE; else cmd=$(cat "$(dirname "$0")/user-statusline" 2>/dev/null); fi
[ -n "$cmd" ] && printf '%s' "$input" | sh -c "$cmd"
exit 0
`;

/** File-editing tools the Design-stage guard checks. */
export const EDIT_TOOLS = ['Write', 'Edit', 'MultiEdit', 'NotebookEdit'];

export function guardScriptPath(paths: Paths): string {
  return join(paths.run, 'guard.sh');
}

export function contextScriptPath(paths: Paths): string {
  return join(paths.run, 'context.sh');
}

export function hookScriptPath(paths: Paths): string {
  return join(paths.run, 'hook.sh');
}

export function statusLineScriptPath(paths: Paths): string {
  return join(paths.run, 'statusline.sh');
}

export interface StatusLineSetting {
  type?: string;
  command?: string;
  padding?: number;
  refreshInterval?: number;
}

/** Claude Code's user settings folder (`CLAUDE_CONFIG_DIR`, else `~/.claude`). */
export function claudeUserDir(): string {
  return process.env.CLAUDE_CONFIG_DIR || join(homedir(), '.claude');
}

function readStatusLine(file: string, ours: string): StatusLineSetting | null {
  try {
    const sl = (JSON.parse(readFileSync(file, 'utf8')) as { statusLine?: unknown }).statusLine;
    if (!sl || typeof sl !== 'object') return null;
    const s = sl as StatusLineSetting;
    // Never wrap our own script (for example, settings copied from a session): match its full path.
    return s.type === 'command' && typeof s.command === 'string' && s.command.trim() && !s.command.includes(ours)
      ? s
      : null;
  } catch {
    return null;
  }
}

/**
 * Meko's status line script as the start of a command (optionally run by `sh`/`bash`, the path
 * optionally quoted). It prints an optional inner command's line, then "Meko: <pinned datapack>".
 */
const MEKO_STATUSLINE = /^\s*(?:(?:ba)?sh\s+)?(['"]?)(?:[^'"\s]*\/)?meko-statusline\.sh\1(?=\s|$)/;

/** Runs what Meko's script would run first (`--inner-cmd <cmd>`, or `MEKO_INNER_STATUSLINE`), and nothing else. */
const MEKO_INNER_ONLY =
  'ak_meko_inner() { if [ "${1:-}" = --inner-cmd ] && [ -n "${2:-}" ]; then eval "$2"; ' +
  'elif [ -n "${MEKO_INNER_STATUSLINE:-}" ]; then eval "$MEKO_INNER_STATUSLINE"; fi; }; ak_meko_inner';

/**
 * Drops Meko's "Meko: <datapack>" status line suffix: sessions always use the project's
 * datapack, and the session screen already shows it. The command it wraps still runs.
 */
export function withoutMekoSuffix(command: string): string {
  return command.replace(MEKO_STATUSLINE, MEKO_INNER_ONLY);
}

/**
 * The status line Claude Code would run without ours (`ours`, the relay's path), since ours
 * (passed with --settings) replaces it: local project settings first, then project, then user.
 */
export function userStatusLine(worktree: string | null, ours: string): StatusLineSetting | null {
  const files = [
    ...(worktree ? [join(worktree, '.claude', 'settings.local.json'), join(worktree, '.claude', 'settings.json')] : []),
    join(claudeUserDir(), 'settings.json'),
  ];
  for (const f of files) {
    const sl = readStatusLine(f, ours);
    if (sl) return { ...sl, command: withoutMekoSuffix(sl.command!) };
  }
  return null;
}

export function hooksSettingsPath(paths: Paths): string {
  return join(paths.run, 'hooks.json');
}

/**
 * Writes the hook script and the --settings file every session is launched with. `disabledPlugins`
 * (plugin keys) are turned off in sessions whatever the user's own settings say.
 */
export function writeHookFiles(paths: Paths, opts: { disabledPlugins?: string[] } = {}): void {
  mkdirSync(paths.run, { recursive: true });
  const script = hookScriptPath(paths);
  writeScript(script, HOOK_SCRIPT);
  const guard = guardScriptPath(paths);
  writeScript(guard, GUARD_SCRIPT);
  const context = contextScriptPath(paths);
  writeScript(context, CONTEXT_SCRIPT);
  const statusLine = statusLineScriptPath(paths);
  writeScript(statusLine, STATUSLINE_SCRIPT);
  // Keep the user's layout options; their command is run by our script (AK_USER_STATUSLINE, or
  // this file for sessions launched before the variable existed).
  const mine = userStatusLine(null, statusLine);
  writeFileSync(join(paths.run, 'user-statusline'), mine?.command ?? '');
  const cmd = { type: 'command', command: script, timeout: 5 };
  const all = [{ hooks: [cmd] }];
  const settings = {
    // The design file lives outside the repo; writing it, and publishing the design as an
    // Artifact (private by default), shouldn't stop for a permission prompt. Edit() rules cover
    // every file-editing tool, Write included; Claude Code ignores Write() rules and warns about them.
    // Images added in the New session dialog reach Claude as paths in its first prompt, for it to read.
    // Claude is given these folders' local spelling (claudePath), so the rules use it too.
    permissions: {
      allow: [`Edit(/${claudePath(paths.designs)}/**)`, 'Artifact', `Read(/${claudePath(paths.pastes)}/**)`],
    },
    statusLine: {
      type: 'command',
      command: statusLine,
      ...(typeof mine?.padding === 'number' ? { padding: mine.padding } : {}),
      ...(typeof mine?.refreshInterval === 'number' ? { refreshInterval: mine.refreshInterval } : {}),
    },
    hooks: {
      // The second hook prints what AgentK's Meko search found for the task (recall.ts).
      SessionStart: [{ hooks: [cmd, { type: 'command', command: context, timeout: 5 }] }],
      UserPromptSubmit: all,
      PreToolUse: [
        // ExitPlanMode carries the design text for sessions that still design in plan mode.
        { matcher: 'ExitPlanMode', hooks: [cmd] },
        // File edits are checked by the Design-stage guard.
        { matcher: EDIT_TOOLS.join('|'), hooks: [{ type: 'command', command: guard, timeout: 5 }] },
      ],
      // PostToolUse on every tool marks the session busy again after a permission prompt.
      PostToolUse: [{ matcher: '*', hooks: [cmd] }],
      PermissionRequest: [{ matcher: '*', hooks: [cmd] }],
      Notification: all,
      Stop: all,
      SessionEnd: all,
    },
    ...(opts.disabledPlugins?.length ? { enabledPlugins: Object.fromEntries(opts.disabledPlugins.map((k) => [k, false])) } : {}),
  };
  writeFileSync(hooksSettingsPath(paths), JSON.stringify(settings, null, 2));
}

/**
 * The oldest Claude Code AgentK's mod runs in: plugins' hooks modules, `$.prompt.submit` with
 * `asUser` (older ones frame the prompt as a plugin's message) and `session.measure` (2.1.286), and
 * for the learn step `$.model.fork`, `$.model.complete`, `$.command.register` and `command.run`
 * (checked in 2.1.287's declarations; 2.1.286's couldn't be).
 */
export const MIN_MOD_CLAUDE = '2.1.287';

/**
 * The mod's files, relative to its folder: src/mod/agentk in the repo (tsx, tests), dist/mod/agentk
 * in a build. Its tests stay behind.
 */
const MOD_FILES = ['.claude-plugin/plugin.json', 'hooks/hooks.json', 'hooks/register.ts'];
const MOD_SOURCE = join(dirname(fileURLToPath(import.meta.url)), '..', 'mod', 'agentk');

/** Where sessions load AgentK's mod from (`--plugin-dir`). */
export function modDirPath(paths: Paths): string {
  return join(paths.run, 'mod', 'agentk');
}

/**
 * Copies AgentK's mod to the run folder. Claude reloads a `--plugin-dir` mod when its files change,
 * so only files that differ are written (a server restart reloads nothing), each through a temp
 * file renamed into place (a session never reads half a module). Every source file is read first:
 * when one is missing, or a write fails, the run copy is removed, so sessions start without the
 * mod (`modDir()` checks for it) rather than with an old or half-updated one. False then; a failed
 * write also throws.
 */
export function writeModFiles(paths: Paths, source = MOD_SOURCE): boolean {
  const dest = modDirPath(paths);
  const texts: string[] = [];
  for (const f of MOD_FILES) {
    try {
      texts.push(readFileSync(join(source, f), 'utf8'));
    } catch {
      rmSync(dest, { recursive: true, force: true });
      return false;
    }
  }
  try {
    MOD_FILES.forEach((f, i) => {
      const to = join(dest, f);
      try {
        if (readFileSync(to, 'utf8') === texts[i]) return;
      } catch {
        /* not there yet */
      }
      mkdirSync(dirname(to), { recursive: true });
      const tmp = `${to}.${randomBytes(4).toString('hex')}.tmp`;
      writeFileSync(tmp, texts[i]);
      renameSync(tmp, to);
    });
  } catch (e) {
    rmSync(dest, { recursive: true, force: true });
    throw e;
  }
  return true;
}

/** How long `claude update` may run before it's stopped. */
const CLAUDE_UPDATE_TIMEOUT_MS = 5 * 60 * 1000;

/**
 * Runs `<command> update`, Claude Code's own updater, and resolves whether it exited 0 and what it
 * printed (stdout, then stderr). Never rejects: a command that can't start is `ok: false`.
 */
export function runClaudeUpdate(command: string): Promise<{ ok: boolean; output: string }> {
  return new Promise((resolve) => {
    execFile(command, ['update'], { timeout: CLAUDE_UPDATE_TIMEOUT_MS, maxBuffer: 1024 * 1024, env: { ...process.env, NO_COLOR: '1' } }, (err, stdout, stderr) => {
      const output = [String(stdout ?? ''), String(stderr ?? '')].join('\n').trim();
      resolve({ ok: !err, output: output || (err ? err.message : '') });
    });
  });
}

export interface LaunchSpec {
  sessionId: string;
  worktree: string;
  claudeCommand: string;
  claudeSessionId: string;
  /** Resume the conversation instead of starting it. */
  resume: boolean;
  name: string;
  planMode: boolean;
  /** Extra directories Claude may work in (the session's design folder). */
  addDirs: string[];
  /** Plugins Claude loads for the session (AgentK's mod), each a `--plugin-dir`. */
  pluginDirs?: string[];
  appendSystemPrompt: string;
  prompt: string | null;
  env: Record<string, string>;
  /** Variables the session must not inherit (from the session host's environment or the user's profile). */
  unsetEnv?: string[];
  /**
   * Load only the user's settings, not the repo's (`.claude/settings*.json`: hooks, permissions,
   * env, status line, plugins) or its `.mcp.json` servers: for a PR head whose code isn't trusted.
   */
  userSettingsOnly?: boolean;
}

/**
 * What a Claude Code session's launcher runs: `claude` with AgentK's settings, the conversation
 * (`--session-id` for a new one, named; `--resume` after), and every path in its local spelling
 * (see claudePath).
 */
export function claudeLauncher(paths: Paths, spec: LaunchSpec): LauncherSpec {
  const args = ['--settings', claudePath(hooksSettingsPath(paths))];
  // AgentK's --settings, its mod and the user's MCP servers still apply.
  if (spec.userSettingsOnly) args.push('--setting-sources', 'user');
  if (spec.resume) {
    args.push('--resume', spec.claudeSessionId);
  } else {
    args.push('--session-id', spec.claudeSessionId, '--name', spec.name);
  }
  if (spec.planMode) args.push('--permission-mode', 'plan');
  for (const d of spec.addDirs) args.push('--add-dir', claudePath(d));
  for (const d of spec.pluginDirs ?? []) args.push('--plugin-dir', claudePath(d));
  if (spec.appendSystemPrompt) args.push('--append-system-prompt', spec.appendSystemPrompt);
  // After "--", a prompt that starts with "-" (such as "---" front matter) isn't read as an option.
  if (spec.prompt) args.push('--', spec.prompt);
  return { sessionId: spec.sessionId, worktree: spec.worktree, command: spec.claudeCommand, args, env: spec.env, unsetEnv: spec.unsetEnv };
}

/**
 * Where Claude Code stores a session transcript for a given cwd. Claude keys the folder on the cwd
 * it started in: the local spelling (claudePath) for sessions launched since AgentK started using
 * it, the stored one for older sessions (a resume from the local spelling finds and keeps adding
 * to their transcript). Whichever exists, the local one if neither does.
 */
export function transcriptPathFor(cwd: string, claudeSessionId: string): string {
  const at = (dir: string) => join(claudeUserDir(), 'projects', dir.replace(/[^A-Za-z0-9]/g, '-'), `${claudeSessionId}.jsonl`);
  const local = at(claudePath(cwd));
  const stored = at(cwd);
  return local === stored || existsSync(local) || !existsSync(stored) ? local : stored;
}
