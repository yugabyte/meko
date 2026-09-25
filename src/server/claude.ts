import { chmodSync, existsSync, mkdirSync, writeFileSync } from 'node:fs';
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

export function hookScriptPath(paths: Paths): string {
  return join(paths.run, 'hook.sh');
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
  const cmd = { type: 'command', command: script, timeout: 5 };
  const all = [{ hooks: [cmd] }];
  const settings = {
    hooks: {
      SessionStart: all,
      UserPromptSubmit: all,
      // PreToolUse only for ExitPlanMode: that's where the design text is.
      PreToolUse: [{ matcher: 'ExitPlanMode', hooks: [cmd] }],
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
  appendSystemPrompt: string;
  prompt: string | null;
  env: Record<string, string>;
}

/**
 * Writes the per-session launcher. It runs Claude through the user's login
 * shell so PATH and tools match an interactive terminal, even when the tmux
 * server was started by systemd with a bare environment.
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
