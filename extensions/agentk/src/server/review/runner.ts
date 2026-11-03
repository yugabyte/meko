import { randomBytes } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { basename, join } from 'node:path';
import {
  DEFAULT_STRATEGY, DESCRIBE_INTERRUPTED, DESCRIBE_LOST, DISMISSED_BY_YOU, MAX_FINDING_NOTE, MIN_SWEEPS, REVIEW_LEFT_CODE, REVIEW_LEFT_REVIEW, YOU_CANCELLED, caughtWithMeko, countsText, meAkReviewLine, noCounts,
  resolveReview, reviewHolds, reviewRunning, strategyName, totalCount,
  type CodeReviewDetail, type CodeReviewFinding, type CodeReviewState, type CodeReviewSummary, type FindingChoice, type FindingOutcome, type GotchaCredit,
  type ReviewSettings, type ReviewStrategyId, type SeverityCounts,
} from '../../shared/codereview.js';
import { HARNESS_CAPS, harnessOf, type HarnessId } from '../../shared/harnesses.js';
import { isGotchasDatapack, parseMekoMd } from '../../shared/mekomd.js';
import { parseGithubRef } from '../../shared/github-refs.js';
import { shortRemote } from '../../shared/datapacks.js';
import { globalReview } from '../config.js';
import type { Core } from '../core.js';
import { all, get, run, tx } from '../db.js';
import { git, readRemotes } from '../git.js';
import * as github from '../github.js';
import { repoConfigTrusted } from '../headtrust.js';
import { errMsg } from '../log.js';
import { claudePath } from '../paths.js';
import { HttpError } from '../projects.js';
import type { FixAfter } from '../prompts.js';
import { parseSettings, projectRow, sessionRepo, sessionReview, sessionRow, type SessionRow } from '../store.js';
import { TOOLS_SERVER } from '../tooldefs.js';
import { readDiff } from './diff.js';
import { findGuides, findMekoMds } from './guides.js';
import { readCommits, readHistory } from './history.js';
import { MIN_CONFIDENCE, applyScore, countsOf, keepScored, mergeNotes, mergeSweep, validate } from './merge.js';
import { basePrompt, passPrompt, scoreBase, scorePrompt, stepList, sweepContext } from './prompt.js';
import { checkOutcomes, checkReport, checkScore } from './schemas.js';
import { strategyFor } from './strategies.js';
import type { Candidate, Diff, PassReport, PriorRound, ReviewContext, ReviewDatapack, ReviewIssue, ScoreReport, Tier } from './types.js';

/** A candidate's credits, with the quote each was given (`Finding.quotes`). */
function findingCredits(f: Candidate['finding']): GotchaCredit[] {
  return f.gotchas.map((id) => ({ id, quote: f.quotes?.[id] ?? '' }));
}

/** The review tools' names, as AgentK's MCP server (`agentk_sessions`) has them. */
export const REVIEW_TOOLS = ['review_report', 'review_score', 'review_outcome'] as const;
export type ReviewTool = (typeof REVIEW_TOOLS)[number];

/** The subagents a review session is launched with, one per model tier. */
export const REVIEW_AGENTS: Record<Tier, string> = {
  large: 'agentk-review-large',
  mid: 'agentk-review-mid',
  small: 'agentk-review-small',
};

/** The subagent each scorer runs on: the cloud's scorers run on the small model. */
const SCORER_TIER: Tier = 'small';

/**
 * The most scorers started at once: a round can have dozens of candidates, so they run in batches
 * of this many, each batch once the one before has finished (a sweep's passes all start at once).
 */
const SCORE_BATCH = 5;

/** How many times AgentK asks again for the steps a turn left unreported, before it goes on without them. */
const ASK_AGAIN = 1;

/** The most of a linked GitHub issue's text written for the passes. */
const ISSUE_MAX_BYTES = 64 * 1024;

/** A review row (`code_reviews`). */
interface ReviewRow {
  id: string;
  task_id: string;
  session_id: string | null;
  strategy: string;
  rounds: number;
  depth: number;
  state: CodeReviewState;
  round: number;
  sweep: number;
  asked: number;
  candidates_json: string;
  findings_json: string;
  notes_json: string;
  datapacks_json: string;
  counts_json: string;
  last_kept: number;
  instructions: string | null;
  by: string;
  detail: string | null;
  lead: string | null;
  /** 'pr': an Open PR started it; 'demand': the user ran it (`review-run`), and it sends no PR prompt. */
  purpose: 'pr' | 'demand';
  /** Where it runs: Code, or Review & PR on the open PR. */
  stage: 'code' | 'review';
  /** 1 once the task left Code after it finished with its PR prompt unsent: Open PR doesn't reuse it. */
  stale: number;
  /** 0: its findings stay with it, never sent to the task (a workshop's first look); 1 for the rest. */
  hand_off: number;
  created_at: number;
  updated_at: number;
  finished_at: number | null;
}

/** The running states: a review in one of them holds the task's Open PR. */
const RUNNING: CodeReviewState[] = ['describing', 'preparing', 'sweeping', 'scoring', 'triage', 'fixing'];

/** How a fix prompt ends: in Code or Review & PR, with a re-review to follow (`more`) or not. */
const fixAfter = (stage: 'code' | 'review', more: boolean): FixAfter => (stage === 'review' ? (more ? 'hold-push' : 'push') : more ? 'review-again' : 'open-later');

/** A stopped review holds the task's Open PR (`reviewHolds`): never one the user ran. */
const holds = (r: ReviewRow | undefined) => reviewHolds(r && { ...r, onDemand: r.purpose === 'demand' });

/** A finished Code review whose PR prompt hasn't gone, which Open PR sends with its line rather than reviewing again. */
const unsent = (r: ReviewRow | undefined): boolean => r?.state === 'done' && !r.finished_at && !r.stale && r.stage === 'code';

/** Every round's kept findings, by severity: what the `me_ak_review:` line adds up (`counts_json`). */
function keptCounts(findings: CodeReviewFinding[]): SeverityCounts {
  const counts = noCounts();
  for (const f of findings) if (f.kept) counts[f.severity]++;
  return counts;
}

/**
 * The user's choice for a finding, from a request: `{ dismissed?: boolean, note?: string }` over
 * what it had (`was`), a field left out keeping its value; the note trimmed and clipped.
 */
function readChoice(body: unknown, was: FindingChoice | null | undefined): FindingChoice {
  const b = body && typeof body === 'object' ? (body as Record<string, unknown>) : {};
  if (b.dismissed !== undefined && typeof b.dismissed !== 'boolean') throw new HttpError(400, 'dismissed must be true or false.');
  if (b.note !== undefined && typeof b.note !== 'string') throw new HttpError(400, 'note must be text.');
  const note = typeof b.note === 'string' ? [...b.note.trim()].slice(0, MAX_FINDING_NOTE).join('') : was?.note ?? '';
  return { dismissed: typeof b.dismissed === 'boolean' ? b.dismissed : !!was?.dismissed, note };
}


/** What the subagent definitions say, for each harness that has them (`ReviewLaunch`). */
export interface ReviewAgent {
  name: string;
  tier: Tier;
  description: string;
  prompt: string;
  /** The harness's model for the tier, or '' for the session's own. */
  model: string;
}

/** What a code-review session is launched with beyond any session's launch (`LaunchInput.review`). */
export interface ReviewLaunch {
  agents: ReviewAgent[];
  /** The review's run folder, which every step reads its prompt from. */
  runDir: string;
}

/** The tool's name as a session's agent calls it (each CLI names MCP tools its own way). */
export function agentToolName(harness: HarnessId, tool: string): string {
  if (harness === 'claude') return `mcp__${TOOLS_SERVER}__${tool}`;
  if (harness === 'opencode') return `${TOOLS_SERVER}_${tool}`;
  return `@${TOOLS_SERVER}/${tool}`;
}

const json = <T>(s: string | null, fallback: T): T => {
  try {
    return s ? (JSON.parse(s) as T) : fallback;
  } catch {
    return fallback;
  }
};

/**
 * AgentK's code review (docs/design/code-review.md): one review per Open PR of a task, run in a
 * code-review session (a child of the task, sharing its worktree) that starts each pass and scorer as
 * a fresh step on its model tier. The steps report through AgentK's tools; this class keeps their
 * reports in `code_review_steps`, applies the cloud reviewer's rules (merge, sweeps, the refutation
 * gate, keepScored, Validate) and moves the review on at each of its session's turn ends: sweeps,
 * scoring, then the task fixes the findings (re-review rounds) or opens the PR with the
 * `me_ak_review:` line. A review in a running state holds the task's Open PR; a failed one, or one the
 * user cancelled, holds it for the user's Retry or Open anyway (one cancelled because the task left
 * Code doesn't: `reviewHolds`). The user can also run one (`runNow`), in Code or in Review & PR: it
 * sends no PR prompt, and Open PR then sends the PR with its line rather than reviewing again.
 */
export class CodeReviews {
  /** Reviews being moved on now, so a turn end and a tool call never advance one twice at once. */
  private advancing = new Map<string, Promise<void>>();

  constructor(private core: Core) {}

  private get db() {
    return this.core.db;
  }

  // ---- reads ----

