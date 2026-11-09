import { existsSync, readFileSync, realpathSync, statSync } from 'node:fs';
import { dirname, extname, join, relative, sep } from 'node:path';
import { DISMISSED_BY_YOU, type CodeReviewFinding } from '../shared/codereview.js';
import { mekoDatapackPath, mekoGapsPath } from '../shared/mekourls.js';
import { scoreDifference, tokensText } from '../shared/tour.js';
import {
  TOUR_SERVER_ACTIONS, type TourAction, type TourActRequest, type TourActResult, type TourLevelInfo, type TourLink, type TourPhase,
  type TourScore, type TourScoreSide, type TourState,
} from '../shared/types.js';
import type { Core } from './core.js';
import { all, get, kvDelete, kvGet, kvSet, run } from './db.js';
import { errMsg } from './log.js';
import { localMeko } from './mekolocal.js';
import { sessionMekoLine } from './mekousage.js';
import { HttpError } from './projects.js';
import { dedicatedDatapack, projectRow, sessionRow, type ProjectRow, type SessionRow } from './store.js';
import { gotchaList, levelPrompts, type StageDef, type StageText, type WorkshopBundle } from './workshops.js';

/**
 * The tours of workshop projects (docs/design/workshops.md): Kode runs them itself, with no agent
 * session hosting. `workshop_tours` keeps the level and stage on screen; everything else about a
 * stage (ready, running, done, failed) is worked out each time from what its work leaves behind:
 * the level's task and its code reviews, the gotchas' upload, the learn session, the question
 * sessions and their learn steps. So a restart loses nothing, and nothing has to be kept in step.
 * The page shows it as `ProjectWithSessions.tour` and presses its buttons with `act`.
 */

/** The kv key, with `:<session id>`, holding when a workshop project's session last ended a turn. */
const TURN_KEY = 'workshop-turn';
/** The kv key, with `:<task id>`, holding why Kode couldn't start a level's first review. */
const REVIEW_FAILED_KEY = 'workshop-review-failed';

/** The review states in which a round's work is still going. */
const REVIEW_RUNNING = ['preparing', 'describing', 'sweeping', 'scoring'];

interface TourRow {
  project_id: string;
  level: string;
  stage: string;
  /** When the level on screen was (re)started: its sessions are the ones made since. */
  level_at: number;
  skipped_json: string;
  finished_json: string;
}

interface ReviewRow {
  id: string;
  task_id: string;
  session_id: string | null;
  state: string;
  hand_off: number;
  findings_json: string;
  detail: string | null;
  created_at: number;
  updated_at: number;
}

/** What working out a stage needs. */
interface Ctx {
  b: WorkshopBundle;
  p: ProjectRow;
  row: TourRow;
  level: WorkshopBundle['levels'][number];
  stage: StageDef;
  /** The tour has a `workshop_tours` row: it has begun. */
  stored: boolean;
}

/** A stage worked out: its phase, the session it works in, the text variant to show and what fills it. */
interface Worked {
  phase: TourPhase;
  session: SessionRow | null;
  variant: keyof StageText;
  failure?: string;
  /** Review with context missed the trap: its button runs the review again. */
  again?: boolean;
}

const json = <T>(s: string | null | undefined, fallback: T): T => {
  try {
    return s ? ((JSON.parse(s) as T | null) ?? fallback) : fallback;
  } catch {
    return fallback;
  }
};

const plural = (n: number, one: string, many = `${one}s`) => `${n} ${n === 1 ? one : many}`;

/** Fills a stage text's `{{…}}` placeholders; one with no value reads as itself, so a gap shows rather than hides. */
export function fillText(text: string, vars: Record<string, string>): string {
  return text.replace(/\{\{([\w.]+)\}\}/g, (m, k: string) => vars[k] ?? m).trim();
}

export class TourService {
  /** Projects whose tour is running an action, so a double press can't begin two sessions. */
  private acting = new Set<string>();

  constructor(private core: Core) {}

  private get db() {
    return this.core.db;
  }

  private get w() {
    return this.core.workshops;
  }

  // ---- the state ----

  /** The project's tour, or null when it isn't a workshop's or its bundle doesn't read. */
  state(projectId: string): TourState | null {
    const c = this.ctx(projectId);
    return c ? this.build(c) : null;
  }

