// Project datapacks: the `adlc/proj/<repo>/<project>` naming, the description AgentK writes on
// the ones it creates, and matching one to a local repo. Shared by the server and the web UI.

/** Datapacks named with this prefix are project datapacks: offered as invites and filled in automatically. */
export const PROJECT_DATAPACK_PREFIX = 'adlc/proj/';

const DESCRIPTION_PREFIX = 'AgentK Local project';

export function isProjectDatapack(name: string): boolean {
  return name.startsWith(PROJECT_DATAPACK_PREFIX);
}

/** The repo and project parts of `adlc/proj/<repo>/<project>` (the project part may hold slashes), or null. */
export function parseProjectDatapack(name: string): { repo: string; project: string } | null {
  if (!isProjectDatapack(name)) return null;
  const parts = name.slice(PROJECT_DATAPACK_PREFIX.length).split('/').map((p) => p.trim());
  if (parts.length < 2 || parts.some((p) => !p)) return null;
  return { repo: parts[0], project: parts.slice(1).join('/') };
}

/** Lowercase, [a-z0-9-] only, dashes collapsed: how repo folder names compare with the datapack's repo part. */
export function slugPart(s: string): string {
  return s.toLowerCase().replace(/[^a-z0-9-]+/g, '-').replace(/-{2,}/g, '-').replace(/^-+|-+$/g, '');
}

/** A git remote as `host/path`, without scheme, user, port or `.git`; null when it doesn't look like one. */
function remoteHostPath(url: string): string | null {
  let s = url.trim();
  if (!s) return null;
  const withScheme = /^[a-z][a-z0-9+.-]*:\/\/(?:[^@/]+@)?([^/:]+)(?::\d+)?\/(.+)$/i.exec(s);
  // scp-like: git@host:org/repo
  const scp = /^(?:[^@/\s]+@)?([^:/\s]+):(?!\/)(.+)$/.exec(s);
  if (/^file:/i.test(s)) return null;
  if (withScheme) s = `${withScheme[1]}/${withScheme[2]}`;
  else if (/^[a-z][a-z0-9+.-]*:\/\//i.test(s)) return null;
  else if (scp) s = `${scp[1]}/${scp[2]}`;
  // The short form the repo list shows (`github.com/org/repo`): the host has a dot, so `../x` or
  // `src/x` (local paths) aren't taken for remotes.
  else if (!/^[a-z0-9][^\s/:@]*\.[^\s/:@]+\/\S+$/i.test(s)) return null;
  s = s.replace(/\/+$/, '').replace(/\.git$/i, '').replace(/\/+$/, '');
  return s.includes('/') ? s : null;
}

/** A remote as the repo list shows it: `git@github.com:acme/x.git` → `github.com/acme/x`. */
export function shortRemote(url: string): string {
  return remoteHostPath(url) ?? url.trim().replace(/\.git$/i, '');
}

/**
 * A git remote as `host/path`, lowercased, without scheme, user, port or `.git`, so the SSH and
 * HTTPS forms of one repo (and the short `host/org/repo` form the repo list shows) compare equal.
 * Null when it doesn't look like a remote.
 */
export function normaliseRemote(url: string): string | null {
  return remoteHostPath(url)?.toLowerCase() ?? null;
}

/**
 * A remote without the credentials some carry (`https://user:token@host/…`): what's safe to write
 * into a datapack description that other people read. A plain SSH user (`git@`, `ssh://git@`) stays.
 */
export function withoutCredentials(url: string): string {
  const s = url.trim();
  const m = /^([a-z][a-z0-9+.-]*:\/\/)([^@/]*)@(.*)$/i.exec(s);
  if (!m) return s;
  const [, scheme, user, rest] = m;
  return /^https?:/i.test(scheme) || user.includes(':') ? `${scheme}${rest}` : s;
}

/** The repo name at the end of a remote (`corp/billing-api.git` → `billing-api`); empty when there's none. */
export function remoteRepoName(url: string): string {
  return url.trim().replace(/\/+$/, '').replace(/\.git$/i, '').split(/[/:]/).pop() ?? '';
}

/** A remote other people can use: a network remote (not a local path), without credentials. Else null. */
function shareableRemote(remote: string | null | undefined): string | null {
  return remote && normaliseRemote(remote) ? withoutCredentials(remote) : null;
}

/**
 * The description AgentK sets on a project datapack it creates. The remote goes in only when it's
 * a network remote, and without credentials: a local path (`/home/me/src/x.git`) would only tell
 * the people it's shared with where things are on this machine.
 */
export function projectDatapackDescription(projectName: string, remote: string | null): string {
  const r = shareableRemote(remote);
  return `${DESCRIPTION_PREFIX} "${projectName}"${r ? ` · repo ${r}` : ''}`;
}

/** The repo remote from a description AgentK wrote, or null. Credentials and local paths never come through. */
export function remoteFromDescription(description: string | null | undefined): string | null {
  if (!description?.startsWith(DESCRIPTION_PREFIX)) return null;
  return shareableRemote(/ · repo (\S+)/.exec(description)?.[1]);
}

export interface RepoCandidate {
  name: string;
  path: string;
  remote: string | null;
  lastTouchedAt: number;
}

export interface RepoMatch<T extends RepoCandidate> {
  repo: T | null;
  /** What it was matched on. */
  by: 'remote' | 'name' | null;
  /** How many repos matched that way (more than one: the most recently touched was picked). */
  count: number;
}

/**
 * The local repo a project datapack belongs to: the one whose origin is the datapack's remote,
 * else the one whose folder name or remote name matches the datapack's repo part. With several,
 * the most recently touched.
 */
export function matchRepo<T extends RepoCandidate>(repos: T[], datapackName: string, remote: string | null): RepoMatch<T> {
  const newest = (list: T[]) => [...list].sort((a, b) => b.lastTouchedAt - a.lastTouchedAt)[0];
  const want = remote ? normaliseRemote(remote) : null;
  if (want) {
    const hits = repos.filter((r) => r.remote && normaliseRemote(r.remote) === want);
    if (hits.length) return { repo: newest(hits), by: 'remote', count: hits.length };
  }
  const parsed = parseProjectDatapack(datapackName);
  const part = parsed ? slugPart(parsed.repo) : '';
  if (part) {
    const hits = repos.filter((r) => slugPart(r.name) === part || (r.remote && slugPart(remoteRepoName(r.remote)) === part));
    if (hits.length) return { repo: newest(hits), by: 'name', count: hits.length };
  }
  return { repo: null, by: null, count: 0 };
}
