import { randomBytes } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { basename, join } from 'node:path';
import {
  MIN_SWEEPS, addCounts, countsText, meAkReviewLine, noCounts, resolveReview, strategyName, totalCount,
  type CodeReviewDetail, type CodeReviewFinding, type CodeReviewState, type CodeReviewSummary, type FindingOutcome,
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
import { parseSettings, projectRow, sessionRepo, sessionReview, sessionRow, type SessionRow } from '../store.js';
import { TOOLS_SERVER } from '../tooldefs.js';
import { readDiff } from './diff.js';
import { findGuides, findMekoMds } from './guides.js';
import { readCommits, readHistory } from './history.js';
import { MIN_CONFIDENCE, applyScore, countsOf, keepScored, mergeNotes, mergeSweep, validate } from './merge.js';
import { basePrompt, passPrompt, scoreBase, scorePrompt, sweepContext } from './prompt.js';
import { checkOutcomes, checkReport, checkScore } from './schemas.js';
import { strategyFor } from './strategies.js';
import type { Candidate, Diff, PassReport, PriorRound, ReviewContext, ReviewDatapack, ReviewIssue, ScoreReport, Tier } from './types.js';

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
  created_at: number;
  updated_at: number;
  finished_at: number | null;
}

/** The running states: a review in one of them holds the task's Open PR. */
const RUNNING: CodeReviewState[] = ['preparing', 'sweeping', 'scoring', 'fixing'];

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
 * AgentK's local code review (docs/design/code-review.md): one review per Open PR of a task, run in a
 * code-review session (a child of the task, sharing its worktree) that starts each pass and scorer as
 * a fresh step on its model tier. The steps report through AgentK's tools; this class keeps their
 * reports in `code_review_steps`, applies the cloud reviewer's rules (merge, sweeps, the refutation
 * gate, keepScored, Validate) and moves the review on at each of its session's turn ends: sweeps,
 * scoring, then the task fixes the findings (re-review rounds) or opens the PR with the
 * `me_ak_review:` line. A review in a running state holds the task's Open PR; a failed or cancelled
 * one holds it for the user's Retry or Open anyway.
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
      // Once the session is gone (cancelled, replaced), there's nothing to open.
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
      mekoCall: !!r.session_id && RUNNING.includes(r.state) && this.core.sessions.mekoCalling(r.session_id),
      detail: r.detail,
    };
  }

  /** GET /api/sessions/:id/code-review. */
  detail(sessionId: string): CodeReviewDetail {
    const r = this.reviewOf(sessionId);
    if (!r) throw new HttpError(404, 'This session has no local review.');
    const findings = json<CodeReviewFinding[]>(r.findings_json, []);
    // The current round's candidates, before they're kept or dropped.
    if (r.state === 'sweeping' || r.state === 'scoring') findings.push(...this.pendingFindings(r));
    return {
      review: this.summarize(r),
      settings: { strategy: r.strategy as ReviewStrategyId, rounds: r.rounds, depth: r.depth },
      datapacks: json<ReviewDatapack[]>(r.datapacks_json, []).map((d) => ({ name: d.name, id: d.id, about: d.about, gotchas: d.gotchas })),
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
        comment: c.finding.comment, passes: c.passes, gotchas: c.finding.gotchas, verdict: s?.verdict ?? null, confidence: s?.confidence ?? null,
        reason: s ? (s.verdict === 'refuted' ? s.refutation : s.reason) : null, kept: false, dropped: null, outcome: null,
      };
    });
  }

  /** A review in a running state holds the task's Open PR. */
  running(taskId: string): boolean {
    const r = this.latest(taskId);
    return !!r && RUNNING.includes(r.state);
  }

  /** The PR line a finished review gives the task's PR, or null when its last review didn't finish (or none ran). */
  prLine(taskId: string): string | null {
    const r = this.latest(taskId);
    return r?.state === 'done' ? meAkReviewLine(json<SeverityCounts>(r.counts_json, noCounts())) : null;
  }

  // ---- starting ----

  /**
   * Open PR on a task whose review settings aren't Off: true when a review now holds it (one just
   * started, or one already running or waiting on Retry), false when the PR prompt may go (review
   * Off, not a task, or the last review finished and its PR hasn't gone yet).
   */
  async openPr(task: SessionRow, opts: { instructions: string; by: 'you' | 'auto' }): Promise<boolean> {
    if (task.kind === 'code-review' || task.kind === 'review-pr') return false;
    const last = this.latest(task.id);
    if (last && RUNNING.includes(last.state)) {
      // Pressed again while it runs: the instructions given now go with the PR.
      if (opts.instructions) this.set(last.id, { instructions: opts.instructions });
      return true;
    }
    if (last && (last.state === 'failed' || last.state === 'cancelled')) {
      throw new HttpError(409, 'The local review stopped. Retry it, or open the PR anyway.');
    }
    // A finished review whose PR prompt didn't go (Claude was busy): the PR goes with its line now.
    if (last?.state === 'done' && !last.finished_at) {
      if (opts.instructions) this.set(last.id, { instructions: opts.instructions });
      await this.finish(last.id);
      return true;
    }
    const settings = this.settingsFor(task);
    if (settings.strategy === 'off') return false;
    await this.start(task, settings, opts);
    return true;
  }

  private async start(task: SessionRow, settings: ReviewSettings, opts: { instructions: string; by: 'you' | 'auto' }): Promise<void> {
    if (settings.strategy === 'off') return;
    const id = `${task.id}--review-${randomBytes(4).toString('hex')}`;
    const now = Date.now();
    // An earlier review's session goes: the task's tab shows only the latest.
    const prev = this.latest(task.id);
    run(this.db, `INSERT INTO code_reviews(id, task_id, strategy, rounds, depth, state, instructions, by, created_at, updated_at)
                  VALUES (?, ?, ?, ?, ?, 'preparing', ?, ?, ?, ?)`, id, task.id, settings.strategy, settings.rounds, settings.depth, opts.instructions || null, opts.by, now, now);
    this.core.sessions.event(task.id, 'review', `Local review started (${strategyName(settings.strategy)}${settings.depth > 1 ? `, depth ${settings.depth}` : ''}${settings.rounds ? `, up to ${settings.rounds} re-review${settings.rounds === 1 ? '' : 's'}` : ''})`);
    this.core.changed();
    if (prev?.session_id) void this.archiveSession(prev.session_id);
    this.kick(id, () => this.beginRound(id));
  }

  /** Runs `fn` for a review once nothing else is moving it on; failures fail the review. */
  private kick(id: string, fn: () => Promise<void>): void {
    const before = this.advancing.get(id) ?? Promise.resolve();
    const next = before.then(fn).catch((e: unknown) => {
      try {
        this.fail(id, errMsg(e));
      } catch (err) {
        // Shutting down: the database is closed, and recover() picks the review up at the next start.
        this.core.log.warn(`local review ${id} stopped: ${errMsg(e)}`, errMsg(err));
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
    const r = this.mustReview(id);
    const task = this.core.sessions.row(r.task_id);
    const dir = this.runDir(r);
    rmSync(dir, { recursive: true, force: true });
    mkdirSync(dir, { recursive: true, mode: 0o700 });
    const { ctx, diff } = await this.readChange(r, task, dir);
    if (!diff.files.length) {
      // Nothing to review: the PR goes as it would have.
      this.set(id, { state: 'done', detail: 'nothing changed against the base' });
      this.core.sessions.event(task.id, 'review', 'Local review: nothing changed against the base, so nothing to review');
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
    const mergeBase = (await git(wt, 'merge-base', base, 'HEAD')).trim();
    const head = (await git(wt, 'rev-parse', 'HEAD')).trim();
    const dirty = !!(await git(wt, 'status', '--porcelain')).trim();
    const paths = diff.files.map((f) => f.path);
    const trusted = repoConfigTrusted(task);
    const datapacks = trusted && this.core.mekoStatus.configured ? this.datapacks(wt, paths) : [];
    const repo = sessionRepo(this.db, task);
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
        description: this.description(task),
        commits: await readCommits(wt, mergeBase),
        guides: findGuides(wt, paths),
        history: await readHistory(wt, mergeBase, diff),
        issues: await this.issues(task, dir),
        datapacks,
        prior: this.prior(r),
        worktree: wt,
        runDir: dir,
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

  /** The task's own account of the change: its first prompt, and where its design is. */
  private description(task: SessionRow): string {
    const parts = [task.initial_prompt ?? ''];
    const design = get<{ artifact_url: string | null }>(this.db, 'SELECT artifact_url FROM designs WHERE session_id = ?', task.id);
    if (design) parts.push(`The task's design: ${design.artifact_url ?? this.core.sessions.designFile(task)}`);
    return parts.filter(Boolean).join('\n\n');
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
          out.push({ title: l.label, path: claudePath(path), url: l.url });
          continue;
        } catch (e) {
          this.core.log.warn(`reading ${l.url} for ${task.id}'s review failed`, errMsg(e));
        }
      }
      out.push({ title: l.label, path: null, url: l.url });
    }
    return out;
  }

  /** The earlier rounds of this review, with what the task did with each finding. */
  private prior(r: ReviewRow): PriorRound[] {
    const findings = json<CodeReviewFinding[]>(r.findings_json, []).filter((f) => f.kept && f.round < r.round);
    const rounds = [...new Set(findings.map((f) => f.round))];
    return rounds.map((round) => ({
      round,
      findings: findings.filter((f) => f.round === round).map((f) => ({ file: f.file, line: f.line, severity: f.severity, comment: f.comment, outcome: f.outcome })),
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
    await this.sendSteps(r, task, 'code-review-score', steps, { what: `${cands.length} finding${cands.length === 1 ? '' : 's'}`, tool: agentToolName(harness, 'review_score') });
  }

  /**
   * Gives the review session its steps: in a harness with subagents, one prompt that runs each as
   * a fresh subagent on its tier; in one without (`turns`), each step as a prompt of its own after
   * clearing the conversation, the model for its tier set first when one is configured.
   */
  private async sendSteps(r: ReviewRow, task: SessionRow, name: 'code-review-sweep' | 'code-review-score', steps: { name: string; tier: Tier; file: string }[], v: { what: string; tool: string }): Promise<void> {
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
    const list = steps.map((s, i) => `${i + 1}. ${s.name}: subagent \`${REVIEW_AGENTS[s.tier]}\`, prompt: \`${read(s.file)}\``).join('\n');
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
        description: `One step of AgentK's local code review on the ${tier} model: a review pass or a scorer. Use only when AgentK's prompt names it.`,
        prompt: 'You are one step of AgentK\'s local code review. Do exactly what the file your prompt names says, report with the AgentK tool it names, and change no file.',
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
    if (!r || !RUNNING.includes(r.state)) return { text: 'This session has no local review running.', error: true };
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
    if (!r) return { text: 'This task has no local review.', error: true };
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
    if (r?.state !== 'fixing') return false;
    // An interrupted fix turn is the user stepping in: the review waits for the next turn to end.
    if (!interrupted) this.kick(r.id, () => this.nextRound(r.id));
    return true;
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
   * at MIN_CONFIDENCE and above), then Validate. Kept findings go to the task: to fix before a
   * re-review while rounds are left, else with the PR prompt.
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
    const findings = json<CodeReviewFinding[]>(r.findings_json, []);
    for (const c of cands) {
      const s = scores.get(`r${r.round}c${keyOf.get(c)!.split(':')[1]}`);
      findings.push({
        key: keyOf.get(c)!, round: r.round, sweep: c.sweep, file: c.finding.file, line: c.finding.line, severity: c.finding.severity, comment: c.finding.comment,
        passes: c.passes, gotchas: c.finding.gotchas, verdict: c.verdict, confidence: c.scored ? c.score : null,
        reason: s ? (s.verdict === 'refuted' ? s.refutation : s.reason) : null, kept: post.includes(c), dropped: post.includes(c) ? null : dropReason.get(c) ?? null, outcome: null,
      });
    }
    const counts = addCounts(json<SeverityCounts>(r.counts_json, noCounts()), countsOf(post));
    this.set(id, { findings_json: JSON.stringify(findings), counts_json: JSON.stringify(counts), last_kept: post.length });
    const task = this.core.sessions.row(r.task_id);
    const roundCounts = countsOf(post);
    this.core.sessions.event(task.id, 'review', `Local review, round ${r.round}: ${totalCount(roundCounts) ? countsText(roundCounts) : 'LGTM'}`);
    if (post.length && r.round <= r.rounds) {
      this.set(id, { state: 'fixing' });
      await this.core.lifecycle.sendReviewFix(task, { strategy: strategyName(r.strategy), round: r.round, findings: this.findingsText(this.mustReview(id), r.round), outcomeHow: this.outcomeHow(task) });
      return;
    }
    this.set(id, { state: 'done' });
    await this.finish(id);
  }

  /** The task fixed a round's findings: the next round reviews the change again. */
  private async nextRound(id: string): Promise<void> {
    const r = this.mustReview(id);
    if (this.taskMovedOn(r)) return;
    this.set(id, { round: r.round + 1, sweep: 1, asked: 0, state: 'preparing' });
    await this.beginRound(id);
  }

  /** The review is done: the PR prompt goes, with the line and the last round's findings when it had some. */
  private async finish(id: string): Promise<void> {
    const r = this.mustReview(id);
    const task = this.core.sessions.row(r.task_id);
    const counts = json<SeverityCounts>(r.counts_json, noCounts());
    const last = json<CodeReviewFinding[]>(r.findings_json, []).filter((f) => f.round === r.round && f.kept);
    const findings = last.length ? this.findingsText(r, r.round) : '';
    this.core.sessions.event(task.id, 'review', `Local review done: ${totalCount(counts) ? countsText(counts) : 'LGTM'}`);
    this.core.changed();
    // The review session stays, idle, for its terminal and findings, until the task goes or another review replaces it.
    const sent = await this.core.lifecycle.sendReviewedPr(task, {
      line: meAkReviewLine(counts), findings, outcomeHow: this.outcomeHow(task), strategy: strategyName(r.strategy), instructions: r.instructions ?? '', by: r.by === 'auto' ? 'auto' : 'you',
    });
    if (sent) this.set(id, { finished_at: Date.now() });
  }

  /** The PR prompt went for a finished review whose prompt couldn't go before (Open PR pressed again). */
  markSent(taskId: string): void {
    const r = this.latest(taskId);
    if (r?.state === 'done' && !r.finished_at) this.set(r.id, { finished_at: Date.now() });
  }

  /** A round's kept findings, numbered by their keys, for the task's prompt. */
  private findingsText(r: ReviewRow, round: number): string {
    return json<CodeReviewFinding[]>(r.findings_json, []).filter((f) => f.round === round && f.kept).map((f) => {
      const gotchas = f.gotchas.length ? ` (Meko gotcha ${f.gotchas.join(', ')})` : '';
      return `- [${f.key}] \`${f.file}:${f.line}\` **${f.severity}**${gotchas}: ${f.comment.trim()}`;
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

  /** The task left Code (a move by hand, an archive) while its review ran: the review stops, its session goes. */
  private taskMovedOn(r: ReviewRow): boolean {
    const task = sessionRow(this.db, r.task_id);
    if (task?.desired === 'running' && task.stage === 'code') return false;
    this.set(r.id, { state: 'cancelled', detail: 'the task left Code' });
    if (r.session_id) void this.archiveSession(r.session_id);
    return true;
  }

  /** The review couldn't go on: the task's Open PR waits for the user's Retry or Open anyway. */
  private fail(id: string, why: string): void {
    const r = this.review(id);
    if (!r || !RUNNING.includes(r.state)) return;
    this.set(id, { state: 'failed', detail: why });
    this.core.log.warn(`local review ${id} failed: ${why}`);
    this.core.sessions.event(r.task_id, 'review', `Local review failed: ${why}. Retry it, or open the PR anyway.`);
    this.core.sessions.notifyUser(r.task_id, 'Local review failed', `${why}. Retry it, or open the PR anyway.`);
    this.core.lifecycle.reviewStopped(r.task_id);
    this.core.changed();
  }

  /** The `review-cancel` action. */
  async cancel(taskId: string): Promise<void> {
    const r = this.latest(taskId);
    if (!r || !RUNNING.includes(r.state)) throw new HttpError(409, 'No local review is running.');
    this.set(r.id, { state: 'cancelled', detail: 'you cancelled it' });
    this.core.sessions.event(taskId, 'review', 'You cancelled the local review. Retry it, or open the PR anyway.');
    if (r.session_id) await this.archiveSession(r.session_id);
    this.core.lifecycle.reviewStopped(taskId);
    this.core.changed();
  }

  /** The `review-retry` action: the round again from the step it stopped at, reusing every step already reported. */
  async retry(taskId: string): Promise<void> {
    const r = this.latest(taskId);
    if (!r || (r.state !== 'failed' && r.state !== 'cancelled')) throw new HttpError(409, 'There is no stopped local review to retry.');
    const dir = this.runDir(r);
    const resumable = existsSync(join(dir, 'base.md')) && existsSync(join(dir, 'context.json'));
    this.set(r.id, { state: resumable ? (json<Candidate[]>(r.candidates_json, []).length && this.steps(r.id, `r${r.round}c`).length ? 'scoring' : 'sweeping') : 'preparing', detail: null, asked: 0 });
    this.core.sessions.event(taskId, 'review', 'Local review retried');
    this.core.changed();
    this.kick(r.id, async () => {
      const cur = this.mustReview(r.id);
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
    if (!r || (r.state !== 'failed' && r.state !== 'cancelled')) throw new HttpError(409, 'There is no stopped local review to open the PR past.');
    const task = this.core.sessions.row(taskId);
    const sent = await this.core.lifecycle.sendReviewedPr(task, { line: null, findings: '', outcomeHow: '', strategy: strategyName(r.strategy), instructions: r.instructions ?? '', by: 'you' });
    if (sent) this.set(r.id, { finished_at: Date.now(), detail: `${r.detail ?? 'stopped'}; the PR opened anyway` });
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
   * again (its prompts may have gone with the old server), and a review with no session left fails.
   */
  async recover(): Promise<void> {
    const rows = all<ReviewRow>(this.db, `SELECT * FROM code_reviews WHERE state IN ('preparing', 'sweeping', 'scoring')`);
    for (const r of rows) {
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
