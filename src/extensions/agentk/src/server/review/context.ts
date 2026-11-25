/**
 * What a review reads, from a folder: the change from where it left its base to the working tree,
 * and everything around it the passes are told (`ReviewContext`). Both drivers build it here: a
 * task's review in its worktree (`runner.ts`) and `meko kode review` in the folder it runs in
 * (`headless.ts`), so they review the same change the same way.
 */
import { readFileSync } from 'node:fs';
import { basename, join } from 'node:path';
import { isGotchasDatapack, parseMekoMd } from '../../shared/mekomd.js';
import { shortRemote } from '../../shared/datapacks.js';
import { git, readRemotes } from '../git.js';
import { claudePath } from '../paths.js';
import { readDiff } from './diff.js';
import { findGuides, findMekoMds } from './guides.js';
import { readCommits, readHistory } from './history.js';
import type { Diff, PriorRound, ReviewContext, ReviewDatapack, ReviewIssue } from './types.js';

/** The repo's GitHub `owner/name` from its origin, else its folder's name. */
export async function repoNameOf(repo: string): Promise<string> {
  const origin = (await readRemotes(repo).catch(() => null))?.origin;
  return origin ? shortRemote(origin).split('/').slice(-2).join('/') : basename(repo);
}

/** A ref that names a commit in `folder`, or null. */
export async function commitRef(folder: string, ref: string): Promise<string | null> {
  try {
    await git(folder, 'rev-parse', '--verify', '--quiet', `${ref}^{commit}`);
    return ref;
  } catch {
    return null;
  }
}

/** The base a change is read against: origin's copy of the base branch when there is one, else the local branch. */
export async function baseRefIn(folder: string, branch: string): Promise<string> {
  for (const ref of [`origin/${branch}`, branch]) {
    if (await commitRef(folder, ref)) return ref;
  }
  return branch;
}

/** The datapacks the MEKO.md files over the change list, each once, the gotchas ones marked. */
export function datapacksIn(folder: string, paths: string[]): ReviewDatapack[] {
  const seen = new Map<string, ReviewDatapack>();
  for (const rel of findMekoMds(folder, paths)) {
    let text: string;
    try {
      text = readFileSync(join(folder, rel), 'utf8');
    } catch {
      continue;
    }
    for (const d of parseMekoMd(text)) if (!seen.has(d.id)) seen.set(d.id, { ...d, gotchas: isGotchasDatapack(d.name) });
  }
  return [...seen.values()];
}

/** What the caller knows of a change beyond its folder: who wrote it, for what, and what came before. */
export interface ChangeAccount {
  /** The folder the passes read (a task's worktree, or the folder `meko kode review` runs in). */
  folder: string;
  /** The repo it's in, for its name (a task's repo, else the folder). */
  repo: string;
  /** The base the change is read against (`baseRefIn`, or what the user named). */
  base: string;
  /** The review's run folder for this round: patches too large to inline go there. */
  dir: string;
  branch: string;
  title: string;
  /** The PR title and description the author wrote, or null. */
  prDescription: string | null;
  /** The task's first prompt, or ''. */
  description: string;
  design: string | null;
  /** Read only when the MEKO.md files may be trusted and there's a Meko to search. */
  meko: boolean;
  /** Read after the diff, with it in hand. */
  issues: () => Promise<ReviewIssue[]>;
  prior: PriorRound[];
}

/** What the passes read: the diff from the merge-base to the working tree, and everything around it. */
export async function readChange(a: ChangeAccount): Promise<{ ctx: ReviewContext; diff: Diff }> {
  const wt = a.folder;
  const diff = await readDiff(wt, a.base, a.dir);
  // The passes read each patch too large to inline by its path: as Claude is told every path.
  for (const f of diff.files) if (f.patchPath) f.patchPath = claudePath(f.patchPath);
  const mergeBase = (await git(wt, 'merge-base', a.base, 'HEAD')).trim();
  const head = (await git(wt, 'rev-parse', 'HEAD')).trim();
  const dirty = !!(await git(wt, 'status', '--porcelain')).trim();
  const paths = diff.files.map((f) => f.path);
  return {
    diff,
    ctx: {
      repo: await repoNameOf(a.repo),
      branch: a.branch,
      baseRef: a.base,
      baseSha: mergeBase,
      headSha: head,
      dirty,
      title: a.title,
      prDescription: a.prDescription,
      description: a.description,
      design: a.design,
      commits: await readCommits(wt, mergeBase),
      guides: findGuides(wt, paths),
      history: await readHistory(wt, mergeBase, diff),
      issues: await a.issues(),
      datapacks: a.meko ? datapacksIn(wt, paths) : [],
      prior: a.prior,
      // As Claude is told every path: Claude Code asks before reading through /net.
      worktree: claudePath(wt),
      runDir: claudePath(a.dir),
    },
  };
}
