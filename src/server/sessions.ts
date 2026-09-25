import { randomBytes, randomUUID } from 'node:crypto';
import { existsSync, rmSync } from 'node:fs';
import { join } from 'node:path';
import type { CreateSessionRequest, Session, SessionAction, SessionStatus, Stage } from '../shared/types.js';
import { STAGE_LABELS } from '../shared/types.js';
import type { Core } from './core.js';
import { get, run, tx } from './db.js';
import { addWorktree, diffStat, removeWorktree, restoreWorktree, tryGit, worktreeClean } from './git.js';
import type { HookPayload } from './hooks.js';
import { errMsg } from './log.js';
import { projectAgentId } from './meko.js';
import { loadPrompt, render, type PromptName } from './prompts.js';
import { HttpError } from './projects.js';
import { transcriptPathFor, writeLauncher } from './claude.js';
import {
  dedicatedDatapack, parseSettings, projectDatapacks, projectRow, sessionRow, sessionRows, slugify, toSession,
  type SessionRow, type SessionRuntime,
} from './store.js';
import { tmuxName } from './tmux.js';
import { TRUST_PROMPT_RE } from './trust.js';

const QUARTER_HOUR = 15 * 60 * 1000;
const HOUR = 60 * 60 * 1000;

export class SessionManager {
  private busy = new Set<string>();
  private diffs = new Map<string, { added: number; removed: number } | null>();
  private mergeInfo = new Map<string, { canMerge: boolean; blockers: string[] }>();
  /** Sessions showing Claude's plan-approval dialog. */
  readonly planPending = new Set<string>();
  /** Sessions being launched right now; reconcile leaves them alone. */
  private launching = new Set<string>();
  /** Sessions we are deliberately stopping; their exit isn't an event. */
  private stopping = new Set<string>();

  constructor(private core: Core) {}

  private get db() {
    return this.core.db;
  }

  // ---- reads ----

  runtime(id: string): SessionRuntime {
    const m = this.mergeInfo.get(id);
    return {
      busy: this.busy.has(id),
      diffStat: this.diffs.get(id) ?? null,
      canMerge: m?.canMerge ?? false,
      mergeBlockers: m?.blockers ?? [],
    };
  }

  listForProject(projectId: string): Session[] {
    return sessionRows(this.db, projectId)
      .filter((r) => r.desired === 'running')
      .map((r) => toSession(this.db, r, this.runtime(r.id)));
  }

  get(id: string): Session {
    return toSession(this.db, this.row(id), this.runtime(id));
  }

  row(id: string): SessionRow {
    const r = sessionRow(this.db, id);
    if (!r) throw new HttpError(404, 'That session no longer exists.');
    return r;
  }

  byToken(token: string): SessionRow | undefined {
    return get<SessionRow>(this.db, 'SELECT * FROM sessions WHERE hook_token = ?', token);
  }

  setBusy(id: string, on: boolean): void {
    if (on) this.busy.add(id);
    else this.busy.delete(id);
    this.core.changed();
  }

  setMergeInfo(id: string, info: { canMerge: boolean; blockers: string[] }): void {
    this.mergeInfo.set(id, info);
  }

  // ---- writes ----

  update(id: string, fields: Partial<Record<keyof SessionRow, string | number | null>>): void {
    const keys = Object.keys(fields);
    if (!keys.length) return;
    run(this.db, `UPDATE sessions SET ${keys.map((k) => `${k} = ?`).join(', ')} WHERE id = ?`,
      ...keys.map((k) => fields[k as keyof SessionRow] ?? null), id);
    this.core.changed();
  }

  setStatus(id: string, status: SessionStatus, detail: string | null = null): void {
    this.update(id, { status, status_detail: detail, last_event_at: Date.now() });
  }

  event(id: string, kind: string, text: string | null, payload?: unknown): void {
    const r = sessionRow(this.db, id);
    if (!r) return;
    run(this.db, 'INSERT INTO session_events(session_id, project_id, kind, text, payload_json, at) VALUES (?, ?, ?, ?, ?, ?)',
      id, r.project_id, kind, text, payload === undefined ? null : JSON.stringify(payload), Date.now());
  }

