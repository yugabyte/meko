/**
 * The files a review finds beside the code it reads: review guides (REVIEW.md, as the agent-k
 * reviewer's `FindGuides`) and MEKO.md, the datapacks that hold context for that code. Each applies
 * to its folder and everything under it, so a changed file's every ancestor folder is looked in, not
 * only the one holding it. Root first, then shallowest to deepest, the order the files assume.
 */
import { statSync } from 'node:fs';
import { join, posix } from 'node:path';

/** The review guide's names, in preference order. */
const GUIDE_NAMES = ['REVIEW.md', 'review.md', 'Review.md'];

/** The repo's root and every ancestor folder of each changed path. */
function folders(paths: string[]): Set<string> {
  const dirs = new Set(['.']);
  for (const p of paths) {
    for (let d = posix.dirname(p); d !== '.' && d !== '/'; d = posix.dirname(d)) dirs.add(d);
  }
  return dirs;
}

function isFile(p: string): boolean {
  try {
    return statSync(p).isFile();
  } catch {
    return false;
  }
}

/** The first of `names` in each folder, repo-relative, root first and shallow to deep. */
function findIn(worktree: string, paths: string[], names: string[]): string[] {
  const out: string[] = [];
  for (const d of folders(paths)) {
    for (const name of names) {
      const rel = d === '.' ? name : posix.join(d, name);
      if (isFile(join(worktree, rel))) {
        out.push(rel);
        break;
      }
    }
  }
  const depth = (p: string) => p.split('/').length;
  return out.sort((a, b) => depth(a) - depth(b) || (a < b ? -1 : a > b ? 1 : 0));
}

/** The review guides that apply to a change touching `paths`. */
export function findGuides(worktree: string, paths: string[]): string[] {
  return findIn(worktree, paths, GUIDE_NAMES);
}

/** The MEKO.md files that apply to a change touching `paths`. */
export function findMekoMds(worktree: string, paths: string[]): string[] {
  return findIn(worktree, paths, ['MEKO.md']);
}
