import { cpSync, existsSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { readOverrides, type ReviewOverrides } from '../shared/codereview.js';
import { defaultProjectDatapackName, freeDatapackName } from '../shared/datapacks.js';
import { harnessOf, isHarnessId, type HarnessId } from '../shared/harnesses.js';
import { isGotchasDatapack, parseMekoMd } from '../shared/mekomd.js';
import { TOUR_STAGE_KINDS, type LearnBrief, type TourStageKind, type Workshop, type WorkshopStart } from '../shared/types.js';
import type { Core } from './core.js';
import { all, get, kvDelete, kvGet, kvSet, run, tx } from './db.js';
import { commitDemoRepo, demoBranch, freeDemoBranches, recommitDemo, type GitIdent } from './git.js';
import type { KnowledgeResult } from './knowledge.js';
import { errMsg } from './log.js';
import { expandHome } from './paths.js';
import { HttpError, isDatapackLimit } from './projects.js';
import { checkLearn } from './sessions.js';
import { dedicatedDatapack, projectRow, projectRows, sessionDatapack, sessionRow, type SessionRow } from './store.js';

/**
 * Workshops bundled with Kode (docs/design/workshops.md): each is a folder of `src/workshops/`
 * (`dist/workshops` in a release) with its levels' stages, their sessions' prompts and the demo repo
 * it's played in. Starting one builds the demo repo with no remote, opens it as a project and makes
 * its gotchas datapack in Meko; its tour (src/server/tour.ts) then runs the stages. What a stage's
 * work needs that isn't the tour's own (a level's code review, its gotchas' upload, the learn step on
 * a question session) is here.
 */

/** Beside the server's own folder, as the session tools' skill is: `src/workshops` in dev, `dist/workshops` in a release. */
export const WORKSHOPS_SOURCE = join(dirname(fileURLToPath(import.meta.url)), '..', 'workshops');

/** The kv key that says a fresh install's tour ran, so it never runs by itself twice. */
const TOUR_STARTED_KEY = 'tour-started';
/** The kv key, with `:<project id>`, that says a workshop project's gotchas are indexed in Meko (the datapack's id). */
const GOTCHAS_IN_KEY = 'workshop-gotchas';
/**
 * The kv key, with `:<project id>`, holding how the gotchas' last upload ended: `indexed`, or why it
 * didn't, for Teach it's receipt.
 */
const GOTCHAS_RESULT_KEY = 'workshop-gotchas-result';
/** The kv key, with `:<session id>`, holding why Kode couldn't run the learn step on a level's question session. */
const LEARN_FAILED_KEY = 'workshop-learn-failed';

/**
 * One session a level's stages begin: its prompt file, its name, and `learn`, whether Kode runs the
 * learn step on it by itself once its first turn ends (Level 2's question session).
 */
export interface LevelPrompt {
  /** The session's name, which is how Kode knows it; null for a level with one unnamed prompt. */
  name: string | null;
  file: string;
  learn: boolean;
}

/** The guide card's text for each phase a stage can be in, with `{{…}}` placeholders (`TOUR_VARS`). */
export interface StageText {
  ready?: string;
  running?: string;
  done: string;
  failed?: string;
  blocked?: string;
  /** Review with context's done text when no finding credits a gotcha. */
  missed?: string;
}

/** A stage of a level (`levels[].stages`): what Kode runs (`kind`), its title on the rail, its card's text and its buttons' labels. */
export interface StageDef {
  id: string;
  kind: TourStageKind;
  title: string;
  text: StageText;
  labels: Partial<Record<'run' | 'next' | 'finish', string>>;
}

/** The placeholders a stage's text may use, filled by the tour (src/server/tour.ts). */
export const TOUR_VARS = [
  'repo', 'datapack', 'gotchasDatapack', 'session', 'failure', 'meko',
  'cold.kept', 'cold.caught', 'cold.tokens', 'context.kept', 'context.caught', 'context.gotchas', 'context.tokens', 'teach.gotchas',
  'gotchas.list',
] as const;

/** The kinds that start work, and so need ready and running text. */
const RUNS: TourStageKind[] = ['review-first-look', 'teach', 'review-context', 'learn', 'ask', 'ask-again'];

/** A workshop's `workshop.json`, checked. */
export interface WorkshopBundle {
  id: string;
  title: string;
  summary: string;
  /** The card shown once before the tour begins, with Start the tour. */
  welcome: { title: string; text: string } | null;
  /** The tour's last card, once every level is finished. */
  end: string;
  /** The bar on top of the game when Play the game opens it, or null. */
  play: string | null;
  repo: {
    /** The demo's files as they are on main, relative to the bundle. */
    dir: string;
    /** The repo's folder name in the repos folder (`-2`, `-3` when taken). */
    folder: string;
    author: GitIdent;
    date: string;
    message: string;
    /** Branches off main, each a `git format-patch` file relative to the bundle. */
    branches: { name: string; patch: string }[];
  };
  /** The gotchas uploaded to the demo's gotchas datapack, never copied into the repo. */
  gotchas: { file: string; datapack: string; about: string } | null;
  /** The project's code review settings (`ProjectSettings.codeReview`): Sirius 1.0's single quick pass for the tour. */
  codeReview: ReviewOverrides | null;
  /**
   * The model each agent's code-review sessions, and the tour's Learn the project session, start on
   * in the workshop's project (the tour's: Claude Code's Sonnet, for speed), by harness since model
   * names differ; the review's passes and scorers keep their tiers' models (`[code_review.models]`).
   * An agent left out starts on its own default.
   */
  reviewModels: Partial<Record<HarnessId, string>>;
  /**
   * `review`: Kode runs its code review itself on the level's session (a task in the project on
   * that branch) as soon as its worktree is made, so the user watches the review rather than starts it.
   * With `gotchasAfterFirst`, that first review runs while the gotchas datapack is still empty and
   * keeps its findings; the tour's Teach stage then adds the gotchas to Meko, and Review with context reviews again (src/server/tour.ts).
   */
  /**
   * `prompt`: a prompt file, or a list of `{ name, file, learn }` for a level with more than one
   * session. `learn`: the brief of Learn the project, which the project home's offer and the tour's
   * Learn the project stage start with. `needs`: `datapack` (the project's own datapack), without
   * which the level shows why and can't run. `teaches`: what the level teaches, over its first
   * stage's card. `stages`: what the tour runs, in order.
   */
  levels: {
    id: string; title: string; teaches: string | null; prompts: LevelPrompt[]; needs: string[];
    review: { branch: string; gotchasAfterFirst: boolean } | null;
    learn: LearnBrief | null;
    stages: StageDef[];
  }[];
  /** The bundle's folder. */
  root: string;
}

const str = (v: unknown, what: string): string => {
  if (typeof v !== 'string' || !v.trim()) throw new Error(`${what} is missing`);
  return v;
};

/** Reads and checks <root>/<id>/workshop.json; throws saying what's wrong. */
export function readWorkshop(id: string, root = WORKSHOPS_SOURCE): WorkshopBundle {
  if (!/^[a-z0-9][a-z0-9-]*$/.test(id)) throw new Error(`${id} isn't a workshop id`);
  const dir = join(root, id);
  const raw = JSON.parse(readFileSync(join(dir, 'workshop.json'), 'utf8')) as Record<string, unknown>;
  if (raw.id !== id) throw new Error(`${id}/workshop.json names ${String(raw.id)}`);
  const repo = raw.repo as Record<string, unknown> | undefined;
  if (!repo) throw new Error('repo is missing');
  const author = repo.author as Record<string, unknown> | undefined;
  const g = raw.gotchas as Record<string, unknown> | null | undefined;
  const levels = Array.isArray(raw.levels) ? (raw.levels as Record<string, unknown>[]) : [];
  const branches = Array.isArray(repo.branches) ? (repo.branches as Record<string, unknown>[]) : [];
  const welcome = raw.welcome as Record<string, unknown> | null | undefined;
  const b: WorkshopBundle = {
    id,
    title: str(raw.title, 'title'),
    summary: str(raw.summary, 'summary'),
    welcome: welcome ? { title: str(welcome.title, 'welcome.title'), text: str(welcome.text, 'welcome.text') } : null,
    end: str(raw.end, 'end'),
    play: raw.play == null ? null : str(raw.play, 'play'),
    repo: {
      dir: str(repo.dir, 'repo.dir'),
      folder: str(repo.folder, 'repo.folder'),
      author: { name: str(author?.name, 'repo.author.name'), email: str(author?.email, 'repo.author.email') },
      date: str(repo.date, 'repo.date'),
      message: str(repo.message, 'repo.message'),
      branches: branches.map((br) => ({ name: str(br.name, 'a branch name'), patch: str(br.patch, 'a branch patch') })),
    },
    gotchas: g ? { file: str(g.file, 'gotchas.file'), datapack: str(g.datapack, 'gotchas.datapack'), about: typeof g.about === 'string' ? g.about : '' } : null,
    codeReview: raw.codeReview ? readOverrides(raw.codeReview) : null,
    reviewModels: Object.fromEntries(Object.entries((raw.reviewModels ?? {}) as Record<string, unknown>)
      .filter((e): e is [HarnessId, string] => isHarnessId(e[0]) && typeof e[1] === 'string' && !!e[1].trim())),
    levels: levels.map((l) => {
      const review = l.review as Record<string, unknown> | null | undefined;
      const lid = str(l.id, 'a level id');
      return {
        id: lid, title: str(l.title, 'a level title'), teaches: l.teaches == null ? null : str(l.teaches, `level ${lid}'s teaches`),
        prompts: levelPromptsOf(l.prompt),
        needs: Array.isArray(l.needs) ? l.needs.filter((n): n is string => typeof n === 'string') : [],
        review: review ? { branch: str(review.branch, 'a level review branch'), gotchasAfterFirst: review.gotchasAfterFirst === true } : null,
        learn: l.learn == null ? null : learnBriefOf(l.learn),
        stages: stagesOf(lid, l.stages),
      };
    }),
    root: dir,
  };
  checkStages(b);
  return b;
}

/** A level's `stages`, each checked: a known kind, unique ids, and the text its kind needs. */
function stagesOf(level: string, v: unknown): StageDef[] {
  if (!Array.isArray(v) || !v.length) throw new Error(`level ${level} has no stages`);
  const out = (v as Record<string, unknown>[]).map((st) => {
    const sid = str(st?.id, `a stage id in level ${level}`);
    const kind = st.kind as TourStageKind;
    if (!TOUR_STAGE_KINDS.includes(kind)) throw new Error(`stage ${sid} has an unknown kind ${String(st.kind)}`);
    const t = (st.text ?? {}) as Record<string, unknown>;
    const text: StageText = { done: str(t.done, `stage ${sid}'s done text`) };
    for (const k of ['ready', 'running', 'failed', 'blocked', 'missed'] as const) {
      if (t[k] != null) text[k] = str(t[k], `stage ${sid}'s ${k} text`);
    }
    if (RUNS.includes(kind) && (!text.ready || !text.running)) throw new Error(`stage ${sid} needs ready and running text`);
    for (const [k, line] of Object.entries(text)) {
      for (const m of (line as string).matchAll(/\{\{([\w.]+)\}\}/g)) {
        if (!(TOUR_VARS as readonly string[]).includes(m[1])) throw new Error(`stage ${sid}'s ${k} text has an unknown placeholder {{${m[1]}}}`);
      }
    }
    const labels = (st.labels ?? {}) as Record<string, unknown>;
    return {
      id: sid, kind, title: str(st.title, `stage ${sid}'s title`), text,
      labels: Object.fromEntries((['run', 'next', 'finish'] as const).filter((k) => typeof labels[k] === 'string' && labels[k]).map((k) => [k, labels[k] as string])),
    };
  });
  if (new Set(out.map((x) => x.id)).size !== out.length) throw new Error(`level ${level}'s stages share an id`);
  return out;
}

/** What each stage's kind needs of its level: a review stage the level's `review`, a question stage its named prompts, Learn the project its brief. */
function checkStages(b: WorkshopBundle): void {
  const ids = new Set<string>();
  for (const l of b.levels) {
    if (ids.has(l.id)) throw new Error(`two levels are called ${l.id}`);
    ids.add(l.id);
    for (const st of l.stages) {
      if (['review-first-look', 'review-context'].includes(st.kind) && !l.review) throw new Error(`stage ${st.id} needs its level's review`);
      if (st.kind === 'review-first-look' && !l.prompts[0]?.name) throw new Error(`stage ${st.id} needs its level's prompt to have a name`);
      if (st.kind === 'teach' && !b.gotchas) throw new Error(`stage ${st.id} needs the workshop's gotchas`);
      if (st.kind === 'learn' && !l.learn) throw new Error(`stage ${st.id} needs its level's learn brief`);
      if (st.kind === 'ask' && !l.prompts.some((p) => p.learn && p.name)) throw new Error(`stage ${st.id} needs a named prompt with learn`);
      if (st.kind === 'ask-again' && !l.prompts.some((p) => !p.learn && p.name)) throw new Error(`stage ${st.id} needs a named prompt without learn`);
    }
  }
}

/** A level's `prompt`: one file, or a list of named sessions' files. */
function levelPromptsOf(v: unknown): LevelPrompt[] {
  if (typeof v === 'string') return [{ name: null, file: str(v, 'a level prompt'), learn: false }];
  if (!Array.isArray(v) || !v.length) throw new Error('a level prompt is missing');
  const out = (v as Record<string, unknown>[]).map((p) => ({
    name: str(p?.name, 'a level prompt name').trim(), file: str(p?.file, 'a level prompt file'), learn: p?.learn === true,
  }));
  if (new Set(out.map((p) => p.name.toLowerCase())).size !== out.length) throw new Error("a level's prompts share a name");
  return out;
}

/** A level's Learn the project brief, checked as a learn session's is. */
function learnBriefOf(v: unknown): LearnBrief {
  try {
    return checkLearn(v);
  } catch (e) {
    throw new Error(`a level's learn brief is wrong: ${errMsg(e)}`, { cause: e });
  }
}

/** The gotchas in the bundle's gotchas file (its `## MG-n: <title>` headings), for the teach stage's list and receipt. */
export function gotchaList(b: WorkshopBundle): { id: string; title: string }[] {
  if (!b.gotchas) return [];
  try {
    return [...readFileSync(join(b.root, b.gotchas.file), 'utf8').matchAll(/^##\s+([A-Z]+-\d+)\b:?[ \t]*(.*)$/gm)].map((m) => ({ id: m[1], title: m[2].trim() }));
  } catch {
    return [];
  }
}

/** Every workshop under <root> whose workshop.json reads, by id. */
export function listBundles(root = WORKSHOPS_SOURCE): WorkshopBundle[] {
  let ids: string[];
  try {
    ids = readdirSync(root, { withFileTypes: true }).filter((d) => d.isDirectory()).map((d) => d.name).sort();
  } catch {
    return [];
  }
  return ids.flatMap((id) => {
    try {
      return [readWorkshop(id, root)];
    } catch {
      return [];
    }
  });
}

/** A level's prompts, as Kode begins its sessions, each with the session's name. */
export function levelPrompts(b: WorkshopBundle, levelId: string): { name: string | null; text: string; learn: boolean }[] {
  const l = b.levels.find((x) => x.id === levelId);
  if (!l) throw new Error(`no level ${levelId}`);
  return l.prompts.map((p) => ({ name: p.name, text: readFileSync(join(b.root, p.file), 'utf8').trim(), learn: p.learn }));
}

/** The tour's gotchas datapack name, `adlc/repo/<demo folder>/gotchas`, from the bundle's `adlc/repo/<repo>/gotchas`. */
export function gotchasName(bundleName: string, folder: string): string {
  return bundleName.replace(/[^/]+\/gotchas$/, `${folder}/gotchas`);
}

/** `<base>`, or `<base>-2`, `<base>-3`, … if it's taken in <parent>. */
export function freeFolder(parent: string, base: string): string {
  let dest = join(parent, base);
  for (let i = 2; existsSync(dest); i++) dest = join(parent, `${base}-${i}`);
  return dest;
}

/** The demo's MEKO.md: the datapacks the code review reads (src/shared/mekomd.ts). */
export function demoMekoMd(datapacks: { name: string; id: string; about: string }[]): string {
  return [
    '# Meko datapacks',
    '',
    "The Meko datapacks that hold context for this repo. Search them before working on it.",
    '',
    "| Datapack | ID | What's in it |",
    '|---|---|---|',
    ...datapacks.map((d) => `| \`${d.name}\` | \`${d.id}\` | ${d.about.replace(/\|/g, '/')} |`),
    '',
  ].join('\n');
}

/** Builds the demo repo at <dest> (which must not exist): main from the bundle's files and <mekoMd>, then each branch's patch. */
export async function buildDemoRepo(b: WorkshopBundle, dest: string, mekoMd: string | null): Promise<void> {
  mkdirSync(dirname(dest), { recursive: true });
  cpSync(join(b.root, b.repo.dir), dest, { recursive: true, errorOnExist: true, force: false });
  try {
    if (mekoMd) writeFileSync(join(dest, 'MEKO.md'), mekoMd);
    await commitDemoRepo(dest, 'main', b.repo.author, b.repo.date, b.repo.message);
    for (const br of b.repo.branches) await demoBranch(dest, 'main', br.name, join(b.root, br.patch), b.repo.author);
  } catch (e) {
    rmSync(dest, { recursive: true, force: true });
    throw e;
  }
}

export class WorkshopService {
  /** One start per workshop at a time: a second one waits and then goes to the first one's project. */
  private starting = new Map<string, Promise<WorkshopStart>>();
  /** Projects whose gotchas are going into Meko, until Meko is done with them. */
  private uploading = new Set<string>();
  /** Level sessions whose learn step Kode is starting (`learnLevel`), until it has started or failed to. */
  private learning = new Set<string>();

  constructor(private core: Core, readonly root = WORKSHOPS_SOURCE) {}

  /** GET /api/workshops. */
  list(): Workshop[] {
    return listBundles(this.root).map((b) => ({
      id: b.id, title: b.title, summary: b.summary, projectId: this.projectOf(b.id),
      learn: b.levels.find((l) => l.learn)?.learn ?? null,
    }));
  }

  /** The workshop's bundle, or null when it doesn't read. */
  bundle(id: string): WorkshopBundle | null {
    try {
      return readWorkshop(id, this.root);
    } catch {
      return null;
    }
  }

  /** POST /api/workshops/:id/start. */
  start(id: string): Promise<WorkshopStart> {
    const going = this.starting.get(id);
    if (going) return going.catch(() => this.start(id));
    const p = this.doStart(id).finally(() => this.starting.delete(id));
    this.starting.set(id, p);
    return p;
  }

  /**
   * A fresh install's tour (`[welcome] tour`): started once while there are no projects, then the
   * key goes. One that fails keeps it, and the next start tries again.
   */
  async startOnFreshInstall(): Promise<void> {
    const id = this.core.config.welcome.tour.trim();
    if (!id) return;
    const clear = () => this.core.updateConfig((c) => {
      c.welcome.tour = '';
    });
    if (kvGet(this.core.db, TOUR_STARTED_KEY) || projectRows(this.core.db).length) {
      clear();
      return;
    }
    try {
      await this.start(id);
    } catch (e) {
      this.core.log.warn(`starting the ${id} workshop for a fresh install failed; trying again at the next start`, errMsg(e));
      return;
    }
    kvSet(this.core.db, TOUR_STARTED_KEY, id);
    clear();
  }

  /**
   * An activity event (SessionManager.event): a level's session gets its code review once its
   * worktree is made (`created`), before its own first turn is done.
   */
  noted(row: SessionRow, kind: string): void {
    if (kind !== 'created') return;
    void this.reviewLevel(row.id);
    this.closeEarlier(row);
  }

  /**
   * A level's later session (its prompts are in order, by name: "Ask again" after "Ask about the
   * game") replaces the earlier ones: the tour begins it only once the user has read them and pressed
   * its button, and Kode archives them then, so the user has one session of the level on screen.
   * Archive, as at a level's end; one still saving what it learned, or about to (`saving`), stays open.
   */
  private closeEarlier(row: SessionRow): void {
    if (row.kind === 'code-review' || row.purpose) return;
    const workshop = projectRow(this.core.db, row.project_id)?.workshop;
    const b = workshop ? this.bundle(workshop) : null;
    if (!b) return;
    const name = row.name.trim().toLowerCase();
    for (const l of b.levels) {
      const at = l.prompts.findIndex((p) => p.name?.toLowerCase() === name);
      if (at <= 0) continue;
      const earlier = new Set(l.prompts.slice(0, at).map((p) => p.name!.toLowerCase()));
      const open = all<SessionRow>(this.core.db,
        `SELECT * FROM sessions WHERE project_id = ? AND desired = 'running' AND kind != 'code-review' AND purpose IS NULL AND id != ?`,
        row.project_id, row.id).filter((s) => earlier.has(s.name.trim().toLowerCase()) && !this.saving(s.id));
      for (const s of open) {
        this.core.sessions.archive(s, { keepBranch: true })
          .catch((e: unknown) => this.core.log.warn(`archiving ${s.id} for the next session of its workshop level failed`, errMsg(e)));
      }
    }
  }

  /**
   * A session's turn ended (SessionManager's turn end, after the diff's refresh): a level's session
   * whose review couldn't start at its creation gets it now, and a level's question session (`learn`
   * on its prompt) gets its learn step.
   */
  turnEnded(id: string, interrupted: boolean): void {
    if (interrupted) return;
    const row = sessionRow(this.core.db, id);
    if (row) this.core.tours.turnEnded(row);
    void this.reviewLevel(id);
    void this.learnLevel(id);
  }

  /**
   * The learn step on a level's question session (a prompt with `learn`, by the session's name), once,
   * after its first turn: Kode saves what it worked out and marks the question Meko couldn't answer
   * taught, while the user watches. The step reads the open questions on Context Gaps itself, once,
   * when it starts: Meko isn't polled for them. Why one couldn't start is kept for the tour's card.
   */
  private async learnLevel(id: string): Promise<void> {
    const row = sessionRow(this.core.db, id);
    if (!row || row.kind === 'code-review' || row.purpose || row.desired !== 'running' || this.learning.has(id)) return;
    const workshop = projectRow(this.core.db, row.project_id)?.workshop;
    const dp = sessionDatapack(this.core.db, row);
    if (!workshop || dp?.role !== 'dedicated' || !this.core.mekoStatus.configured) return;
    const b = this.bundle(workshop);
    const name = row.name.trim().toLowerCase();
    if (!b?.levels.some((l) => l.prompts.some((p) => p.learn && p.name?.toLowerCase() === name))) return;
    if (get(this.core.db, 'SELECT 1 AS x FROM meko_learns WHERE session_id = ? LIMIT 1', id)) return;
    this.learning.add(id);
    try {
      await this.core.learn.start(row, 'manual');
    } catch (e) {
      this.core.log.warn(`the learn step on ${id} for its workshop level didn't start`, errMsg(e));
      kvSet(this.core.db, `${LEARN_FAILED_KEY}:${id}`, errMsg(e));
    } finally {
      this.learning.delete(id);
      this.core.changed();
    }
  }

  /** Why Kode couldn't run the learn step on a level's question session, or null. */
  learnFailed(id: string): string | null {
    return kvGet(this.core.db, `${LEARN_FAILED_KEY}:${id}`);
  }

  /** The tour's Retry on that learn step: the reason goes, and the step starts again. */
  clearLearnFailed(id: string): void {
    kvDelete(this.core.db, `${LEARN_FAILED_KEY}:${id}`);
  }

  /** A learn step runs on the session, or Kode is starting one: archiving it would cut the save short. */
  saving(id: string): boolean {
    return this.learning.has(id) || this.core.learn.running(id);
  }

  /**
   * Runs the code review on a level's session (`levels[].review`): the first task at Code on the
   * level's branch in its workshop's project, once, with no PR description first (no PR to describe).
   */
  private async reviewLevel(id: string): Promise<void> {
    const row = sessionRow(this.core.db, id);
    if (!row || row.kind === 'code-review' || row.purpose || row.stage !== 'code' || row.desired !== 'running') return;
    const workshop = projectRow(this.core.db, row.project_id)?.workshop;
    const level = workshop ? this.bundle(workshop)?.levels.find((l) => l.review?.branch === row.branch) : undefined;
    if (!level) return;
    // Only while no other open session in the project has a review: one the level's open session
    // runs makes the others (check:demo-review's) run theirs by hand, and a replay of the level gets its own.
    if (get(this.core.db, `SELECT 1 FROM code_reviews c JOIN sessions s ON s.id = c.task_id
      WHERE s.project_id = ? AND s.desired = 'running' LIMIT 1`, row.project_id)) return;
    // Without a gotchas datapack (no Meko when it was built) there's nothing to add later, and once
    // the gotchas are in (a replay) there's no empty datapack to look with: one review.
    const firstLook = !!level.review?.gotchasAfterFirst && !!this.gotchasOf(row.project_id) && !this.gotchasIn(row.project_id);
    await this.runLevelReview(row, firstLook).catch((e: unknown) => this.core.tours.reviewFailed(row.id, errMsg(e)));
  }

  /** The model <task>'s code-review session starts on, when its project is a workshop's that names one for its agent (`reviewModels`). */
  reviewModel(task: SessionRow): string | null {
    return this.modelFor(task.project_id, harnessOf(task.harness));
  }

  /** The model the workshop's own sessions on <harness> start on in <projectId> (`reviewModels`), or null for the agent's default. */
  modelFor(projectId: string, harness: HarnessId): string | null {
    const workshop = projectRow(this.core.db, projectId)?.workshop;
    return (workshop ? this.bundle(workshop)?.reviewModels[harness] : null) ?? null;
  }

  /**
   * Drops the project's gotchas datapack and makes it again, empty, under the same name (Review without
   * context's Run the review), so that review never finds gotchas from an earlier run: faster than
   * deleting what's in it. The new id goes into the demo's MEKO.md.
   */
  async freshGotchas(projectId: string): Promise<void> {
    const old = this.gotchasOf(projectId);
    if (!old) throw new HttpError(409, "The tour has no gotchas datapack: Meko wasn't running when it started. Close the Kode tour project, then start the tour again.");
    await this.freeBranches(projectId);
    const dp = await this.remake(old);
    kvDelete(this.core.db, `${GOTCHAS_IN_KEY}:${projectId}`);
    kvDelete(this.core.db, `${GOTCHAS_RESULT_KEY}:${projectId}`);
    await this.swapInMekoMd(projectId, old.id, dp.id);
    this.core.changed();
  }

  /**
   * Drops the project's own datapack and makes it again, empty, under the same name (Learn the
   * project), so Level 2 always learns from nothing; the project keeps it as its dedicated datapack.
   */
  async freshProjectDatapack(projectId: string): Promise<void> {
    const old = dedicatedDatapack(this.core.db, projectId);
    if (!old) throw new HttpError(409, "The tour's project has no datapack of its own.");
    await this.freeBranches(projectId);
    const dp = await this.remake(old);
    tx(this.core.db, () => {
      run(this.core.db, 'UPDATE project_datapacks SET datapack_id = ?, name = ? WHERE project_id = ? AND datapack_id = ?', dp.id, dp.name, projectId, old.id);
      // Its server-side conversation was made in the old datapack.
      run(this.core.db, 'UPDATE projects SET meko_conversation_id = NULL WHERE id = ?', projectId);
    });
    this.core.projects.ensureConversation(projectId).catch((e: unknown) => this.core.log.warn('Meko conversation create failed', errMsg(e)));
    await this.swapInMekoMd(projectId, old.id, dp.id);
    this.core.changed();
  }

  /**
   * Before a fresh datapack drops anything, the demo's branches are made free for recommitDemo, so
   * its MEKO.md and branches can't be left naming different datapacks: a worktree a closed session
   * left on one (Archive keeps a worktree with uncommitted changes on its branch) is detached, its
   * changes kept; an open session on one is a 409. A replay starts the branch again from the
   * teammate's fix, so commits a level's session made on it go.
   */
  private async freeBranches(projectId: string): Promise<void> {
    const p = projectRow(this.core.db, projectId);
    const b = p?.workshop ? this.bundle(p.workshop) : null;
    if (!p || !b) return;
    const names = b.repo.branches.map((br) => br.name);
    const open = all<{ name: string; branch: string }>(this.core.db, `SELECT name, branch FROM sessions WHERE project_id = ? AND desired = 'running' AND kind != 'code-review'`, projectId)
      .find((s) => names.includes(s.branch));
    if (open) throw new HttpError(409, `${open.name} is still open on ${open.branch}. Archive it, then try again.`);
    if (!(await freeDemoBranches(p.repo_path, names))) throw new HttpError(409, `A worktree on ${names.join(', ')} couldn't be let go of, so the demo can't be made again.`);
  }

  /** <old> deleted in Meko (one already gone is fine) and made again under its name. */
  private async remake(old: { id: string; name: string }): Promise<{ id: string; name: string }> {
    await this.core.meko.deleteDatapack(old.id).catch((e: unknown) => this.core.log.warn(`deleting the tour's datapack ${old.name} failed`, errMsg(e)));
    try {
      return await this.core.meko.createDatapack(old.name);
    } catch (e) {
      throw new HttpError(502, `Meko couldn't make ${old.name} again: ${errMsg(e)}`);
    }
  }

  /** The demo's MEKO.md with <oldId> replaced by <newId>, committed on main, its branches made again on top. */
  private async swapInMekoMd(projectId: string, oldId: string, newId: string): Promise<void> {
    const p = projectRow(this.core.db, projectId);
    const b = p?.workshop ? this.bundle(p.workshop) : null;
    if (!p || !b) return;
    const file = join(p.repo_path, 'MEKO.md');
    let text: string;
    try {
      text = readFileSync(file, 'utf8');
    } catch {
      return;
    }
    if (!text.includes(oldId)) return;
    writeFileSync(file, text.split(oldId).join(newId));
    try {
      await recommitDemo(p.repo_path, 'main', 'MEKO.md', "Point MEKO.md at the tour's new datapack", b.repo.branches.map((br) => ({ name: br.name, patch: join(b.root, br.patch) })), b.repo.author);
    } catch (e) {
      throw new HttpError(500, `Couldn't update the demo's MEKO.md: ${errMsg(e)}`);
    }
  }

  /** POST /api/workshops/:id/go: after a level's first look, the gotchas go into Meko and Kode reviews again. */
  async go(id: string): Promise<void> {
    const projectId = this.projectOf(id);
    if (!projectId) throw new HttpError(404, `The ${id} workshop isn't open.`);
    const task = this.waitingFirstLook(projectId);
    if (!task || this.uploading.has(projectId)) throw new HttpError(409, "The tour isn't waiting for a go: no level's first look is done and waiting.");
    const after = () => {
      const cur = sessionRow(this.core.db, task);
      if (cur?.desired === 'running') this.runLevelReview(cur, false).catch((e: unknown) => this.core.tours.reviewFailed(cur.id, errMsg(e)));
    };
    if (this.gotchasIn(projectId)) after();
    else if (!this.addGotchas(projectId, after)) throw new HttpError(409, 'This tour has no gotchas to add.');
  }

  /** The level's task whose first look is done with no review with the gotchas after it, or null. */
  private waitingFirstLook(projectId: string): string | null {
    return get<{ task_id: string }>(this.core.db,
      `SELECT c.task_id FROM code_reviews c JOIN sessions s ON s.id = c.task_id
       WHERE s.project_id = ? AND s.desired = 'running' AND c.hand_off = 0 AND c.state = 'done'
         AND NOT EXISTS (SELECT 1 FROM code_reviews d WHERE d.task_id = c.task_id AND d.hand_off = 1)
       ORDER BY c.created_at LIMIT 1`, projectId)?.task_id ?? null;
  }

  /**
   * Uploads the workshop's gotchas to the project's gotchas datapack (Teach it), then calls <after>
   * once Meko is done with them, indexed or not, or the upload failed. False when there's nothing to
   * upload or an upload is already under way.
   */
  addGotchas(projectId: string, after: () => void = () => undefined): boolean {
    const workshop = projectRow(this.core.db, projectId)?.workshop;
    const b = workshop ? this.bundle(workshop) : null;
    const dp = this.gotchasOf(projectId);
    if (!b?.gotchas || !dp || this.uploading.has(projectId)) return false;
    this.uploading.add(projectId);
    void this.uploadGotchas(projectId, dp, join(b.root, b.gotchas.file), (current) => {
      this.uploading.delete(projectId);
      this.core.changed();
      if (current) after();
    });
    this.core.changed();
    return true;
  }

  /** The gotchas are going into Meko for the project. */
  addingGotchas(projectId: string): boolean {
    return this.uploading.has(projectId);
  }

  /** How the gotchas' last upload ended: `indexed`, a reason it didn't, or null before any. */
  gotchasResult(projectId: string): string | null {
    return kvGet(this.core.db, `${GOTCHAS_RESULT_KEY}:${projectId}`);
  }

  /** The gotchas datapack the project's demo lists in its MEKO.md, or null. */
  gotchasOf(projectId: string): { id: string; name: string } | null {
    const p = projectRow(this.core.db, projectId);
    try {
      const d = p && parseMekoMd(readFileSync(join(p.repo_path, 'MEKO.md'), 'utf8')).find((x) => isGotchasDatapack(x.name));
      return d ? { id: d.id, name: d.name } : null;
    } catch {
      return null;
    }
  }

  /** Whether the project's gotchas are indexed in Meko (`uploadGotchas`) in the datapack its MEKO.md names now, so a level's first look can't find its datapack empty. */
  gotchasIn(projectId: string): boolean {
    const id = kvGet(this.core.db, `${GOTCHAS_IN_KEY}:${projectId}`);
    return id !== null && id === this.gotchasOf(projectId)?.id;
  }

  /**
   * Kode runs the level's review: a first look keeps its findings; the real one's wait for the user.
   * A refusal is logged and thrown: each caller shows it on the tour's card, through the act's answer
   * or `tours.reviewFailed`.
   */
  async runLevelReview(row: SessionRow, firstLook: boolean): Promise<void> {
    try {
      await this.core.reviews.runNow(row, 'auto', { describe: false, handOff: !firstLook });
    } catch (e) {
      this.core.log.warn(`running the code review of ${row.id} for its workshop failed`, errMsg(e));
      throw e;
    }
  }

  /** The workshop's open project, or null. */
  projectOf(id: string): string | null {
    return get<{ id: string }>(this.core.db, 'SELECT id FROM projects WHERE workshop = ? AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1', id)?.id ?? null;
  }

  private async doStart(id: string): Promise<WorkshopStart> {
    const b = this.bundle(id);
    if (!b) throw new HttpError(404, `There's no workshop ${id} in this Kode.`);
    const open = this.projectOf(id);
    if (open) return { projectId: open };
    const meko = this.core.mekoStatus.configured;
    const dest = freeFolder(expandHome(this.core.config.repos.cloneInto), b.repo.folder);
    // The gotchas datapack goes first: at the plan's datapack limit, the project goes without its own.
    const gotchas = meko && b.gotchas ? await this.gotchasDatapack(gotchasName(b.gotchas.datapack, basename(dest))) : null;
    const dedicated = meko ? await this.dedicatedDatapack(b, dest) : null;
    const listed = [
      ...(gotchas ? [{ ...gotchas, about: b.gotchas?.about ?? '' }] : []),
      ...(dedicated ? [{ ...dedicated, about: `What the team knows about ${b.title}'s demo` }] : []),
    ];
    try {
      await buildDemoRepo(b, dest, listed.length ? demoMekoMd(listed) : null);
    } catch (e) {
      // Nothing points at the datapacks made for it: left, each try would add two more toward the plan's limit.
      for (const dp of [gotchas, dedicated]) {
        if (dp) await this.core.meko.deleteDatapack(dp.id).catch((d: unknown) => this.core.log.warn(`deleting the unused datapack ${dp.name} failed`, errMsg(d)));
      }
      throw new HttpError(500, `Couldn't build the ${b.title} demo in ${dest}: ${errMsg(e)}`);
    }
    const project = await this.core.projects.create({
      repoPath: dest, name: b.title, datapack: dedicated ? { mode: 'existing', id: dedicated.id, name: dedicated.name } : { mode: 'none' },
    });
    run(this.core.db, 'UPDATE projects SET workshop = ? WHERE id = ?', id, project.id);
    if (b.codeReview) this.core.projects.update(project.id, { settings: { codeReview: b.codeReview } });
    this.core.changed();
    this.core.log.info(`workshop ${id} started: project ${project.id} in ${dest}`);
    // A level that looks first without them gets the gotchas at its Teach it; otherwise they go in now.
    if (gotchas && b.gotchas && !b.levels.some((l) => l.review?.gotchasAfterFirst)) this.addGotchas(project.id);
    return { projectId: project.id };
  }

  /**
   * A new gotchas datapack for this tour, never one reused by name: a level's first review must find
   * it empty. A name taken (in Meko, where names are unique) gets a suffix on its repo part, keeping
   * the /gotchas ending the review looks for. Null if Meko refuses.
   */
  private async gotchasDatapack(name: string): Promise<{ id: string; name: string } | null> {
    try {
      const taken = new Set((await this.core.meko.listDatapacks()).map((d) => d.name));
      for (let i = 2; taken.has(name); i++) name = name.replace(/(-\d+)?\/gotchas$/, `-${i}/gotchas`);
      try {
        return await this.core.meko.createDatapack(name);
      } catch (e) {
        if (isDatapackLimit(e)) throw e;
        // Datapack names are unique across Meko: keep the /gotchas ending the review looks for.
        const suffixed = name.replace(/\/gotchas$/, `-${Math.random().toString(16).slice(2, 6)}/gotchas`);
        return await this.core.meko.createDatapack(suffixed);
      }
    } catch (e) {
      this.core.log.warn(`the workshop's gotchas datapack ${name} couldn't be made; its code review runs without it`, errMsg(e));
      return null;
    }
  }

  /** The project's dedicated datapack, named as Open project names one; null at the plan's limit or when Meko refuses. */
  private async dedicatedDatapack(b: WorkshopBundle, dest: string): Promise<{ id: string; name: string } | null> {
    try {
      const taken = new Set((await this.core.meko.listDatapacks()).map((d) => d.name));
      const name = freeDatapackName(defaultProjectDatapackName(basename(dest), b.title), taken);
      return await this.core.meko.createDatapack(name);
    } catch (e) {
      this.core.log.warn(`the ${b.title} project's datapack couldn't be made; it opens without one`, errMsg(e));
      return null;
    }
  }

  /** Uploads the gotchas, then calls <after> once Meko is done with them; how it ended is kept for Teach it's receipt. */
  private async uploadGotchas(projectId: string, dp: { id: string; name: string }, file: string, after: (current: boolean) => void): Promise<void> {
    // An upload that ends after Run the review made the datapack again (freshGotchas) went to the
    // old one: what it found is dropped, so the new run's first look and Teach stage start clean.
    const current = () => this.gotchasOf(projectId)?.id === dp.id;
    const done = (result: KnowledgeResult) => {
      if (!current()) return after(false);
      if (result === 'indexed') kvSet(this.core.db, `${GOTCHAS_IN_KEY}:${projectId}`, dp.id);
      kvSet(this.core.db, `${GOTCHAS_RESULT_KEY}:${projectId}`, result === 'indexed' ? 'indexed' : `Meko didn't finish indexing them (${result}), so the review may not find them.`);
      after(true);
    };
    try {
      await this.core.knowledge.uploadFile(dp, 'gotchas.md', readFileSync(file), done);
    } catch (e) {
      this.core.log.warn(`uploading the workshop's gotchas to ${dp.name} failed`, errMsg(e));
      if (!current()) return after(false);
      kvSet(this.core.db, `${GOTCHAS_RESULT_KEY}:${projectId}`, `the upload failed (${errMsg(e)}).`);
      after(true);
    }
  }
}
