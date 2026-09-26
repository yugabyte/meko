import { chmodSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { homedir, userInfo } from 'node:os';
import { join } from 'node:path';
import type { Paths } from './paths.js';

/**
 * Markers a running Claude Code session puts in its children's environment.
 * If agent-k-local itself was started from inside Claude Code, these must not
 * reach the sessions it launches (e.g. CLAUDE_CODE_CHILD_SESSION turns off
 * transcript saving, which breaks --resume). User settings such as
 * CLAUDE_CODE_USE_BEDROCK are deliberately kept.
 */
export const CLAUDE_SESSION_MARKERS = [
  'CLAUDECODE', 'CLAUDE_CODE_ENTRYPOINT', 'CLAUDE_CODE_SSE_PORT', 'CLAUDE_CODE_CHILD_SESSION', 'CLAUDE_CODE_BRIDGE_SESSION_ID',
  'CLAUDE_CODE_EXECPATH', 'CLAUDE_CODE_MESSAGING_SOCKET', 'CLAUDE_CODE_MESSAGING_TOKEN', 'CLAUDE_CODE_SESSION_ATTENDED',
  'CLAUDE_CODE_SESSION_ID', 'CLAUDE_PID', 'CLAUDE_EFFORT',
];

export function scrubClaudeMarkers(env: NodeJS.ProcessEnv = process.env): void {
  for (const k of CLAUDE_SESSION_MARKERS) delete env[k];
}

/** Single-quote for POSIX sh. */
export function shq(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`;
}

/**
 * Hook script: forwards the hook payload (stdin) to the server over its unix
 * socket, tagged with the session's token. It must never block or fail Claude,
 * so it always exits 0 and gives up after 3 seconds.
 */
const HOOK_SCRIPT = `#!/bin/sh
# Written by agent-k-local. Forwards Claude Code hook events to the server.
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
# Written by agent-k-local. Asks the server whether a file edit may go ahead.
[ -n "$AK_HOOK_TOKEN" ] && [ -S "$AK_HOOK_SOCK" ] || { cat >/dev/null; exit 0; }
curl -sf --max-time 3 --unix-socket "$AK_HOOK_SOCK" \\
  -H "X-AK-Token: $AK_HOOK_TOKEN" -H "Content-Type: application/json" \\
  --data-binary @- http://localhost/guard 2>/dev/null
exit 0
`;

/**
 * Status line: Claude Code runs it with a JSON description of the session, which includes
 * the plan usage limits (5-hour and weekly) on Pro and Max plans. It passes those to the
 * server in the background, then runs the user's own status line command with the same
 * input, so the terminal looks as it would without AgentK Local.
 */
const STATUSLINE_SCRIPT = `#!/bin/sh
# Written by agent-k-local. Reports plan usage limits, then prints the user's status line.
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
    if (sl) return sl;
  }
  return null;
}

export function hooksSettingsPath(paths: Paths): string {
  return join(paths.run, 'hooks.json');
}

/** Writes the hook script and the --settings file every session is launched with. */
export function writeHookFiles(paths: Paths): void {
  mkdirSync(paths.run, { recursive: true });
  const script = hookScriptPath(paths);
  writeFileSync(script, HOOK_SCRIPT);
  chmodSync(script, 0o755);
  const guard = guardScriptPath(paths);
  writeFileSync(guard, GUARD_SCRIPT);
  chmodSync(guard, 0o755);
  const statusLine = statusLineScriptPath(paths);
  writeFileSync(statusLine, STATUSLINE_SCRIPT);
  chmodSync(statusLine, 0o755);
  // Keep the user's layout options; their command is run by our script (AK_USER_STATUSLINE, or
  // this file for sessions launched before the variable existed).
  const mine = userStatusLine(null, statusLine);
  writeFileSync(join(paths.run, 'user-statusline'), mine?.command ?? '');
  const cmd = { type: 'command', command: script, timeout: 5 };
  const all = [{ hooks: [cmd] }];
  const settings = {
    // The design file lives outside the repo; writing it, and publishing the design as an
    // Artifact (private by default), shouldn't stop for a permission prompt.
    permissions: {
      allow: [`Write(/${paths.designs}/**)`, `Edit(/${paths.designs}/**)`, 'Artifact'],
    },
    statusLine: {
      type: 'command',
      command: statusLine,
      ...(typeof mine?.padding === 'number' ? { padding: mine.padding } : {}),
      ...(typeof mine?.refreshInterval === 'number' ? { refreshInterval: mine.refreshInterval } : {}),
    },
    hooks: {
      SessionStart: all,
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
  };
  writeFileSync(hooksSettingsPath(paths), JSON.stringify(settings, null, 2));
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
  appendSystemPrompt: string;
  prompt: string | null;
  env: Record<string, string>;
}

/**
 * Writes the per-session launcher. It runs Claude through the user's login
 * shell so PATH and tools match an interactive terminal, even when the tmux
 * server was started by systemd or launchd with a bare environment.
 */
export function writeLauncher(paths: Paths, spec: LaunchSpec): string {
  mkdirSync(paths.run, { recursive: true });
  const args = ['--settings', hooksSettingsPath(paths)];
  if (spec.resume) {
    args.push('--resume', spec.claudeSessionId);
  } else {
    args.push('--session-id', spec.claudeSessionId, '--name', spec.name);
  }
  if (spec.planMode) args.push('--permission-mode', 'plan');
  for (const d of spec.addDirs) args.push('--add-dir', d);
  if (spec.appendSystemPrompt) args.push('--append-system-prompt', spec.appendSystemPrompt);
  if (spec.prompt) args.push(spec.prompt);

  const inner = `exec ${shq(spec.claudeCommand)} ${args.map(shq).join(' ')}`;
  const shell = loginShell();
  const lines = [
    '#!/bin/sh',
    `# agent-k-local launcher for session ${spec.sessionId}. Rewritten on every launch.`,
    `cd ${shq(spec.worktree)} || exit 1`,
    // Never let an outer Claude Code session's markers leak in.
    `unset ${CLAUDE_SESSION_MARKERS.join(' ')} TMUX TMUX_PANE`,
    ...Object.entries(spec.env).map(([k, v]) => `export ${k}=${shq(v)}`),
    `exec ${shq(shell)} -l -c ${shq(inner)}`,
    '',
  ];
  const file = join(paths.run, `${spec.sessionId}.sh`);
  writeFileSync(file, lines.join('\n'), { mode: 0o700 });
  chmodSync(file, 0o700);
  return file;
}

export function loginShell(): string {
  try {
    const s = userInfo().shell;
    if (s && existsSync(s)) return s;
  } catch {
    /* fall through */
  }
  return process.env.SHELL && existsSync(process.env.SHELL) ? process.env.SHELL : '/bin/sh';
}

/** Where Claude Code stores a session transcript for a given cwd. */
export function transcriptPathFor(cwd: string, claudeSessionId: string): string {
  return join(homedir(), '.claude', 'projects', cwd.replace(/[^A-Za-z0-9]/g, '-'), `${claudeSessionId}.jsonl`);
}