  private ctx(projectId: string): Ctx | null {
    const p = projectRow(this.db, projectId);
    const b = p?.workshop ? this.w.bundle(p.workshop) : null;
    if (!p || !b?.levels.length) return null;
    const kept = get<TourRow>(this.db, 'SELECT * FROM workshop_tours WHERE project_id = ?', p.id);
    const row = kept ?? this.row(p, b);
    const level = b.levels.find((l) => l.id === row.level) ?? b.levels[0];
    const stage = level.stages.find((s) => s.id === row.stage) ?? level.stages[0];
    return { b, p, row, level, stage, stored: !!kept };
  }

  /**
   * Where a tour without a row starts (just started, or played with a host before Kode ran tours
   * itself), from its records: Level 1's teach stage after a first look, its review with context
   * after one, else its first stage.
   */
  private row(p: ProjectRow, b: WorkshopBundle): TourRow {
    const first = b.levels[0];
    const reviews = all<{ hand_off: number }>(this.db,
      `SELECT c.hand_off FROM code_reviews c JOIN sessions s ON s.id = c.task_id WHERE s.project_id = ? AND s.desired = 'running'`, p.id);
    const kind = reviews.some((r) => r.hand_off) ? 'review-context' : reviews.length ? 'teach' : null;
    const stage = (kind && first.stages.find((s) => s.kind === kind)) || first.stages[0];
    return { project_id: p.id, level: first.id, stage: stage.id, level_at: 0, skipped_json: '[]', finished_json: '[]' };
  }

  private save(r: TourRow): void {
    run(this.db, `INSERT INTO workshop_tours (project_id, level, stage, level_at, skipped_json, finished_json, updated_at)
      VALUES (?, ?, ?, ?, ?, ?, ?)
      ON CONFLICT(project_id) DO UPDATE SET level = excluded.level, stage = excluded.stage, level_at = excluded.level_at,
        skipped_json = excluded.skipped_json, finished_json = excluded.finished_json, updated_at = excluded.updated_at`,
    r.project_id, r.level, r.stage, r.level_at, r.skipped_json, r.finished_json, Date.now());
    this.core.changed();
  }

  /** Why a level can't run here, or null: it needs a datapack of the project's own. */
  private blocked(c: Pick<Ctx, 'p'>, level: WorkshopBundle['levels'][number]): string | null {
    if (level.needs.includes('datapack') && !dedicatedDatapack(this.db, c.p.id)) {
      return "This level needs a datapack of the project's own, which the tour couldn't make. Add one on the project's home.";
    }
    return null;
  }

  private build(c: Ctx): TourState {
    const finished = new Set(json<string[]>(c.row.finished_json, []));
    const skipped = new Set(json<string[]>(c.row.skipped_json, []));
    const done = c.b.levels.every((l) => finished.has(l.id));
    const at = c.level.stages.indexOf(c.stage);
    const levels: TourLevelInfo[] = c.b.levels.map((l) => ({
      id: l.id, title: l.title, finished: finished.has(l.id), blocked: this.blocked(c, l),
      stages: l.stages.map((s, i) => ({
        id: s.id, title: s.title,
        status: finished.has(l.id) || (l === c.level && i < at)
          ? (skipped.has(`${l.id}/${s.id}`) ? 'skipped' : 'done')
          : l === c.level && i === at && !done ? 'current' : 'todo',
      })),
    }));
    const base = {
      workshop: c.b.id, levels, level: c.level.id, stage: c.stage.id, kind: c.stage.kind, title: c.stage.title,
      score: c.level.review ? this.score(c) : null, lead: null, welcome: false,
    };
    // The welcome card, once: before the tour has a row or any session.
    if (c.b.welcome && !c.stored && !get(this.db, 'SELECT 1 AS x FROM sessions WHERE project_id = ? LIMIT 1', c.p.id)) {
      return {
        ...base, title: c.b.welcome.title, phase: 'ready', text: c.b.welcome.text, actions: [{ id: 'begin', label: 'Start the tour', primary: true }],
        links: [], sessionId: null, score: null, welcome: true, finished: false,
      };
    }
    if (done) {
      return { ...base, phase: 'done', text: c.b.end, actions: [{ id: 'own-repo', label: 'Use Kode on your own repo', primary: true }, { id: 'close', label: 'Close tour', danger: true }], links: [], sessionId: null, finished: true };
    }
    const why = this.blocked(c, c.level);
    if (why) {
      return { ...base, phase: 'blocked', text: why, actions: [{ id: 'finish', label: 'Skip this level', primary: true }, { id: 'own-repo', label: 'Skip, use my own repo' }], links: [], sessionId: null, finished: false };
    }
    const wk = this.work(c);
    const last = at === c.level.stages.length - 1;
    const actions: TourAction[] = [];
    const nextLabel = c.stage.labels.next ?? (last ? '' : `Next: ${c.level.stages[at + 1].title}`);
    const finishLabel = c.stage.labels.finish ?? 'Finish this level';
    const onward = (label?: string): TourAction => (last ? { id: 'finish', label: label ?? finishLabel, primary: true } : { id: 'next', label: label ?? nextLabel, primary: true });
    switch (wk.phase) {
      case 'ready':
        actions.push({ id: 'run', label: c.stage.labels.run ?? 'Start', primary: true });
        break;
      case 'done':
        if (wk.again) actions.push({ id: 'run', label: c.stage.labels.run ?? 'Run it again' });
        actions.push(onward());
        break;
      case 'failed':
        actions.push({ id: 'retry', label: 'Retry', primary: true });
        if (c.stage.kind === 'ask') actions.push({ ...onward(), primary: false });
        break;
      case 'blocked':
        actions.push(onward(last ? undefined : 'Skip this stage'));
        break;
      case 'running':
        break;
    }
    const text = c.stage.text[wk.variant] ?? c.stage.text.done;
    return {
      ...base, phase: wk.phase, text: fillText(text, this.vars(c, wk, base.score)), actions, links: this.links(c, wk),
      sessionId: wk.session?.desired === 'running' ? wk.session.id : null, lead: at === 0 ? c.level.teaches : null, finished: false,
    };
  }

