/**
 * Kode's code review (docs/design/code-review.md): the review strategies, the four
 * settings (strategy, re-review rounds, depth, check) and how a session's, its project's and the global
 * values resolve, the `me_kode_review:` line a PR description carries, and the short label the
 * review's part of a task's tab shows. Used by the server and the web UI.
 */

/** A review strategy's id, stamped on every review and never reused (`STRATEGIES`). */
export type ReviewStrategyId = 'sagittarius-3.0' | 'sirius-1.0';

/** The strategy setting: a strategy, or no code review. */
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
    summary: 'One Opus-level pass that covers all five mandates. No scorers: cheaper and faster.',
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
/** A round's findings wait for the user to note or dismiss them before they go to the task (Run to merge never waits). */
export const DEFAULT_CHECK = true;

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

/** The four settings, as resolved for one review. */
export interface ReviewSettings {
  strategy: ReviewStrategySetting;
  rounds: number;
  depth: number;
  /** A round's findings wait for the user (`triage`) before they go to the task, unless Run to merge is on. */
  check: boolean;
}

/** A project's or a session's overrides: a value left out (or null) keeps the next level's. */
export interface ReviewOverrides {
  strategy?: ReviewStrategySetting | null;
  rounds?: number | null;
  depth?: number | null;
  check?: boolean | null;
}

export const DEFAULT_REVIEW: ReviewSettings = { strategy: DEFAULT_STRATEGY, rounds: DEFAULT_ROUNDS, depth: DEFAULT_DEPTH, check: DEFAULT_CHECK };

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
  if (typeof o.check === 'boolean') out.check = o.check;
  return out;
}

/** True when the overrides set nothing. */
export function noOverrides(o: ReviewOverrides): boolean {
  return o.strategy == null && o.rounds == null && o.depth == null && o.check == null;
}