  /** The settings a task's review runs with: its own, then its project's, then the global ones. */
  settingsFor(task: SessionRow): ReviewSettings {
    const p = projectRow(this.db, task.project_id);
    const project = p ? parseSettings(p.settings_json).codeReview : {};
    return resolveReview(globalReview(this.core.config), project, sessionReview(task));
  }

  private latest(taskId: string): ReviewRow | undefined {
    return get<ReviewRow>(this.db, 'SELECT * FROM code_reviews WHERE task_id = ? ORDER BY created_at DESC LIMIT 1', taskId);
  }

  private review(id: string): ReviewRow | undefined {
    return get<ReviewRow>(this.db, 'SELECT * FROM code_reviews WHERE id = ?', id);
  }

  private bySession(sessionId: string): ReviewRow | undefined {
    return get<ReviewRow>(this.db, 'SELECT * FROM code_reviews WHERE session_id = ? ORDER BY created_at DESC LIMIT 1', sessionId);
  }

  /** A session's review: a task's latest, or a code-review session's own; null when it has none. */
  private reviewOf(sessionId: string): ReviewRow | undefined {
    const row = sessionRow(this.db, sessionId);
    if (!row) return undefined;
    return row.kind === 'code-review' ? this.bySession(sessionId) : this.latest(sessionId);
  }

  /** `Session.codeReview`. */
  summary(sessionId: string): CodeReviewSummary | null {
    const r = this.reviewOf(sessionId);
    return r ? this.summarize(r) : null;
  }

  private summarize(r: ReviewRow): CodeReviewSummary {
    const strategy = strategyFor(r.strategy);
    const sweepSteps = this.steps(r.id, `r${r.round}s${r.sweep}:`);
    const scoreSteps = this.steps(r.id, `r${r.round}c`);
    const cands = json<Candidate[]>(r.candidates_json, []);
    return {
      id: r.id,
      // Once the session is gone (done, cancelled, replaced), nothing runs in it.
      sessionId: r.session_id && sessionRow(this.db, r.session_id)?.desired === 'running' ? r.session_id : null,
      strategy: strategy.id,
      state: r.state,
      round: r.round,
      rounds: r.rounds + 1,
      sweep: r.sweep,
      depth: r.depth,
      passesDone: sweepSteps.length,
      passes: strategy.passes.length,
      scoresDone: scoreSteps.length,
      candidates: cands.length,
      counts: json<SeverityCounts>(r.counts_json, noCounts()),
      // Every round's: the next round runs at the fix turn's end whether or not the task said what it did.
      addressed: r.state === 'done' && json<CodeReviewFinding[]>(r.findings_json, []).every((f) => !f.kept || !!f.outcome),
      mekoCall: !!r.session_id && RUNNING.includes(r.state) && this.core.sessions.mekoCalling(r.session_id),
      // A permission prompt or a question in the review's terminal, which has no tab to show it. Only
      // while its steps run: in fixing the UI opens the findings and has no terminal button.
      waiting: !!r.session_id && reviewRunning(r) && sessionRow(this.db, r.session_id)?.status === 'needs_input',
      detail: r.detail,
      onDemand: r.purpose === 'demand',
      ...(r.hand_off ? {} : { firstLook: true }),
      stage: r.stage,
    };
  }

  /** GET /api/sessions/:id/code-review. */
  detail(sessionId: string): CodeReviewDetail {
    const r = this.reviewOf(sessionId);
    if (!r) throw new HttpError(404, 'This session has no code review.');
    // A review from before the credits' quotes were kept has its ids only.
    const findings = json<CodeReviewFinding[]>(r.findings_json, []).map((f) => ({ ...f, credits: f.credits ?? f.gotchas.map((id) => ({ id, quote: '' })) }));
    // The current round's candidates, before they're kept or dropped.
    if (r.state === 'sweeping' || r.state === 'scoring') findings.push(...this.pendingFindings(r));
    return {
      review: this.summarize(r),
      settings: { strategy: r.strategy as ReviewStrategyId, rounds: r.rounds, depth: r.depth, check: this.settingsFor(this.core.sessions.row(r.task_id)).check },
      datapacks: json<ReviewDatapack[]>(r.datapacks_json, []).map((d) => ({ name: d.name, id: d.id, gotchas: d.gotchas })),
      notes: json<{ round: number; text: string }[]>(r.notes_json, []),
      findings,
    };
  }

  private pendingFindings(r: ReviewRow): CodeReviewFinding[] {
    const scores = new Map(this.steps(r.id, `r${r.round}c`).map((s) => [s.step, s.report as ScoreReport]));
    return json<Candidate[]>(r.candidates_json, []).map((c, i) => {
      const s = scores.get(`r${r.round}c${i + 1}`);
      return {
        key: `${r.round}:${i + 1}`, round: r.round, sweep: c.sweep, file: c.finding.file, line: c.finding.line, severity: c.finding.severity,
        comment: c.finding.comment, passes: c.passes, gotchas: c.finding.gotchas, credits: findingCredits(c.finding), verdict: s?.verdict ?? null, confidence: s?.confidence ?? null,
        reason: s ? (s.verdict === 'refuted' ? s.refutation : s.reason) : null, kept: false, dropped: null, pending: true, outcome: null,
      };
    });
  }

  /** A review in a running state holds the task's Open PR. */
  running(taskId: string): boolean {
    const r = this.latest(taskId);
    return !!r && RUNNING.includes(r.state);
  }

  /** The task's latest review runs, and it started in `stage` (a merge waits for one in Review & PR). */
  runningIn(taskId: string, stage: 'code' | 'review'): boolean {
    const r = this.latest(taskId);
    return !!r && r.stage === stage && RUNNING.includes(r.state);
  }

  /** The task's latest review, from `stage`, waits for the user to send its findings (`triage`). */
  waitsOnYouIn(taskId: string, stage: 'code' | 'review'): boolean {
    const r = this.latest(taskId);
    return r?.state === 'triage' && r.stage === stage;
  }

  /** The PR line a finished review gives the task's PR, or null when its last review didn't finish (or none ran). */
  prLine(taskId: string): string | null {
    const r = this.latest(taskId);
    // A stale one reviewed the change as it was before the task left Code.
    return r?.state === 'done' && !r.stale ? meAkReviewLine(json<SeverityCounts>(r.counts_json, noCounts()), r.strategy) : null;
  }

  // ---- starting ----

  /**
   * Open PR (or a move to Review & PR with work to push) on a task: true when a review now holds it
   * (one just started, or one already running), false when the PR prompt may go (review Off, not a
   * task). A finished review whose PR prompt didn't go sends it now (unless a queued Open PR, `queued`,
   * is withdrawn meanwhile), one the user ran included, with the strategy Off too; a stopped one
   * waiting on Retry or Open anyway answers 409 (checkNotHeld). A review the user ran that's still
   * running is taken over: it sends the PR prompt when it's done.
   */
  async openPr(task: SessionRow, opts: { instructions: string; by: 'you' | 'auto'; lead?: string; queued?: boolean }): Promise<boolean> {
    if (task.kind === 'code-review' || task.kind === 'review-pr') return false;
    const last = this.latest(task.id);
    if (last && RUNNING.includes(last.state)) {
      if (last.purpose === 'demand') {
        this.set(last.id, { purpose: 'pr' });
        this.core.sessions.event(task.id, 'review', 'The PR prompt goes when the code review you ran is done');
      }
      // Pressed again while it runs: the instructions given now go with the PR, and a move's line before the next prompt.
      if (opts.instructions) this.set(last.id, { instructions: opts.instructions });
      if (opts.lead) this.set(last.id, { lead: opts.lead });
      // The user's press after Run to merge's: the PR goes as theirs (Run to merge stopped at the interrupt).
      if (opts.by === 'you' && last.by !== 'you') this.set(last.id, { by: 'you' });
      // Open PR (or a move) is only taken between turns: a review still waiting for the task's PR
      // description or fixes then had that turn interrupted (or its ask lost), and the press says to go on.
      if (last.state === 'describing') this.kick(last.id, () => (last.detail === DESCRIBE_LOST ? this.describe(last.id) : this.beginRound(last.id)));
      // Not while its fix prompt waits in the queue (one the user ran): the end of the turn that takes it moves it on.
      if (last.state === 'fixing' && !this.core.lifecycle.codeReviewPending(task.id)) this.kick(last.id, () => this.nextRound(last.id));
      return true;
    }
    this.checkNotHeld(task.id);
    // A finished review whose PR prompt didn't go (Claude was busy, or the user ran it): the PR goes
    // with its line now. One the user ran gave the task its last findings as a fix prompt; one still
    // waiting in the queue (Claude lost it, or never took it) goes with the PR prompt instead.
    if (last && unsent(last)) {
      this.set(last.id, { instructions: opts.instructions || last.instructions, lead: opts.lead || last.lead, by: opts.by === 'you' ? 'you' : last.by });
      if (last.purpose === 'demand') {
        const untaken = this.core.lifecycle.codeReviewPending(task.id);
        this.core.lifecycle.clearCodeReview(task.id);
        await this.sendPr(last.id, opts.queued, untaken);
      } else await this.finish(last.id, opts.queued);
      return true;
    }
    const settings = this.settingsFor(task);
    if (settings.strategy === 'off') return false;
    await this.start(task, settings, opts);
    return true;
  }