  /** What a stage's text fills in. */
  private vars(c: Ctx, wk: Worked, score: TourScore | null): Record<string, string> {
    const side = (k: 'cold' | 'context', s: TourScoreSide | null | undefined): Record<string, string> => (s ? {
      [`${k}.kept`]: plural(s.kept, 'issue'), [`${k}.caught`]: `${s.caught}`, [`${k}.tokens`]: tokensText(s),
      [`${k}.gotchas`]: s.gotchas.length ? s.gotchas.join(', ') : 'none',
    } : {});
    const gotchas = gotchaList(c.b);
    const ids = gotchas.map((g) => g.id);
    return {
      repo: c.p.repo_path,
      datapack: dedicatedDatapack(this.db, c.p.id)?.name ?? 'none',
      gotchasDatapack: this.w.gotchasOf(c.p.id)?.name ?? 'none',
      session: wk.session?.name ?? '',
      failure: wk.failure ?? '',
      meko: wk.session ? sessionMekoLine(this.db, wk.session.id) ?? '' : '',
      'teach.gotchas': ids.length > 1 ? `${ids.slice(0, -1).join(', ')} and ${ids[ids.length - 1]}` : ids[0] ?? "the team's gotchas",
      // A function's name in a title reads as code: `newGame()`.
      'gotchas.list': gotchas.map((g) => `- **${g.id}:** ${g.title.replace(/\b(\w+\(\))/g, '`$1`')}`).join('\n'),
      ...side('cold', score?.cold),
      ...side('context', score?.context),
    };
  }

  private links(c: Ctx, wk: Worked): TourLink[] {
    if (c.stage.kind === 'repo') {
      return [{ label: 'Play the game', href: `api/workshops/${encodeURIComponent(c.b.id)}/play/main/index.html`, kind: 'play', first: true }];
    }
    const dp = dedicatedDatapack(this.db, c.p.id);
    const web = localMeko().meko?.webUrl?.replace(/\/+$/, '');
    if (!dp || !web) return [];
    // Its memories, to watch them arrive while it learns.
    if (c.stage.kind === 'learn' && wk.phase === 'running') return [{ label: `Open ${dp.name} in Meko`, href: `${web}${mekoDatapackPath(dp.id)}/memories`, kind: 'meko' }];
    // The questions Kode's learn step marked taught.
    if ((c.stage.kind === 'ask' || c.stage.kind === 'ask-again') && wk.phase !== 'ready') return [{ label: "Meko's Context Gaps", href: `${web}${mekoGapsPath(dp.id)}/taught`, kind: 'meko' }];
    return [];
  }

  // ---- each stage's phase ----

