import { readOverrides } from '../shared/codereview.js';
import { existsSync, mkdirSync, readdirSync, statSync, writeFileSync, renameSync, rmSync } from 'node:fs';
import { basename, dirname, isAbsolute, join, relative, resolve, sep } from 'node:path';
import type {
  ActivityItem, AttachDatapackRequest, ClosedProject, CreateProjectRequest, DatapackChoice, DatapackRef, LearnScope, OpenedProject, Project, ProjectHome, RepoBranches, ReposResponse, SearchResult, SetDedicatedDatapackRequest, UpdateProjectRequest,
} from '../shared/types.js';
import { LEARN_MONTHS_MAX, LEARN_TEXT_MAX, newRepoNameProblem, REVIEWER_LABELS } from '../shared/types.js';
import { mekoCaptureDir } from './capture.js';
import type { Core } from './core.js';
import { all, get, run, tx, type Db } from './db.js';
import { normaliseRemote, projectDatapackDescription } from '../shared/datapacks.js';
import {
  canonicalRemote, checkBranchName, cloneRepo, defaultBranch, ensureFirstCommit, initFolder, initRepo, isFreshRepo, isMainRepo, listBranches, readRemotes, repoNameFromUrl, scanRepos,
} from './git.js';
import { errMsg } from './log.js';
import { AGENTK_AGENT_ID, pinSlug } from './meko.js';
import { learnScope } from './learnscope.js';
import { expandHome, repoKey } from './paths.js';
import { repoRoots } from './config.js';
import { projectUsage } from './usage.js';
import {
  DEFAULT_SETTINGS, dedicatedDatapack, projectDatapacks, projectRow, projectRows, slugify, toProject, type ProjectRow,
} from './store.js';

/** How long the project home waits for each datapack's counts. */
const DESCRIBE_TIMEOUT_MS = 5000;
/** How long an archived session's Meko pin stays: the plugin's SessionEnd hook may still read it. */
export const PIN_KEEP_MS = 24 * 60 * 60 * 1000;

export class HttpError extends Error {
  /** `code`: a machine-readable reason the UI can act on (sent as `code` next to `error`). */
  constructor(public status: number, message: string, public code?: string) {
    super(message);
  }
}

/**
 * Meko refused to create a datapack because the account is at its plan's datapack limit: its code
 * (FREE_TIER_DATAPACK_LIMIT, STANDARD_TIER_DATAPACK_LIMIT), its reason, or its upgrade message
 * ("Upgrade to Standard to create a new datapack."), not just any refusal that mentions datapacks.
 */
function isDatapackLimit(e: unknown): boolean {
  return /TIER_DATAPACK_LIMIT|tier datapack limit reached|Upgrade to \w+ to create a new datapack/i.test(errMsg(e));
}

/** A repo's folder that's already there (or being made by another open); <fix> says what to do instead. */
/** <dir> is a folder with nothing in it. */
function isEmptyFolder(dir: string): boolean {
  try {
    return statSync(dir).isDirectory() && readdirSync(dir).length === 0;
  } catch {
    return false;
  }
}

function folderTaken(dest: string, fix = 'Pick it from the list, or choose another name.'): HttpError {
  return new HttpError(409, `${dest} already exists. ${fix}`);
}

/**
 * How a new repo's folder is held while it's being made: case folded, since on macOS "Fresh" and
 * "fresh" are one folder (on Linux this only also holds the other spelling for that moment).
 */
function newRepoClaim(dest: string): string {
  return dest.toLowerCase();
}

/** A project name on a repo, as names are compared there (trimmed, any case). */
function nameKey(repo: string, name: string): string {
  return `${repo}\0${name.trim().toLowerCase()}`;
}

export class ProjectService {
  /** Projects getting a dedicated datapack right now (setDedicatedDatapack). */
  private addingDatapack = new Set<string>();
  /** Names of projects being created right now, per repo (nameKey): taken until their row is written. */
  private creatingNames = new Set<string>();
  /** New repos' folders whose project is being opened right now: taken until its row is written. */
  private creatingRepos = new Set<string>();

  /** Project Meko conversations being created, so concurrent callers share one. */
  private creatingConversation = new Map<string, Promise<string>>();

  constructor(private core: Core) {}

  private get db() {
    return this.core.db;
  }

  get(id: string): Project {
    const r = projectRow(this.db, id);
    if (!r) throw new HttpError(404, 'That project no longer exists.');
    return toProject(this.db, r);
  }

  row(id: string): ProjectRow {
    const r = projectRow(this.db, id);
    if (!r) throw new HttpError(404, 'That project no longer exists.');
    return r;
  }

