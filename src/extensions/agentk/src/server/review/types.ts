/**
 * The code review's data (docs/design/code-review.md), ported from the agent-k reviewer's
 * `pkg/agenticreview`: passes and strategies, the diff they read, findings, candidates and scores.
 * The algorithm is in `merge.ts` (merging, the refutation gate, keepScored, Validate), the prompts in
 * `prompt.ts`, the diff and its withholding in `diff.ts`; `runner.ts` drives a review through its
 * session.
 */
import type { FindingOutcome, ReviewStrategyId, ScoreVerdict, Severity } from '../../shared/codereview.js';

/**
 * Which model a step runs on. The model for each tier is the harness's (`HARNESS_CAPS`), or
 * `[code_review.models.<harness>]`'s: this module never names a model.
 */
export type Tier = 'large' | 'mid' | 'small';

/** One reviewer with one job (the cloud's `Pass`). */
export interface Pass {
  name: string;
  tier: Tier;
  /** What the pass owns, word for word from the cloud. */
  mandate: string;
  /**
   * The JSON schema its report must meet (`review_report`'s `report`): the shared
   * `{ summary, findings }`, or a pass's own with a required ledger (dependents, claims).
   */
  schema: Record<string, unknown>;
  /** The pass is the whole review (Sirius 1.0): it isn't told other passes cover the rest. */
  alone?: boolean;
}

/** A named, versioned bundle of what a review does (the cloud's `Strategy`). */
export interface Strategy {
  id: ReviewStrategyId;
  name: string;
  passes: Pass[];
  /** A scorer runs for each candidate (false: every candidate is kept, as with MinConfidence 0). */
  scored: boolean;
}

/** One line-anchored finding a pass reported. */
export interface Finding {
  /** Repo-relative path of a file the change touches. */
  file: string;
  /** Line in the file as it is in the working tree. */
  line: number;
  severity: Severity;
  comment: string;
  /** Meko gotchas (`MG-<n>`) the finding is a direct instance of. */
  gotchas: string[];
  /** The sentence of each of those gotchas it instantiates, by id; absent in candidates from before they were kept. */
  quotes?: Record<string, string>;
}

/** A pass's report, as `review_report` took it (already checked against the pass's schema). */
export interface PassReport {
  pass: string;
  round: number;
  sweep: number;
  /** "LGTM", or a real problem that isn't one of the findings. */
  summary: string;
  findings: Finding[];
}

/** A scorer's answer for one candidate (the cloud's score schema). */
export interface ScoreReport {
  read: string[];
  refutation: string;
  verdict: ScoreVerdict;
  confidence: number;
  reason: string;
}

/** A finding proposed by one or more passes, awaiting scoring (the cloud's `Candidate`). */
export interface Candidate {
  finding: Finding;
  /** The passes that independently raised it. */
  passes: string[];
  /** The sweep that first raised it, from 1. */
  sweep: number;
  score: number;
  reason: string;
  verdict: ScoreVerdict | null;
  /** Set only when the scorer named the fact that makes it wrong and what it read. */
  refuted: boolean;
  /** A scorer returned a verdict. An unscored candidate survives. */
  scored: boolean;
}

/**
 * Why a changed file's patch is kept out of the prompts (`withhold.go`, `generated.go`). A file
 * the change deleted is `deleted` in `FileChange.deleted`, whatever its reason; `'deleted'` here is
 * never set.
 */
export type Withheld = '' | 'secret' | 'generated' | 'deleted';

/** One file the change touches. */
export interface FileChange {
  path: string;
  /** For a rename, where it was. */
  oldPath: string | null;
  status: 'added' | 'modified' | 'deleted' | 'renamed';
  additions: number;
  deletions: number;
  /** The file's own unified diff ('' when withheld or binary). */
  patch: string;
  withheld: Withheld;
  /** Lines of the new file that the diff shows (added lines and their context): what a finding may anchor on. */
  anchors: number[];
  /** The same hunks' line ranges on the base side, inclusive: what the history blames. */
  baseRanges: [number, number][];
  /** A withheld file that's gone from the working tree: the change deleted it, so there's nothing to read. */
  deleted: boolean;
  /** Function names its hunk headers name. */
  symbols: string[];
  /**
   * Where its patch was written for the passes to read, when the diff isn't inline; null otherwise.
   * Kode's spelling: the prompts give it through `claudePath`.
   */
  patchPath: string | null;
}

/** The change a review reads: from the merge-base with the base branch to the working tree. */
export interface Diff {
  files: FileChange[];
  /** The whole patch, withheld files left out. */
  patch: string;
  /** The patch goes in the prompt; otherwise each file's is written to the run folder. */
  inline: boolean;
  /** Where the change left its base (`diffBase` in changes.ts): the ref it was found on and the merge-base commit. */
  mergeBase: { ref: string; sha: string };
}

/** A datapack a MEKO.md lists (`src/shared/mekomd.ts`). */
export interface ReviewDatapack {
  name: string;
  id: string;
  about: string;
  /** Its name ends in `/gotchas`: the datapack the passes search for traps. */
  gotchas: boolean;
}

/** A linked issue the passes are shown: a GitHub issue fetched into the run folder, or a link by its title. */
export interface ReviewIssue {
  title: string;
  /**
   * Where its text was written (absolute, Kode's spelling: the prompts give it through
   * `claudePath`), or null for a link the passes only see named.
   */
  path: string | null;
  url: string;
}

/** One earlier round of this review, as the next round is told about it (the cloud's prior rounds). */
export interface PriorRound {
  round: number;
  /**
   * `dismissed`: the user dismissed it, so the task never had it; `note`: what the user said of it ('' for nothing).
   * Both absent in a context saved before the user could.
   */
  findings: { file: string; line: number; severity: Severity; comment: string; outcome: FindingOutcome | null; dismissed?: boolean; note?: string }[];
}

/** Everything the base prompt is built from, apart from the diff. */
export interface ReviewContext {
  /** The repo's GitHub `owner/name`, or its folder name. */
  repo: string;
  branch: string;
  baseRef: string;
  baseSha: string;
  headSha: string;
  /** The working tree has changes not committed yet (the review reads them too). */
  dirty: boolean;
  /** The task's name. */
  title: string;
  /**
   * The PR title (its first line) and description the task wrote before the review, Meko lines
   * included; null when it wrote none. Absent in a context saved before the task was asked for one.
   */
  prDescription?: string | null;
  /** The task's first prompt. */
  description: string;
  /**
   * Where the task's design is, when it has one: its Artifact's link, or its file in Kode's
   * spelling (the prompts give it through `claudePath`). Absent in a context saved before it was kept
   * apart from the description, which named it.
   */
  design?: string | null;
  /** `git log base..HEAD` subjects and bodies, newest first. */
  commits: string[];
  /** Review guides (REVIEW.md) that apply, repo-relative, root first. */
  guides: string[];
  /** The history section, rendered (`readHistory`), or ''. */
  history: string;
  issues: ReviewIssue[];
  datapacks: ReviewDatapack[];
  prior: PriorRound[];
  /** The worktree the passes read (absolute), in Kode's spelling: the prompts give it through `claudePath`. */
  worktree: string;
  /** The review's run folder (absolute): prompts, patches, issues. Kode's spelling, as `worktree`. */
  runDir: string;
}