  private work(c: Ctx): Worked {
    switch (c.stage.kind) {
      case 'repo':
        return { phase: 'done', session: null, variant: 'done' };
      case 'review-first-look':
        return this.firstLookStage(c);
      case 'teach':
        return this.teachStage(c);
      case 'review-context':
        return this.contextStage(c);
      case 'learn':
        return this.learnStage(c);
      case 'ask':
        return this.askStage(c);
      case 'ask-again':
        return this.askAgainStage(c);
    }
  }

  /** The level's task: the newest task in the project on its review branch, begun since the level (re)started. */
  private levelTask(c: Ctx): SessionRow | null {
    if (!c.level.review) return null;
    return get<SessionRow>(this.db, `SELECT * FROM sessions WHERE project_id = ? AND kind != 'code-review' AND purpose IS NULL AND branch = ?
      AND created_at >= ? ORDER BY created_at DESC LIMIT 1`, c.p.id, c.level.review.branch, c.row.level_at) ?? null;
  }

  private reviewOf(taskId: string, handOff: boolean): ReviewRow | null {
    return get<ReviewRow>(this.db, 'SELECT * FROM code_reviews WHERE task_id = ? AND hand_off = ? ORDER BY created_at DESC LIMIT 1', taskId, handOff ? 1 : 0) ?? null;
  }

  /** A review's phase: running while a round works, failed when it stopped, done once its findings are in. */
  private reviewPhase(r: ReviewRow): TourPhase {
    if (REVIEW_RUNNING.includes(r.state)) return 'running';
    if (r.state === 'failed' || r.state === 'cancelled') return 'failed';
    return 'done';
  }

  private firstLookStage(c: Ctx): Worked {
    const task = this.levelTask(c);
    if (!task) return { phase: 'ready', session: null, variant: 'ready' };
    // Its first look; a level played before Kode made the gotchas datapack again each run may have
    // had the review with the gotchas alone.
    const one = this.reviewOf(task.id, false) ?? this.reviewOf(task.id, true);
    const startFailed = kvGet(this.db, `${REVIEW_FAILED_KEY}:${task.id}`);
    if (!one) {
      if (startFailed) return { phase: 'failed', session: task, variant: 'failed', failure: startFailed };
      return task.desired === 'running' ? { phase: 'running', session: task, variant: 'running' } : { phase: 'blocked', session: task, variant: 'blocked' };
    }
    const phase = this.reviewPhase(one);
    if (phase === 'failed') return { phase, session: task, variant: 'failed', failure: one.detail ?? 'the review stopped.' };
    if (phase === 'running') return { phase, session: task, variant: 'running' };
    return { phase: 'done', session: task, variant: 'done' };
  }

  private teachStage(c: Ctx): Worked {
    const task = this.levelTask(c);
    if (!this.w.gotchasOf(c.p.id)) return { phase: 'blocked', session: task, variant: 'blocked' };
    if (this.w.addingGotchas(c.p.id)) return { phase: 'running', session: task, variant: 'running' };
    const result = this.w.gotchasResult(c.p.id);
    if (this.w.gotchasIn(c.p.id) || result === 'indexed') return { phase: 'done', session: task, variant: 'done' };
    if (result) return { phase: 'failed', session: task, variant: 'failed', failure: result };
    return { phase: 'ready', session: task, variant: 'ready' };
  }

  private contextStage(c: Ctx): Worked {
    const task = this.levelTask(c);
    if (!this.w.gotchasOf(c.p.id) || !task) return { phase: 'blocked', session: task, variant: 'blocked' };
    const r = this.reviewOf(task.id, true);
    if (!r) return task.desired === 'running' ? { phase: 'ready', session: task, variant: 'ready' } : { phase: 'blocked', session: task, variant: 'blocked' };
    const phase = this.reviewPhase(r);
    if (phase === 'failed') return { phase, session: task, variant: 'failed', failure: r.detail ?? 'the review stopped.' };
    if (phase === 'running') return { phase, session: task, variant: 'running' };
    if (this.side(r).caught || !c.stage.text.missed) return { phase: 'done', session: task, variant: 'done' };
    return { phase: 'done', session: task, variant: 'missed', again: task.desired === 'running' };
  }

  private learnStage(c: Ctx): Worked {
    const s = get<SessionRow>(this.db, `SELECT * FROM sessions WHERE project_id = ? AND purpose = 'learn' AND created_at >= ? ORDER BY created_at DESC LIMIT 1`,
      c.p.id, c.row.level_at) ?? null;
    if (!s) return { phase: 'ready', session: null, variant: 'ready' };
    return s.desired === 'running' ? { phase: 'running', session: s, variant: 'running' } : { phase: 'done', session: s, variant: 'done' };
  }