  setStage(id: string, stage: Stage, by: 'you' | 'auto'): void {
    const now = Date.now();
    tx(this.db, () => {
      run(this.db, 'UPDATE session_stages SET left_at = ? WHERE session_id = ? AND left_at IS NULL', now, id);
      run(this.db, 'INSERT INTO session_stages(session_id, stage, entered_at, moved_by) VALUES (?, ?, ?, ?)', id, stage, now, by);
      run(this.db, 'UPDATE sessions SET stage = ? WHERE id = ?', stage, id);
    });
    this.event(id, 'stage', stage === 'done' ? 'Done' : `Moved to ${STAGE_LABELS[stage]}`);
    this.core.changed();
  }

  notifyUser(id: string, title: string, body: string): void {
    const r = sessionRow(this.db, id);
    const p = r && projectRow(this.db, r.project_id);
    if (!r || !p || !parseSettings(p.settings_json).notify) return;
    this.core.notify({ projectId: r.project_id, sessionId: id, title: `${r.name}: ${title}`, body });
  }

  // ---- create / launch ----

  async create(projectId: string, req: CreateSessionRequest): Promise<Session> {
    const p = projectRow(this.db, projectId);
    if (!p) throw new HttpError(404, 'That project no longer exists.');
    const name = (req.name ?? '').trim();
    if (!name) throw new HttpError(400, 'Give the session a name.');
    if (!dedicatedDatapack(this.db, projectId)) throw new HttpError(409, 'This project has no dedicated datapack.');
    const settings = parseSettings(p.settings_json);
    const base = (req.baseBranch ?? '').trim() || p.default_branch;
    const task = (req.prompt ?? '').trim();
    const stage: Stage = req.skipDesign ? 'code' : 'design';

    const slug = await this.uniqueSlug(projectId, p.repo_path, settings.branchPrefix, slugify(name));
    const branch = `${settings.branchPrefix}${slug}`;
    const worktree = join(this.core.paths.worktrees, projectId, slug);
    try {
      await addWorktree(p.repo_path, worktree, branch, base);
    } catch (e) {
      throw new HttpError(400, `Couldn't create the worktree: ${errMsg(e)}`);
    }

    const id = `${projectId}--${slug}`;
    const now = Date.now();
    tx(this.db, () => {
      run(this.db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path,
                      tmux_name, hook_token, desired, status, stage, initial_prompt, created_at, last_event_at)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'running', 'starting', ?, ?, ?, ?)`,
        id, projectId, slug, name, randomUUID(), branch, base, worktree, tmuxName(projectId, slug),
        randomBytes(24).toString('base64url'), stage, task || null, now, now);
      run(this.db, `INSERT INTO session_stages(session_id, stage, entered_at, moved_by) VALUES (?, ?, ?, 'you')`, id, stage, now);
    });
    this.event(id, 'created', `Started on ${branch} from ${base}`);

    const row = this.row(id);
    const prompt = task ? this.prompt(row, stage === 'design' ? 'design' : 'code', { task }) : null;
    try {
      await this.launch(row, { resume: false, prompt });
    } catch (e) {
      this.setStatus(id, 'exited', `Launch failed: ${errMsg(e)}`);
      throw new HttpError(500, `Claude couldn't be started: ${errMsg(e)}`);
    }
    this.core.log.info(`session created: ${id}`);
    this.core.changed();
    return this.get(id);
  }

  private async uniqueSlug(projectId: string, repo: string, prefix: string, base: string): Promise<string> {
    for (let i = 1; ; i++) {
      const slug = i === 1 ? base : `${base}-${i}`;
      const taken =
        get(this.db, 'SELECT 1 AS x FROM sessions WHERE project_id = ? AND slug = ?', projectId, slug) ||
        (await tryGit(repo, 'rev-parse', '--verify', '--quiet', `refs/heads/${prefix}${slug}`)) ||
        existsSync(join(this.core.paths.worktrees, projectId, slug));
      if (!taken) return slug;
    }
  }

