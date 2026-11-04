import { cpSync, existsSync, mkdirSync, readdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { basename, dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { readOverrides, type ReviewOverrides } from '../shared/codereview.js';
import { defaultProjectDatapackName, freeDatapackName } from '../shared/datapacks.js';
import { HARNESS_CAPS, harnessOf, isHarnessId, type HarnessId } from '../shared/harnesses.js';
import { isGotchasDatapack, parseMekoMd } from '../shared/mekomd.js';
import type { DiffStat, Workshop, WorkshopStart } from '../shared/types.js';
import type { Core } from './core.js';
import { all, get, kvGet, kvSet, run } from './db.js';
import { commitDemoRepo, demoBranch, type GitIdent } from './git.js';
import type { KnowledgeResult } from './knowledge.js';
import { errMsg } from './log.js';
import { expandHome } from './paths.js';
import { HttpError, isDatapackLimit } from './projects.js';
import { projectRow, projectRows, sessionRow, type SessionRow } from './store.js';

/**
 * Workshops bundled with AgentK (docs/design/workshops.md): each is a folder of `src/workshops/`
 * (`dist/workshops` in a release) with a host's script and the demo repo it's played in. Starting
 * one builds the demo repo with no remote, opens it as a project, seeds its gotchas datapack in Meko
 * and begins its host, a task at Explore with `purpose = 'guide'`. GuideEvents tells the host what
 * happens in the project's other sessions.
 */

/** Beside the server's own folder, as the session tools' skill is: `src/workshops` in dev, `dist/workshops` in a release. */
export const WORKSHOPS_SOURCE = join(dirname(fileURLToPath(import.meta.url)), '..', 'workshops');

/** The kv key that says a fresh install's tour ran, so it never runs by itself twice. */
const TOUR_STARTED_KEY = 'tour-started';
/** The kv key, with `:<project id>`, that says a workshop project's gotchas are indexed in Meko (the datapack's id). */
const GOTCHAS_IN_KEY = 'workshop-gotchas';

/** A workshop's `workshop.json`, checked. */
export interface WorkshopBundle {
  id: string;
  title: string;
  summary: string;
  /** The host's script, relative to the bundle. */
  guide: string;
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
   * The model each agent's code-review sessions start on in the workshop's project (the tour's:
   * Claude Code's Sonnet), by harness since model names differ; its passes and scorers keep their
   * tiers' models (`[code_review.models]`). An agent left out starts on its own default.
   */
  reviewModels: Partial<Record<HarnessId, string>>;
  /**
   * `review`: AgentK runs its code review itself on the level's session (a task in the project on
   * that branch) as soon as its worktree is made, so the user watches the review rather than starts it.
   * With `gotchasAfterFirst`, that first review runs while the gotchas datapack is still empty and
   * keeps its findings; AgentK then adds the gotchas to Meko and reviews again (`firstLookDone`).
   */
  levels: { id: string; title: string; prompt: string; needs: string[]; review: { branch: string; gotchasAfterFirst: boolean } | null }[];
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
  return {
    id,
    title: str(raw.title, 'title'),
    summary: str(raw.summary, 'summary'),
    guide: str(raw.guide, 'guide'),
    repo: {
      dir: str(repo.dir, 'repo.dir'),
      folder: str(repo.folder, 'repo.folder'),
      author: { name: str(author?.name, 'repo.author.name'), email: str(author?.email, 'repo.author.email') },
      date: str(repo.date, 'repo.date'),
      message: str(repo.message, 'repo.message'),
      branches: branches.map((b) => ({ name: str(b.name, 'a branch name'), patch: str(b.patch, 'a branch patch') })),
    },
    gotchas: g ? { file: str(g.file, 'gotchas.file'), datapack: str(g.datapack, 'gotchas.datapack'), about: typeof g.about === 'string' ? g.about : '' } : null,
    codeReview: raw.codeReview ? readOverrides(raw.codeReview) : null,
    reviewModels: Object.fromEntries(Object.entries((raw.reviewModels ?? {}) as Record<string, unknown>)
      .filter((e): e is [HarnessId, string] => isHarnessId(e[0]) && typeof e[1] === 'string' && !!e[1].trim())),
    levels: levels.map((l) => {
      const review = l.review as Record<string, unknown> | null | undefined;
      return {
        id: str(l.id, 'a level id'), title: str(l.title, 'a level title'), prompt: str(l.prompt, 'a level prompt'),
        needs: Array.isArray(l.needs) ? l.needs.filter((n): n is string => typeof n === 'string') : [],
        review: review ? { branch: str(review.branch, 'a level review branch'), gotchasAfterFirst: review.gotchasAfterFirst === true } : null,
      };
    }),
    root: dir,
  };
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

/** A level's prompt, as the host passes it to start_sessions. */
export function levelPrompt(b: WorkshopBundle, levelId: string): string {
  const l = b.levels.find((x) => x.id === levelId);
  if (!l) throw new Error(`no level ${levelId}`);
  return readFileSync(join(b.root, l.prompt), 'utf8').trim();
}

/** What GUIDE.md's placeholders take. */
export interface GuideVars {
  projectId: string;
  repo: string;
  meko: 'on' | 'off';
  harness: string;
  gotchas: string;
}

/** The host's first prompt: GUIDE.md with its placeholders filled in, each level's prompt under {{levels}}. */
export function renderGuide(b: WorkshopBundle, vars: GuideVars): string {
  const levels = b.levels.map((l, i) => `### Level ${i + 1} (${l.id}): ${l.title}\n\n\`\`\`\n${levelPrompt(b, l.id)}\n\`\`\``).join('\n\n');
  const all: Record<string, string> = { ...vars, levels };
  return readFileSync(join(b.root, b.guide), 'utf8').replace(/\{\{(\w+)\}\}/g, (m, k: string) => all[k] ?? m).trim();
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

/** The project's open host, if any: its newest guide session that isn't archived. */
export function hostOf(core: Core, projectId: string): SessionRow | null {
  return get<SessionRow>(core.db,
    `SELECT * FROM sessions WHERE project_id = ? AND purpose = 'guide' AND desired = 'running' ORDER BY created_at DESC LIMIT 1`, projectId) ?? null;
}

export class WorkshopService {
  readonly events: GuideEvents;
  /** One start per workshop at a time: a second one waits and then goes to the first one's host. */
  private starting = new Map<string, Promise<WorkshopStart>>();
  /** The level sessions whose gotchas are going into Meko after the user's go, until their review starts. */
  private going = new Set<string>();

  constructor(private core: Core, private root = WORKSHOPS_SOURCE) {
    this.events = new GuideEvents(core);
  }

  /** GET /api/workshops. */
  list(): Workshop[] {
    return listBundles(this.root).map((b) => {
      const p = this.projectOf(b.id);
      return { id: b.id, title: b.title, summary: b.summary, projectId: p, hostId: p ? hostOf(this.core, p)?.id ?? null : null };
    });
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
   * An activity event (SessionManager.event): its host hears of it, and a level's session gets its
   * code review once its worktree is made (`created`), before its own first turn is done.
   */
  noted(row: SessionRow, kind: string, text: string | null): void {
    this.events.noted(row, kind, text);
    if (kind === 'created') void this.reviewLevel(row.id);
  }

  /**
   * A session's turn ended (SessionManager's turn end, after the diff's refresh): its host hears of
   * it, and a level's session whose review couldn't start at its creation gets it now.
   */
  turnEnded(id: string, interrupted: boolean): void {
    this.events.turnEnded(id, interrupted);
    if (!interrupted) void this.reviewLevel(id);
  }

  /**
   * Runs the code review on a level's session (`levels[].review`): the first task at Code on the
   * level's branch in its workshop's project, once, with no PR description first (no PR to describe).
   */
  private async reviewLevel(id: string): Promise<void> {
    const row = sessionRow(this.core.db, id);
    if (!row || row.kind === 'code-review' || row.purpose || row.stage !== 'code' || row.desired !== 'running') return;
    const workshop = projectRow(this.core.db, row.project_id)?.workshop;
    if (!workshop) return;
    let b: WorkshopBundle;
    try {
      b = readWorkshop(workshop, this.root);
    } catch {
      return;
    }
    const level = b.levels.find((l) => l.review?.branch === row.branch);
    if (!level) return;
    // Only while no other open session in the project has a review: one the level's open session
    // runs makes the others (check:demo-review's) run theirs by hand, and a replay of the level gets its own.
    if (get(this.core.db, `SELECT 1 FROM code_reviews c JOIN sessions s ON s.id = c.task_id
      WHERE s.project_id = ? AND s.desired = 'running' LIMIT 1`, row.project_id)) return;
    // Without a gotchas datapack (no Meko when it was built) there's nothing to add later, and once
    // the gotchas are in (a replay) there's no empty datapack to look with: one review.
    const firstLook = !!level.review?.gotchasAfterFirst && !!this.gotchasOf(row.project_id) && !this.gotchasIn(row.project_id);
    await this.runLevelReview(row, firstLook);
  }

  /** The model <task>'s code-review session starts on, when its project is a workshop's that names one for its agent (`reviewModels`). */
  reviewModel(task: SessionRow): string | null {
    const workshop = projectRow(this.core.db, task.project_id)?.workshop;
    if (!workshop) return null;
    try {
      return readWorkshop(workshop, this.root).reviewModels[harnessOf(task.harness)] ?? null;
    } catch {
      return null;
    }
  }

  /** The level's first look is done: AgentK waits for the user's go (`go`) to add the gotchas and review again. */
  firstLookDone(taskId: string): void {
    const row = sessionRow(this.core.db, taskId);
    const dp = row && projectRow(this.core.db, row.project_id)?.workshop ? this.gotchasOf(row.project_id) : null;
    if (row && dp) this.events.tell(row.project_id, `AgentK waits for the user's go to add the team's gotchas to Meko (${dp.name}) and review ${row.name} again.`);
  }

  /**
   * A prompt the host's user typed (its UserPromptSubmit): "go" (as its first word) goes on from
   * the level's pause. Anything else, and a go with nothing waiting, is the host's to answer.
   */
  hostPrompt(row: SessionRow, text: string | undefined): void {
    if (row.purpose !== 'guide' || !text || !/^\W*go\b/i.test(text)) return;
    this.goOn(row.project_id).catch(() => undefined);
  }

  /**
   * What the host said as its turn ended (its Stop's `last_assistant_message`): a last line of
   * "That's the end of Level N" archives that level's sessions, so the user has nothing left to
   * tidy. Archive, not Discard: a worktree with uncommitted changes (the level's fixes) is kept.
   */
  hostSaid(row: SessionRow, text: string | undefined): void {
    const last = text?.trim().split('\n').pop() ?? '';
    const n = row.purpose === 'guide' ? /^\W*that['’]s the end of level (\d+)\W*$/i.exec(last)?.[1] : undefined;
    const p = n ? projectRow(this.core.db, row.project_id) : undefined;
    if (!p?.workshop) return;
    let branch: string | undefined;
    try {
      branch = readWorkshop(p.workshop, this.root).levels[Number(n) - 1]?.review?.branch;
    } catch {
      return;
    }
    if (!branch) return;
    const ids = all<{ id: string }>(this.core.db,
      `SELECT id FROM sessions WHERE project_id = ? AND desired = 'running' AND branch = ? AND kind != 'code-review' AND purpose IS NOT 'guide'`,
      row.project_id, branch);
    for (const { id } of ids) {
      const s = sessionRow(this.core.db, id);
      if (s) this.core.sessions.archive(s, { keepBranch: true }).catch((e: unknown) => this.core.log.warn(`archiving ${id} at the end of its workshop level failed`, errMsg(e)));
    }
  }

  /** POST /api/workshops/:id/go: what the user's go in the host does. */
  async go(id: string): Promise<void> {
    const projectId = this.projectOf(id);
    if (!projectId) throw new HttpError(404, `The ${id} workshop isn't open.`);
    if (!await this.goOn(projectId)) throw new HttpError(409, "The tour isn't waiting for a go: no level's first look is done and waiting.");
  }

  /** Goes on from the pause after a level's first look: the gotchas go into Meko, and once they're indexed AgentK reviews again. */
  private async goOn(projectId: string): Promise<boolean> {
    const p = projectRow(this.core.db, projectId);
    const dp = this.gotchasOf(projectId);
    // The first look done, and no review with the gotchas yet.
    const task = get<{ task_id: string }>(this.core.db,
      `SELECT c.task_id FROM code_reviews c JOIN sessions s ON s.id = c.task_id
       WHERE s.project_id = ? AND s.desired = 'running' AND c.hand_off = 0 AND c.state = 'done'
         AND NOT EXISTS (SELECT 1 FROM code_reviews d WHERE d.task_id = c.task_id AND d.hand_off = 1)
       ORDER BY c.created_at LIMIT 1`, projectId)?.task_id;
    if (!p?.workshop || !dp || !task || this.going.has(task)) return false;
    let b: WorkshopBundle;
    try {
      b = readWorkshop(p.workshop, this.root);
    } catch {
      return false;
    }
    if (!b.gotchas) return false;
    this.going.add(task);
    void this.uploadGotchas(projectId, dp, join(b.root, b.gotchas.file), () => {
      const cur = sessionRow(this.core.db, task);
      if (cur?.desired === 'running') void this.runLevelReview(cur, false).finally(() => this.going.delete(task));
      else this.going.delete(task);
    });
    return true;
  }

  /** The gotchas datapack the project's demo lists in its MEKO.md, or null. */
  private gotchasOf(projectId: string): { id: string; name: string } | null {
    const p = projectRow(this.core.db, projectId);
    try {
      const d = p && parseMekoMd(readFileSync(join(p.repo_path, 'MEKO.md'), 'utf8')).find((x) => isGotchasDatapack(x.name));
      return d ? { id: d.id, name: d.name } : null;
    } catch {
      return null;
    }
  }

  /** AgentK runs the level's review: a first look keeps its findings; the real one's wait for the user. */
  private async runLevelReview(row: SessionRow, firstLook: boolean): Promise<void> {
    try {
      await this.core.reviews.runNow(row, 'auto', { describe: false, handOff: !firstLook });
    } catch (e) {
      this.core.log.warn(`running the code review of ${row.id} for its workshop failed`, errMsg(e));
      this.events.tell(row.project_id, `AgentK couldn't start the code review of ${row.name}: ${errMsg(e)} The user can press Run code review.`);
    }
  }

  /** The host's first prompt (the `guide` prompt), for a guide session. */
  guidePrompt(row: SessionRow): string {
    const p = projectRow(this.core.db, row.project_id);
    if (!p?.workshop) throw new Error(`${row.id} isn't in a workshop's project`);
    const b = readWorkshop(p.workshop, this.root);
    // The gotchas datapack is the one the demo's MEKO.md lists: it isn't attached to the project.
    return renderGuide(b, {
      projectId: p.id, repo: p.repo_path, meko: this.core.mekoStatus.configured ? 'on' : 'off',
      harness: HARNESS_CAPS[harnessOf(row.harness)].label, gotchas: this.gotchasOf(p.id)?.name ?? 'none',
    });
  }

  private projectOf(id: string): string | null {
    return get<{ id: string }>(this.core.db, 'SELECT id FROM projects WHERE workshop = ? AND deleted_at IS NULL ORDER BY created_at DESC LIMIT 1', id)?.id ?? null;
  }

  private async doStart(id: string): Promise<WorkshopStart> {
    let b: WorkshopBundle;
    try {
      b = readWorkshop(id, this.root);
    } catch {
      throw new HttpError(404, `There's no workshop ${id} in this AgentK.`);
    }
    const open = this.projectOf(id);
    if (open) {
      const host = hostOf(this.core, open);
      return { projectId: open, sessionId: host?.id ?? await this.beginHost(open) };
    }
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
    const sessionId = await this.beginHost(project.id);
    // A level that looks first without them gets the gotchas after its first review; otherwise they go in now.
    if (gotchas && b.gotchas && !b.levels.some((l) => l.review?.gotchasAfterFirst)) {
      void this.uploadGotchas(project.id, gotchas, join(b.root, b.gotchas.file), () => undefined);
    }
    return { projectId: project.id, sessionId };
  }

  private async beginHost(projectId: string): Promise<string> {
    const { session, done } = await this.core.sessions.begin(projectId, {
      name: 'AgentK tour', kind: 'task', start: 'explore', prompt: 'Host the AgentK tour.',
    }, { purpose: 'guide' });
    // The tab shows the start's progress; a failed start is kept for Try again, as any other.
    done.catch((e) => this.core.log.warn(`the ${projectId} tour's host didn't start`, errMsg(e)));
    return session.id;
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

  /**
   * Uploads the gotchas, then calls <after> once Meko is done with them, indexed or not, or the
   * upload failed. The host hears only when it went wrong.
   */
  /** Whether the project's gotchas are indexed in Meko (`uploadGotchas`), so a level's first look can't find its datapack empty. */
  private gotchasIn(projectId: string): boolean {
    return kvGet(this.core.db, `${GOTCHAS_IN_KEY}:${projectId}`) !== null;
  }

  private async uploadGotchas(projectId: string, dp: { id: string; name: string }, file: string, after: () => void): Promise<void> {
    const done = (result: KnowledgeResult) => {
      if (result === 'indexed') kvSet(this.core.db, `${GOTCHAS_IN_KEY}:${projectId}`, dp.id);
      else this.events.tell(projectId, `The demo's gotchas didn't finish indexing in Meko (${result}), so the code review may not find them.`);
      after();
    };
    try {
      await this.core.knowledge.uploadFile(dp, 'gotchas.md', readFileSync(file), done);
    } catch (e) {
      this.core.log.warn(`uploading the workshop's gotchas to ${dp.name} failed`, errMsg(e));
      this.events.tell(projectId, `The demo's gotchas couldn't be uploaded to Meko (${errMsg(e)}), so the code review runs without them.`);
      after();
    }
  }
}

/** The activity events a host hears about from the other sessions in its project. */
const FOLLOWED_KINDS = new Set(['created', 'stage', 'review', 'design', 'archived', 'discarded', 'exited']);
/** How long after an event the host gets it when it's between turns, so a burst goes as one prompt. */
export const GUIDE_SETTLE_MS = 3000;
/** The most events one prompt carries; older ones are dropped first. */
const GUIDE_EVENTS_MAX = 30;

/**
 * Tells a workshop's host what happens in its project's other sessions: the activity events
 * SessionManager.event records (`noted`), each turn's end (`turnEnded`), and the workshop's own
 * notices (`tell`). They go to the host as one `guide-event` prompt, a moment after the first one
 * while the host is between turns, else at its turn's end. Kept in memory: after a restart the
 * host reads where things stand with `list`.
 */
export class GuideEvents {
  private pending = new Map<string, string[]>();
  private timers = new Map<string, NodeJS.Timeout>();
  private delivering = new Set<string>();
  /** Each followed session's diff at its last turn end, to say when a turn changed it. */
  private diffs = new Map<string, string>();

  constructor(private core: Core, private settleMs = GUIDE_SETTLE_MS) {}

  /** From SessionManager.event: an activity event of <row>. */
  noted(row: SessionRow, kind: string, text: string | null): void {
    if (!text || !FOLLOWED_KINDS.has(kind)) return;
    const host = this.hostFor(row);
    if (host) this.queue(host.id, `${row.name}: ${text}`);
  }

  /** A session's turn ended: a followed one's is an event; the host's own lets what waited go. */
  turnEnded(id: string, interrupted: boolean): void {
    const row = sessionRow(this.core.db, id);
    if (!row) return;
    if (row.purpose === 'guide') {
      void this.deliver(row.id);
      return;
    }
    const host = this.hostFor(row);
    if (!host) return;
    const stat = this.core.sessions.runtime(id).diffStat;
    const key = JSON.stringify(stat);
    const before = this.diffs.get(id);
    this.diffs.set(id, key);
    const changed = before !== undefined && before !== key ? `; the change is now ${diffText(stat)}` : '';
    this.queue(host.id, `${row.name}: ${interrupted ? 'the user interrupted its turn' : "AgentK's turn ended"}${changed}`);
  }

  /** A notice of the workshop's own (its gotchas' upload) for the project's host. */
  tell(projectId: string, text: string): void {
    const host = hostOf(this.core, projectId);
    if (host) this.queue(host.id, text);
  }

  /** The host's open project is a workshop's, and <row> isn't the host or a code review's session. */
  private hostFor(row: SessionRow): SessionRow | null {
    if (row.purpose === 'guide' || row.kind === 'code-review') return null;
    if (!projectRow(this.core.db, row.project_id)?.workshop) return null;
    return hostOf(this.core, row.project_id);
  }

  private queue(hostId: string, line: string): void {
    const list = this.pending.get(hostId) ?? [];
    list.push(line);
    this.pending.set(hostId, list.slice(-GUIDE_EVENTS_MAX));
    this.arm(hostId);
  }

  /** Lines that didn't reach the host go back before any that came since; with `soon`, they're sent again in a moment. */
  private requeue(hostId: string, lines: string[], soon: boolean): void {
    this.pending.set(hostId, [...lines, ...(this.pending.get(hostId) ?? [])].slice(-GUIDE_EVENTS_MAX));
    if (soon) this.arm(hostId);
  }

  private arm(hostId: string): void {
    if (this.timers.has(hostId)) return;
    const t = setTimeout(() => {
      this.timers.delete(hostId);
      void this.deliver(hostId);
    }, this.settleMs);
    t.unref?.();
    this.timers.set(hostId, t);
  }

  /** Sends what waits to the host while it's between turns; otherwise it waits for its turn's end. */
  async deliver(hostId: string): Promise<void> {
    const lines = this.pending.get(hostId);
    if (!lines?.length || this.delivering.has(hostId)) return;
    const row = sessionRow(this.core.db, hostId);
    if (!row || row.desired !== 'running') {
      this.pending.delete(hostId);
      return;
    }
    if ((row.status !== 'idle' && row.status !== 'recovered') || this.core.sessions.runtime(hostId).busy) return;
    this.pending.delete(hostId);
    this.delivering.add(hostId);
    try {
      // Hidden in the host's transcript: the user sees only what the host makes of them. Lost when
      // Claude goes before its turn starts (it exited, or was relaunched): they go again.
      const text = this.core.sessions.prompt(row, 'guide-event', { events: lines.map((l) => `- ${l}`).join('\n') });
      await this.core.sessions.submit(hostId, text, () => this.requeue(hostId, lines, true), true);
    } catch (e) {
      // Kept for the host's next turn end.
      this.requeue(hostId, lines, false);
      this.core.log.warn(`telling ${hostId} what happened in its tour failed`, errMsg(e));
    } finally {
      this.delivering.delete(hostId);
    }
  }

  stop(): void {
    for (const t of this.timers.values()) clearTimeout(t);
    this.timers.clear();
  }
}

function diffText(d: DiffStat | null): string {
  if (!d || !d.files) return 'empty';
  return `${d.files} file${d.files === 1 ? '' : 's'}, +${d.added} −${d.removed}`;
}