  /** The level's question session by its prompt's name, begun since the level (re)started. */
  private named(c: Ctx, learn: boolean): SessionRow | null {
    const name = c.level.prompts.find((p) => p.learn === learn && p.name)?.name;
    if (!name) return null;
    return get<SessionRow>(this.db, `SELECT * FROM sessions WHERE project_id = ? AND kind != 'code-review' AND purpose IS NULL AND lower(trim(name)) = ?
      AND created_at >= ? ORDER BY created_at DESC LIMIT 1`, c.p.id, name.toLowerCase(), c.row.level_at) ?? null;
  }

  private hadTurn(id: string): boolean {
    return kvGet(this.db, `${TURN_KEY}:${id}`) !== null;
  }

  private askStage(c: Ctx): Worked {
    const s = this.named(c, true);
    if (!s) return { phase: 'ready', session: null, variant: 'ready' };
    if (!this.hadTurn(s.id)) return s.desired === 'running' ? { phase: 'running', session: s, variant: 'running' } : { phase: 'done', session: s, variant: 'done' };
    const failed = this.w.learnFailed(s.id);
    if (failed) return { phase: 'failed', session: s, variant: 'failed', failure: failed };
    if (this.w.saving(s.id)) return { phase: 'running', session: s, variant: 'running' };
    return { phase: 'done', session: s, variant: 'done' };
  }

  private askAgainStage(c: Ctx): Worked {
    const s = this.named(c, false);
    if (!s) return { phase: 'ready', session: null, variant: 'ready' };
    if (!this.hadTurn(s.id) && s.desired === 'running') return { phase: 'running', session: s, variant: 'running' };
    return { phase: 'done', session: s, variant: 'done' };
  }

  // ---- the scoreboard ----

  /** One review's side: its first round's kept findings (ones the user dismissed count too: the review found them), and its session's tokens. */
  private side(r: ReviewRow): TourScoreSide {
    const found = json<CodeReviewFinding[]>(r.findings_json, []).filter((f) => f.round === 1 && (f.kept || f.dropped === DISMISSED_BY_YOU));
    const credited = found.filter((f) => f.gotchas.length);
    const u = r.session_id
      ? get<{ tokens: number; credits: number }>(this.db,
        'SELECT COALESCE(SUM(input + cache_read + cache_write), 0) AS tokens, COALESCE(SUM(credits), 0) AS credits FROM usage_messages WHERE session_id = ?', r.session_id)
      : undefined;
    return {
      kept: found.length, caught: credited.length, gotchas: [...new Set(credited.flatMap((f) => f.gotchas))],
      tokensIn: u?.tokens ?? 0, credits: u?.credits ?? 0,
    };
  }

  private score(c: Ctx): TourScore {
    const task = this.levelTask(c);
    const look = task ? this.reviewOf(task.id, false) : null;
    const proper = task ? this.reviewOf(task.id, true) : null;
    const settled = (r: ReviewRow | null): r is ReviewRow => !!r && !REVIEW_RUNNING.includes(r.state) && r.state !== 'failed' && r.state !== 'cancelled';
    // The first look is the review without context; the one that hands off, the review with it.
    const cold = settled(look) ? this.side(look) : null;
    const context = settled(proper) ? this.side(proper) : null;
    return { cold, context, difference: cold && context ? scoreDifference(context) : null };
  }

  // ---- the buttons ----

  /** POST /api/workshops/:id/act. */
  async act(workshopId: string, req: TourActRequest): Promise<TourActResult> {
    const projectId = this.w.projectOf(workshopId);
    if (!projectId) throw new HttpError(404, `The ${workshopId} workshop isn't open.`);
    if (!req || typeof req.stage !== 'string' || !TOUR_SERVER_ACTIONS.includes(req.action)) throw new HttpError(400, 'Name the stage and one of the tour\'s actions.');
    if (this.acting.has(projectId)) throw new HttpError(409, 'The tour is already doing that.');
    this.acting.add(projectId);
    try {
      await this.doAct(projectId, req);
    } finally {
      this.acting.delete(projectId);
    }
    return { sessionId: this.state(projectId)?.sessionId ?? null };
  }

