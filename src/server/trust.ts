import { existsSync, readFileSync, renameSync, statSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join, resolve } from 'node:path';

/** Claude Code's global config (holds per-folder trust decisions). */
export function claudeConfigPath(): string {
  return process.env.CLAUDE_CONFIG_DIR ? join(process.env.CLAUDE_CONFIG_DIR, '.claude.json') : join(homedir(), '.claude.json');
}

interface ClaudeJson {
  projects?: Record<string, { hasTrustDialogAccepted?: boolean } & Record<string, unknown>>;
  [k: string]: unknown;
}

function ancestors(dir: string): string[] {
  const out: string[] = [];
  let d = resolve(dir);
  for (;;) {
    out.push(d);
    const up = dirname(d);
    if (up === d) return out;
    d = up;
  }
}

export function isTrusted(dir: string, cfg: ClaudeJson): boolean {
  const projects = cfg.projects ?? {};
  return ancestors(dir).some((a) => projects[a]?.hasTrustDialogAccepted === true);
}

/**
 * Claude Code asks "Do you trust this folder?" the first time it runs in a
 * directory, unless that directory or a parent is trusted. For a git worktree
 * the decision is keyed by the main repository, so the server trusts each
 * project's repo before launching a session (and the worktrees root as well).
 */
export function ensureWorkspaceTrust(dir: string): 'already' | 'added' | 'no-config' {
  const file = claudeConfigPath();
  if (!existsSync(file)) return 'no-config';
  const raw = readFileSync(file, 'utf8');
  const cfg = JSON.parse(raw) as ClaudeJson;
  if (isTrusted(dir, cfg)) return 'already';
  // Re-read right before writing to keep the window with concurrent Claude writes small.
  const fresh = JSON.parse(readFileSync(file, 'utf8')) as ClaudeJson;
  fresh.projects ??= {};
  fresh.projects[resolve(dir)] = { ...(fresh.projects[resolve(dir)] ?? {}), hasTrustDialogAccepted: true };
  const tmp = `${file}.agent-k-local.tmp`;
  writeFileSync(tmp, JSON.stringify(fresh, null, 2), { mode: statSync(file).mode & 0o777 });
  renameSync(tmp, file);
  return 'added';
}

/** Text Claude Code shows in its folder-trust dialog. */
export const TRUST_PROMPT_RE = /trust this folder|do you trust the files|Is this a project you created or one you trust/i;
