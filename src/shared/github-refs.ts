// GitHub references people type or paste: repo names, PRs and issues. Used by the server and the web UI.

const REPO_RE = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const LINK_RE = /^(?:https?:\/\/)?(?:www\.)?github\.com\/([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)\/(pull|issues)\/(\d{1,7})(?:[/?#].*)?$/i;

/** True for an `owner/name` GitHub repo. */
export function isRepoName(s: unknown): s is string {
  return typeof s === 'string' && REPO_RE.test(s) && !s.split('/').some((p) => p === '.' || p === '..');
}

/**
 * A PR the user typed: `21`, `#21`, `owner/name#21`, or a PR link
 * (`https://github.com/owner/name/pull/21`, with anything after the number). A bare number has
 * no repo (null). Null for anything else.
 */
export function parsePrInput(input: string): { repo: string | null; number: number } | null {
  const bare = /^#?(\d{1,7})$/.exec(input.trim());
  if (bare) return Number(bare[1]) > 0 ? { repo: null, number: Number(bare[1]) } : null;
  const ref = parseGithubRef(input);
  return ref && ref.kind !== 'issue' ? { repo: ref.repo, number: ref.number } : null;
}

/** A PR or issue on GitHub, as pasted: `kind` is what a link says (null for `owner/name#N`, which could be either). */
export interface GithubRef {
  repo: string;
  number: number;
  kind: 'pr' | 'issue' | null;
}

/**
 * A pasted PR or issue: a github.com link to `/pull/N` or `/issues/N` (anything after the number),
 * or `owner/name#N`. Null for anything else, a bare number included: it names no repo.
 */
export function parseGithubRef(input: string): GithubRef | null {
  const s = input.trim();
  const link = LINK_RE.exec(s);
  if (link) {
    const number = Number(link[3]);
    if (!isRepoName(link[1]) || number <= 0) return null;
    return { repo: link[1], number, kind: link[2].toLowerCase() === 'pull' ? 'pr' : 'issue' };
  }
  const m = /^([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)#(\d{1,7})$/.exec(s);
  if (!m || !isRepoName(m[1]) || Number(m[2]) <= 0) return null;
  return { repo: m[1], number: Number(m[2]), kind: null };
}
