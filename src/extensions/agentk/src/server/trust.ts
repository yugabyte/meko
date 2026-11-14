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
  hasCompletedOnboarding?: boolean;
  [k: string]: unknown;
}

/** Writes Claude Code's config whole, by renaming a temp file over it, keeping its mode. */
function writeClaudeJson(file: string, cfg: ClaudeJson): void {
  const tmp = `${file}.agent-k-local.tmp`;
  writeFileSync(tmp, JSON.stringify(cfg, null, 2), { mode: statSync(file).mode & 0o777 });
  renameSync(tmp, file);
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
  writeClaudeJson(file, fresh);
  return 'added';
}

/**
 * Claude Code opens on its first-run setup (the text style, then how to sign in) until its config
 * says `hasCompletedOnboarding`, which signing in with `claude auth login` doesn't set. It fires no
 * hooks meanwhile, so a session Kode starts would wait there behind the Starting panel: before a
 * launch, Kode marks it done. Claude then keeps its default text style. Only once the config shows
 * a sign-in (`oauthAccount`, as `claudeAccount` reads it): the file can exist without one (Meko's
 * `claude mcp add`, an install that went on with Claude Code signed out), and then the setup's
 * sign-in screen is what shows the session as waiting on you (`SETUP_PROMPT_RE`). Nothing is written
 * without a config either.
 */
export function ensureOnboarded(): 'already' | 'added' | 'signed-out' | 'no-config' {
  const file = claudeConfigPath();
  if (!existsSync(file)) return 'no-config';
  const cfg = JSON.parse(readFileSync(file, 'utf8')) as ClaudeJson;
  if (cfg.hasCompletedOnboarding === true) return 'already';
  if (!cfg.oauthAccount || typeof cfg.oauthAccount !== 'object') return 'signed-out';
  // Re-read right before writing, as ensureWorkspaceTrust does.
  const fresh = JSON.parse(readFileSync(file, 'utf8')) as ClaudeJson;
  fresh.hasCompletedOnboarding = true;
  writeClaudeJson(file, fresh);
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
  writeClaudeJson(file, cfg);
  return added;
}

/** Text Claude Code shows in its folder-trust dialog. */
export const TRUST_PROMPT_RE = /trust this folder|do you trust the files|Is this a project you created or one you trust/i;

/** Text Claude Code shows in its first-run setup (measured on 2.1.319): its text style, then how to sign in. */
export const SETUP_PROMPT_RE = /Choose the text style that looks best with your terminal|Select login method/i;

/**
 * Runs a write to ~/.claude.json until it sticks. Other Claude processes rewrite the file with
 * their own copy when they start, which can drop a fresh entry, so a write that added something is
 * checked again after a moment. `done` is logged once. Never throws.
 */
async function keepWritten(write: () => string, done: string, failed: string, log: Logger): Promise<void> {
  try {
    for (let i = 0; i < 3; i++) {
      if (write() !== 'added') return;
      if (i === 0) log.info(done);
      await new Promise((r) => setTimeout(r, 400));
    }
  } catch (e) {
    log.warn(failed, errMsg(e));
  }
}

/** Makes sure Claude Code trusts `dir` (`ensureWorkspaceTrust`). Never throws. */
export async function trustFolder(dir: string, log: Logger): Promise<void> {
  await keepWritten(() => ensureWorkspaceTrust(dir), `trusted ${dir} in Claude Code, so sessions there skip its folder-trust prompt`,
    `could not mark ${dir} as trusted in Claude Code`, log);
}

/** Makes sure Claude Code's first-run setup is marked done (`ensureOnboarded`). Never throws. */
export async function skipClaudeSetup(log: Logger): Promise<void> {
  await keepWritten(ensureOnboarded, "marked Claude Code's first-run setup done, so sessions skip it", "could not mark Claude Code's first-run setup done", log);
}