  prompt(row: SessionRow, name: PromptName, extra: Record<string, string | number | null> = {}): string {
    const p = projectRow(this.db, row.project_id);
    const settings = p ? parseSettings(p.settings_json) : null;
    return render(loadPrompt(this.db, name, row.worktree_path), {
      task: row.initial_prompt ?? '',
      branch: row.branch,
      base: row.base_branch,
      pr: row.pr_number,
      prUrl: row.pr_url,
      reviewer: settings?.reviewerLogin ?? 'the reviewer',
      repo: '{owner}/{repo}',
      ...extra,
    });
  }

  private env(row: SessionRow): Record<string, string> {
    const dps = projectDatapacks(this.db, row.project_id);
    const dedicated = dps.find((d) => d.role === 'dedicated');
    return {
      AK_SESSION: row.id,
      AK_HOOK_TOKEN: row.hook_token,
      AK_HOOK_SOCK: this.core.paths.hookSock,
      MEKO_AGENT_ID: projectAgentId(row.project_id),
      MEKO_DATAPACK_ID: dedicated?.id ?? '',
      MEKO_DATAPACK_IDS: dps.map((d) => d.id).join(','),
    };
  }

  private systemPrompt(row: SessionRow): string {
    const p = projectRow(this.db, row.project_id);
    const dps = projectDatapacks(this.db, row.project_id);
    const dedicated = dps.find((d) => d.role === 'dedicated');
    const shared = dps.filter((d) => d.role === 'shared');
    const lines = [
      `This Claude Code session is managed by agent-k-local. Project: ${p?.name ?? row.project_id}. Task: ${row.name}. Branch: ${row.branch} (base ${row.base_branch}). Stages: Design → Code → PR → Review → Merge → Cleanup; the user approves the design, opening the PR, and merging from the agent-k-local UI.`,
    ];
    if (dedicated) {
      lines.push(`Meko: this project's dedicated datapack is "${dedicated.name}" (datapack_id ${dedicated.id}). Pass that datapack_id and agent_id "${projectAgentId(row.project_id)}" on Meko calls that write.`);
    }
    if (shared.length) {
      lines.push(`Shared datapacks you may search (read-only, pass datapack_id explicitly): ${shared.map((d) => `"${d.name}" (${d.id})`).join(', ')}.`);
    }
    return lines.join('\n');
  }

  private transcriptExists(row: SessionRow): boolean {
    return (!!row.transcript_path && existsSync(row.transcript_path)) || existsSync(transcriptPathFor(row.worktree_path, row.claude_session_id));
  }

  /** Starts (or restarts) Claude in the session's tmux session. */
  async launch(row: SessionRow, opts: { resume: boolean; prompt: string | null }): Promise<void> {
    this.launching.add(row.id);
    try {
      const script = writeLauncher(this.core.paths, {
        sessionId: row.id,
        worktree: row.worktree_path,
        claudeCommand: this.core.config.claude.command,
        claudeSessionId: row.claude_session_id,
        resume: opts.resume,
        name: row.name,
        planMode: row.stage === 'design',
        appendSystemPrompt: this.systemPrompt(row),
        prompt: opts.prompt,
        env: this.env(row),
      });
      const command = `exec sh '${script}'`;
      const panes = await this.core.tmux.panes();
      if (panes.has(row.tmux_name)) await this.core.tmux.respawn(row.tmux_name, row.worktree_path, command);
      else await this.core.tmux.newSession(row.tmux_name, row.worktree_path, command);
    } finally {
      this.launching.delete(row.id);
    }
  }

  /** Types a prompt into the session's Claude TUI and submits it. */
  async submit(id: string, text: string): Promise<void> {
    const r = this.row(id);
    await this.core.tmux.submit(r.tmux_name, text);
    this.setStatus(id, 'working');
  }

  // ---- hooks ----

