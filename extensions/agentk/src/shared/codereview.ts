/**
 * AgentK's local code review (docs/design/code-review.md): the review strategies, the three
 * settings (strategy, re-review rounds, depth) and how a session's, its project's and the global
 * values resolve, the `me_ak_review:` line a PR description carries, and the short label the
 * review's part of a task's tab shows. Used by the server and the web UI.
 */

/** A review strategy's id, stamped on every review and never reused (`STRATEGIES`). */
export type ReviewStrategyId = 'sagittarius-3.0' | 'sirius-1.0';

/** The strategy setting: a strategy, or no local review. */
export type ReviewStrategySetting = ReviewStrategyId | 'off';

export interface ReviewStrategyInfo {
  id: ReviewStrategyId;
  name: string;
  /** One line on what it does, for the settings' ⓘ. */
  summary: string;
  /** Passes in one sweep. */
  passes: number;
  /** It runs a scorer for each candidate. */
  scored: boolean;
}

/**
 * The registry, in the order the settings list them after Off: the lightest review first. Nothing is
 * ever removed: a review recorded under an old id has to stay readable. Sagittarius 3.0 is the agent-k
 * reviewer's Golum 1.0 algorithm; the cloud keeps its own name.
 */
export const STRATEGIES: ReviewStrategyInfo[] = [
  {
    id: 'sirius-1.0',
    name: 'Sirius 1.0',
    summary: 'One Sonnet-level pass that covers all five mandates. No scorers: cheaper and faster.',
    passes: 1,
    scored: false,
  },
  {
    id: 'sagittarius-3.0',
    name: 'Sagittarius 3.0',
    summary: 'Five specialist passes (guide, diff, dependents, history, claims), then a scorer per finding that can only drop it by refuting it.',
    passes: 5,
    scored: true,
  },
];

export const DEFAULT_STRATEGY: ReviewStrategyId = 'sagittarius-3.0';
/** Re-review rounds: reviews after the first, each after the task fixes the last round's findings. */
export const DEFAULT_ROUNDS = 0;
export const MAX_ROUNDS = 5;
/** Review depth: sweeps in each round. A sweep runs each of the strategy's passes once; each after the first is shown what the earlier ones found. */
export const DEFAULT_DEPTH = 1;
export const MAX_DEPTH = 5;
/** The sweep from which one that adds nothing new ends the review's sweeps; the ones before it always run. */
export const MIN_SWEEPS = 2;

export function strategyInfo(id: string): ReviewStrategyInfo | undefined {
  return STRATEGIES.find((s) => s.id === id);
}

/** A strategy's display name; an unknown id shows as itself, since it's a record of what ran. */
export function strategyName(id: string): string {
  return strategyInfo(id)?.name ?? id;
}

export function isStrategySetting(v: unknown): v is ReviewStrategySetting {
  return v === 'off' || (typeof v === 'string' && !!strategyInfo(v));
}

/** The three settings, as resolved for one review. */
export interface ReviewSettings {
  strategy: ReviewStrategySetting;
  rounds: number;
  depth: number;
}

/** A project's or a session's overrides: a value left out (or null) keeps the next level's. */
export interface ReviewOverrides {
  strategy?: ReviewStrategySetting | null;
  rounds?: number | null;
  depth?: number | null;
}

export const DEFAULT_REVIEW: ReviewSettings = { strategy: DEFAULT_STRATEGY, rounds: DEFAULT_ROUNDS, depth: DEFAULT_DEPTH };

/** Clamps a count setting into its range, or null when it isn't a whole number. */
function count(v: unknown, min: number, max: number): number | null {
  if (typeof v !== 'number' || !Number.isInteger(v)) return null;
  return Math.min(max, Math.max(min, v));
}

/**
 * Reads overrides from untrusted input (a request, a stored JSON column), dropping what isn't
 * valid. Null (or a missing key) means "default"; numbers are clamped into range.
 */
export function readOverrides(v: unknown): ReviewOverrides {
  if (!v || typeof v !== 'object') return {};
  const o = v as Record<string, unknown>;
  const out: ReviewOverrides = {};
  if (isStrategySetting(o.strategy)) out.strategy = o.strategy;
  const rounds = count(o.rounds, 0, MAX_ROUNDS);
  if (rounds !== null) out.rounds = rounds;
  const depth = count(o.depth, 1, MAX_DEPTH);
  if (depth !== null) out.depth = depth;
  return out;
}