  async repos(): Promise<ReposResponse> {
    const cfg = this.core.config;
    const roots = repoRoots(cfg);
    const scanned = await scanRepos(roots, cfg.repos.depth);
    // Projects store their repo's real path, so a scanned repo is matched by its own.
    const used = new Map<string, { id: string; name: string }[]>();
    for (const p of projectRows(this.db)) {
      used.set(p.repo_path, [...(used.get(p.repo_path) ?? []), { id: p.id, name: p.name }]);
    }
    return {
      roots: cfg.repos.roots,
      cloneInto: cfg.repos.cloneInto,
      repos: scanned
        .map((r) => ({ ...r, projects: used.get(repoKey(r.path)) ?? [] }))
        .sort((a, b) => b.lastTouchedAt - a.lastTouchedAt),
    };
  }

  /**
   * `POST /api/projects`: create(), and whether the project's dedicated datapack is empty, for the
   * New session dialog to offer a learn session first: true for a datapack the open made, and for
   * one Meko says has no memories and no knowledge. Meko failing or slow here costs only the offer.
   */
  async open(req: CreateProjectRequest): Promise<OpenedProject> {
    const project = await this.create(req);
    const dp = dedicatedDatapack(this.db, project.id);
    let datapackEmpty = false;
    if (dp && req.datapack.mode === 'create') {
      datapackEmpty = true;
    } else if (dp) {
      const counts = await this.core.meko.describeDatapack(dp.id, DESCRIBE_TIMEOUT_MS).catch(() => null);
      datapackEmpty = counts?.memoryCount === 0 && counts.knowledgeCount === 0;
    }
    return { ...project, datapackEmpty };
  }

  async create(req: CreateProjectRequest): Promise<Project> {
    const name = (req.name ?? '').trim();
    if (!name) throw new HttpError(400, 'Give the project a name.');
    if (!req.datapack) throw new HttpError(400, 'Choose a dedicated datapack, or no datapack.');
    if ([req.repoPath, req.cloneUrl, req.newRepo].filter(Boolean).length !== 1) {
      throw new HttpError(400, 'Pick a repo, give a URL to clone, or name a new repo.');
    }
    if (req.datapack.mode !== 'none') this.needMeko('the project can only be opened without a datapack');

    // A new repo's folder is held from here until its project is written, so a second open of the
    // same name meanwhile is refused instead of taking the repo this one just made.
    const newDest = req.newRepo ? this.newRepoPath(req.newRepo) : null;
    if (!newDest) return this.openOn(req, name, null);
    const claim = newRepoClaim(newDest);
    if (this.creatingRepos.has(claim)) throw folderTaken(newDest);
    this.creatingRepos.add(claim);
    try {
      return await this.openOn(req, name, newDest);
    } finally {
      this.creatingRepos.delete(claim);
    }
  }

