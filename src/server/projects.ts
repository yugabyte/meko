import { existsSync, mkdirSync, writeFileSync, renameSync } from 'node:fs';
import { join, resolve } from 'node:path';
import type {
  ActivityItem, AttachDatapackRequest, CreateProjectRequest, DatapackChoice, Project, ProjectHome, ReposResponse, SearchResult,
  UpdateProjectRequest,
} from '../shared/types.js';
import { REVIEWER_LABELS } from '../shared/types.js';
import { mekoCaptureDir } from './capture.js';
import type { Core } from './core.js';
import { all, get, run, tx } from './db.js';
import { normaliseRemote, projectDatapackDescription } from '../shared/datapacks.js';
import { canonicalRemote, cloneRepo, defaultBranch, isMainRepo, listBranches, readRemotes, repoNameFromUrl, scanRepos } from './git.js';
import { errMsg } from './log.js';
import { pinSlug, projectAgentId } from './meko.js';
import { expandHome } from './paths.js';
import { repoRoots } from './config.js';
import { repoKey } from './worktrees.js';
import { projectUsage } from './usage.js';
import {
  DEFAULT_SETTINGS, dedicatedDatapack, projectDatapacks, projectRow, projectRows, slugify, toProject, type ProjectRow,
} from './store.js';

/** How long the project home waits for each datapack's counts. */
const DESCRIBE_TIMEOUT_MS = 5000;

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