/** True when the overrides set nothing. */
export function noOverrides(o: ReviewOverrides): boolean {
  return o.strategy == null && o.rounds == null && o.depth == null;
}

/** The settings a review runs with: the session's overrides, then the project's, then the global ones. */
export function resolveReview(global: ReviewSettings, project: ReviewOverrides, session: ReviewOverrides): ReviewSettings {
  return {
    strategy: session.strategy ?? project.strategy ?? global.strategy,
    rounds: session.rounds ?? project.rounds ?? global.rounds,
    depth: session.depth ?? project.depth ?? global.depth,
  };
}

export type Severity = 'blocker' | 'major' | 'minor';
export const SEVERITIES: Severity[] = ['blocker', 'major', 'minor'];

export type SeverityCounts = Record<Severity, number>;

export function noCounts(): SeverityCounts {
  return { blocker: 0, major: 0, minor: 0 };
}

export function addCounts(a: SeverityCounts, b: SeverityCounts): SeverityCounts {
  return { blocker: a.blocker + b.blocker, major: a.major + b.major, minor: a.minor + b.minor };
}

export function totalCount(c: SeverityCounts): number {
  return c.blocker + c.major + c.minor;
}

/** `N blocker` or `N blockers`; major and minor read the same for any N. */
function severityPart(s: Severity, n: number): string {
  return s === 'blocker' ? `${n} blocker${n === 1 ? '' : 's'}` : `${n} ${s}`;
}

/** The prefix of the PR description's line. */
export const ME_AK_REVIEW = 'me_ak_review:';

/**
 * The PR description's line for a finished local review: every round's findings added up, each
 * severity that has some, in the order blocker, major, minor; `LGTM` when no round found any.
 */
export function meAkReviewLine(counts: SeverityCounts): string {
  const parts = SEVERITIES.filter((s) => counts[s] > 0).map((s) => severityPart(s, counts[s]));
  return ME_AK_REVIEW + (parts.length ? `${parts.join(',')} addressed` : 'LGTM');
}

/** Reads a `me_ak_review:` line back: its counts, or null when it isn't one. */
export function parseMeAkReviewLine(line: string): SeverityCounts | null {
  const t = line.trim();
  if (!t.startsWith(ME_AK_REVIEW)) return null;
  const rest = t.slice(ME_AK_REVIEW.length).trim();
  if (rest === 'LGTM') return noCounts();
  const m = /^(.*)\s+addressed$/.exec(rest);
  if (!m) return null;
  const counts = noCounts();
  for (const part of m[1].split(',')) {
    const pm = /^\s*(\d+)\s+(blockers?|major|minor)\s*$/.exec(part);
    if (!pm) return null;
    const sev = (pm[2].startsWith('blocker') ? 'blocker' : pm[2]) as Severity;
    counts[sev] += Number(pm[1]);
  }
  return totalCount(counts) ? counts : null;
}

/** The `me_ak_review:` line in a PR description, or null when it has none. */
export function findMeAkReviewLine(body: string): string | null {
  for (const line of body.split('\n')) if (line.trim().startsWith(ME_AK_REVIEW)) return line.trim();
  return null;
}

/** A PR description with its `me_ak_review:` line set to `line` (replaced, or added at the end). */
export function withMeAkReviewLine(body: string, line: string): string {
  const lines = body.split('\n');
  const i = lines.findIndex((l) => l.trim().startsWith(ME_AK_REVIEW));
  if (i >= 0) {
    lines[i] = line;
    return lines.join('\n');
  }
  const trimmed = body.replace(/\s+$/, '');
  return trimmed ? `${trimmed}\n\n${line}` : line;
}

/**
 * Where a local review is (`CodeReviewSummary.state`): `describing` while the task writes the PR's
 * title and description for the passes to read, before the first round.
 */
export type CodeReviewState = 'describing' | 'preparing' | 'sweeping' | 'scoring' | 'fixing' | 'done' | 'failed' | 'cancelled';