  /** The rest of create(): the repo (made at <newDest> for a new one), the datapack and the row. */
  private async openOn(req: CreateProjectRequest, name: string, newDest: string | null): Promise<Project> {
    let repoPath: string;
    if (req.cloneUrl) {
      repoPath = await this.clone(req.cloneUrl);
    } else if (newDest) {
      repoPath = await this.newRepo(newDest);
    } else {
      repoPath = resolve(expandHome(req.repoPath!));
    }
    // An empty folder typed under Other folder is made a repo below, once the name is checked.
    const makeRepo = !!req.repoPath && isEmptyFolder(repoPath);
    if (!makeRepo && !isMainRepo(repoPath)) {
      throw new HttpError(400, `${repoPath} isn't a git repository (or it's a worktree). Run git init there and commit its files, or pick an empty folder and AgentK makes it a repo.`);
    }
    // Stored as its real path, so every path to the repo (a symlink, say) names the same one.
    repoPath = repoKey(repoPath);
    // The name is taken from here until the row is written, so a second create of it (or a rename
    // to it) while Meko answers is refused.
    this.checkNameFree(repoPath, name);
    const claim = nameKey(repoPath, name);
    this.creatingNames.add(claim);
    let id: string;
    let dp: { id: string; name: string } | null;
    let reopened = false;
    try {
      // The repo gets what sessions need before Meko is asked, so a datapack is never made for an
      // open that then fails here: the empty folder becomes a repo, and a repo with no commits
      // (just `git init`) gets an empty first commit, so sessions have a base to branch from.
      if (makeRepo) await this.makeFolderRepo(repoPath);
      try {
        await ensureFirstCommit(repoPath);
      } catch (e) {
        throw new HttpError(400, errMsg(e));
      }
      // Datapack before the row: if Meko refuses, no project is written. A repo or first commit
      // made above stays, and a retry opens the repo as it is.
      dp = req.datapack.mode === 'none' ? null : await this.resolveDatapack(req.datapack, repoPath, name);
      const branch = await defaultBranch(repoPath);
      const settings = { ...DEFAULT_SETTINGS };
      id = tx(this.db, () => {
        const pos = (get<{ m: number | null }>(this.db, 'SELECT MAX(position) AS m FROM projects WHERE deleted_at IS NULL')?.m ?? -1) + 1;
        // A closed project with this dedicated datapack opens again as itself: its id, settings,
        // sessions, usage and attached datapacks carry on, under the name and repo given now.
        const closed = dp && closedProjectWith(this.db, dp.id);
        if (dp && closed) {
          run(this.db, 'UPDATE projects SET name = ?, repo_path = ?, default_branch = ?, position = ?, deleted_at = NULL WHERE id = ?',
            name, repoPath, branch, pos, closed);
          run(this.db, 'UPDATE project_datapacks SET name = ? WHERE project_id = ? AND datapack_id = ?', dp.name, closed, dp.id);
          run(this.db, `INSERT INTO project_events(project_id, kind, at) VALUES (?, 'reopened', ?)`, closed, Date.now());
          reopened = true;
          return closed;
        }
        // The id and position are worked out in the write, so projects created meanwhile count. The
        // first project on a repo is named after it; later ones add their own name.
        const repoSlug = slugify(basename(repoPath) || name);
        const pid = this.uniqueId(this.siblings(repoPath).length ? slugify(`${repoSlug}-${slugify(name)}`, 60) : repoSlug);
        run(this.db, `INSERT INTO projects(id, name, repo_path, default_branch, position, settings_json, created_at)
                      VALUES (?, ?, ?, ?, ?, ?, ?)`, pid, name, repoPath, branch, pos, JSON.stringify(settings), Date.now());
        if (dp) run(this.db, `INSERT INTO project_datapacks(project_id, datapack_id, name, role) VALUES (?, ?, ?, 'dedicated')`, pid, dp.id, dp.name);
        return pid;
      });
    } finally {
      this.creatingNames.delete(claim);
    }
    if (dp) this.core.invites.markUsed(dp.id);
    // Trust the repo now (Claude Code's folder trust), before any session of it starts.
    for (const h of this.core.harnesses.list()) await h.trustFolder?.(repoPath, this.core.log);
    if (dp) this.ensureConversation(id).catch((e) => this.core.log.warn('Meko conversation create failed', errMsg(e)));
    this.core.log.info(`project ${reopened ? 'opened again' : 'opened'}: ${id} (${repoPath}) ${dp ? `datapack ${dp.name}` : 'without a datapack'}`);
    this.core.changed();
    return this.get(id);
  }

  /**
   * Clones <url> into the repos folder (`[repos] clone_into`) and returns where. A clone of this
   * same URL already there (one an earlier try made, say) is used as it is; anything else there is
   * left alone (409).
   */
  async clone(url: string): Promise<string> {
    const dest = this.cloneDest(url);
    if (existsSync(dest)) {
      const origin = isMainRepo(dest) ? (await readRemotes(dest)).origin : null;
      const want = normaliseRemote(url);
      const same = !!origin && (origin === url.trim() || (!!want && normaliseRemote(origin) === want));
      if (!same) throw folderTaken(dest, 'Pick it from the list instead.');
      return dest;
    }
    try {
      await cloneRepo(url, dest);
    } catch (e) {
      throw new HttpError(400, `Clone failed: ${errMsg(e)}`);
    }
    return dest;
  }

  /** Where clone() puts <url>: the repos folder (`[repos] clone_into`) and the repo's name. */
  cloneDest(url: string): string {
    return join(expandHome(this.core.config.repos.cloneInto), repoNameFromUrl(url));
  }

  /** Whether a repo at <path> (there or not yet) would be a scanned repo, so another project's sessions could work in it. */
  scannable(path: string): boolean {
    return this.underRoots(join(repoKey(dirname(path)), basename(path)));
  }

  /** Where the new repo <name> goes: the repos folder (`[repos] clone_into`), once the name is checked. */
  private newRepoPath(name: unknown): string {
    const problem = newRepoNameProblem(typeof name === 'string' ? name : '');
    if (problem) throw new HttpError(400, problem);
    return join(expandHome(this.core.config.repos.cloneInto), (name as string).trim());
  }

  /**
   * Makes the new repo at <dest> (from newRepoPath) and returns it: git init and an empty first
   * commit. A repo there just as an earlier try left it (spelled the same, no remote, no files, and
   * not in use) is used as it is; anything else there is left alone (409).
   */
  private async newRepo(dest: string): Promise<string> {
    if (existsSync(dest)) {
      // On a case-insensitive disk, "Fresh" finds "fresh": that's another repo, not this one.
      const sameSpelling = readdirSync(dirname(dest)).includes(basename(dest));
      if (sameSpelling && await isFreshRepo(dest) && !this.repoInUse(repoKey(dest))) return dest;
      throw folderTaken(dest);
    }
    try {
      await initRepo(dest);
    } catch (e) {
      // Made meanwhile, by something other than AgentK.
      if ((e as NodeJS.ErrnoException).code === 'EEXIST') throw folderTaken(dest);
      throw new HttpError(400, `Couldn't create the repo: ${errMsg(e)}`);
    }
    return dest;
  }

