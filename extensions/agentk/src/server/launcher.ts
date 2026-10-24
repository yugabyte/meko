import { chmodSync, existsSync, mkdirSync, writeFileSync } from 'node:fs';
import { userInfo } from 'node:os';
import { basename, join } from 'node:path';
import { CLAUDE_SESSION_MARKERS } from './claude.js';
import { KIRO_SESSION_MARKERS } from './kiro.js';
import { OPENCODE_SESSION_MARKERS } from './opencode.js';
import { claudePath, type Paths } from './paths.js';

/**
 * Markers a running coding agent puts in its children's environment. If AgentK itself was
 * started from inside one, these must not reach the sessions it launches, of any harness.
 */
export const SESSION_MARKERS = [...CLAUDE_SESSION_MARKERS, ...OPENCODE_SESSION_MARKERS, ...KIRO_SESSION_MARKERS];

export function scrubSessionMarkers(env: NodeJS.ProcessEnv = process.env): void {
  for (const k of SESSION_MARKERS) delete env[k];
}

/** Single-quote for POSIX sh. */
export function shq(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`;
}

/** What a session's launcher starts: the harness's command and arguments, in the worktree, with these variables. */
export interface LauncherSpec {
  sessionId: string;
  worktree: string;
  command: string;
  args: string[];
  env: Record<string, string>;
  /** Variables the session must not inherit (from the session host's environment or the user's profile). */
  unsetEnv?: string[];
}

/** Shells that parse `-c` text the POSIX way, so the quoting in our launcher holds. */
const POSIX_SHELLS = new Set(['sh', 'bash', 'zsh', 'ksh', 'mksh', 'pdksh', 'oksh', 'dash', 'ash', 'yash']);
/** Other shells whose `-l -c` can run a plain `exec /bin/sh <path>` when the path needs no quoting. */
const PLAIN_EXEC_SHELLS = new Set(['fish']);
/** Argument passed to the launcher when it re-runs itself inside the login shell. */
const INNER = '--in-login-shell';

/**
 * How the launcher runs its second half through the user's login shell: `[shell, -c text]`.
 * The text is plain POSIX for POSIX shells; fish quotes differently, so it only gets a path
 * that needs no quoting at all. Anything else (tcsh, csh: no `-l` with `-c`) falls back to `/bin/sh -l`.
 */
export function loginShellCommand(shell: string, script: string): [string, string] {
  const name = basename(shell).replace(/^-/, '');
  const inner = (path: string) => `exec /bin/sh ${path} ${INNER}`;
  if (POSIX_SHELLS.has(name)) return [shell, inner(shq(script))];
  if (PLAIN_EXEC_SHELLS.has(name) && /^[\w./-]+$/.test(script)) return [shell, inner(script)];
  return ['/bin/sh', inner(shq(script))];
}

/**
 * Writes the per-session launcher. It runs the session's agent through the user's login shell so
 * PATH and tools match an interactive terminal, even when the session host was started by systemd
 * or launchd with a bare environment. The script runs twice: first it clears inherited markers and
 * re-runs itself inside the login shell; then, after the user's profile, it sets the session's
 * variables (so a profile can't override them) and starts the agent. The adapter gives every path
 * in `args` in its local spelling (claudePath); the worktree is spelled so here.
 */
export function writeLauncher(paths: Paths, spec: LauncherSpec): string {
  mkdirSync(paths.run, { recursive: true });
  const cwd = claudePath(spec.worktree);
  const file = join(paths.run, `${spec.sessionId}.sh`);
  const [shell, cmd] = loginShellCommand(loginShell(), file);
  const unset = (spec.unsetEnv ?? []).filter((k) => !(k in spec.env));
  const lines = [
    '#!/bin/sh',
    `# AgentK launcher for session ${spec.sessionId}. Rewritten on every launch.`,
    `if [ "$1" != ${INNER} ]; then`,
    `  cd ${shq(cwd)} || exit 1`,
    // Never let an outer agent session's markers, or this install's own home and app, leak in.
    `  unset ${SESSION_MARKERS.join(' ')} TMUX TMUX_PANE AK_HOME AK_APP_DIR AK_VERSION`,
    `  exec ${shq(shell)} -l -c ${shq(cmd)}`,
    'fi',
    `cd ${shq(cwd)} || exit 1`,
    // The installer puts Claude Code and gh in ~/.local/bin, which a login profile (zsh on macOS
    // by default) may not add to PATH.
    'case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *) PATH="$PATH:$HOME/.local/bin"; export PATH ;; esac',
    ...(unset.length ? [`unset ${unset.join(' ')}`] : []),
    ...Object.entries(spec.env).map(([k, v]) => `export ${k}=${shq(v)}`),
    `exec ${shq(spec.command)} ${spec.args.map(shq).join(' ')}`,
    '',
  ];
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