  private async doAct(projectId: string, req: TourActRequest): Promise<void> {
    const c = this.ctx(projectId);
    if (!c) throw new HttpError(404, "This workshop's tour can't be read.");
    if (req.action === 'goto') return this.goto(c, req.level);
    const st = this.build(c);
    if (req.action === 'begin') {
      if (!st.welcome) throw new HttpError(409, 'The tour has already begun. Reload to see where it is.');
      this.save(c.row);
      return;
    }
    if (req.stage !== st.stage || st.finished) throw new HttpError(409, 'The tour has moved on from that stage. Reload to see where it is.');
    if (!st.actions.some((a) => a.id === req.action)) throw new HttpError(409, "That button isn't on the tour's card now.");
    const at = c.level.stages.indexOf(c.stage);
    const skipped = json<string[]>(c.row.skipped_json, []);
    switch (req.action) {
      case 'next': {
        // Passing over a stage that didn't run (one blocked here) marks it skipped on the rail.
        if (st.phase !== 'done') skipped.push(`${c.level.id}/${c.stage.id}`);
        this.save({ ...c.row, stage: c.level.stages[at + 1].id, skipped_json: JSON.stringify(skipped) });
        return;
      }
      case 'finish':
        return this.finish(c);
      case 'run':
      case 'retry':
        return this.run(c, req.action === 'retry');
    }
  }

  /** Starts the stage's work (or tries it again). */
  private async run(c: Ctx, retry: boolean): Promise<void> {
    const task = this.levelTask(c);
    switch (c.stage.kind) {
      case 'review-first-look': {
        if (retry && task) {
          kvDelete(this.db, `${REVIEW_FAILED_KEY}:${task.id}`);
          const failed = this.reviewOf(task.id, false) ?? this.reviewOf(task.id, true);
          if (failed) await this.core.reviews.retry(task.id);
          else await this.firstReview(task);
          return;
        }
        // The review without context starts from an empty gotchas datapack, even on a replay.
        await this.w.freshGotchas(c.p.id);
        const prompt = levelPrompts(c.b, c.level.id)[0];
        this.started(await this.core.sessions.begin(c.p.id, {
          name: prompt.name ?? c.stage.title, kind: 'task', start: 'code', prompt: prompt.text, branch: c.level.review!.branch, existingBranch: true,
        }));
        this.persist(c);
        return;
      }
      case 'teach':
        if (!this.w.addGotchas(c.p.id)) throw new HttpError(409, 'The gotchas are already going into Meko.');
        this.persist(c);
        return;
      case 'review-context':
        if (!task || task.desired !== 'running') throw new HttpError(409, "The level's session was closed.");
        if (retry) await this.core.reviews.retry(task.id);
        else await this.w.runLevelReview(task, false);
        return;
      case 'learn': {
        // Level 2 learns from nothing, even on a replay; on the tour's model (Sonnet on Claude Code), for speed.
        await this.w.freshProjectDatapack(c.p.id);
        const harness = (await this.core.harnesses.pick(this.core.config, undefined)).id;
        const model = this.w.modelFor(c.p.id, harness);
        this.started(await this.core.sessions.begin(c.p.id, { name: 'Learn the project', kind: 'task', learn: c.level.learn!, harness, ...(model ? { model } : {}) }));
        this.persist(c);
        return;
      }
      case 'ask':
      case 'ask-again': {
        const learn = c.stage.kind === 'ask';
        const p = levelPrompts(c.b, c.level.id).find((x) => x.learn === learn && x.name);
        if (!p) throw new HttpError(409, 'This stage has no prompt.');
        if (retry) {
          const s = this.named(c, learn);
          if (s) {
            this.w.clearLearnFailed(s.id);
            await this.core.learn.start(s, 'manual');
          }
          return;
        }
        this.started(await this.core.sessions.begin(c.p.id, { name: p.name!, kind: 'task', start: 'explore', prompt: p.text }));
        this.persist(c);
        return;
      }
      case 'repo':
        return;
    }
  }

  /**
   * A stage's session, begun: one whose worktree or launch then fails is a failed start the page
   * shows with Try again (`sessions.begin`), so its rejection is only logged here.
   */
  private started({ session, done }: { session: { id: string }; done: Promise<unknown> }): void {
    done.catch((e: unknown) => this.core.log.warn(`session ${session.id} didn't start`, errMsg(e)));
  }