  /**
   * A review runs at the task's Open PR, or one already ran for it: a task whose review settings
   * aren't Off, whose finished review Open PR sends the PR with, or whose review in Code still runs
   * (Open PR takes it over, one the user ran with the strategy Off included).
   */
  applies(task: SessionRow): boolean {
    const last = this.latest(task.id);
    return task.kind !== 'code-review' && task.kind !== 'review-pr'
      && (this.settingsFor(task).strategy !== 'off' || unsent(last) || (!!last && last.stage === 'code' && RUNNING.includes(last.state)));
  }

  /**
   * Refuses while a stopped review holds the task's Open PR for Retry or Open anyway. One cancelled
   * because the task left Code doesn't: the next Open PR (or move to Review & PR) starts a new one.
   */
  checkNotHeld(taskId: string): void {
    if (holds(this.latest(taskId))) throw new HttpError(409, 'The code review stopped. Retry it, or open the PR anyway.');
  }

  /**
   * The `review-run` action: the task's code review runs now, at the user's request, in Code (the
   * task writes its PR description first, as for Open PR) or in Review & PR (on the open PR, with its
   * title and description). With the strategy Off it runs the default one. It sends no PR prompt:
   * Open PR sends the PR with its line later.
   *
   * `by`: the user (the review-run action), or AgentK itself (a workshop's level, src/server/workshops.ts).
   * `describe: false` skips the PR description the task writes first, as a workshop's project always
   * does, since it opens no PR: nothing goes to the task until the findings (queued for its turn's
   * end), so it may be mid-turn. `handOff: false` keeps the findings with the review: shown, never
   * sent to the task.
   */
  async runNow(task: SessionRow, by: 'you' | 'auto' = 'you', o: { describe?: boolean; handOff?: boolean } = {}): Promise<void> {
    const describe = o.describe !== false && !projectRow(this.core.db, task.project_id)?.workshop;
    if (task.kind === 'code-review' || task.kind === 'review-pr') throw new HttpError(409, 'Only a task has a code review.');
    if (task.desired !== 'running') throw new HttpError(409, 'This session is archived.');
    if (task.stage !== 'code' && task.stage !== 'review') throw new HttpError(409, 'The code review runs in Code, or in Review & PR once the PR is open.');
    if (task.stage === 'review' && !task.pr_number) throw new HttpError(409, "AgentK hasn't found the PR yet. Try again once it has.");
    const last = this.latest(task.id);
    if (last && RUNNING.includes(last.state)) throw new HttpError(409, 'A code review is already running.');
    if (this.core.lifecycle.codeReviewPending(task.id)) {
      throw new HttpError(409, "AgentK hasn't taken the last code review's fixes yet. Run it again once it has.");
    }
    if (describe) this.core.lifecycle.checkPromptable(task);
    const settings = this.settingsFor(task);
    // A stopped review it replaces may hold the Open PR's instructions and a move's `moved` line: they go on with this one.
    const held = task.stage === 'code' && last && holds(last) && !last.finished_at ? last : undefined;
    await this.start(task, settings.strategy === 'off' ? { ...settings, strategy: DEFAULT_STRATEGY } : settings,
      { instructions: held?.instructions ?? '', lead: held?.lead ?? undefined, by, purpose: 'demand', stage: task.stage, describe, handOff: o.handOff !== false });
  }

  /**
   * An Open PR queued during a turn of a review the user ran in Code (its PR description or fixes)
   * takes it over at the turn's end: it sends the PR prompt when it's done, with the queued
   * instructions, as a press between turns would (openPr).
   */
  takeOver(taskId: string, opts: { instructions: string; by: 'you' | 'auto' }): void {
    const r = this.latest(taskId);
    if (!r || r.purpose !== 'demand' || r.stage !== 'code' || !RUNNING.includes(r.state)) return;
    this.set(r.id, { purpose: 'pr', instructions: opts.instructions || r.instructions, by: opts.by });
    this.core.sessions.event(taskId, 'review', 'The PR prompt goes when the code review you ran is done');
  }