  /** Makes <dir>, an empty folder typed under Other folder, a repo as a new repo is made (git init and an empty first commit). */
  private async makeFolderRepo(dir: string): Promise<void> {
    try {
      await initFolder(dir);
    } catch (e) {
      throw new HttpError(400, `Couldn't make ${dir} a repo: ${errMsg(e)}`);
    }
  }

  /**
   * Meko must be on for what's asked; `otherwise` says what can still be done. Whether it answers
   * shows when it's asked: its error is passed on.
   */
  private needMeko(otherwise: string): void {
    if (!this.core.mekoStatus.configured) throw new HttpError(409, `Meko isn't set up, so ${otherwise}. Start Meko on this computer (meko start), or link Meko Cloud in Meko's web UI (Settings › Meko Cloud).`, 'MEKO_OFF');
  }

  /**
   * The dedicated datapack a choice names: a new one created in Meko (with the repo's remote in
   * its description), or an existing one we can write to (an invite to it accepted first).
   */
  private async resolveDatapack(
    choice: Exclude<DatapackChoice, { mode: 'none' }>, repoPath: string, projectName: string, forExisting = false,
  ): Promise<{ id: string; name: string }> {
    if (choice.mode === 'create') {
      const dpName = choice.name.trim();
      if (!dpName) throw new HttpError(400, 'Give the new datapack a name.');
      let dp: { id: string; name: string };
      try {
        dp = await this.core.meko.createDatapack(dpName);
      } catch (e) {
        // At the plan's limit: Open project offers to go on without a datapack, or to pick an
        // existing one; an existing project can use one of the datapacks it already has access to.
        if (isDatapackLimit(e)) {
          const fix = forExisting
            ? 'Pick one of your datapacks under Use an existing one'
            : 'Pick one of your projects or datapacks, open the project without one';
          throw new HttpError(402, `Your Meko plan can't have more datapacks (${errMsg(e)}). ${fix}, or upgrade your plan in Meko.`, 'DATAPACK_LIMIT');
        }
        throw new HttpError(502, `Meko couldn't create the datapack "${dpName}": ${errMsg(e)}`);
      }
      // The repo's remote in the description lets whoever it's shared with find the repo: the
      // upstream repo of a fork, not the fork. Best effort, so it doesn't hold up the project: a
      // failure is only logged.
      const remote = canonicalRemote(await readRemotes(repoPath));
      void this.core.meko.setDatapackDescription(dp.id, projectDatapackDescription(projectName, remote)).catch((e) =>
        this.core.log.warn(`couldn't set the description of datapack ${dp.name}`, errMsg(e)));
      return dp;
    }
    if (!choice.id || !choice.name) throw new HttpError(400, 'Choose a datapack.');
    const dp = { id: choice.id, name: choice.name };
    // Sessions write to the dedicated datapack, so a read-only one can't be it. The shares table
    // knows the grant of a share; otherwise Meko is asked (a failure there doesn't block).
    const grant = this.core.invites.grantOf(dp.id)
      ?? (await this.core.meko.describeDatapack(dp.id, DESCRIBE_TIMEOUT_MS).then((d) => d.grant, () => null));
    if (grant === 'viewer') {
      throw new HttpError(400, `You can only read ${dp.name} (viewer), so sessions couldn't write to it. Pick another datapack for the project, and attach this one from the project's home.`);
    }
    // An invite waiting in Meko is accepted first: if Meko refuses, nothing is written here.
    if (choice.inviteId) await this.core.invites.acceptForProject(dp.id, choice.inviteId);
    return dp;
  }