  async onHook(token: string, p: HookPayload): Promise<void> {
    const row = this.byToken(token);
    if (!row || row.desired !== 'running') return;
    const id = row.id;
    const ev = p.hook_event_name ?? '';
    const now = Date.now();
    run(this.db, 'UPDATE sessions SET last_event_at = ? WHERE id = ?', now, id);

    switch (ev) {
      case 'SessionStart': {
        if (p.transcript_path) run(this.db, 'UPDATE sessions SET transcript_path = ? WHERE id = ?', p.transcript_path, id);
        const trustPrompt = row.status === 'needs_input' && /trust this folder/.test(row.status_detail ?? '');
        if (row.status === 'starting' || row.status === 'exited' || trustPrompt) this.setStatus(id, 'idle');
        break;
      }
      case 'UserPromptSubmit':
        this.planPending.delete(id);
        this.update(id, { interrupted: 0 });
        this.setStatus(id, 'working');
        break;
      case 'PreToolUse': {
        if (p.tool_name === 'ExitPlanMode') {
          const plan = typeof p.tool_input?.plan === 'string' ? p.tool_input.plan : '';
          if (plan) await this.core.lifecycle.saveDesign(id, plan);
          this.planPending.add(id);
          this.setStatus(id, 'needs_input', 'Design ready for your approval');
          this.notifyUser(id, 'Design ready', 'Review and approve the design.');
        }
        break;
      }
      case 'PostToolUse': {
        if (p.tool_name === 'ExitPlanMode') {
          this.planPending.delete(id);
          const resp = JSON.stringify(p.tool_response ?? '').toLowerCase();
          if (row.stage === 'design' && !/reject|denied|keep planning/.test(resp)) this.core.lifecycle.designApproved(id, 'you');
        }
        if (row.status !== 'working') this.setStatus(id, 'working');
        break;
      }
      case 'PermissionRequest': {
        const detail = p.tool_name ? `Permission needed for ${p.tool_name}` : 'Permission needed';
        if (row.status !== 'needs_input') this.notifyUser(id, 'Needs you', detail);
        this.setStatus(id, 'needs_input', detail);
        break;
      }
      case 'Notification': {
        const msg = p.message ?? '';
        if (p.notification_type === 'idle_prompt' || /waiting for your input/i.test(msg)) {
          if (row.status !== 'needs_input' && row.status !== 'working') this.setStatus(id, 'idle');
        } else if (msg) {
          if (row.status !== 'needs_input') this.notifyUser(id, 'Needs you', msg);
          this.setStatus(id, 'needs_input', msg);
        }
        break;
      }
      case 'Stop': {
        if (this.planPending.has(id)) break;
        this.update(id, { interrupted: 0 });
        this.setStatus(id, 'idle');
        this.refreshDiff(id).catch(() => undefined);
        await this.core.lifecycle.onTurnEnd(id);
        break;
      }
      case 'SessionEnd':
        if (this.stopping.has(id)) break;
        if (row.status === 'working') this.update(id, { interrupted: 1 });
        this.setStatus(id, 'exited', p.reason ? `Claude exited (${p.reason})` : 'Claude exited');
        break;
    }
    this.core.changed();
  }

  // ---- liveness, recovery ----

  /** Boot-time recovery: bring back every session that should be running. */
  async recover(): Promise<void> {
    await this.core.tmux.ensureServer();
    const panes = await this.core.tmux.panes();
    for (const row of sessionRows(this.db)) {
      if (row.desired !== 'running' || !projectRow(this.db, row.project_id)) continue;
      const pane = panes.get(row.tmux_name);
      if (pane && !pane.dead) continue; // server restart; Claude kept running
      if (pane && pane.dead) {
        if (row.status !== 'exited') this.setStatus(row.id, 'exited', 'Claude exited while the server was down');
        continue;
      }
      await this.recoverOne(row).catch((e) => this.core.log.error(`recovery failed for ${row.id}`, e));
    }
  }