  private async start(task: SessionRow, settings: ReviewSettings, opts: {
    instructions: string; by: 'you' | 'auto'; lead?: string; purpose?: 'pr' | 'demand'; stage?: 'code' | 'review'; describe?: boolean; handOff?: boolean;
  }): Promise<void> {
    if (settings.strategy === 'off') return;
    const id = `${task.id}--review-${randomBytes(4).toString('hex')}`;
    const now = Date.now();
    const purpose = opts.purpose ?? 'pr';
    const stage = opts.stage ?? 'code';
    // In Code the task writes the PR description first, unless there's no PR to describe.
    const describe = stage === 'code' && opts.describe !== false;
    // An earlier review's session goes: the task's tab shows only the latest.
    const prev = this.latest(task.id);
    // In Review & PR the PR has its title and description already: the first round starts at once.
    run(this.db, `INSERT INTO code_reviews(id, task_id, strategy, rounds, depth, state, instructions, by, lead, purpose, stage, hand_off, created_at, updated_at)
                  VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, task.id, settings.strategy, settings.rounds, settings.depth, describe ? 'describing' : 'preparing',
    opts.instructions || null, opts.by, opts.lead || null, purpose, stage, opts.handOff === false ? 0 : 1, now, now);
    const how = `${strategyName(settings.strategy)}${settings.depth > 1 ? `, depth ${settings.depth}` : ''}${settings.rounds ? `, up to ${settings.rounds} re-review${settings.rounds === 1 ? '' : 's'}` : ''}`;
    const started = purpose === 'pr' ? 'Code review started' : opts.handOff === false ? 'AgentK ran a first look (its findings stay with the review)'
      : opts.by === 'auto' ? 'AgentK ran the code review' : 'You ran the code review';
    this.core.sessions.event(task.id, 'review', `${started} (${how})`);
    this.core.changed();
    if (prev?.session_id) void this.archiveSession(prev.session_id);
    this.kick(id, () => (describe ? this.describe(id) : this.beginRound(id)));
  }

  /**
   * Before the first round, the task writes the PR's title and description, Meko lines included, to
   * its PR description file: the passes check the change against it, as the cloud's do a PR's, and
   * the PR prompt opens the PR with it. The task's turn's end starts the first round (onTurnEnd).
   */
  private async describe(id: string): Promise<void> {
    const r = this.mustReview(id);
    const task = this.core.sessions.row(r.task_id);
    if (r.state !== 'describing' || r.detail) this.set(id, { state: 'describing', detail: null });
    // An earlier review's description would pass for this one's if the task didn't write it.
    rmSync(this.core.sessions.prDescriptionPath(task), { force: true });
    await this.core.lifecycle.sendPrDescription(task, { moved: r.lead, lost: () => this.describeLost(id) });
    if (r.lead) this.set(id, { lead: null });
  }

  /** The task's agent exited before taking the ask for the PR description: the review waits, and Open PR asks again. */
  private describeLost(id: string): void {
    const r = this.review(id);
    if (r?.state !== 'describing') return;
    this.set(id, { detail: DESCRIBE_LOST });
    this.core.sessions.event(r.task_id, 'review', `Code review waits: ${DESCRIBE_LOST}. Review now asks again.`);
    this.core.sessions.notifyUser(r.task_id, 'Code review waits', `${DESCRIBE_LOST[0].toUpperCase()}${DESCRIBE_LOST.slice(1)}. Once it's back, press Review now to ask again.`);
  }

  /** The task's PR description file, as Claude is told it, once the task has written it; null before. */
  private descriptionFile(task: SessionRow): string | null {
    return existsSync(this.core.sessions.prDescriptionPath(task)) ? this.core.sessions.prDescriptionFile(task) : null;
  }

  /** The PR title (its first line) and description the task wrote, or null without one. basePrompt clips and quotes it. */
  private readDescription(task: SessionRow): string | null {
    try {
      return readFileSync(this.core.sessions.prDescriptionPath(task), 'utf8').trim() || null;
    } catch {
      return null;
    }
  }

  /** Runs `fn` for a review once nothing else is moving it on; failures fail the review. */
  private kick(id: string, fn: () => Promise<void>): void {
    const before = this.advancing.get(id) ?? Promise.resolve();
    const next = before.then(fn).catch((e: unknown) => {
      try {
        this.fail(id, errMsg(e));
      } catch (err) {
        // Shutting down: the database is closed, and recover() picks the review up at the next start.
        this.core.log.warn(`code review ${id} stopped: ${errMsg(e)}`, errMsg(err));
      }
    });
    this.advancing.set(id, next);
    void next.finally(() => {
      if (this.advancing.get(id) === next) this.advancing.delete(id);
    });
  }

  /** Waits for whatever is moving a review on (tests, archive). */
  async settled(id: string): Promise<void> {
    await this.advancing.get(id);
  }

  // ---- rounds and sweeps ----

  private runDir(r: ReviewRow, round = r.round): string {
    return join(this.core.paths.run, 'reviews', r.id, `r${round}`);
  }

  /** A round starts: the change is read again (the task may have fixed things), and its first sweep goes out. */
  private async beginRound(id: string): Promise<void> {
    let r = this.mustReview(id);
    // Asked for twice (the description turn's end and an Open PR right after it): the first one started it.
    if (r.state !== 'describing' && r.state !== 'preparing') return;
    if (this.taskMovedOn(r)) return;
    if (r.state === 'describing') {
      this.set(id, { state: 'preparing', detail: null });
      r = this.mustReview(id);
    }
    const task = this.core.sessions.row(r.task_id);
    const dir = this.runDir(r);
    rmSync(dir, { recursive: true, force: true });
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    const { ctx, diff } = await this.readChange(r, task, dir);
    // Cancelled while the change was read (the user, or the task leaving Code): it stays cancelled.
    if (this.mustReview(id).state !== 'preparing') return;
    if (!diff.files.length) {
      // Nothing to review: the PR goes as it would have.
      this.set(id, { state: 'done', detail: 'nothing changed against the base' });
      this.core.sessions.event(task.id, 'review', 'Code review: nothing changed against the base, so nothing to review');
      await this.finish(id);
      return;
    }
    writeFileSync(join(dir, 'base.md'), basePrompt(ctx, diff));
    writeFileSync(join(dir, 'context.json'), JSON.stringify({ ctx, diff: { ...diff, patch: '' } }));
    this.set(id, { state: 'sweeping', sweep: 1, asked: 0, candidates_json: '[]', datapacks_json: JSON.stringify(ctx.datapacks) });
    await this.sendSweep(id);
  }

  /** What the passes read: the diff from the merge-base to the working tree, and everything around it. */
  private async readChange(r: ReviewRow, task: SessionRow, dir: string): Promise<{ ctx: ReviewContext; diff: Diff }> {
    const wt = task.worktree_path;
    const base = await this.baseRef(task);
    const diff = await readDiff(wt, base, dir);
    // The passes read each patch too large to inline by its path: as Claude is told every path.
    for (const f of diff.files) if (f.patchPath) f.patchPath = claudePath(f.patchPath);
    const mergeBase = (await git(wt, 'merge-base', base, 'HEAD')).trim();
    const head = (await git(wt, 'rev-parse', 'HEAD')).trim();
    const dirty = !!(await git(wt, 'status', '--porcelain')).trim();
    const paths = diff.files.map((f) => f.path);
    const trusted = repoConfigTrusted(task);
    const datapacks = trusted && this.core.mekoStatus.configured ? this.datapacks(wt, paths) : [];
    const repo = sessionRepo(this.db, task);
    // In Review & PR, the open PR's own title and description.
    const prDescription = r.stage === 'review' ? await this.core.lifecycle.prText(task) : this.readDescription(task);
    return {
      diff,
      ctx: {
        repo: await this.repoName(repo ?? wt),
        branch: task.branch,
        baseRef: base,
        baseSha: mergeBase,
        headSha: head,
        dirty,
        title: task.name,
        prDescription,
        description: task.initial_prompt ?? '',
        design: this.design(task),
        commits: await readCommits(wt, mergeBase),
        guides: findGuides(wt, paths),
        history: await readHistory(wt, mergeBase, diff),
        issues: await this.issues(task, dir),
        datapacks,
        prior: this.prior(r),
        // As Claude is told every path: Claude Code asks before reading through /net.
        worktree: claudePath(wt),
        runDir: claudePath(dir),
      },
    };
  }

  /** The repo's GitHub `owner/name` from its origin, else its folder's name. */
  private async repoName(repo: string): Promise<string> {
    const origin = (await readRemotes(repo).catch(() => null))?.origin;
    return origin ? shortRemote(origin).split('/').slice(-2).join('/') : basename(repo);
  }

  /** The base the change is read against: origin's copy of the base branch when there is one, else the local branch. */
  private async baseRef(task: SessionRow): Promise<string> {
    for (const ref of [`origin/${task.base_branch}`, task.base_branch]) {
      try {
        await git(task.worktree_path, 'rev-parse', '--verify', '--quiet', `${ref}^{commit}`);
        return ref;
      } catch {
        /* not this one */
      }
    }
    return task.base_branch;
  }

  /** The datapacks the MEKO.md files over the change list, each once, the gotchas ones marked. */
  private datapacks(wt: string, paths: string[]): ReviewDatapack[] {
    const seen = new Map<string, ReviewDatapack>();
    for (const rel of findMekoMds(wt, paths)) {
      let text: string;
      try {
        text = readFileSync(join(wt, rel), 'utf8');
      } catch {
        continue;
      }
      for (const d of parseMekoMd(text)) if (!seen.has(d.id)) seen.set(d.id, { ...d, gotchas: isGotchasDatapack(d.name) });
    }
    return [...seen.values()];
  }

  /** Where the task's design is: its Artifact's link, else its file; null without one. */
  private design(task: SessionRow): string | null {
    const design = get<{ artifact_url: string | null }>(this.db, 'SELECT artifact_url FROM designs WHERE session_id = ?', task.id);
    return design ? (design.artifact_url ?? this.core.sessions.designFile(task)) : null;
  }

  /**
   * The task's linked issues (its work item's links): a GitHub issue's text written to the run
   * folder for the passes to read, as the cloud does; any other link named by its title.
   */
  private async issues(task: SessionRow, dir: string): Promise<ReviewIssue[]> {
    const links = all<{ url: string | null; label: string }>(this.db,
      `SELECT l.url, l.label FROM work_item_links l JOIN work_items w ON w.id = l.item_id WHERE w.session_id = ? ORDER BY l.ord`, task.id);
    const out: ReviewIssue[] = [];
    for (const l of links) {
      if (!l.url) continue;
      const ref = parseGithubRef(l.url);
      if (ref?.kind === 'issue') {
        try {
          const issue = await github.issueOrPr(task.worktree_path, ref.repo, ref.number);
          const text = `# ${issue.title}\n\n${issue.body}`;
          const path = join(dir, 'issues', `${ref.repo.replace('/', '-')}-${ref.number}.md`);
          mkdirSync(join(dir, 'issues'), { recursive: true });
          writeFileSync(path, text.slice(0, ISSUE_MAX_BYTES));
          out.push({ title: l.label, path, url: l.url });
          continue;
        } catch (e) {
          this.core.log.warn(`reading ${l.url} for ${task.id}'s review failed`, errMsg(e));
        }
      }
      out.push({ title: l.label, path: null, url: l.url });
    }
    return out;
  }

  /** The earlier rounds of this review, with what the task did with each finding, and what the user said or dismissed. */
  private prior(r: ReviewRow): PriorRound[] {
    const findings = json<CodeReviewFinding[]>(r.findings_json, []).filter((f) => (f.kept || f.dropped === DISMISSED_BY_YOU) && f.round < r.round);
    const rounds = [...new Set(findings.map((f) => f.round))];
    return rounds.map((round) => ({
      round,
      findings: findings.filter((f) => f.round === round).map((f) => ({
        file: f.file, line: f.line, severity: f.severity, comment: f.comment, outcome: f.outcome, dismissed: !f.kept, note: f.you?.note ?? '',
      })),
    }));
  }

  /** The base prompt and context of the review's current round, from its run folder. */
  private loadRound(r: ReviewRow): { base: string; ctx: ReviewContext; diff: Diff } {
    const dir = this.runDir(r);
    const base = readFileSync(join(dir, 'base.md'), 'utf8');
    const saved = json<{ ctx: ReviewContext; diff: Diff }>(readFileSync(join(dir, 'context.json'), 'utf8'), { ctx: null as unknown as ReviewContext, diff: null as unknown as Diff });
    return { base, ctx: saved.ctx, diff: saved.diff };
  }

  /** Sends the current sweep's passes to the review session (starting it on the first), or only those still missing. */
  private async sendSweep(id: string, missing?: string[]): Promise<void> {
    const r = this.mustReview(id);
    const task = this.core.sessions.row(r.task_id);
    const strategy = strategyFor(r.strategy);
    const { base } = this.loadRound(r);
    const dir = this.runDir(r);
    const ctx = sweepContext(r.sweep, r.depth, json<Candidate[]>(r.candidates_json, []));
    const harness = harnessOf(task.harness);
    const passes = strategy.passes.filter((p) => !missing || missing.includes(p.name));
    const steps = passes.map((p) => {
      const step = `r${r.round}s${r.sweep}:${p.name}`;
      const file = join(dir, `s${r.sweep}-${p.name}.md`);
      writeFileSync(file, passPrompt(base, p, ctx, { tool: agentToolName(harness, 'review_report'), step }));
      return { name: p.name, tier: p.tier, file };
    });
    await this.sendSteps(r, task, 'code-review-sweep', steps, {
      what: `sweep ${r.sweep} of ${r.depth}${r.rounds ? `, round ${r.round} of ${r.rounds + 1}` : ''}`,
      tool: agentToolName(harness, 'review_report'),
    });
  }

  /** Sends the scorers for the current round's candidates (or only those still missing). */
  private async sendScores(id: string, missing?: number[]): Promise<void> {
    const r = this.mustReview(id);
    const task = this.core.sessions.row(r.task_id);
    const { ctx, diff } = this.loadRound(r);
    const dir = this.runDir(r);
    const base = scoreBase(ctx, diff);
    const harness = harnessOf(task.harness);
    const cands = json<Candidate[]>(r.candidates_json, []);
    const steps = cands.map((c, i) => ({ c, n: i + 1 })).filter(({ n }) => !missing || missing.includes(n)).map(({ c, n }) => {
      const file = join(dir, `score-${n}.md`);
      writeFileSync(file, scorePrompt(base, c, { tool: agentToolName(harness, 'review_score'), step: `r${r.round}c${n}` }));
      return { name: `score ${n}`, tier: SCORER_TIER, file };
    });
    await this.sendSteps(r, task, 'code-review-score', steps, {
      what: `${cands.length} finding${cands.length === 1 ? '' : 's'}`,
      tool: agentToolName(harness, 'review_score'),
      batch: SCORE_BATCH,
    });
  }

  /**
   * Gives the review session its steps: in a harness with subagents, one prompt that runs each as
   * a fresh subagent on its tier, listed in batches of `v.batch` when it's set; in one without
   * (`turns`), each step as a prompt of its own after clearing the conversation, the model for its
   * tier set first when one is configured.
   */
  private async sendSteps(r: ReviewRow, task: SessionRow, name: 'code-review-sweep' | 'code-review-score', steps: { name: string; tier: Tier; file: string }[], v: { what: string; tool: string; batch?: number }): Promise<void> {
    const harness = harnessOf(task.harness);
    const read = (file: string) => `Read ${claudePath(file)} and do what it says.`;
    if (HARNESS_CAPS[harness].reviewSteps === 'turns') {
      const session = await this.ensureSession(r, task, null);
      const models = this.core.config.codeReview.models[harness];
      for (const s of steps) {
        await this.core.sessions.submit(session.id, '/clear');
        if (models[s.tier]) await this.core.sessions.submit(session.id, `/model ${models[s.tier]}`);
        await this.core.sessions.submit(session.id, read(s.file));
      }
      return;
    }
    const list = stepList(steps.map((s) => ({ name: s.name, agent: REVIEW_AGENTS[s.tier], prompt: read(s.file) })), v.batch);
    const session = sessionRow(this.db, r.session_id ?? '');
    const text = this.core.sessions.prompt(session ?? task, name, { steps: list, what: v.what, reportTool: v.tool, task: task.name, branch: task.branch, base: task.base_branch });
    if (!session || session.desired !== 'running') {
      await this.ensureSession(r, task, text);
      return;
    }
    await this.core.sessions.submit(session.id, text);
  }

  /** The review session, started now (with `prompt` as its first) when the review has none running. */
  private async ensureSession(r: ReviewRow, task: SessionRow, prompt: string | null): Promise<SessionRow> {
    const cur = r.session_id ? sessionRow(this.db, r.session_id) : undefined;
    if (cur?.desired === 'running') {
      if (prompt) await this.core.sessions.submit(cur.id, prompt);
      return cur;
    }
    const row = await this.core.sessions.startReviewSession(task, r.id, prompt);
    this.set(r.id, { session_id: row.id });
    return row;
  }

  /** What a code-review session is launched with: its task's latest review's (at every launch, a resume's too). */
  launchForSession(row: SessionRow): ReviewLaunch | null {
    const task = row.parent_id ? sessionRow(this.db, row.parent_id) : undefined;
    const r = task ? this.latest(task.id) : undefined;
    return task && r ? this.launchFor(r, task) : null;
  }

  /** The subagents a review session gets, one per tier, on the harness's models. */
  launchFor(r: { id: string }, task: SessionRow): ReviewLaunch {
    const harness = harnessOf(task.harness);
    const models = this.core.config.codeReview.models[harness];
    const tiers: Tier[] = ['large', 'mid', 'small'];
    return {
      runDir: join(this.core.paths.run, 'reviews', r.id),
      agents: tiers.map((tier) => ({
        name: REVIEW_AGENTS[tier],
        tier,
        model: models[tier],
        description: `One step of AgentK's code review on the ${tier} model: a review pass or a scorer. Use only when AgentK's prompt names it.`,
        prompt: 'You are one step of AgentK\'s code review. Do exactly what the file your prompt names says, report with the AgentK tool it names, and change no file.',
      })),
    };
  }

  // ---- reports ----

  /**
   * A review tool call (`review_report`, `review_score` from the review session; `review_outcome`
   * from the task): checked against its schema and kept. The text goes back to the agent; an error
   * says what to fix, and the agent calls again.
   */
  onTool(row: SessionRow, tool: ReviewTool, input: Record<string, unknown>): { text: string; error: boolean } {
    if (tool === 'review_outcome') return this.onOutcome(row, input);
    const r = row.kind === 'code-review' ? this.bySession(row.id) : undefined;
    if (!r || !RUNNING.includes(r.state)) return { text: 'This session has no code review running.', error: true };
    const step = typeof input.step === 'string' ? input.step.trim() : '';
    if (tool === 'review_report') {
      const m = /^r(\d+)s(\d+):([a-z-]+)$/.exec(step);
      const pass = m && Number(m[1]) === r.round && Number(m[2]) === r.sweep ? strategyFor(r.strategy).passes.find((p) => p.name === m[3]) : undefined;
      if (r.state !== 'sweeping' || !pass) return { text: `"${step}" isn't a pass this review is waiting for. Use the step your file names.`, error: true };
      const checked = checkReport(pass, input.report);
      if ('error' in checked) return { text: `The report doesn't fit the schema: ${checked.error}. Fix it and call ${tool} again.`, error: true };
      const report: PassReport = { pass: pass.name, round: r.round, sweep: r.sweep, summary: checked.summary, findings: checked.findings };
      this.keep(r.id, step, report);
      return { text: `AgentK has the ${pass.name} pass's report (${report.findings.length} finding${report.findings.length === 1 ? '' : 's'}).`, error: false };
    }
    const m = /^r(\d+)c(\d+)$/.exec(step);
    const n = m && Number(m[1]) === r.round ? Number(m[2]) : 0;
    const cands = json<Candidate[]>(r.candidates_json, []);
    if (r.state !== 'scoring' || n < 1 || n > cands.length) return { text: `"${step}" isn't a finding this review is scoring. Use the step your file names.`, error: true };
    const checked = checkScore(input.score);
    if ('error' in checked) return { text: `The score doesn't fit the schema: ${checked.error}. Fix it and call ${tool} again.`, error: true };
    this.keep(r.id, step, checked);
    return { text: `AgentK has the score for finding ${n}.`, error: false };
  }

  /** What the task did with each finding of a round (`review_outcome`), for the panel and the next round. */
  private onOutcome(row: SessionRow, input: Record<string, unknown>): { text: string; error: boolean } {
    const r = row.kind === 'code-review' ? undefined : this.latest(row.id);
    if (!r) return { text: 'This task has no code review.', error: true };
    const checked = checkOutcomes(input);
    if ('error' in checked) return { text: `The outcomes don't fit the schema: ${checked.error}. Fix them and call review_outcome again.`, error: true };
    const findings = json<CodeReviewFinding[]>(r.findings_json, []);
    const unknown: string[] = [];
    for (const o of checked) {
      const f = findings.find((x) => x.key === o.key && x.kept);
      if (!f) unknown.push(o.key);
      else f.outcome = { status: o.status, note: o.note } satisfies FindingOutcome;
    }
    this.set(r.id, { findings_json: JSON.stringify(findings) });
    this.core.changed();
    return unknown.length
      ? { text: `AgentK kept the rest; it has no finding ${unknown.join(', ')}. Use the keys from AgentK's list.`, error: true }
      : { text: `AgentK has what you did with ${checked.length} finding${checked.length === 1 ? '' : 's'}.`, error: false };
  }

  private keep(reviewId: string, step: string, report: unknown): void {
    run(this.db, `INSERT INTO code_review_steps(review_id, step, report_json, at) VALUES (?, ?, ?, ?)
                  ON CONFLICT(review_id, step) DO UPDATE SET report_json = excluded.report_json, at = excluded.at`, reviewId, step, JSON.stringify(report), Date.now());
    this.set(reviewId, {});
    this.core.changed();
  }

  private steps(reviewId: string, prefix: string): { step: string; report: unknown }[] {
    return all<{ step: string; report_json: string }>(this.db, 'SELECT step, report_json FROM code_review_steps WHERE review_id = ? AND step LIKE ?', reviewId, `${prefix}%`)
      .map((s) => ({ step: s.step, report: json<unknown>(s.report_json, null) }));
  }

  // ---- turn ends ----

  /**
   * A turn ended in a code-review session, or in a task whose review waits for its fixes: the review
   * moves on. True when the turn was the review's (so nothing else acts on it).
   */
  async onTurnEnd(row: SessionRow, interrupted: boolean): Promise<boolean> {
    if (row.kind === 'code-review') {
      const r = this.bySession(row.id);
      if (r && (r.state === 'sweeping' || r.state === 'scoring')) this.kick(r.id, () => this.stepsEnded(r.id, interrupted));
      return true;
    }
    const r = this.latest(row.id);
    if ((r?.state !== 'describing' && r?.state !== 'fixing') || r.stage !== 'code') return false;
    // Its fix prompt waits in the queue (one the user ran, or one sent from triage): the end of the turn that takes it moves it on (Lifecycle).
    if (r.state === 'fixing' && this.core.lifecycle.codeReviewPending(row.id)) return true;
    // An interrupted turn is the user stepping in: the review waits for the next turn to end, or Open
    // PR. A describing review keeps why, so a restart leaves it waiting too.
    if (interrupted) {
      if (r.state === 'describing') this.set(r.id, { detail: DESCRIBE_INTERRUPTED });
      return true;
    }
    this.kick(r.id, () => (r.state === 'describing' ? this.beginRound(r.id) : this.nextRound(r.id)));
    return true;
  }

  /**
   * The turn that took a review's fix prompt from the queue ended (Lifecycle, at its `local-review`
   * entry): the next round reviews the change again. Past an interrupt, an Open PR's review in Code
   * waits, as it does when its fix prompt goes straight to the task (onTurnEnd): the next turn's end,
   * or Open PR (the gate's Review now), moves it on.
   */
  codeReviewTurnEnded(taskId: string, interrupted = false): void {
    const r = this.latest(taskId);
    if (r?.state !== 'fixing') return;
    if (interrupted && r.purpose === 'pr' && r.stage === 'code') return;
    this.kick(r.id, () => this.nextRound(r.id));
  }

  /** The review session's turn ended: every step reported, or ask again once for the missing ones. */
  private async stepsEnded(id: string, interrupted: boolean): Promise<void> {
    const r = this.mustReview(id);
    if (this.taskMovedOn(r)) return;
    if (r.state === 'sweeping') {
      const strategy = strategyFor(r.strategy);
      const done = new Set(this.steps(id, `r${r.round}s${r.sweep}:`).map((s) => s.step.split(':')[1]));
      const missing = strategy.passes.map((p) => p.name).filter((n) => !done.has(n));
      if (missing.length && !interrupted && r.asked < ASK_AGAIN) {
        this.set(id, { asked: r.asked + 1 });
        await this.sendSweep(id, missing);
        return;
      }
      if (missing.length && interrupted) return;
      await this.sweepDone(id);
      return;
    }
    const cands = json<Candidate[]>(r.candidates_json, []);
    const done = new Set(this.steps(id, `r${r.round}c`).map((s) => Number(s.step.slice(`r${r.round}c`.length))));
    const missing = cands.map((_, i) => i + 1).filter((n) => !done.has(n));
    if (missing.length && !interrupted && r.asked < ASK_AGAIN) {
      this.set(id, { asked: r.asked + 1 });
      await this.sendScores(id, missing);
      return;
    }
    if (missing.length && interrupted) return;
    await this.finishRound(id);
  }

  /**
   * A sweep is done: its reports merged into the round's candidates. Another sweep follows while the
   * depth allows and, from MinSweeps on, the last one added something new; then scoring.
   */
  private async sweepDone(id: string): Promise<void> {
    const r = this.mustReview(id);
    const reports = this.steps(id, `r${r.round}s${r.sweep}:`).map((s) => s.report as PassReport);
    const before = json<Candidate[]>(r.candidates_json, []);
    if (!reports.length) {
      // Nothing to post from this sweep. A later one ends the sweeping; the first fails the review.
      if (r.sweep === 1) throw new Error('no review pass reported');
      return this.afterSweeps(id);
    }
    const { candidates, added } = mergeSweep(before, reports, r.sweep);
    this.set(id, { candidates_json: JSON.stringify(candidates), asked: 0 });
    this.addNotes(id, r.round, mergeNotes(reports));
    const minSweeps = Math.min(MIN_SWEEPS, r.depth);
    if (r.sweep < r.depth && (r.sweep < minSweeps || added > 0)) {
      this.set(id, { sweep: r.sweep + 1 });
      await this.sendSweep(id);
      return;
    }
    await this.afterSweeps(id);
  }

  private async afterSweeps(id: string): Promise<void> {
    const r = this.mustReview(id);
    const cands = json<Candidate[]>(r.candidates_json, []);
    if (strategyFor(r.strategy).scored && cands.length) {
      this.set(id, { state: 'scoring', asked: 0 });
      await this.sendScores(id);
      return;
    }
    await this.finishRound(id);
  }

  private addNotes(id: string, round: number, text: string): void {
    if (!text.trim() || text.trim() === 'LGTM') return;
    const r = this.mustReview(id);
    const notes = json<{ round: number; text: string }[]>(r.notes_json, []);
    notes.push({ round, text: text.trim() });
    this.set(id, { notes_json: JSON.stringify(notes) });
  }

  /**
   * A round is done: the scorers' verdicts applied (refuted findings dropped, unverified ones kept
   * at MIN_CONFIDENCE and above), then Validate. Kept findings go to the task (`handOff`), or first
   * wait for the user to note or dismiss them (`triage`) when the task's settings check them and Run
   * to merge is off. Run again for the same round (a Retry), it replaces that round's findings.
   */
  private async finishRound(id: string): Promise<void> {
    const r = this.mustReview(id);
    const cands = json<Candidate[]>(r.candidates_json, []);
    const strategy = strategyFor(r.strategy);
    const scores = new Map(this.steps(id, `r${r.round}c`).map((s) => [s.step, s.report as ScoreReport]));
    cands.forEach((c, i) => {
      const s = scores.get(`r${r.round}c${i + 1}`);
      if (s) applyScore(c, s);
    });
    const kept = strategy.scored ? keepScored(cands, MIN_CONFIDENCE) : cands;
    const { diff } = this.loadRound(r);
    // validate gives back copies of what it posts, and the candidates themselves for what it withholds.
    const { withheld } = validate(kept, diff);
    const out = new Set(withheld.map((w) => w.candidate));
    const post = kept.filter((c) => !out.has(c));
    const keyOf = new Map(cands.map((c, i) => [c, `${r.round}:${i + 1}`]));
    const dropReason = new Map<Candidate, string>(withheld.map((w) => [w.candidate, w.reason]));
    for (const c of cands) {
      if (post.includes(c) || dropReason.has(c)) continue;
      dropReason.set(c, c.refuted ? 'refuted' : `confidence below ${MIN_CONFIDENCE}`);
    }
    const findings = json<CodeReviewFinding[]>(r.findings_json, []).filter((f) => f.round !== r.round);
    for (const c of cands) {
      const s = scores.get(`r${r.round}c${keyOf.get(c)!.split(':')[1]}`);
      findings.push({
        key: keyOf.get(c)!, round: r.round, sweep: c.sweep, file: c.finding.file, line: c.finding.line, severity: c.finding.severity, comment: c.finding.comment,
        passes: c.passes, gotchas: c.finding.gotchas, credits: findingCredits(c.finding), verdict: c.verdict, confidence: c.scored ? c.score : null,
        reason: s ? (s.verdict === 'refuted' ? s.refutation : s.reason) : null, kept: post.includes(c), dropped: post.includes(c) ? null : dropReason.get(c) ?? null, outcome: null,
      });
    }
    this.set(id, { findings_json: JSON.stringify(findings), counts_json: JSON.stringify(keptCounts(findings)), last_kept: post.length });
    const task = this.core.sessions.row(r.task_id);
    const roundCounts = countsOf(post);
    const caught = caughtWithMeko(findings.filter((f) => f.round === r.round));
    this.core.sessions.event(task.id, 'review', `Code review, round ${r.round}: ${totalCount(roundCounts) ? countsText(roundCounts) : 'LGTM'}${caught ? `, ${caught}` : ''}`);
    // A first look's findings stay with it: the review is done, and nothing goes to the task.
    if (!r.hand_off) {
      this.set(id, { state: 'done' });
      await this.finish(id);
      return;
    }
    if (post.length && this.checksFindings(task)) {
      this.set(id, { state: 'triage' });
      this.core.sessions.event(task.id, 'review', `Round ${r.round}'s findings wait on you: add notes or dismiss some, then send them to AgentK`);
      this.core.sessions.notifyUser(task.id, 'Code review findings', `Round ${r.round} found ${countsText(roundCounts)}. Check them, then send them to AgentK.`);
      this.core.lifecycle.codeReviewWaitsChanged(task.id);
      return;
    }
    await this.handOff(id);
  }

  /** A round's findings wait for the user: the task's review settings check them, and Run to merge is off (read now, not from `by`). */
  private checksFindings(task: SessionRow): boolean {
    return this.settingsFor(task).check && !sessionRow(this.db, task.id)?.auto_run;
  }

  /**
   * The round's kept findings go to the task: to fix before a re-review while rounds are left, else
   * with the PR prompt (a review the user ran: as a fix prompt, as there's no PR prompt to carry
   * them). With none kept (or every one dismissed), the review is done. `queued`: the fix prompt goes
   * through the task's prompt queue, after any turn of the user's (it comes from `triage`, where the
   * user may have been working with the task).
   */
  private async handOff(id: string, queued = false): Promise<void> {
    const r = this.mustReview(id);
    const task = this.core.sessions.row(r.task_id);
    const post = json<CodeReviewFinding[]>(r.findings_json, []).filter((f) => f.round === r.round && f.kept);
    const more = r.round <= r.rounds;
    // A review the user ran gives the task its last round's findings to fix too, as there's no PR prompt to carry them.
    if (post.length && (more || r.purpose === 'demand')) {
      if (more) this.set(id, { state: 'fixing' });
      await this.core.lifecycle.sendReviewFix(task, {
        strategy: strategyName(r.strategy), round: r.round, findings: this.findingsText(r, r.round), outcomeHow: this.outcomeHow(task), moved: r.lead,
        description: r.stage === 'code' ? this.descriptionFile(task) : null, why: r.purpose === 'demand' ? 'demand' : 'pr', after: fixAfter(r.stage, more), queued,
      });
      if (r.lead) this.set(id, { lead: null });
      if (more) return;
    } else if (r.stage === 'review' && r.round > 1 && await this.core.lifecycle.unpushedToPr(task)) {
      // Fixes from earlier rounds wait uncommitted: in Review & PR they're pushed once, now that a re-review found nothing.
      await this.core.lifecycle.sendReviewPush(task, { strategy: strategyName(r.strategy), round: r.round });
    }
    this.set(id, { state: 'done' });
    await this.finish(id);
  }

  /**
   * The `review-send` action: the findings that waited on the user (`triage`) go to the task, with
   * their notes, and the dismissed ones leave the prompt and the counts. `auto`: Run to merge was
   * turned on while they waited.
   */
  async send(taskId: string, auto = false): Promise<void> {
    const r = this.latest(taskId);
    if (r?.state !== 'triage') throw new HttpError(409, 'No code review findings wait on you.');
    const task = this.core.sessions.row(taskId);
    if (task.desired !== 'running' || task.stage !== r.stage) throw new HttpError(409, `The code review ran in ${r.stage === 'code' ? 'Code' : 'Review & PR'}. Move the session back there first.`);
    this.kick(r.id, async () => {
      const cur = this.mustReview(r.id);
      if (cur.state !== 'triage') return;
      const findings = json<CodeReviewFinding[]>(cur.findings_json, []);
      let sent = 0;
      let dismissed = 0;
      for (const f of findings) {
        if (f.round !== cur.round || !f.kept) continue;
        if (f.you?.dismissed) {
          f.kept = false;
          f.dropped = DISMISSED_BY_YOU;
          dismissed++;
        } else sent++;
      }
      // The PR prompt (the last round's) goes as the user's: they sent it.
      this.set(r.id, { findings_json: JSON.stringify(findings), counts_json: JSON.stringify(keptCounts(findings)), last_kept: sent, ...(auto ? {} : { by: 'you' }) });
      const what = `${sent} finding${sent === 1 ? '' : 's'}${dismissed ? ` (${dismissed} dismissed)` : ''}`;
      this.core.sessions.event(taskId, 'review', auto ? `Run to merge sent round ${cur.round}'s ${what} to AgentK` : sent ? `You sent ${what} to AgentK` : `You dismissed round ${cur.round}'s findings`);
      await this.handOff(r.id, true);
      this.core.lifecycle.codeReviewWaitsChanged(taskId);
    });
    await this.settled(r.id);
  }

  /** Run to merge was turned on: findings waiting on the user go to the task as they are. */
  async autoSend(taskId: string): Promise<void> {
    if (this.latest(taskId)?.state === 'triage') await this.send(taskId, true);
  }

  /**
   * PATCH /api/sessions/:id/code-review/findings/:key: the user's note on a finding of the round that
   * waits on them, or its dismissal (with an optional note); a field left out keeps its value. An
   * empty note and no dismissal clears it.
   */
  setFinding(taskId: string, key: string, body: unknown): CodeReviewFinding {
    const r = this.latest(taskId);
    if (r?.state !== 'triage') throw new HttpError(409, "No code review findings wait on you: they've gone to AgentK, or the review stopped.");
    const findings = json<CodeReviewFinding[]>(r.findings_json, []);
    const f = findings.find((x) => x.key === key && x.round === r.round && x.kept);
    if (!f) throw new HttpError(404, `The code review has no finding ${key} waiting on you.`);
    const choice = readChoice(body, f.you);
    f.you = choice.dismissed || choice.note ? choice : null;
    this.set(r.id, { findings_json: JSON.stringify(findings) });
    return f;
  }

  /** The task fixed a round's findings: the next round reviews the change again. */
  private async nextRound(id: string): Promise<void> {
    const r = this.mustReview(id);
    // Asked for twice (a fix turn's end and an Open PR right after it): the first one moved it on.
    if (r.state !== 'fixing') return;
    if (this.taskMovedOn(r)) return;
    // A fix prompt still queued (Claude lost it) isn't wanted once the next round reads the change.
    this.core.lifecycle.clearCodeReview(r.task_id);
    this.set(id, { round: r.round + 1, sweep: 1, asked: 0, state: 'preparing' });
    await this.beginRound(id);
  }

  /**
   * The review is done: the PR prompt goes, with the line and the last round's findings when it had
   * some. `queued`: for a queued Open PR, which a Cancel or Run to merge stopping still withdraws.
   * One the user ran sends no PR prompt: in Code, Open PR sends it later (openPr); in Review & PR,
   * its line goes in the PR's description.
   */
  private async finish(id: string, queued = false): Promise<void> {
    const r = this.mustReview(id);
    const counts = json<SeverityCounts>(r.counts_json, noCounts());
    const caught = caughtWithMeko(json<CodeReviewFinding[]>(r.findings_json, []));
    this.core.sessions.event(r.task_id, 'review', `${r.hand_off ? 'Code review' : 'First look'} done: ${totalCount(counts) ? countsText(counts) : 'LGTM'}${caught ? `, ${caught}` : ''}`);
    // Its session has nothing left to do: the findings are kept with the review, and the task's tab opens them.
    if (r.session_id) void this.archiveSession(r.session_id);
    this.core.changed();
    if (r.purpose === 'pr') await this.sendPr(id, queued, true);
    else if (r.stage === 'review') await this.core.lifecycle.syncReviewLine(r.task_id);
    // A workshop's level may review again once its gotchas are in Meko (src/server/workshops.ts).
    if (!r.hand_off) this.core.workshops.firstLookDone(r.task_id);
  }

  /** A done review's PR prompt, with its line, and its last round's findings unless they went as a fix prompt (`findings`). */
  private async sendPr(id: string, queued: boolean | undefined, withFindings: boolean): Promise<void> {
    const r = this.mustReview(id);
    const task = this.core.sessions.row(r.task_id);
    const counts = json<SeverityCounts>(r.counts_json, noCounts());
    const last = json<CodeReviewFinding[]>(r.findings_json, []).filter((f) => f.round === r.round && f.kept);
    const findings = withFindings && last.length ? this.findingsText(r, r.round) : '';
    const sent = await this.core.lifecycle.sendReviewedPr(task, {
      line: meAkReviewLine(counts, r.strategy), findings, outcomeHow: this.outcomeHow(task), strategy: strategyName(r.strategy), instructions: r.instructions ?? '', by: r.by === 'auto' ? 'auto' : 'you',
      moved: r.lead, description: this.descriptionFile(task), queued,
    });
    if (sent) this.set(id, { finished_at: Date.now(), lead: null });
  }

  /** The task's latest review is done, but its PR prompt hasn't gone yet. */
  prUnsent(taskId: string): boolean {
    return unsent(this.latest(taskId));
  }

  /** The PR prompt went for a finished review whose prompt couldn't go before (Open PR pressed again). */
  markSent(taskId: string): void {
    const r = this.latest(taskId);
    if (r?.state === 'done' && !r.finished_at) this.set(r.id, { finished_at: Date.now(), lead: null });
  }

  /** Claude exited before taking a finished review's PR prompt: Open PR sends it again, with the findings. */
  markUnsent(taskId: string): void {
    const r = this.latest(taskId);
    if (r?.state === 'done' && r.finished_at) this.set(r.id, { finished_at: null });
  }

  /** A round's kept findings, numbered by their keys, for the task's prompt. */
  private findingsText(r: ReviewRow, round: number): string {
    return json<CodeReviewFinding[]>(r.findings_json, []).filter((f) => f.round === round && f.kept).map((f) => {
      const gotchas = f.gotchas.length ? ` (Meko gotcha ${f.gotchas.join(', ')})` : '';
      const note = f.you?.note ? `\n  - The user's note: ${f.you.note.replace(/\s*\n\s*/g, ' ')}` : '';
      return `- [${f.key}] \`${f.file}:${f.line}\` **${f.severity}**${gotchas}: ${f.comment.trim()}${note}`;
    }).join('\n');
  }

  /** How the task reports what it did with each finding: AgentK's tool when it has it, else in its reply. */
  private outcomeHow(task: SessionRow): string {
    const tool = agentToolName(harnessOf(task.harness), 'review_outcome');
    return this.core.config.tools.enabled && repoConfigTrusted(task)
      ? `Then report what you did with each finding, by its key in brackets, with AgentK's \`${tool}\` tool: fixed, or declined with why.`
      : 'Then say what you did with each finding, by its key in brackets: fixed, or declined with why.';
  }

  // ---- stopping ----

  /** The task left the review's stage (a move by hand, a merge, an archive) while it ran: the review stops, its session goes. */
  private taskMovedOn(r: ReviewRow): boolean {
    const task = sessionRow(this.db, r.task_id);
    if (task?.desired === 'running' && task.stage === r.stage) return false;
    this.set(r.id, { state: 'cancelled', detail: r.stage === 'code' ? REVIEW_LEFT_CODE : REVIEW_LEFT_REVIEW });
    if (r.session_id) void this.archiveSession(r.session_id);
    return true;
  }

  /**
   * A move took the task out of a stage (or a move to Review & PR found it outside Code): its review
   * from that stage, running or stopped, holds nothing now (Retry and Open anyway are Code's). It's
   * cancelled as left and its session goes, so the next Open PR or move starts a new review on the
   * change as it is. A finished Code review whose PR prompt hasn't gone turns stale: Open PR reviews
   * again rather than reuse it.
   */
  leftStage(taskId: string, stage: 'code' | 'review'): void {
    const r = this.latest(taskId);
    if (!r || r.stage !== stage) return;
    // Its fix or push prompt isn't wanted outside its stage.
    this.core.lifecycle.clearCodeReview(taskId);
    if (unsent(r)) {
      this.set(r.id, { stale: 1 });
      return;
    }
    const why = stage === 'code' ? REVIEW_LEFT_CODE : REVIEW_LEFT_REVIEW;
    // A stopped one the user ran has its Retry in its own stage only: it's left behind too, and a
    // failed one's session, kept for that Retry, goes.
    if (r.purpose === 'demand' && (r.state === 'failed' || r.state === 'cancelled')) {
      if (r.detail !== why) this.set(r.id, { state: 'cancelled', detail: why });
      if (r.session_id) void this.archiveSession(r.session_id);
      return;
    }
    if (!(RUNNING.includes(r.state) || holds(r))) return;
    this.set(r.id, { state: 'cancelled', detail: why });
    this.core.sessions.event(taskId, 'review', `Code review stopped: ${why}`);
    if (r.session_id) void this.archiveSession(r.session_id);
  }

  /** The review couldn't go on: the task's Open PR waits for the user's Retry or Open anyway. */
  private fail(id: string, why: string): void {
    const r = this.review(id);
    if (!r || !RUNNING.includes(r.state)) return;
    this.set(id, { state: 'failed', detail: why });
    this.core.log.warn(`code review ${id} failed: ${why}`);
    const next = r.purpose === 'demand' ? 'Retry it, or run it again later.' : 'Retry it, or open the PR anyway.';
    this.core.sessions.event(r.task_id, 'review', `Code review failed: ${why}. ${next}`);
    this.core.sessions.notifyUser(r.task_id, 'Code review failed', `${why}. ${next}`);
    this.core.lifecycle.reviewStopped(r.task_id, r.purpose === 'pr');
    // Its findings no longer wait on the user: the merge blockers are read again.
    if (r.state === 'triage') this.core.lifecycle.codeReviewWaitsChanged(r.task_id);
    this.core.changed();
  }

  /** The `review-cancel` action. */
  async cancel(taskId: string): Promise<void> {
    const r = this.latest(taskId);
    if (!r || !RUNNING.includes(r.state)) throw new HttpError(409, 'No code review is running.');
    this.set(r.id, { state: 'cancelled', detail: YOU_CANCELLED });
    this.core.sessions.event(taskId, 'review', `You cancelled the code review.${r.purpose === 'demand' ? '' : ' Retry it, or open the PR anyway.'}`);
    if (r.session_id) await this.archiveSession(r.session_id);
    this.core.lifecycle.reviewStopped(taskId, r.purpose === 'pr');
    // Its findings no longer wait on the user: the merge blockers are read again.
    if (r.state === 'triage') this.core.lifecycle.codeReviewWaitsChanged(taskId);
    this.core.changed();
  }

  /** The `review-retry` action: the round again from the step it stopped at, reusing every step already reported. */
  async retry(taskId: string): Promise<void> {
    const r = this.latest(taskId);
    if (!r || (r.state !== 'failed' && r.state !== 'cancelled')) throw new HttpError(409, 'There is no stopped code review to retry.');
    if (r.purpose === 'demand' && sessionRow(this.db, taskId)?.stage !== r.stage) {
      throw new HttpError(409, `That code review ran in ${r.stage === 'code' ? 'Code' : 'Review & PR'}. Run the code review again here instead.`);
    }
    const dir = this.runDir(r);
    const resumable = existsSync(join(dir, 'base.md')) && existsSync(join(dir, 'context.json'));
    // Stopped before round 1 had the task's PR description: the task is asked for it again first
    // (not in Review & PR, where the PR has one).
    const describe = !resumable && r.round === 1 && r.stage === 'code' && !existsSync(this.core.sessions.prDescriptionPath(this.core.sessions.row(taskId)));
    const state = resumable ? (json<Candidate[]>(r.candidates_json, []).length && this.steps(r.id, `r${r.round}c`).length ? 'scoring' : 'sweeping') : describe ? 'describing' : 'preparing';
    this.set(r.id, { state, detail: null, asked: 0 });
    this.core.sessions.event(taskId, 'review', 'Code review retried');
    this.core.changed();
    this.kick(r.id, async () => {
      const cur = this.mustReview(r.id);
      if (cur.state === 'describing') return this.describe(r.id);
      if (cur.state === 'preparing') return this.beginRound(r.id);
      if (cur.state === 'scoring') {
        const done = new Set(this.steps(r.id, `r${cur.round}c`).map((s) => Number(s.step.slice(`r${cur.round}c`.length))));
        const missing = json<Candidate[]>(cur.candidates_json, []).map((_, i) => i + 1).filter((n) => !done.has(n));
        return missing.length ? this.sendScores(r.id, missing) : this.finishRound(r.id);
      }
      const done = new Set(this.steps(r.id, `r${cur.round}s${cur.sweep}:`).map((s) => s.step.split(':')[1]));
      const missing = strategyFor(cur.strategy).passes.map((p) => p.name).filter((n) => !done.has(n));
      return missing.length ? this.sendSweep(r.id, missing) : this.sweepDone(r.id);
    });
  }

  /** The `review-open-anyway` action: the PR prompt goes without a `me_ak_review:` line. */
  async openAnyway(taskId: string): Promise<void> {
    const r = this.latest(taskId);
    if (!r || (r.state !== 'failed' && r.state !== 'cancelled') || r.purpose === 'demand') throw new HttpError(409, 'There is no stopped code review to open the PR past.');
    const task = this.core.sessions.row(taskId);
    const sent = await this.core.lifecycle.sendReviewedPr(task, {
      line: null, findings: '', outcomeHow: '', strategy: strategyName(r.strategy), instructions: r.instructions ?? '', by: 'you', moved: r.lead, description: this.descriptionFile(task),
    });
    if (sent) this.set(r.id, { finished_at: Date.now(), lead: null, detail: `${r.detail ?? 'stopped'}; the PR opened anyway` });
  }

  /** The task was archived or moved out of Code: its review stops, and its session goes. */
  async taskGone(taskId: string, why: string): Promise<void> {
    const r = this.latest(taskId);
    if (r && RUNNING.includes(r.state)) this.set(r.id, { state: 'cancelled', detail: why });
    for (const child of all<{ id: string }>(this.db, `SELECT id FROM sessions WHERE parent_id = ? AND desired = 'running'`, taskId)) await this.archiveSession(child.id);
    this.core.changed();
  }

  private async archiveSession(sessionId: string): Promise<void> {
    const row = sessionRow(this.db, sessionId);
    if (!row || row.desired !== 'running') return;
    await this.core.sessions.archive(row, { keepBranch: true }).catch((e: unknown) => this.core.log.warn(`archiving review session ${sessionId} failed`, errMsg(e)));
  }

  /**
   * After a restart: a review whose session is idle with steps still to report gets them asked for
   * again (its prompts may have gone with the old server), a review with no session left fails, and
   * one waiting on its task's PR description starts its first round once the task is idle, unless it
   * waits on the user (an interrupted turn, a lost ask: its `detail`).
   */
  async recover(): Promise<void> {
    const rows = all<ReviewRow>(this.db, `SELECT * FROM code_reviews WHERE state IN ('describing', 'preparing', 'sweeping', 'scoring')`);
    for (const r of rows) {
      if (r.state === 'describing') {
        // The task's PR description turn may have ended while AgentK was down: the first round goes on
        // with what the task wrote. Not while that turn runs or asks something (needs_input), and not
        // past an interrupt or a lost ask, which wait for the user's Open PR.
        const task = sessionRow(this.db, r.task_id);
        if (!r.detail && (task?.status === 'idle' || task?.status === 'recovered')) this.kick(r.id, () => this.beginRound(r.id));
        continue;
      }
      const session = r.session_id ? sessionRow(this.db, r.session_id) : undefined;
      if (r.state === 'preparing' || !session || session.desired !== 'running') {
        this.kick(r.id, async () => {
          const cur = this.mustReview(r.id);
          if (cur.state === 'preparing') return this.beginRound(r.id);
          throw new Error('its session ended');
        });
        continue;
      }
      if (session.status === 'idle' || session.status === 'recovered') this.kick(r.id, () => this.stepsEnded(r.id, false));
    }
  }

  // ---- rows ----

  private mustReview(id: string): ReviewRow {
    const r = this.review(id);
    if (!r) throw new Error('the review is gone');
    return r;
  }

  private set(id: string, fields: Partial<Record<keyof ReviewRow, string | number | null>>): void {
    const keys = Object.keys(fields);
    tx(this.db, () => {
      run(this.db, `UPDATE code_reviews SET ${[...keys.map((k) => `${k} = ?`), 'updated_at = ?'].join(', ')} WHERE id = ?`,
        ...keys.map((k) => fields[k as keyof ReviewRow] as string | number | null), Date.now(), id);
    });
    this.core.changed();
  }
}