  /**
   * Gives a project without one its dedicated datapack (for a project created without Meko, or at
   * the plan's datapack limit), or with `replace` switches it to another one: the old one stays
   * attached as shared, or is detached with `detachOld`. A datapack already attached as shared
   * becomes the dedicated one. New and resumed sessions use it; running ones keep the one they
   * were launched with (sessionDatapack), for their own Meko calls, capture and AgentK's writes.
   */
  async setDedicatedDatapack(id: string, req: SetDedicatedDatapackRequest): Promise<Project> {
    const r = this.row(id);
    if (!req || req.mode === 'none') throw new HttpError(400, 'Choose a datapack, or give a name for a new one.');
    const replace = req.replace;
    if (replace !== undefined && (typeof replace !== 'string' || !replace)) throw new HttpError(400, 'Name the dedicated datapack to replace.');
    if (req.detachOld !== undefined && typeof req.detachOld !== 'boolean') throw new HttpError(400, 'detachOld must be true or false.');
    if (req.detachOld && !replace) throw new HttpError(400, 'Only a datapack being replaced can be detached.');
    if (replace && req.mode === 'existing' && req.id === replace) throw new HttpError(400, 'That\'s already the dedicated datapack.');
    // Checked again inside the transaction: another tab may have added or switched one meanwhile.
    const check = () => {
      const current = dedicatedDatapack(this.db, id);
      if (!replace && current) throw new HttpError(409, 'This project already has a dedicated datapack.');
      if (replace && current?.id !== replace) {
        throw new HttpError(409, 'The dedicated datapack changed in another tab. Reload and try again.');
      }
      return current;
    };
    check();
    // One at a time per project: two tabs adding at once would make two datapacks in Meko.
    if (this.addingDatapack.has(id)) throw new HttpError(409, 'A datapack is already being added to this project.');
    this.addingDatapack.add(id);
    let dp: { id: string; name: string };
    let old: DatapackRef | null;
    try {
      this.needMeko('a datapack can\'t be added');
      dp = await this.resolveDatapack(req, r.repo_path, r.name, true);
      old = tx(this.db, () => {
        const was = check();
        // The old one goes first: a project can't have two dedicated datapacks (one_dedicated).
        if (was) {
          if (req.detachOld) run(this.db, 'DELETE FROM project_datapacks WHERE project_id = ? AND datapack_id = ?', id, was.id);
          else run(this.db, `UPDATE project_datapacks SET role = 'shared' WHERE project_id = ? AND datapack_id = ?`, id, was.id);
        }
        run(this.db, 'DELETE FROM project_datapacks WHERE project_id = ? AND datapack_id = ?', id, dp.id);
        run(this.db, `INSERT INTO project_datapacks(project_id, datapack_id, name, role) VALUES (?, ?, ?, 'dedicated')`, id, dp.id, dp.name);
        // Its server-side conversation was made in the old datapack, or without one (for a search): make a new one.
        run(this.db, 'UPDATE projects SET meko_conversation_id = NULL WHERE id = ?', id);
        return was;
      });
    } finally {
      this.addingDatapack.delete(id);
    }
    this.core.invites.markUsed(dp.id);
    this.ensureConversation(id).catch((e) => this.core.log.warn('Meko conversation create failed', errMsg(e)));
    this.core.log.info(old
      ? `project ${id} switched datapack ${old.name} → ${dp.name}${req.detachOld ? ` (${old.name} detached)` : ''}`
      : `project ${id} now has datapack ${dp.name}`);
    this.core.changed();
    return this.get(id);
  }

  /** The projects on a repo, by its stored (real) path. */
  /** A project is on <repo> (a real path), or a running session of an open project works in it. */
  private repoInUse(repo: string): boolean {
    if (this.siblings(repo).length) return true;
    return !!get(this.db, `SELECT 1 FROM sessions s JOIN projects p ON p.id = s.project_id
                           WHERE p.deleted_at IS NULL AND s.desired = 'running' AND s.repo_path = ? LIMIT 1`, repo);
  }

  private siblings(repo: string): ProjectRow[] {
    return projectRows(this.db).filter((p) => p.repo_path === repo);
  }

  /** A repo can back several projects, but not two with the same name (counting ones being created). */
  private checkNameFree(repo: string, name: string, exceptId?: string): void {
    const clash = this.siblings(repo).find((p) => p.id !== exceptId && nameKey(repo, p.name) === nameKey(repo, name));
    if (clash) throw new HttpError(409, `That repo already has a project named "${clash.name}". Pick another name.`);
    if (this.creatingNames.has(nameKey(repo, name))) {
      throw new HttpError(409, `A project named "${name.trim()}" is being opened on that repo. Pick another name.`);
    }
  }

  private uniqueId(base: string): string {
    let id = base;
    for (let i = 2; get(this.db, 'SELECT 1 AS x FROM projects WHERE id = ?', id); i++) id = `${base}-${i}`;
    return id;
  }