  /** The level's first review, started again after a refusal: a first look while the gotchas aren't in yet. */
  private async firstReview(task: SessionRow): Promise<void> {
    const firstLook = !!this.w.gotchasOf(task.project_id) && !this.w.gotchasIn(task.project_id);
    try {
      await this.w.runLevelReview(task, firstLook);
    } catch (e) {
      kvSet(this.db, `${REVIEW_FAILED_KEY}:${task.id}`, errMsg(e));
      throw e;
    }
  }

  /** A level's first review couldn't start (WorkshopService.reviewLevel): kept for the card. */
  reviewFailed(taskId: string, why: string): void {
    kvSet(this.db, `${REVIEW_FAILED_KEY}:${taskId}`, why);
    this.core.changed();
  }

  /** Keeps the tour's row once a stage's work began, so a tour with none yet doesn't fall back to its records' guess. */
  private persist(c: Ctx): void {
    if (!get(this.db, 'SELECT 1 AS x FROM workshop_tours WHERE project_id = ?', c.p.id)) this.save(c.row);
  }

  /**
   * Finish (or Skip this level): the level's sessions are archived, the level is marked finished,
   * and the tour moves to the next level not finished yet, or to its end.
   */
  private async finish(c: Ctx): Promise<void> {
    await this.archiveLevel(c.p.id);
    const finished = [...new Set([...json<string[]>(c.row.finished_json, []), c.level.id])];
    const next = c.b.levels.find((l) => !finished.includes(l.id));
    this.save({
      ...c.row, finished_json: JSON.stringify(finished),
      ...(next ? { level: next.id, stage: next.stages[0].id, level_at: Date.now() } : {}),
    });
  }

  /** Switches to another level (the rail's level names): the open level's sessions are archived, and that level starts at its first stage. */
  private async goto(c: Ctx, level: string | undefined): Promise<void> {
    const l = c.b.levels.find((x) => x.id === level);
    if (!l) throw new HttpError(400, `The tour has no level ${String(level)}.`);
    if (l === c.level && !json<string[]>(c.row.finished_json, []).includes(l.id)) return;
    await this.archiveLevel(c.p.id);
    const finished = json<string[]>(c.row.finished_json, []).filter((x) => x !== l.id);
    this.save({ ...c.row, level: l.id, stage: l.stages[0].id, level_at: Date.now(), finished_json: JSON.stringify(finished) });
  }

  /**
   * Archives every open session in the tour's project but code reviews' own (they go with their
   * task), since one level runs at a time. Archive, not Discard: a worktree with the level's
   * uncommitted fixes is kept, and the branch stays. One still saving what it learned stays open,
   * since Archive would cut the save short. Waited for, so the next stage's Run never finds the
   * level's worktree still being let go of (one action runs per project at a time).
   */
  private async archiveLevel(projectId: string): Promise<void> {
    const ids = all<{ id: string }>(this.db, `SELECT id FROM sessions WHERE project_id = ? AND desired = 'running' AND kind != 'code-review'`, projectId);
    await Promise.all(ids.map(async ({ id }) => {
      const s = this.w.saving(id) ? null : sessionRow(this.db, id);
      if (s) await this.core.sessions.archive(s, { keepBranch: true }).catch((e: unknown) => this.core.log.warn(`archiving ${id} at the end of its tour level failed`, errMsg(e)));
    }));
  }

  // ---- what the tour hears ----

  /** A session in a workshop's project ended a turn (not interrupted): ask and ask-again's done. */
  turnEnded(row: SessionRow): void {
    if (!projectRow(this.db, row.project_id)?.workshop) return;
    kvSet(this.db, `${TURN_KEY}:${row.id}`, String(Date.now()));
    this.core.changed();
  }

  /**
   * At the server's start, before recovery: a workshop's host from before Kode ran tours itself
   * (`purpose = 'guide'`) is archived, and its tour goes on from its records.
   */
  async archiveOldHosts(): Promise<void> {
    for (const r of all<SessionRow>(this.db, `SELECT * FROM sessions WHERE purpose = 'guide' AND desired = 'running'`)) {
      try {
        await this.core.sessions.archive(r, { keepBranch: true });
        this.core.log.info(`archived ${r.id}, a tour host from before Kode ran tours itself`);
      } catch (e) {
        this.core.log.warn(`archiving the old tour host ${r.id} failed`, errMsg(e));
      }
    }
  }

  // ---- playing the demo ----

