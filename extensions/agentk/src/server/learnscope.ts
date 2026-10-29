import type { LearnScope } from '../shared/types.js';
import { gitWithin, tryGit } from './git.js';
import { learnSince } from './prompts.js';

/** How long each git read for the estimate may take before it's given up. */
const GIT_TIMEOUT_MS = 10_000;
/** The most words of Where looked at as paths. */
const MAX_WORDS = 50;

/** A tracked file on the base branch: its path and size. */
interface TreeFile {
  path: string;
  size: number;
}

/**
 * The words of a learn session's Where that could name paths in the repo: split on whitespace,
 * commas and semicolons, with quotes, backticks, brackets and trailing punctuation trimmed and a
 * leading `./` or `/` dropped. A word must have a `/`, a glob character or a file extension (prose
 * like "the billing service" has none), and URLs are left out.
 */
export function whereWords(where: string): string[] {
  const out: string[] = [];
  for (const raw of where.split(/[\s,;]+/)) {
    const w = raw
      .replace(/^[`'"([{<]+/, '')
      .replace(/[`'")\]}>.:!?]+$/, '')
      .replace(/^\.?\/+/, '');
    if (!w || w.includes('://') || w.startsWith('..') || w.startsWith(':')) continue;
    if (!/[/*?[]|\.\w+$/.test(w)) continue;
    if (!out.includes(w)) out.push(w);
    if (out.length >= MAX_WORDS) break;
  }
  return out;
}

const hasGlob = (w: string) => /[*?[]/.test(w);

/**
 * A git `:(glob)` pathspec as a regex over a whole path: `*` and `?` stay inside a folder, `**`
 * crosses folders, `[…]` is a class. Like any pathspec, it also matches the files under a folder it names.
 */
export function globRegex(spec: string): RegExp {
  let re = '';
  for (let i = 0; i < spec.length; i++) {
    const c = spec[i];
    if (c === '*' && spec[i + 1] === '*') {
      // `**/` is any folders (none included); a trailing `**` is everything under.
      if (spec[i + 2] === '/') {
        re += '(?:.*/)?';
        i += 2;
      } else {
        re += '.*';
        i += 1;
      }
    } else if (c === '*') re += '[^/]*';
    else if (c === '?') re += '[^/]';
    else if (c === '[') {
      const end = spec.indexOf(']', i + 2);
      if (end < 0) re += '\\[';
      else {
        re += `[${spec.slice(i + 1, end).replace(/^!/, '^').replace(/\\/g, '\\\\')}]`;
        i = end;
      }
    } else re += c.replace(/[.+^${}()|\\]/g, '\\$&');
  }
  return new RegExp(`^${re.replace(/\/$/, '')}(?:/.*)?$`);
}

/** Whether <path> is named by a word of Where: the file, a folder above it, or a glob over it. */
function named(path: string, word: string): boolean {
  if (hasGlob(word)) return globRegex(word).test(path);
  const w = word.replace(/\/+$/, '');
  return path === w || path.startsWith(`${w}/`);
}

/** The words of Where that name at least one of <files>. */
export function wherePaths(where: string, files: TreeFile[]): string[] {
  return whereWords(where).filter((w) => files.some((f) => named(f.path, w)));
}

/** The base branch's tracked files with their sizes (`ls-tree -r -l`); submodules aren't files. */
export function parseTree(out: string): TreeFile[] {
  const files: TreeFile[] = [];
  for (const entry of out.split('\0')) {
    // <mode> SP blob SP <oid> SP+ <size> TAB <path>
    const tab = entry.indexOf('\t');
    if (tab < 0) continue;
    const [, type, , size] = entry.slice(0, tab).split(/ +/);
    if (type !== 'blob') continue;
    files.push({ path: entry.slice(tab + 1), size: Number(size) || 0 });
  }
  return files;
}

/**
 * How much a learn session's brief covers (`LearnScope`), from <repo>'s own git, with no fetch and
 * no GitHub calls: on `origin/<base>` as last fetched, else the local <base> (the worktree branches
 * from the newer of the two), the merged PRs in the window that starts `learnSince(months, now)`
 * and the tracked files, both in the paths Where names, or in the whole repo when it names none.
 * The PRs are first-parent commits, one per PR for squash and merge commits (more for a repo that
 * rebases PRs in). Null when neither ref exists.
 */
export async function learnScope(repo: string, base: string, where: string, months: number, now: number): Promise<LearnScope | null> {
  const remote = `refs/remotes/origin/${base}`;
  const ref = (await tryGit(repo, 'rev-parse', '--verify', '--quiet', remote)) ? remote
    : (await tryGit(repo, 'rev-parse', '--verify', '--quiet', `refs/heads/${base}`)) ? `refs/heads/${base}` : null;
  if (!ref) return null;
  const all = parseTree(await gitWithin(GIT_TIMEOUT_MS, repo, 'ls-tree', '-r', '-l', '-z', ref));
  const paths = wherePaths(where, all);
  const files = paths.length ? all.filter((f) => paths.some((p) => named(f.path, p))) : all;
  let prs = 0;
  if (months > 0) {
    const specs = paths.map((p) => (hasGlob(p) ? `:(glob)${p}` : p));
    const out = await gitWithin(GIT_TIMEOUT_MS, repo, 'rev-list', '--count', '--first-parent', `--since=${learnSince(months, now)}`, ref, '--', ...specs);
    prs = Number(out.trim()) || 0;
  }
  return { prs, files: files.length, bytes: files.reduce((n, f) => n + f.size, 0), paths };
}