  update(id: string, req: UpdateProjectRequest): Project {
    const r = this.row(id);
    if (req.name !== undefined) {
      const n = req.name.trim();
      if (!n) throw new HttpError(400, 'The name can\'t be empty.');
      this.checkNameFree(r.repo_path, n, id);
      run(this.db, 'UPDATE projects SET name = ? WHERE id = ?', n, id);
    }
    if (req.settings) {
      const next = { ...toProject(this.db, r).settings, ...req.settings };
      if (!next.branchPrefix.trim()) throw new HttpError(400, 'The branch prefix can\'t be empty.');
      if (!Object.hasOwn(REVIEWER_LABELS, next.reviewer)) {
        throw new HttpError(400, 'Reviewer must be Yours only, People or All reviewers.');
      }
      // What isn't a valid setting goes back to the global value (readOverrides drops it).
      if (req.settings.codeReview !== undefined) next.codeReview = readOverrides(req.settings.codeReview);
      run(this.db, 'UPDATE projects SET settings_json = ? WHERE id = ?', JSON.stringify(next), id);
    }
    if (req.position !== undefined) this.move(id, req.position);
    this.core.changed();
    return this.get(id);
  }

  private move(id: string, position: number): void {
    const ids = projectRows(this.db).map((p) => p.id).filter((x) => x !== id);
    ids.splice(Math.max(0, Math.min(position, ids.length)), 0, id);
    tx(this.db, () => ids.forEach((pid, i) => run(this.db, 'UPDATE projects SET position = ? WHERE id = ?', i, pid)));
  }

  async delete(id: string): Promise<void> {
    this.row(id);
    await this.core.sessions.teardownProject(id);
    const now = Date.now();
    tx(this.db, () => {
      run(this.db, 'UPDATE projects SET deleted_at = ? WHERE id = ?', now, id);
      run(this.db, `INSERT INTO project_events(project_id, kind, at) VALUES (?, 'closed', ?)`, id, now);
    });
    this.core.log.info(`project closed: ${id}`);
    this.core.changed();
  }

  attachDatapack(id: string, req: AttachDatapackRequest): Project {
    this.row(id);
    if (!req.id || !req.name) throw new HttpError(400, 'Choose a datapack to attach.');
    if (projectDatapacks(this.db, id).some((d) => d.id === req.id)) throw new HttpError(409, 'That datapack is already attached.');
    run(this.db, `INSERT INTO project_datapacks(project_id, datapack_id, name, role) VALUES (?, ?, ?, 'shared')`, id, req.id, req.name);
    this.core.invites.markUsed(req.id);
    this.core.changed();
    return this.get(id);
  }

  detachDatapack(id: string, dpId: string): Project {
    const dp = projectDatapacks(this.db, id).find((d) => d.id === dpId);
    if (!dp) throw new HttpError(404, 'That datapack isn\'t attached.');
    if (dp.role === 'dedicated') throw new HttpError(400, 'The dedicated datapack can\'t be detached.');
    run(this.db, 'DELETE FROM project_datapacks WHERE project_id = ? AND datapack_id = ?', id, dpId);
    this.core.changed();
    return this.get(id);
  }

  async branches(id: string): Promise<string[]> {
    return listBranches(this.row(id).repo_path);
  }

  /**
   * How much a learn session's brief would cover (`LearnScope`), for the New session dialog's
   * estimate: in <repoPath> (the project's repo when left out), on <base> (its default branch when
   * left out), for <where> and <months> as the brief gives them. A 400 for anything off.
   */
  async learnScope(id: string, q: { repoPath?: unknown; base?: unknown; where?: unknown; months?: unknown }): Promise<LearnScope> {
    const p = this.row(id);
    const repo = this.sessionRepo(p, q.repoPath);
    if (q.where != null && typeof q.where !== 'string') throw new HttpError(400, 'Where its files are must be text.');
    const where = (q.where ?? '').trim();
    if (where.length > LEARN_TEXT_MAX) throw new HttpError(400, `Keep where its files are to ${LEARN_TEXT_MAX.toLocaleString('en-US')} characters.`);
    const months = Number(q.months);
    if (typeof q.months !== 'string' || !/^\d+$/.test(q.months) || months > LEARN_MONTHS_MAX) {
      throw new HttpError(400, `Pick how many months of PRs to read: a whole number from 0 to ${LEARN_MONTHS_MAX}.`);
    }
    if (q.base != null && typeof q.base !== 'string') throw new HttpError(400, 'The base branch must be text.');
    const base = (typeof q.base === 'string' ? q.base.trim() : '') || (repo === p.repo_path ? p.default_branch : await defaultBranch(repo));
    try {
      await checkBranchName(repo, base);
    } catch (e) {
      throw new HttpError(400, errMsg(e));
    }
    const scope = await learnScope(repo, base, where, months, Date.now());
    if (!scope) throw new HttpError(400, `Branch ${base} isn't in ${basename(repo)} or on its origin.`);
    return scope;
  }

  /**
   * The repo (its key) a new session of <project> works in: the project's, or <path> when it is one
   * of the scanned repos. Throws a 400 otherwise.
   */
  sessionRepo(project: ProjectRow, path: unknown): string {
    if (path == null || path === '') return project.repo_path;
    if (typeof path !== 'string') throw new HttpError(400, 'The session\'s repo must be a path.');
    const key = repoKey(resolve(expandHome(path.trim())));
    if (key === project.repo_path) return key;
    return this.scannedRepo(key);
  }

