import { existsSync, readFileSync, realpathSync, renameSync, statSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { join, resolve } from 'node:path';
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

/**
 * The spellings Claude Code may key `dir`'s trust by: the path as given and, when it's a different
 * one, its real path (Claude goes by the real path, so an entry under a symlinked spelling doesn't
 * count). Just the given one when the folder can't be resolved (it's gone).
 */
function spellings(dir: string): string[] {
  const given = resolve(dir);
  try {
    const real = realpathSync(given);
    return real === given ? [given] : [given, real];
  } catch {
    return [given];
  }
}

/**
 * Whether Claude Code trusts `dir` by its own entry. A trusted parent doesn't count: Claude asks
 * about a git repo (and its worktrees) unless the repo itself is trusted, whatever its parents are
 * (measured on Claude Code 2.1.318 and 2.1.319).
 */
export function isTrusted(dir: string, cfg: ClaudeJson): boolean {
  return cfg.projects?.[resolve(dir)]?.hasTrustDialogAccepted === true;
}

/**
 * Claude Code asks "Do you trust this folder?" the first time it runs in a git repo, unless that
 * repo's own entry in its config says so: a trusted parent (the home folder, the repos folder)
 * doesn't count. For a git worktree the decision is keyed by the main repository, so the server
 * trusts each project's repo before launching a session; and a worktree Claude can't tie to its
 * repo (started by another spelling) by its own path. Each of `dir`'s spellings gets an entry.
 */
export function ensureWorkspaceTrust(dir: string): 'already' | 'added' | 'no-config' {
  const file = claudeConfigPath();
  if (!existsSync(file)) return 'no-config';
  const dirs = spellings(dir);
  const cfg = JSON.parse(readFileSync(file, 'utf8')) as ClaudeJson;
  if (dirs.every((d) => isTrusted(d, cfg))) return 'already';
  // Re-read right before writing to keep the window with concurrent Claude writes small.
  const fresh = JSON.parse(readFileSync(file, 'utf8')) as ClaudeJson;
  fresh.projects ??= {};
  for (const d of dirs) fresh.projects[d] = { ...(fresh.projects[d] ?? {}), hasTrustDialogAccepted: true };
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
export async function trustFolder(dir: string, log: Logger): Promise<void> {
  try {
    for (let i = 0; i < 3; i++) {
      if (ensureWorkspaceTrust(dir) !== 'added') return;
      if (i === 0) log.info(`trusted ${dir} in Claude Code, so sessions there skip its folder-trust prompt`);
      await new Promise((r) => setTimeout(r, 400));
    }
  } catch (e) {
    log.warn(`could not mark ${dir} as trusted in Claude Code`, errMsg(e));
  }
}
