/**
 * The git context the passes can't gather themselves, having no git (`history.go`): the branch's
 * commits, and for each changed file the commits that last wrote the lines the change rewrites and
 * what recently happened to it. Enrichment, never an error: a file git can't answer for (added by
 * the change, binary) contributes nothing.
 */
import { gitRaw } from '../git.js';
import type { Diff, FileChange } from './types.js';

/** How much history a review gathers (`DefaultLimits`), so a long-lived file doesn't crowd the prompt. */
export const HISTORY_LIMITS = { maxFiles: 25, recentPerFile: 5, blamePerFile: 5 };

/** At most this many of the branch's commits are listed. */
const MAX_COMMITS = 50;

interface HistCommit {
  sha: string;
  author: string;
  date: string;
  subject: string;
}

const FORMAT = '--format=%h%x1f%an%x1f%ad%x1f%s';

function parseCommits(out: string): HistCommit[] {
  return out.split('\n').flatMap((line) => {
    const f = line.trim().split('\x1f');
    return f.length >= 4 ? [{ sha: f[0], author: f[1], date: f[2], subject: f[3] }] : [];
  });
}

/** The branch's commits since `mergeBase`, newest first: each one's subject and body. */
export async function readCommits(worktree: string, mergeBase: string): Promise<string[]> {
  const out = await gitRaw(worktree, 'log', '-z', `--max-count=${MAX_COMMITS}`, '--format=%h %s%n%n%b', `${mergeBase}..HEAD`).catch(() => '');
  return out.split('\0').map((c) => c.trim()).filter(Boolean);
}

/** What happened to a file before the branch: the walk starts at the merge base, so the branch's own commits never show. */
async function recent(worktree: string, mergeBase: string, path: string): Promise<HistCommit[]> {
  const out = await gitRaw(worktree, 'log', `--max-count=${HISTORY_LIMITS.recentPerFile}`, FORMAT, '--date=short', mergeBase, '--', path).catch(() => '');
  return parseCommits(out);
}

/**
 * The commits that last wrote the lines the change rewrites: blamed at the merge base over the
 * base-side hunk ranges, the only place those lines still exist.
 */
async function blamed(worktree: string, mergeBase: string, f: FileChange): Promise<HistCommit[]> {
  const shas = new Set<string>();
  for (const [from, to] of f.baseRanges) {
    const out = await gitRaw(worktree, 'blame', '--line-porcelain', `-L${from},${to}`, mergeBase, '--', f.oldPath ?? f.path).catch(() => '');
    for (const line of out.split('\n')) {
      // Each porcelain block opens with "<sha> <orig> <final> [count]".
      if (/^[0-9a-f]{40} /.test(line)) shas.add(line.slice(0, 40));
    }
  }
  if (!shas.size) return [];
  const picked = [...shas].sort().slice(0, HISTORY_LIMITS.blamePerFile);
  const out = await gitRaw(worktree, 'show', '--no-patch', FORMAT, '--date=short', ...picked).catch(() => '');
  return parseCommits(out);
}

function row(c: HistCommit): string {
  return `  ${c.sha}  ${c.date.padEnd(10)} ${c.author}  ${c.subject}`;
}

/** The history section's text, or '' when there's nothing to show. */
export async function readHistory(worktree: string, mergeBase: string, diff: Diff): Promise<string> {
  const parts: string[] = [];
  for (const f of diff.files.slice(0, HISTORY_LIMITS.maxFiles)) {
    if (f.status === 'added') continue;
    const [blame, rec] = await Promise.all([blamed(worktree, mergeBase, f), recent(worktree, mergeBase, f.oldPath ?? f.path)]);
    if (!blame.length && !rec.length) continue;
    const lines = [`### ${f.path}`, ''];
    if (blame.length) lines.push('Last wrote the lines this PR changes:', ...blame.map(row), '');
    if (rec.length) lines.push('Recent commits to this file, before this PR:', ...rec.map(row), '');
    parts.push(lines.join('\n'));
  }
  return parts.join('\n').trimEnd();
}