  /**
   * <key> when it's the real path of a repo the Open project dialog lists (a main repo at most
   * `depth` folders under a root, through no hidden folder or other repo), else a 400. Checks the
   * path's own folders instead of scanning the roots.
   */
  private scannedRepo(key: string): string {
    const cfg = this.core.config;
    const listed = isMainRepo(key) && this.underRoots(key);
    if (!listed) throw new HttpError(400, `${key} isn't one of the repos under ${cfg.repos.roots.join(', ') || 'your repos folders'}.`);
    return key;
  }

  /** <key> is at most `depth` folders under a repos root, through no hidden folder or other repo (what the scan lists). */
  private underRoots(key: string): boolean {
    const cfg = this.core.config;
    return repoRoots(cfg).some((root) => {
      const rel = relative(repoKey(root), key);
      if (!rel || rel.startsWith('..') || isAbsolute(rel)) return false;
      const parts = rel.split(sep);
      if (parts.length > cfg.repos.depth || parts.some((d) => d.startsWith('.'))) return false;
      // The scan stops at a repo, so one nested inside another isn't listed.
      return parts.slice(0, -1).every((_, i) => !isMainRepo(join(repoKey(root), ...parts.slice(0, i + 1))));
    });
  }

  /** A scanned repo's branches and default branch, for the New session dialog. */
  async repoBranches(path: unknown): Promise<RepoBranches> {
    if (typeof path !== 'string' || !path.trim()) throw new HttpError(400, 'Give the repo\'s path.');
    const key = repoKey(resolve(expandHome(path.trim())));
    // A project's repo may sit outside the scanned folders.
    const repo = projectRows(this.db).some((p) => p.repo_path === key) ? key : this.scannedRepo(key);
    const [branches, dflt] = await Promise.all([listBranches(repo), defaultBranch(repo)]);
    return { defaultBranch: dflt, branches: branches.includes(dflt) ? branches : [dflt, ...branches] };
  }

  async home(id: string): Promise<ProjectHome> {
    const project = this.get(id);
    // Counts are extras: with Meko off, down or slow (a short timeout), the page shows without them.
    const on = this.core.mekoStatus.configured;
    // Meko's Context Gaps for the dedicated datapack, read beside the counts: none when Meko doesn't answer.
    const dedicated = dedicatedDatapack(this.db, id);
    const mekoGaps = dedicated && on ? this.core.mekoQuestions.openCounts(dedicated.id) : null;
    const datapacks = await Promise.all(
      project.datapacks.map(async (d) => {
        try {
          if (!on) throw new Error('Meko is off');
          return { ...d, ...(await this.core.meko.describeDatapack(d.id, DESCRIBE_TIMEOUT_MS)) };
        } catch {
          return { ...d, memoryCount: null, knowledgeCount: null, grant: null, ownerName: null, ownerEmail: null };
        }
      }),
    );
    const activity = all<{ at: number; session_id: string; name: string | null; kind: string; text: string | null }>(
      this.db,
      `SELECT e.at, e.session_id, s.name, e.kind, e.text FROM session_events e
       LEFT JOIN sessions s ON s.id = e.session_id
       WHERE e.project_id = ? AND e.text IS NOT NULL ORDER BY e.at DESC LIMIT 50`,
      id,
    ).map<ActivityItem>((r) => ({ at: r.at, sessionId: r.session_id, sessionName: r.name, kind: r.kind, text: r.text ?? '' }));
    // A learn session still open on the dedicated datapack: one launched on another (before a switch)
    // doesn't count, and once it's closed, a datapack it left empty is offered one again.
    const learnSessionOpen = !!dedicated && !!get(this.db,
      `SELECT 1 AS x FROM sessions WHERE project_id = ? AND purpose = 'learn' AND status != 'archived'
         AND (meko_datapack_id IS NULL OR meko_datapack_id = ?) LIMIT 1`, id, dedicated.id);
    return { project, datapacks, learnSessionOpen, activity, usage: projectUsage(this.db, id), mekoGaps: await mekoGaps };
  }

  async search(id: string, query: string): Promise<SearchResult[]> {
    const q = (query ?? '').trim();
    if (!q) return [];
    this.get(id);
    return (await this.searchDatapacks(id, q)).results;
  }