  /**
   * GET /api/workshops/:id/play/:where/*: a file of the workshop's demo (`main`, the demo repo) or of a
   * session's worktree in its project, read-only: nothing outside that folder (by its real path),
   * no dot-file or dot-folder. An HTML page comes back with the scripts and stylesheets it names in
   * its folder put inline: it runs sandboxed (an opaque origin), whose own requests Meko refuses as
   * another site's, so the page has to load whole.
   */
  playFile(workshopId: string, where: string, path: string): { body: string | Buffer; type: string } {
    const projectId = this.w.projectOf(workshopId);
    const p = projectId ? projectRow(this.db, projectId) : null;
    const s = p && where !== 'main' ? sessionRow(this.db, where) : null;
    const root = !p ? null : where === 'main' ? p.repo_path : s?.project_id === p.id ? s.worktree_path : null;
    const notFound = () => new HttpError(404, 'There is no such file in the demo.');
    if (!root) throw notFound();
    const file = insideRoot(root, path || 'index.html');
    if (!file) throw notFound();
    const type = PLAY_TYPES[extname(file).toLowerCase()];
    if (!type) throw notFound();
    if (type.startsWith('text/html')) {
      const b = this.w.bundle(workshopId);
      return { body: withBar(inlineAssets(readFileSync(file, 'utf8'), realpathSync(root), dirname(file)), b?.play ?? null), type };
    }
    return { body: readFileSync(file), type };
  }
}

/** The most a played file can be. */
const PLAY_MAX = 5 * 1024 * 1024;

const PLAY_TYPES: Record<string, string> = {
  '.html': 'text/html; charset=utf-8', '.js': 'text/javascript; charset=utf-8', '.css': 'text/css; charset=utf-8',
  '.json': 'application/json', '.png': 'image/png', '.svg': 'image/svg+xml', '.jpg': 'image/jpeg', '.gif': 'image/gif', '.txt': 'text/plain; charset=utf-8',
};

/** <rel> under <root> as a real path that stays inside it, names no dot-file or dot-folder and is a file within PLAY_MAX; else null. */
export function insideRoot(root: string, rel: string): string | null {
  const parts = rel.split(/[\\/]+/).filter(Boolean);
  if (!parts.length || parts.some((x) => x.startsWith('.'))) return null;
  try {
    const realRoot = realpathSync(root);
    const joined = join(realRoot, ...parts);
    if (!existsSync(joined)) return null;
    const real = realpathSync(joined);
    if (!real.startsWith(realRoot + sep)) return null;
    const st = statSync(real);
    return st.isFile() && st.size <= PLAY_MAX ? real : null;
  } catch {
    return null;
  }
}

/** The workshop's `play` line in a bar across the top of a played page (right after `<body>`), or the page as it is. */
export function withBar(html: string, line: string | null): string {
  if (!line) return html;
  const esc = line.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  const bar = `<div role="note" style="position:sticky;top:0;z-index:2147483647;margin:0;padding:10px 16px;background:#1d4ed8;color:#fff;font:600 15px/1.4 system-ui,sans-serif;text-align:center">${esc}</div>`;
  const body = /<body\b[^>]*>/i.exec(html);
  return body ? html.slice(0, body.index + body[0].length) + bar + html.slice(body.index + body[0].length) : bar + html;
}

/**
 * A page with the scripts (`<script src>`) and stylesheets (`<link rel="stylesheet" href>`) it names
 * under <root> put inline; any other reference is left as it is. <root> and <dir>, the page's folder,
 * are real paths.
 */
export function inlineAssets(html: string, root: string, dir: string): string {
  const read = (ref: string): string | null => {
    if (/^[a-z][a-z0-9+.-]*:|^\/\//i.test(ref)) return null;
    const file = insideRoot(root, relative(root, join(dir, ref.split(/[?#]/)[0])));
    return file ? readFileSync(file, 'utf8') : null;
  };
  return html
    .replace(/<script\s+src="([^"]+)"\s*><\/script>/g, (m, ref: string) => {
      const js = read(ref);
      return js === null ? m : `<script>\n${js.replace(/<\/script/gi, '<\\/script')}\n</script>`;
    })
    .replace(/<link\s+rel="stylesheet"\s+href="([^"]+)"\s*\/?>/g, (m, ref: string) => {
      const css = read(ref);
      return css === null ? m : `<style>\n${css.replace(/<\/style/gi, '<\\/style')}\n</style>`;
    });
}