  /** Periodic check: dead panes become `exited`; missing panes (tmux died) are recovered. */
  async reconcile(): Promise<void> {
    if (!(await this.core.tmux.serverRunning())) {
      this.core.log.warn('tmux server is gone; recovering sessions');
      await this.recover();
      return;
    }
    const panes = await this.core.tmux.panes();
    for (const row of sessionRows(this.db)) {
      if (row.desired !== 'running' || this.launching.has(row.id) || this.stopping.has(row.id)) continue;
      const pane = panes.get(row.tmux_name);
      if (!pane) {
        await this.recoverOne(row).catch((e) => this.core.log.error(`recovery failed for ${row.id}`, e));
      } else if (!pane.dead && row.status === 'starting' && Date.now() - (row.last_event_at ?? 0) > 4000) {
        // Claude fires no hooks until the folder-trust dialog is answered.
        const screen = await this.core.tmux.capture(row.tmux_name).catch(() => '');
        if (TRUST_PROMPT_RE.test(screen)) {
          this.setStatus(row.id, 'needs_input', 'Claude is asking whether you trust this folder. Answer in the terminal.');
          this.notifyUser(row.id, 'Needs you', 'Confirm folder trust in the terminal.');
        }
      } else if (pane.dead && row.status !== 'exited') {
        if (row.status === 'working') this.update(row.id, { interrupted: 1 });
        this.planPending.delete(row.id);
        this.busy.delete(row.id);
        this.setStatus(row.id, 'exited', pane.exitStatus ? `Claude exited with status ${pane.exitStatus}` : 'Claude exited');
        this.event(row.id, 'exited', 'Claude exited');
        this.notifyUser(row.id, 'Exited', 'Claude exited. Resume it from the session header.');
      }
    }
  }

  /**
   * Relaunches a session whose tmux pane is gone (host reboot, tmux crash).
   * A session that was mid-turn gets a continue prompt, at most once per
   * 15 minutes; after two recoveries in an hour it waits for the user instead.
   */
  async recoverOne(row: SessionRow): Promise<void> {
    const p = projectRow(this.db, row.project_id);
    if (!p) return;
    await restoreWorktree(p.repo_path, row.worktree_path, row.branch, row.base_branch);
    const now = Date.now();
    const history = (JSON.parse(row.auto_continue_at) as number[]).filter((t) => now - t < HOUR);
    const wasWorking = row.status === 'working' || row.interrupted === 1;
    const autoContinue = parseSettings(p.settings_json).autoContinue;
    const tooSoon = history.some((t) => now - t < QUARTER_HOUR);
    const parked = wasWorking && autoContinue && history.length >= 2;
    const cont = wasWorking && autoContinue && !tooSoon && !parked;
    const resume = this.transcriptExists(row);

    let prompt: string | null = null;
    if (!resume) prompt = row.initial_prompt ? this.prompt(row, row.stage === 'design' ? 'design' : 'code') : null;
    else if (cont) prompt = this.prompt(row, row.stage === 'review' ? 'continue-review' : 'continue');

    this.busy.delete(row.id);
    this.planPending.delete(row.id);
    await this.launch(row, { resume, prompt });
    const status: SessionStatus = cont ? 'working' : parked ? 'needs_input' : 'recovered';
    const detail = cont
      ? 'Recovered after a restart and continuing'
      : parked
        ? 'Recovered twice in the last hour; waiting for you before continuing'
        : wasWorking && tooSoon
          ? 'Recovered after a restart; not auto-continued (recovered less than 15 minutes ago)'
          : 'Recovered after a restart';
    this.update(row.id, {
      status,
      status_detail: detail,
      recovered_at: now,
      interrupted: 0,
      auto_continue_at: JSON.stringify(cont ? [...history, now] : history),
    });
    this.event(row.id, 'recovered', detail);
    if (parked) this.notifyUser(row.id, 'Needs you', detail);
    this.core.log.info(`recovered ${row.id}: ${detail}`);
  }

  // ---- actions ----