/** The settings a review runs with: the session's overrides, then the project's, then the global ones. */
export function resolveReview(global: ReviewSettings, project: ReviewOverrides, session: ReviewOverrides): ReviewSettings {
  return {
    strategy: session.strategy ?? project.strategy ?? global.strategy,
    rounds: session.rounds ?? project.rounds ?? global.rounds,
    depth: session.depth ?? project.depth ?? global.depth,
    check: session.check ?? project.check ?? global.check,
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
export const ME_AK_REVIEW = 'me_kode_review:';

/** Its prefix in PRs opened before Kode was called Kode, which is read as this one. */
export const OLD_ME_AK_REVIEW = 'me_ak_review:';

/** The line's prefix, the new or the old one, or null for a line that isn't one. */
function reviewPrefix(line: string): string | null {
  return [ME_AK_REVIEW, OLD_ME_AK_REVIEW].find((p) => line.startsWith(p)) ?? null;
}

/** A strategy as the line names it: its display name without spaces (`Sagittarius3.0`). */
export function strategyTag(id: string): string {
  return strategyName(id).replace(/\s+/g, '');
}

/** What a `me_kode_review:` line says: the findings and, after `;`, the strategy's tag (null on a line without one). */
export interface MeAkReview {
  counts: SeverityCounts;
  strategy: string | null;
}

/**
 * The PR description's line for a finished code review: every round's findings added up, each
 * severity that has some, in the order blocker, major, minor; `LGTM` when no round found any; then
 * `;` and the strategy that ran (`strategyTag`).
 */
export function meAkReviewLine(counts: SeverityCounts, strategy: string): string {
  const parts = SEVERITIES.filter((s) => counts[s] > 0).map((s) => severityPart(s, counts[s]));
  return `${ME_AK_REVIEW}${parts.length ? `${parts.join(',')} addressed` : 'LGTM'};${strategyTag(strategy)}`;
}

/** Reads a `me_kode_review:` line back, with or without its strategy, or null when it isn't one. */
export function parseMeAkReviewLine(line: string): MeAkReview | null {
  const t = line.trim();
  const prefix = reviewPrefix(t);
  if (!prefix) return null;
  const body = t.slice(prefix.length).trim();
  const sm = /^(.*);([^\s;]+)$/.exec(body);
  const rest = (sm ? sm[1] : body).trim();
  const strategy = sm ? sm[2] : null;
  if (rest === 'LGTM') return { counts: noCounts(), strategy };
  const m = /^(.*)\s+addressed$/.exec(rest);
  if (!m) return null;
  const counts = noCounts();
  for (const part of m[1].split(',')) {
    const pm = /^\s*(\d+)\s+(blockers?|major|minor)\s*$/.exec(part);
    if (!pm) return null;
    const sev = (pm[2].startsWith('blocker') ? 'blocker' : pm[2]) as Severity;
    counts[sev] += Number(pm[1]);
  }
  return totalCount(counts) ? { counts, strategy } : null;
}

/** The `me_kode_review:` line in a PR description, or null when it has none. */
export function findMeAkReviewLine(body: string): string | null {
  for (const line of body.split('\n')) if (reviewPrefix(line.trim())) return line.trim();
  return null;
}

/** A PR description with its `me_kode_review:` line set to `line` (replaced, or added at the end). */
export function withMeAkReviewLine(body: string, line: string): string {
  const lines = body.split('\n');
  const i = lines.findIndex((l) => reviewPrefix(l.trim()) !== null);
  if (i >= 0) {
    lines[i] = line;
    return lines.join('\n');
  }
  const trimmed = body.replace(/\s+$/, '');
  return trimmed ? `${trimmed}\n\n${line}` : line;
}

/**
 * Where a code review is (`CodeReviewSummary.state`): `describing` while the task writes the PR's
 * title and description for the passes to read, before the first round; `triage` while a round's
 * kept findings wait for the user to note or dismiss them and send them to the task.
 */
export type CodeReviewState = 'describing' | 'preparing' | 'sweeping' | 'scoring' | 'triage' | 'fixing' | 'done' | 'failed' | 'cancelled';

/** What a task's tab and rail row show of its code review (`Session.codeReview`). */
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
  /**
   * The user ran it (the `review-run` action), not an Open PR: it sends no PR prompt when it's done,
   * and a stopped one holds nothing. An Open PR while it runs takes it over (false from then on).
   */
  onDemand: boolean;
  /** Where it runs: in Code, or in Pull Request on the open PR. Leaving that stage cancels it. */
  stage: 'code' | 'review';
  /** A first look (a workshop's, before its gotchas are in Meko): its findings stay with it, never sent to the task. */
  firstLook?: boolean;
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
function doneShowsCounts(r: Pick<CodeReviewSummary, 'counts' | 'addressed'>): boolean {
  return totalCount(r.counts) > 0 && !r.addressed;
}

/**
 * The review is done and reads LGTM: it kept no findings, or the task said what it did with every
 * one (`addressed`). Its task's tab and rail row then leave its magnifier out (`reviewBadge()`).
 */
export function reviewSettled(r: Pick<CodeReviewSummary, 'state' | 'counts' | 'addressed'> | null | undefined): boolean {
  return !!r && r.state === 'done' && !doneShowsCounts(r);
}

/**
 * The review's part of a task's tab: the short count after the magnifier. Its findings read
 * blockers·majors·minors (`2·1·4`, `severityBadge`).
 */
export function reviewSegmentText(r: CodeReviewSummary): string {
  switch (r.state) {
    case 'failed':
    case 'cancelled':
      return '!';
    case 'done':
      return doneShowsCounts(r) ? severityBadge(r.counts) : 'LGTM';
    case 'triage':
      return severityBadge(r.counts);
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
    case 'triage': what = 'its findings wait on you'; break;
    case 'fixing': what = 'waiting for the fixes'; break;
    case 'done': what = doneShowsCounts(r) ? countsText(r.counts) : totalCount(r.counts) ? `LGTM, ${countsText(r.counts)} addressed` : 'LGTM'; break;
    case 'failed': what = `failed${r.detail ? `: ${r.detail}` : ''}. ${r.onDemand ? 'Retry it' : 'Retry or Open anyway'}`; break;
    case 'cancelled': what = reviewHolds(r) ? 'cancelled. Retry or Open anyway' : r.onDemand && r.detail === YOU_CANCELLED ? 'cancelled. Retry it' : `cancelled: ${r.detail}`; break;
  }
  const meko = r.mekoCall ? ', searching Meko' : '';
  const waiting = r.waiting ? ', waiting on you in its terminal' : '';
  return `Code review: ${[name, ...where, what].join(', ')}${meko}${waiting}`;
}

/** The Code gate's line for a done review: what it found until the task addressed it, then LGTM. */
export function reviewDoneText(r: CodeReviewSummary): string {
  return doneShowsCounts(r) ? `Reviewed: ${countsText(r.counts)}` : 'Reviewed: LGTM';
}

/** `2·1·4`: blockers·majors·minors, zeros included, for the review's part of a task's tab. */
function severityBadge(c: SeverityCounts): string {
  return SEVERITIES.map((s) => c[s]).join('·');
}

/** `2 blockers, 1 major` (or `no findings`), for messages. */
export function countsText(c: SeverityCounts): string {
  const parts = SEVERITIES.filter((s) => c[s] > 0).map((s) => severityPart(s, c[s]));
  return parts.length ? parts.join(', ') : 'no findings';
}

/**
 * `1 caught with Meko (MG-2)`: how many kept findings credit a Meko gotcha, and which gotchas, for
 * the review's done event; '' when none do.
 */
export function caughtWithMeko(findings: Pick<CodeReviewFinding, 'kept' | 'gotchas'>[]): string {
  const credited = findings.filter((f) => f.kept && f.gotchas.length);
  if (!credited.length) return '';
  const ids = [...new Set(credited.flatMap((f) => f.gotchas))];
  return `${credited.length} caught with Meko (${ids.join(', ')})`;
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
/** Why a review run in Pull Request stopped when its task left that stage. */
export const REVIEW_LEFT_REVIEW = 'the task left Pull Request';
/** Why a review stopped when the user cancelled it (the `review-cancel` action). */
export const YOU_CANCELLED = 'you cancelled it';

/** The review failed or was cancelled: its magnifier shows it stopped, whether or not it holds the PR. */
export function reviewStopped(r: Pick<CodeReviewSummary, 'state'> | null | undefined): boolean {
  return !!r && (r.state === 'failed' || r.state === 'cancelled');
}

/**
 * The review holds the task's Open PR for the user to pick Retry or Open anyway. One the user ran
 * (`onDemand`) never does: Open PR starts a review of its own.
 */
export function reviewHolds(r: Pick<CodeReviewSummary, 'state' | 'detail' | 'onDemand'> | null | undefined): boolean {
  return !!r && !r.onDemand && (r.state === 'failed' || (r.state === 'cancelled' && r.detail !== REVIEW_LEFT_CODE));
}

/** A round's findings wait for the user to note or dismiss them, then send them to the task (`triage`). */
export function reviewWaitsOnYou(r: Pick<CodeReviewSummary, 'state'> | null | undefined): boolean {
  return r?.state === 'triage';
}

/** The review is still working (its lens pulses): its steps run, not the task's fixes. */
export function reviewRunning(r: Pick<CodeReviewSummary, 'state'> | null): boolean {
  return !!r && (r.state === 'preparing' || r.state === 'sweeping' || r.state === 'scoring');
}

/** A scorer's verdict on a candidate (the cloud's score schema). */
export type ScoreVerdict = 'refuted' | 'confirmed' | 'unverified';

/**
 * What the user did with a finding while its round waited on them (`triage`): a note for the task,
 * or a dismissal (with an optional note), which leaves it out of the prompt and the counts.
 */
export interface FindingChoice {
  dismissed: boolean;
  note: string;
}

/** The longest note the user can give a finding. */
export const MAX_FINDING_NOTE = 2000;

/** Why a finding the user dismissed was dropped (`CodeReviewFinding.dropped`). */
export const DISMISSED_BY_YOU = 'dismissed by you';

/** What the task did with a finding (`review_outcome`), shown in the panel and sent to the next round. */
export interface FindingOutcome {
  status: 'fixed' | 'declined';
  note: string;
}

/** A finding's credit to a Meko gotcha: its id and the sentence of its body the finding instantiates. */
export interface GotchaCredit {
  id: string;
  quote: string;
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
  /**
   * The same credits with the sentence of each gotcha the finding instantiates, quoted (the UI's
   * Caught with Meko); a review from before they were kept has an empty quote.
   */
  credits: GotchaCredit[];
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
  /** The user's note or dismissal, given while its round waited on them; absent or null when they gave none. */
  you?: FindingChoice | null;
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
