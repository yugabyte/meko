import { existsSync, readFileSync, renameSync, statSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { errMsg, type Logger } from './log.js';

/** Claude Code's global config (holds per-folder trust decisions). */
export function claudeConfigPath(): string {
  return process.env.CLAUDE_CONFIG_DIR ? join(process.env.CLAUDE_CONFIG_DIR, '.claude.json') : join(homedir(), '.claude.json');
}

let accountCache: { key: string; account: string | null } | null = null;

/**
 * The Claude account Claude Code is signed in to (`oauthAccount` in its global config, which a
 * sign-in rewrites), as `accountUuid/organizationUuid`, or null when it has none. Read again
 * only when the file changes.
 */
export function claudeAccount(): string | null {
  const file = claudeConfigPath();
  let key: string;
  try {
    const st = statSync(file);
    key = `${st.mtimeMs}:${st.size}`;
  } catch {
    return null;
  }
  if (accountCache?.key === key) return accountCache.account;
  let account: string | null = null;
  try {
    const o = (JSON.parse(readFileSync(file, 'utf8')) as { oauthAccount?: { accountUuid?: unknown; organizationUuid?: unknown } }).oauthAccount;
    if (typeof o?.accountUuid === 'string' && o.accountUuid) account = `${o.accountUuid}/${typeof o.organizationUuid === 'string' ? o.organizationUuid : ''}`;
  } catch {
    return accountCache?.account ?? null; // caught mid-write: keep what it said
  }
  accountCache = { key, account };
  return account;
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

/** Whether `dir`, or (unless `exact`) one of its parents, is trusted. */
export function isTrusted(dir: string, cfg: ClaudeJson, exact = false): boolean {
  const projects = cfg.projects ?? {};
  return (exact ? [resolve(dir)] : ancestors(dir)).some((a) => projects[a]?.hasTrustDialogAccepted === true);
}

/**
 * Claude Code asks "Do you trust this folder?" the first time it runs in a
 * directory, unless that directory or a parent is trusted. For a git worktree
 * the decision is keyed by the main repository, so the server trusts each
 * project's repo before launching a session (and the worktrees root as well). `exact`: only an
 * entry for `dir` itself counts (Claude doesn't look above a worktree it can't tie to its repo).
 */
export function ensureWorkspaceTrust(dir: string, exact = false): 'already' | 'added' | 'no-config' {
  const file = claudeConfigPath();
  if (!existsSync(file)) return 'no-config';
  const raw = readFileSync(file, 'utf8');
  const cfg = JSON.parse(raw) as ClaudeJson;
  if (isTrusted(dir, cfg, exact)) return 'already';
  // Re-read right before writing to keep the window with concurrent Claude writes small.
  const fresh = JSON.parse(readFileSync(file, 'utf8')) as ClaudeJson;
  fresh.projects ??= {};
  fresh.projects[resolve(dir)] = { ...(fresh.projects[resolve(dir)] ?? {}), hasTrustDialogAccepted: true };
  const tmp = `${file}.agent-k-local.tmp`;
  writeFileSync(tmp, JSON.stringify(fresh, null, 2), { mode: statSync(file).mode & 0o777 });
  renameSync(tmp, file);
  return 'added';
}

/**
 * Gives each folder `map` moves its `projects` entry in Claude Code's config (trust, and the state
 * Claude keeps per folder) under its new path, when that has none yet. Old entries stay. Returns how
 * many it added; 0 without a config.
 */
export function copyClaudeProjects(map: (dir: string) => string | null): number {
  const file = claudeConfigPath();
  if (!existsSync(file)) return 0;
  // Read right before writing, as ensureWorkspaceTrust does: other Claudes rewrite this file.
  const cfg = JSON.parse(readFileSync(file, 'utf8')) as ClaudeJson;
  const projects = cfg.projects ?? {};
  let added = 0;
  for (const [dir, entry] of Object.entries(projects)) {
    const to = map(dir);
    if (!to || to in projects) continue;
    projects[to] = entry;
    added++;
  }
  if (!added) return 0;
  const tmp = `${file}.agent-k-local.tmp`;
  writeFileSync(tmp, JSON.stringify(cfg, null, 2), { mode: statSync(file).mode & 0o777 });
  renameSync(tmp, file);
  return added;
}

/** Text Claude Code shows in its folder-trust dialog. */
export const TRUST_PROMPT_RE = /trust this folder|do you trust the files|Is this a project you created or one you trust/i;

/**
 * Makes sure Claude Code trusts `dir` (`ensureWorkspaceTrust`). Other Claude processes rewrite
 * ~/.claude.json with their own copy when they start, which can drop a fresh entry, so a new entry
 * is checked again after a moment. Never throws.
 */
export async function trustFolder(dir: string, log: Logger, exact = false): Promise<void> {
  try {
    for (let i = 0; i < 3; i++) {
      if (ensureWorkspaceTrust(dir, exact) !== 'added') return;
      if (i === 0) log.info(`trusted ${dir} in Claude Code, so sessions there skip its folder-trust prompt`);
      await new Promise((r) => setTimeout(r, 400));
    }
  } catch (e) {
    log.warn(`could not mark ${dir} as trusted in Claude Code`, errMsg(e));
  }
}
