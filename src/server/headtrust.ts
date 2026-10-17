import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { parsePrInput } from '../shared/github-refs.js';
import type { RepoVisibility } from './github.js';
import type { SessionRow } from './store.js';

/** The roles on a repo (GitHub's `authorAssociation`) whose people AgentK trusts. Anyone else can comment on a public repo. */
export const TRUSTED_ASSOCIATIONS = ['OWNER', 'MEMBER', 'COLLABORATOR'];

/** How long a repo's visibility is used before it's read again (a private repo can be made public). */
const VISIBILITY_TTL_MS = 10 * 60 * 1000;

/** `owner/name` from a PR's URL. */
const prRepo = (url: string | null): string | null => (url ? parsePrInput(url)?.repo ?? null : null);

/**
 * The GitHub repo of the PR whose head a session works on, when that head is someone else's
 * unmerged code: a review's PR, or a task started on a PR (or on an existing branch whose open PR
 * it took). `{ repo: null }` when the session is one of those but the repo isn't known. Null for a
 * session on its own new branch, which starts from merged code.
 */
export function prHead(row: Pick<SessionRow, 'kind' | 'pr_repo' | 'pr_url' | 'pr_number' | 'branch_reused'>): { repo: string | null } | null {
  if (row.kind === 'review-pr') return { repo: row.pr_repo ?? prRepo(row.pr_url) };
  if (row.branch_reused && row.pr_number) return { repo: prRepo(row.pr_url) };
  return null;
}

/**
 * Whether the code on a PR's head is trusted. On a private or internal repo only people with access
 * can push or open a PR (a private repo's forks are private too), so its code is trusted as the
 * user's own. On a public repo anyone can open one, so it isn't, and neither is a repo whose
 * visibility gh couldn't read. Visibilities are kept for a while; a failed read is tried again next time.
 */
export class HeadTrust {
  private seen = new Map<string, { visibility: RepoVisibility; at: number }>();

  /** `read`: the repo's visibility from GitHub (`repoVisibility` in github.ts), throwing when it can't say. */
  constructor(private read: (repo: string) => Promise<RepoVisibility>) {}

  /** The repo's visibility, or null when gh couldn't say. */
  async visibility(repo: string, now = Date.now()): Promise<RepoVisibility | null> {
    const key = repo.toLowerCase();
    const hit = this.seen.get(key);
    if (hit && now - hit.at < VISIBILITY_TTL_MS) return hit.visibility;
    try {
      const visibility = await this.read(repo);
      this.seen.set(key, { visibility, at: now });
      return visibility;
    } catch {
      return null;
    }
  }

  /** Why the session's code isn't trusted, in words for its event log, or null when it is. */
  async untrusted(row: Parameters<typeof prHead>[0]): Promise<string | null> {
    const head = prHead(row);
    if (!head) return null;
    if (!head.repo) return "AgentK doesn't know which repo this PR is on";
    const v = await this.visibility(head.repo);
    if (v === 'private' || v === 'internal') return null;
    return v === 'public' ? `${head.repo} is public` : `GitHub didn't say whether ${head.repo} is public`;
  }
}

/** What an untrusted session's event log says it runs without. */
export const UNTRUSTED_TEXT = "so anyone can have written this PR's code: the session runs without the repo's own config "
  + '(its .claude settings, hooks, status line and MCP servers, its Kiro agents, and its .agent-k/stages prompts)';

/**
 * Kiro config in a worktree that would take the place of AgentK's: a workspace agent named `agentk`
 * (Kiro uses it over the global one) and workspace MCP servers (`.kiro/settings/mcp.json`, which
 * the agent loads). Kiro has no flag to skip them, so an untrusted session can't start on Kiro while
 * they're there. Paths relative to the worktree.
 */
export function kiroWorkspaceConfig(worktree: string, agent: string): string[] {
  const found: string[] = [];
  const agents = join(worktree, '.kiro', 'agents');
  let files: string[] = [];
  try {
    files = readdirSync(agents).filter((f) => f.endsWith('.json'));
  } catch {
    /* none */
  }
  for (const f of files) {
    let name: unknown = null;
    try {
      name = (JSON.parse(readFileSync(join(agents, f), 'utf8')) as { name?: unknown }).name;
    } catch {
      /* unreadable: Kiro names it by its file */
    }
    if (f === `${agent}.json` || name === agent) found.push(`.kiro/agents/${f}`);
  }
  if (existsSync(join(worktree, '.kiro', 'settings', 'mcp.json'))) found.push('.kiro/settings/mcp.json');
  return found;
}

/** Whether a session may use its repo's own config: it's on its own branch, or its PR head was checked and is trusted. */
export function repoConfigTrusted(row: Parameters<typeof prHead>[0] & Pick<SessionRow, 'untrusted'>): boolean {
  return !prHead(row) || row.untrusted === 0;
}