/** What a task's tab and rail row show of its local review (`Session.codeReview`). */
export interface CodeReviewSummary {
  /** The review's id (`code_reviews.id`). */
  id: string;
  /** The review session (a `code-review` session whose parent is the task); null before it's created. */
  sessionId: string | null;
  strategy: ReviewStrategyId;
  state: CodeReviewState;
  /** Round 1 is the first review; re-reviews follow. */
  round: number;
  rounds: number;
  sweep: number;
  depth: number;
  /** In `sweeping`: passes reported in this sweep, and how many it runs. */
  passesDone: number;
  passes: number;
  /** In `scoring`: candidates scored, and how many there are. */
  scoresDone: number;
  candidates: number;
  /** Findings kept so far, every round's added up. */
  counts: SeverityCounts;
  /** Done, and the task said what it did (fixed or declined, `review_outcome`) with every kept finding, every round's. */
  addressed: boolean;
  /** The review session is in a Meko tool call (the lens shows Meko's m). */
  mekoCall: boolean;
  /** The review session waits on the user in its terminal (a permission prompt or a question): the glass knocks. */
  waiting: boolean;
  /** Why it failed or was cancelled, or why a `describing` review waits on you, in plain words; null otherwise. */
  detail: string | null;
}

/**
 * The steps the badge counts: each pass of a sweep, then scoring as one step when the strategy
 * scores (Sagittarius 3.0's 5 passes and its scorers are 6 steps, Sirius 1.0's one pass is 1).
 * The count moves on as each pass reports, whether a sweep's passes run at once (Claude Code,
 * OpenCode) or one after another (Kiro). With depth > 1 the passes count again for each sweep (the
 * tooltip names the sweep); scoring follows the last.
 */
export function reviewSteps(r: Pick<CodeReviewSummary, 'strategy' | 'passes'>): number {
  return r.passes + (strategyInfo(r.strategy)?.scored ? 1 : 0);
}

/** The step the badge shows a running review at, from 1: one past the passes reported (at most the last pass), or scoring. */
function reviewStep(r: CodeReviewSummary): number {
  return r.state === 'scoring' ? reviewSteps(r) : Math.min(r.passesDone + 1, r.passes);
}

/** What a done review shows: LGTM once nothing is left to address, else what it found. */
function doneShowsCounts(r: CodeReviewSummary): boolean {
  return totalCount(r.counts) > 0 && !r.addressed;
}

/** The review's part of a task's tab: the short count after the magnifier. */
export function reviewSegmentText(r: CodeReviewSummary): string {
  switch (r.state) {
    case 'failed':
    case 'cancelled':
      return '!';
    case 'done':
      return doneShowsCounts(r) ? `${r.counts.blocker}·${r.counts.major}` : 'LGTM';
    case 'sweeping':
    case 'scoring':
      return `${reviewStep(r)}/${reviewSteps(r)}`;
    case 'fixing':
      return `R${r.round}`;
    default:
      return '…';
  }
}

/** The review's part of a task's tab spelled out, for its tooltip and aria-label. */
export function reviewSegmentLabel(r: CodeReviewSummary): string {
  const name = strategyName(r.strategy);
  const where: string[] = [];
  if (r.rounds > 1) where.push(`round ${r.round} of ${r.rounds}`);
  if (r.depth > 1 && r.state === 'sweeping') where.push(`sweep ${r.sweep} of ${r.depth}`);
  let what: string;
  switch (r.state) {
    case 'preparing': what = 'preparing'; break;
    // The passes may all be running, and the scorers a batch at a time, so neither counts the
    // pass or scorer under way: only how many have reported. Scoring's "step n of n" is the
    // review's own step (`reviewSteps`), which stays.
    case 'sweeping': what = `${r.passesDone} of ${r.passes} passes reported`; break;
    case 'scoring': what = `step ${reviewSteps(r)} of ${reviewSteps(r)}, ${r.scoresDone} of ${r.candidates} scored`; break;
    case 'describing': what = 'waiting for the PR description'; break;
    case 'fixing': what = 'waiting for the fixes'; break;
    case 'done': what = doneShowsCounts(r) ? countsText(r.counts) : totalCount(r.counts) ? `LGTM, ${countsText(r.counts)} addressed` : 'LGTM'; break;
    case 'failed': what = `failed${r.detail ? `: ${r.detail}` : ''}. Retry or Open anyway`; break;
    case 'cancelled': what = reviewHolds(r) ? 'cancelled. Retry or Open anyway' : `cancelled: ${r.detail}`; break;
  }
  const meko = r.mekoCall ? ', searching Meko' : '';
  const waiting = r.waiting ? ', waiting on you in its terminal' : '';
  return `Local review: ${[name, ...where, what].join(', ')}${meko}${waiting}`;
}