export class ProjectService {
  /** Projects getting a dedicated datapack right now (setDedicatedDatapack). */
  private addingDatapack = new Set<string>();

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
    // Keyed by real path, so a repo is marked with its projects however each of them reached it.
    const used = new Map<string, { id: string; name: string }[]>();
    for (const p of projectRows(this.db)) {
      const key = repoKey(p.repo_path);
      used.set(key, [...(used.get(key) ?? []), { id: p.id, name: p.name }]);
    }
    return {
      roots: cfg.repos.roots,
      repos: scanned
        .map((r) => ({ ...r, projects: used.get(repoKey(r.path)) ?? [] }))
        .sort((a, b) => b.lastTouchedAt - a.lastTouchedAt),
    };
  }

  async create(req: CreateProjectRequest): Promise<Project> {
    const name = (req.name ?? '').trim();
    if (!name) throw new HttpError(400, 'Give the project a name.');
    if (!req.datapack) throw new HttpError(400, 'Choose a dedicated datapack, or no datapack.');
    if (!!req.repoPath === !!req.cloneUrl) throw new HttpError(400, 'Pick a repo or give a URL to clone.');
    if (req.datapack.mode !== 'none') this.needMeko('the project can only be created without a datapack');

    let repoPath: string;
    if (req.cloneUrl) {
      const dest = join(expandHome(this.core.config.repos.cloneInto), repoNameFromUrl(req.cloneUrl));
      if (existsSync(dest)) {
        // A clone of this same URL (one an earlier try made before Meko refused its datapack) is
        // used as it is; anything else there is left alone.
        const origin = isMainRepo(dest) ? (await readRemotes(dest)).origin : null;
        const want = normaliseRemote(req.cloneUrl);
        const same = !!origin && (origin === req.cloneUrl.trim() || (!!want && normaliseRemote(origin) === want));
        if (!same) {
          throw new HttpError(409, `${dest} already exists. Pick it from the list instead.`);
        }
      } else {
        try {
          await cloneRepo(req.cloneUrl, dest);
        } catch (e) {
          throw new HttpError(400, `Clone failed: ${errMsg(e)}`);
        }
      }
      repoPath = dest;
    } else {
      repoPath = resolve(expandHome(req.repoPath!));
    }
    if (!isMainRepo(repoPath)) throw new HttpError(400, `${repoPath} isn't a git repository (or it's a worktree).`);
    const siblings = this.siblings(repoPath);
    this.checkNameFree(siblings, name);

    // Datapack first: if Meko refuses, nothing is written locally.
    const dp = req.datapack.mode === 'none' ? null : await this.resolveDatapack(req.datapack, repoPath, name);

    // The first project on a repo is named after it (its real folder); later ones add their own name.
    const repoSlug = slugify(repoKey(repoPath).split('/').pop() ?? name);
    const id = this.uniqueId(siblings.length ? slugify(`${repoSlug}-${slugify(name)}`, 60) : repoSlug);
    const settings = { ...DEFAULT_SETTINGS };
    const branch = await defaultBranch(repoPath);
    const pos = (get<{ m: number | null }>(this.db, 'SELECT MAX(position) AS m FROM projects WHERE deleted_at IS NULL')?.m ?? -1) + 1;
    tx(this.db, () => {
      run(this.db, `INSERT INTO projects(id, name, repo_path, default_branch, position, settings_json, created_at)
                    VALUES (?, ?, ?, ?, ?, ?, ?)`, id, name, repoPath, branch, pos, JSON.stringify(settings), Date.now());
      if (dp) run(this.db, `INSERT INTO project_datapacks(project_id, datapack_id, name, role) VALUES (?, ?, ?, 'dedicated')`, id, dp.id, dp.name);
    });
    if (dp) {
      this.writePin(id, dp);
      this.core.invites.markUsed(dp.id);
    }
    // Trust the repo in Claude Code now, before any session of it starts.
    await this.core.sessions.trustRepo(repoPath);
    if (dp) this.ensureConversation(id).catch((e) => this.core.log.warn('Meko conversation create failed', errMsg(e)));
    this.core.log.info(`project created: ${id} (${repoPath}) ${dp ? `datapack ${dp.name}` : 'without a datapack'}`);
    this.core.changed();
    return this.get(id);
  }

  /**
   * Meko must be on for what's asked; `otherwise` says what can still be done. Whether it answers
   * shows when it's asked: its error is passed on.
   */
  private needMeko(otherwise: string): void {
    if (!this.core.mekoStatus.configured) throw new HttpError(409, `Meko is off, so ${otherwise}. Add a Meko API key on the Meko tab in Settings.`, 'MEKO_OFF');
  }

  /**
   * The dedicated datapack a choice names: a new one created in Meko (with the repo's remote in
   * its description), or an existing one we can write to (an invite to it accepted first).
   */
  private async resolveDatapack(choice: Exclude<DatapackChoice, { mode: 'none' }>, repoPath: string, projectName: string): Promise<{ id: string; name: string }> {
    if (choice.mode === 'create') {
      const dpName = choice.name.trim();
      if (!dpName) throw new HttpError(400, 'Give the new datapack a name.');
      let dp: { id: string; name: string };
      try {
        dp = await this.core.meko.createDatapack(dpName);
      } catch (e) {
        // At the plan's limit: the UI offers to go on without a datapack.
        if (isDatapackLimit(e)) {
          throw new HttpError(402, `Your Meko plan can't have more datapacks (${errMsg(e)}). Create the project without one, or upgrade your plan in Meko.`, 'DATAPACK_LIMIT');
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
   * the plan's datapack limit). A datapack already attached as shared becomes the dedicated one.
   * New sessions use it; running ones keep what they started with.
   */
  async setDedicatedDatapack(id: string, choice: DatapackChoice): Promise<Project> {
    const r = this.row(id);
    const has = () => {
      if (dedicatedDatapack(this.db, id)) throw new HttpError(409, 'This project already has a dedicated datapack.');
    };
    has();
    if (!choice || choice.mode === 'none') throw new HttpError(400, 'Choose a datapack, or give a name for a new one.');
    // One at a time per project: two tabs adding at once would make two datapacks in Meko.
    if (this.addingDatapack.has(id)) throw new HttpError(409, 'A datapack is already being added to this project.');
    this.addingDatapack.add(id);
    let dp: { id: string; name: string };
    try {
      this.needMeko('a datapack can\'t be added');
      dp = await this.resolveDatapack(choice, r.repo_path, r.name);
      tx(this.db, () => {
        has();
        run(this.db, 'DELETE FROM project_datapacks WHERE project_id = ? AND datapack_id = ?', id, dp.id);
        run(this.db, `INSERT INTO project_datapacks(project_id, datapack_id, name, role) VALUES (?, ?, ?, 'dedicated')`, id, dp.id, dp.name);
        // Its server-side conversation was made without this datapack (for a search): make a new one.
        run(this.db, 'UPDATE projects SET meko_conversation_id = NULL WHERE id = ?', id);
      });
    } finally {
      this.addingDatapack.delete(id);
    }
    this.writePin(id, dp);
    this.core.invites.markUsed(dp.id);
    this.ensureConversation(id).catch((e) => this.core.log.warn('Meko conversation create failed', errMsg(e)));
    this.core.log.info(`project ${id} now has datapack ${dp.name}`);
    this.core.changed();
    return this.get(id);
  }

  /** The projects on a repo, matched by real path (as the worktree pool does), so a symlink or another path to it counts. */
  private siblings(repo: string): ProjectRow[] {
    const key = repoKey(repo);
    return projectRows(this.db).filter((p) => repoKey(p.repo_path) === key);
  }

  /** A repo can back several projects, but not two with the same name. */
  private checkNameFree(siblings: ProjectRow[], name: string): void {
    const clash = siblings.find((p) => p.name.trim().toLowerCase() === name.toLowerCase());
    if (clash) throw new HttpError(409, `That repo already has a project named "${clash.name}". Pick another name.`);
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
      this.checkNameFree(this.siblings(r.repo_path).filter((p) => p.id !== id), n);
      run(this.db, 'UPDATE projects SET name = ? WHERE id = ?', n, id);
    }
    if (req.settings) {
      const next = { ...toProject(this.db, r).settings, ...req.settings };
      if (!next.branchPrefix.trim()) throw new HttpError(400, 'The branch prefix can\'t be empty.');
      if (!Object.hasOwn(REVIEWER_LABELS, next.reviewer)) {
        throw new HttpError(400, 'Reviewer must be All reviewers, AgentK + People or People only.');
      }
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
    run(this.db, 'UPDATE projects SET deleted_at = ? WHERE id = ?', Date.now(), id);
    this.core.log.info(`project deleted: ${id}`);
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

  async home(id: string): Promise<ProjectHome> {
    const project = this.get(id);
    // Counts are extras: with Meko off, down or slow (a short timeout), the page shows without them.
    const on = this.core.mekoStatus.configured;
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
    return { project, datapacks, activity, usage: projectUsage(this.db, id) };
  }

  async search(id: string, query: string): Promise<SearchResult[]> {
    const q = (query ?? '').trim();
    if (!q) return [];
    const project = this.get(id);
    // Nothing to search: no datapacks, or Meko is off.
    if (!project.datapacks.length || !this.core.mekoStatus.configured) return [];
    const conv = await this.ensureConversation(id);
    const agentId = projectAgentId(id);
    const results = await Promise.all(
      project.datapacks.map((d) => this.core.meko.search(q, d, agentId, conv).catch((e) => {
        this.core.log.warn(`search in ${d.name} failed`, errMsg(e));
        return [] as SearchResult[];
      })),
    );
    return results.flat();
  }

  /** Meko conversation for server-side writes on behalf of a project (stable per project). */
  async ensureConversation(id: string): Promise<string> {
    const r = this.row(id);
    if (r.meko_conversation_id) return r.meko_conversation_id;
    const dp = dedicatedDatapack(this.db, id);
    const conv = await this.core.meko.ensureConversation(projectAgentId(id), dp?.id ?? null, `agent-k-local-project-${id}`, `AgentK Local: ${r.name}`);
    run(this.db, 'UPDATE projects SET meko_conversation_id = ? WHERE id = ?', conv, id);
    return conv;
  }

  /**
   * Pins the dedicated datapack for the project's Meko agent id, so the Meko
   * capture hooks (which honour MEKO_AGENT_ID) write into it.
   */
  writePin(projectId: string, dp: { id: string; name: string }): void {
    const dir = mekoCaptureDir();
    mkdirSync(dir, { recursive: true });
    const file = join(dir, `pin-${pinSlug(projectAgentId(projectId))}.json`);
    const tmp = `${file}.tmp`;
    writeFileSync(tmp, JSON.stringify({ datapack_id: dp.id, datapack_name: dp.name, selected_at: new Date().toISOString() }, null, 2));
    renameSync(tmp, file);
  }
}
