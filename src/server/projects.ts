import { existsSync, mkdirSync, writeFileSync, renameSync } from 'node:fs';
import { join, resolve } from 'node:path';
import type {
  ActivityItem, AttachDatapackRequest, CreateProjectRequest, Project, ProjectHome, ReposResponse, SearchResult,
  UpdateProjectRequest,
} from '../shared/types.js';
import { REVIEWER_LABELS } from '../shared/types.js';
import { mekoCaptureDir } from './capture.js';
import type { Core } from './core.js';
import { all, get, run, tx } from './db.js';
import { projectDatapackDescription } from '../shared/datapacks.js';
import { cloneRepo, defaultBranch, isMainRepo, listBranches, repoNameFromUrl, scanRepos, tryGit } from './git.js';
import { errMsg } from './log.js';
import { pinSlug, projectAgentId } from './meko.js';
import { expandHome } from './paths.js';
import { repoRoots } from './config.js';
import { projectUsage } from './usage.js';
import {
  DEFAULT_SETTINGS, dedicatedDatapack, projectDatapacks, projectRow, projectRows, slugify, toProject, type ProjectRow,
} from './store.js';

/** How long the project home waits for each datapack's counts. */
const DESCRIBE_TIMEOUT_MS = 5000;

export class HttpError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

export class ProjectService {
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
    const used = new Map<string, { id: string; name: string }[]>();
    for (const p of projectRows(this.db)) {
      const key = resolve(p.repo_path);
      used.set(key, [...(used.get(key) ?? []), { id: p.id, name: p.name }]);
    }
    return {
      roots: cfg.repos.roots,
      repos: scanned
        .map((r) => ({ ...r, projects: used.get(resolve(r.path)) ?? [] }))
        .sort((a, b) => b.lastTouchedAt - a.lastTouchedAt),
    };
  }

  async create(req: CreateProjectRequest): Promise<Project> {
    const name = (req.name ?? '').trim();
    if (!name) throw new HttpError(400, 'Give the project a name.');
    if (!req.datapack) throw new HttpError(400, 'Choose a dedicated datapack.');
    if (!!req.repoPath === !!req.cloneUrl) throw new HttpError(400, 'Pick a repo or give a URL to clone.');
    if (!this.core.mekoStatus.ok) {
      await this.core.checkMeko();
      if (!this.core.mekoStatus.ok) throw new HttpError(503, `Meko is unreachable, so a project can't be created right now. (${this.core.mekoStatus.error})`);
    }

    let repoPath: string;
    if (req.cloneUrl) {
      const dest = join(expandHome(this.core.config.repos.cloneInto), repoNameFromUrl(req.cloneUrl));
      if (existsSync(dest)) throw new HttpError(409, `${dest} already exists. Pick it from the list instead.`);
      try {
        await cloneRepo(req.cloneUrl, dest);
      } catch (e) {
        throw new HttpError(400, `Clone failed: ${errMsg(e)}`);
      }
      repoPath = dest;
    } else {
      repoPath = resolve(expandHome(req.repoPath!));
    }
    if (!isMainRepo(repoPath)) throw new HttpError(400, `${repoPath} isn't a git repository (or it's a worktree).`);
    const siblings = projectRows(this.db).filter((p) => resolve(p.repo_path) === repoPath);
    this.checkNameFree(siblings, name);

    // Datapack first: if Meko refuses, nothing is written locally.
    let dp: { id: string; name: string };
    if (req.datapack.mode === 'create') {
      const dpName = req.datapack.name.trim();
      if (!dpName) throw new HttpError(400, 'Give the new datapack a name.');
      try {
        dp = await this.core.meko.createDatapack(dpName);
      } catch (e) {
        throw new HttpError(502, `Meko couldn't create the datapack "${dpName}": ${errMsg(e)}`);
      }
      // The repo's remote in the description lets whoever it's shared with find the repo. Best
      // effort, so it doesn't hold up the project: a failure is only logged.
      const remote = (await tryGit(repoPath, 'config', '--get', 'remote.origin.url'))?.trim() || null;
      const created = dp;
      void this.core.meko.setDatapackDescription(created.id, projectDatapackDescription(name, remote)).catch((e) =>
        this.core.log.warn(`couldn't set the description of datapack ${created.name}`, errMsg(e)));
    } else {
      if (!req.datapack.id || !req.datapack.name) throw new HttpError(400, 'Choose a datapack.');
      dp = { id: req.datapack.id, name: req.datapack.name };
      // An invite waiting in Meko is accepted first: if Meko refuses, nothing is written here.
      if (req.datapack.inviteId) await this.core.invites.acceptForProject(dp.id, req.datapack.inviteId);
    }

    // The first project on a repo is named after it; later ones add their own name.
    const repoSlug = slugify(repoPath.split('/').pop() ?? name);
    const id = this.uniqueId(siblings.length ? slugify(`${repoSlug}-${slugify(name)}`, 60) : repoSlug);
    const settings = { ...DEFAULT_SETTINGS };
    const branch = await defaultBranch(repoPath);
    const pos = (get<{ m: number | null }>(this.db, 'SELECT MAX(position) AS m FROM projects WHERE deleted_at IS NULL')?.m ?? -1) + 1;
    tx(this.db, () => {
      run(this.db, `INSERT INTO projects(id, name, repo_path, default_branch, position, settings_json, created_at)
                    VALUES (?, ?, ?, ?, ?, ?, ?)`, id, name, repoPath, branch, pos, JSON.stringify(settings), Date.now());
      run(this.db, `INSERT INTO project_datapacks(project_id, datapack_id, name, role) VALUES (?, ?, ?, 'dedicated')`, id, dp.id, dp.name);
    });
    this.writePin(id, dp);
    this.core.invites.markUsed(dp.id);
    // Trust the repo in Claude Code now, before any session of it starts.
    await this.core.sessions.trustRepo(repoPath);
    this.ensureConversation(id).catch((e) => this.core.log.warn('Meko conversation create failed', errMsg(e)));
    this.core.log.info(`project created: ${id} (${repoPath}) datapack ${dp.name}`);
    this.core.changed();
    return this.get(id);
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
      this.checkNameFree(projectRows(this.db).filter((p) => p.id !== id && resolve(p.repo_path) === resolve(r.repo_path)), n);
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
    // Counts are extras: with Meko down, or slow, the page shows without them.
    const mekoOk = this.core.mekoStatus.ok;
    const datapacks = await Promise.all(
      project.datapacks.map(async (d) => {
        try {
          if (!mekoOk) throw new Error('Meko is unreachable');
          return { ...d, ...(await this.core.meko.describeDatapack(d.id, DESCRIBE_TIMEOUT_MS)) };
        } catch {
          return { ...d, memoryCount: null, knowledgeCount: null, grant: null };
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