/** The Code gate's line for a done review: what it found until the task addressed it, then LGTM. */
export function reviewDoneText(r: CodeReviewSummary): string {
  return doneShowsCounts(r) ? `Reviewed: ${countsText(r.counts)}` : 'Reviewed: LGTM';
}

/** `2 blockers, 1 major` (or `no findings`), for messages. */
export function countsText(c: SeverityCounts): string {
  const parts = SEVERITIES.filter((s) => c[s] > 0).map((s) => severityPart(s, c[s]));
  return parts.length ? parts.join(', ') : 'no findings';
}

/**
 * Why a `describing` review waits on the user (its `detail`): the task's PR description turn was
 * interrupted, or its agent exited before taking the ask. Open PR (the gate's Review now) then
 * starts the review, or asks again for a lost ask; a restart leaves either waiting.
 */
export const DESCRIBE_INTERRUPTED = 'you interrupted the PR description turn';
export const DESCRIBE_LOST = "the task's agent exited before taking the ask for the PR description";

/** Why a review stopped when its task left Code: it doesn't hold the next Open PR, which starts a new review. */
export const REVIEW_LEFT_CODE = 'the task left Code';

/** The review failed or was cancelled: its magnifier shows it stopped, whether or not it holds the PR. */
export function reviewStopped(r: Pick<CodeReviewSummary, 'state'> | null | undefined): boolean {
  return !!r && (r.state === 'failed' || r.state === 'cancelled');
}

/** The review holds the task's Open PR for the user to pick Retry or Open anyway. */
export function reviewHolds(r: Pick<CodeReviewSummary, 'state' | 'detail'> | null | undefined): boolean {
  return !!r && (r.state === 'failed' || (r.state === 'cancelled' && r.detail !== REVIEW_LEFT_CODE));
}

/** The review is still working (its lens pulses): its steps run, not the task's fixes. */
export function reviewRunning(r: Pick<CodeReviewSummary, 'state'> | null): boolean {
  return !!r && (r.state === 'preparing' || r.state === 'sweeping' || r.state === 'scoring');
}

/** A scorer's verdict on a candidate (the cloud's score schema). */
export type ScoreVerdict = 'refuted' | 'confirmed' | 'unverified';

/** What the task did with a finding (`review_outcome`), shown in the panel and sent to the next round. */
export interface FindingOutcome {
  status: 'fixed' | 'declined';
  note: string;
}

/** One candidate in the review's findings panel (GET /api/sessions/:id/code-review). */
export interface CodeReviewFinding {
  /** Stable within the review: `<round>:<index>`. */
  key: string;
  round: number;
  sweep: number;
  file: string;
  line: number;
  severity: Severity;
  comment: string;
  /** The passes that raised it; more than one is corroboration. */
  passes: string[];
  /** Meko gotchas it credits (`MG-<n>`). */
  gotchas: string[];
  verdict: ScoreVerdict | null;
  confidence: number | null;
  /** The scorer's reason, or its refutation for a refuted one. */
  reason: string | null;
  /** Kept and sent to the task. */
  kept: boolean;
  /** Why it was dropped (`refuted`, `below 50`, `line is outside the diff`, …); null when kept. */
  dropped: string | null;
  /** The running round's candidate, not yet kept or dropped (sent while sweeping or scoring, never stored). */
  pending?: boolean;
  outcome: FindingOutcome | null;
}

/** GET /api/sessions/:id/code-review: a task's (or a code-review session's) latest review in full. */
export interface CodeReviewDetail {
  review: CodeReviewSummary;
  settings: ReviewSettings;
  /** The datapacks the review's MEKO.md files listed, the gotchas one marked. */
  datapacks: { name: string; id: string; gotchas: boolean }[];
  /** Each round's passes' notes (their summaries when not LGTM). */
  notes: { round: number; text: string }[];
  findings: CodeReviewFinding[];
}