  /**
   * Searches each of the project's datapacks (the project home's search, and the search at session
   * start). A datapack whose search fails is logged and skipped; `failed` says how many did. Nothing
   * is searched with no datapacks or Meko off.
   */
  async searchDatapacks(id: string, query: string, timeoutMs?: number): Promise<{ results: SearchResult[]; searched: number; failed: number; error: unknown }> {
    const datapacks = this.core.mekoStatus.configured ? projectDatapacks(this.db, id) : [];
    if (!datapacks.length) return { results: [], searched: 0, failed: 0, error: null };
    const conv = await this.ensureConversation(id, timeoutMs);
    const settled = await Promise.allSettled(datapacks.map((d) => this.core.meko.search(query, d, AGENTK_AGENT_ID, conv, timeoutMs)));
    const out = { results: [] as SearchResult[], searched: datapacks.length, failed: 0, error: null as unknown };
    settled.forEach((s, i) => {
      if (s.status === 'fulfilled') {
        out.results.push(...s.value);
        return;
      }
      this.core.log.warn(`search in ${datapacks[i].name} failed`, errMsg(s.reason));
      out.failed++;
      out.error ??= s.reason;
    });
    return out;
  }

  /**
   * The project's own Meko conversation (stable per project), for its searches; writes for a
   * session go under the session's. Calls made while it's being created share the one creation.
   */
  async ensureConversation(id: string, timeoutMs?: number): Promise<string> {
    const r = this.row(id);
    if (r.meko_conversation_id) return r.meko_conversation_id;
    let p = this.creatingConversation.get(id);
    if (!p) {
      const dp = dedicatedDatapack(this.db, id);
      p = this.core.meko.ensureConversation(AGENTK_AGENT_ID, dp?.id ?? null, projectConversationKey(id), `AgentK: ${r.name}`, timeoutMs)
        .then((conv) => {
          run(this.db, 'UPDATE projects SET meko_conversation_id = ? WHERE id = ?', conv, id);
          return conv;
        })
        .finally(() => this.creatingConversation.delete(id));
      this.creatingConversation.set(id, p);
    }
    return p;
  }

  /**
   * Removes the pins of sessions archived more than PIN_KEEP_MS ago (a restore pins again). Only
   * per-session pins: the project-wide ones sessions used before (`claude_code:<project id>`) are
   * left alone, since that's also the agent id the Meko plugin gives plain Claude Code in a repo
   * of that name, so the pin may be the user's own.
   */
  prunePins(now = Date.now()): void {
    const dir = mekoCaptureDir();
    const gone = all<{ a: string }>(this.db, `SELECT meko_agent_id AS a FROM sessions
      WHERE meko_agent_id IS NOT NULL AND desired = 'archived' AND archived_at < ?`, now - PIN_KEEP_MS);
    for (const { a } of gone) rmSync(join(dir, `pin-${pinSlug(a)}.json`), { force: true });
  }

  /** Closed projects by their dedicated datapack: the one create would open again for each. */
  closed(): ClosedProject[] {
    const rows = all<{ datapack_id: string; id: string; name: string; repo_path: string }>(this.db,
      `SELECT d.datapack_id, p.id, p.name, p.repo_path FROM projects p JOIN project_datapacks d ON d.project_id = p.id
        WHERE d.role = 'dedicated' AND p.deleted_at IS NOT NULL ORDER BY p.deleted_at DESC`);
    const seen = new Set<string>();
    return rows.filter((r) => !seen.has(r.datapack_id) && !!seen.add(r.datapack_id))
      .map((r) => ({ datapackId: r.datapack_id, id: r.id, name: r.name, repoPath: r.repo_path }));
  }

  /**
   * Pins a datapack for a Meko agent id (a session's), so the Meko capture hooks (which honour
   * MEKO_AGENT_ID) write into it.
   */
  writePin(agentId: string, dp: { id: string; name: string }): void {
    const dir = mekoCaptureDir();
    mkdirSync(dir, { recursive: true });
    const file = join(dir, `pin-${pinSlug(agentId)}.json`);
    const tmp = `${file}.tmp`;
    writeFileSync(tmp, JSON.stringify({ datapack_id: dp.id, datapack_name: dp.name, selected_at: new Date().toISOString() }, null, 2));
    renameSync(tmp, file);
  }
}

/** The `session_id` a project's own Meko conversation is created under (Projects.ensureConversation). */
export function projectConversationKey(projectId: string): string {
  return `agent-k-local-project-${projectId}`;
}

/** The most recently closed project whose dedicated datapack this is, if any (what create opens again). */
function closedProjectWith(db: Db, datapackId: string): string | null {
  return get<{ id: string }>(db, `SELECT p.id FROM projects p JOIN project_datapacks d ON d.project_id = p.id
    WHERE d.datapack_id = ? AND d.role = 'dedicated' AND p.deleted_at IS NOT NULL
    ORDER BY p.deleted_at DESC LIMIT 1`, datapackId)?.id ?? null;
}