  async action(id: string, action: SessionAction): Promise<Session> {
    const row = this.row(id);
    if (row.desired !== 'running') throw new HttpError(409, 'This session is archived.');
    const lc = this.core.lifecycle;
    switch (action) {
      case 'approve-design': await lc.approveDesign(row); break;
      case 'open-pr': await lc.openPr(row); break;
      case 'merge': await lc.merge(row); break;
      case 'request-review': await lc.requestReview(row, false); break;
      case 'force-review': await lc.requestReview(row, true); break;
      case 'resume': await this.resume(row); break;
      case 'archive': await this.archive(row, { keepBranch: true }); break;
      default: throw new HttpError(400, `Unknown action ${String(action)}.`);
    }
    return this.get(id);
  }

  private async resume(row: SessionRow): Promise<void> {
    const pane = (await this.core.tmux.panes()).get(row.tmux_name);
    if (pane && !pane.dead) throw new HttpError(409, 'Claude is already running in this session.');
    const p = projectRow(this.db, row.project_id);
    if (p) await restoreWorktree(p.repo_path, row.worktree_path, row.branch, row.base_branch);
    const resume = this.transcriptExists(row);
    await this.launch(row, { resume, prompt: resume ? null : row.initial_prompt ? this.prompt(row, row.stage === 'design' ? 'design' : 'code') : null });
    this.setStatus(row.id, 'starting', 'Resuming');
    this.event(row.id, 'resumed', 'Resumed');
  }

  /** Stops Claude and removes the worktree if it's clean. The branch is kept unless asked. */
  async archive(row: SessionRow, opts: { keepBranch: boolean; stage?: Stage }): Promise<{ worktreeRemoved: boolean }> {
    this.stopping.add(row.id);
    try {
      run(this.db, `UPDATE sessions SET desired = 'archived', status = 'archived', archived_at = ? WHERE id = ?`, Date.now(), row.id);
      await this.core.tmux.kill(row.tmux_name);
      const p = projectRow(this.db, row.project_id);
      let worktreeRemoved = false;
      if (p) {
        if (!existsSync(row.worktree_path) || (await worktreeClean(row.worktree_path))) {
          worktreeRemoved = await removeWorktree(p.repo_path, row.worktree_path);
        }
        if (worktreeRemoved && !opts.keepBranch) await tryGit(p.repo_path, 'branch', '-D', row.branch);
      }
      rmSync(join(this.core.paths.run, `${row.id}.sh`), { force: true });
      this.busy.delete(row.id);
      this.planPending.delete(row.id);
      this.mergeInfo.delete(row.id);
      this.diffs.delete(row.id);
      this.event(row.id, 'archived', worktreeRemoved ? 'Archived; worktree removed' : 'Archived; worktree kept because it has uncommitted changes');
      this.core.changed();
      return { worktreeRemoved };
    } finally {
      this.stopping.delete(row.id);
    }
  }

  /** Used when a project is deleted: stop everything, remove worktrees, keep branches. */
  async teardownProject(projectId: string): Promise<void> {
    const p = projectRow(this.db, projectId);
    for (const row of sessionRows(this.db, projectId)) {
      if (row.desired === 'running') {
        this.stopping.add(row.id);
        run(this.db, `UPDATE sessions SET desired = 'archived', status = 'archived', archived_at = ? WHERE id = ?`, Date.now(), row.id);
        await this.core.tmux.kill(row.tmux_name);
        this.stopping.delete(row.id);
      }
      if (p) await removeWorktree(p.repo_path, row.worktree_path, true);
      rmSync(join(this.core.paths.run, `${row.id}.sh`), { force: true });
    }
  }

  // ---- diff stats ----

  async refreshDiff(id: string): Promise<void> {
    const r = sessionRow(this.db, id);
    if (!r || r.desired !== 'running' || !existsSync(r.worktree_path)) return;
    const d = await diffStat(r.worktree_path, r.base_branch);
    const prev = this.diffs.get(id);
    this.diffs.set(id, d);
    if (JSON.stringify(prev) !== JSON.stringify(d)) this.core.changed();
  }

  async refreshDiffStats(): Promise<void> {
    for (const r of sessionRows(this.db)) {
      if (r.desired === 'running') await this.refreshDiff(r.id).catch(() => undefined);
    }
  }
}
