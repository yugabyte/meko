// Minimal client-side GitHub REST client for the PR-review demo app. Called
// directly from the browser with a user-pasted PAT — GitHub's REST API sets
// permissive CORS headers for PAT-authenticated requests, so this needs no
// server-side relay (unlike the Slack client in ./slackApi.ts).

export type ParsedPrRef = { owner: string; repo: string; number: number };

const PR_REF_RE = /^([\w.-]+)\/([\w.-]+)#(\d+)$/;
// A pasted PR link, e.g. "https://github.com/owner/repo/pull/123" (with or
// without a trailing "/files", "/commits", query string, or fragment).
const PR_URL_RE = /^https?:\/\/github\.com\/([\w.-]+)\/([\w.-]+)\/pull\/(\d+)(?:[/?#].*)?$/;

/** Parses "owner/repo#123" or a full GitHub PR URL -- pasting the URL
 *  straight from the browser's address bar is the more natural thing to do,
 *  so both are accepted. Returns null for anything else so the caller can
 *  show a clear validation error instead of a confusing fetch failure. */
export function parsePrRef(input: string): ParsedPrRef | null {
  const trimmed = input.trim();
  const match = PR_REF_RE.exec(trimmed) ?? PR_URL_RE.exec(trimmed);
  if (!match) return null;
  return { owner: match[1], repo: match[2], number: Number(match[3]) };
}

export class GitHubApiError extends Error {}

/** Fetches a PR's unified diff via the `.diff` media type on the pulls
 *  endpoint — one request, no pagination, works for PRs of any file count. */
export async function fetchPullRequestDiff(ref: ParsedPrRef, token: string): Promise<string> {
  const res = await fetch(
    `https://api.github.com/repos/${encodeURIComponent(ref.owner)}/${encodeURIComponent(ref.repo)}/pulls/${ref.number}`,
    {
      headers: {
        Authorization: `Bearer ${token}`,
        Accept: "application/vnd.github.v3.diff",
      },
    },
  );
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new GitHubApiError(`GitHub API request failed: ${res.status} ${res.statusText} ${body}`.trim());
  }
  return res.text();
}

export type PullRequestSummary = {
  title: string;
  htmlUrl: string;
};

/** Fetches a PR's title + web URL, used to label the diff in the transcript. */
export async function fetchPullRequestSummary(
  ref: ParsedPrRef,
  token: string,
): Promise<PullRequestSummary> {
  const res = await fetch(
    `https://api.github.com/repos/${encodeURIComponent(ref.owner)}/${encodeURIComponent(ref.repo)}/pulls/${ref.number}`,
    { headers: { Authorization: `Bearer ${token}`, Accept: "application/vnd.github+json" } },
  );
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new GitHubApiError(`GitHub API request failed: ${res.status} ${res.statusText} ${body}`.trim());
  }
  const data = (await res.json()) as { title?: string; html_url?: string };
  return { title: data.title ?? `${ref.owner}/${ref.repo}#${ref.number}`, htmlUrl: data.html_url ?? "" };
}
