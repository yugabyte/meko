import { randomBytes, randomUUID } from 'node:crypto';
import { existsSync, mkdirSync, realpathSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import type { ChangeSide, CreateSessionRequest, DiscardCheck, Session, SessionChanges, SessionAction, SessionKind, SessionStatus, Stage, StartupState, StartupStep, TokenUsage } from '../shared/types.js';
import { KIND_STAGES, SESSION_KINDS, STAGE_LABELS } from '../shared/types.js';
import type { Core } from './core.js';
import { all, get, run, tx } from './db.js';
import { changedImage, changes, diffStat } from './changes.js';
import { BranchGoneError, addWorktree, git, removeWorktree, restoreWorktree, tryGit, uncommittedCount, unpushedCount, worktreeClean, type SyncResult } from './git.js';
import type { GuardDecision, HookPayload } from './hooks.js';
import { errMsg } from './log.js';
import { projectAgentId } from './meko.js';
import { loadPrompt, render, type PromptName } from './prompts.js';
import { HttpError } from './projects.js';
import { EDIT_TOOLS, shq, statusLineScriptPath, transcriptPathFor, userStatusLine, writeLauncher } from './claude.js';
import {
  dedicatedDatapack, parseSettings, projectDatapacks, projectRow, sessionRow, sessionRows, slugify, toSession,
  type SessionRow, type SessionRuntime,
} from './store.js';
import { sleep, tmuxName, type PaneInfo } from './tmux.js';
import { ZERO_USAGE, ingestSessionUsage, sessionTranscript, sessionUsage, usageBySession } from './usage.js';
import { TRUST_PROMPT_RE, ensureWorkspaceTrust } from './trust.js';

/** What the appended system prompt says about the session's workflow, per kind. */
const WORKFLOW: Record<SessionKind, string> = {
  explore: 'This is an Exploration session: investigate, answer questions, build POCs and run experiments in this worktree, and push experimental branches when useful. AgentK Local runs no design, PR, review or merge stages for it; the user ends the session from the AgentK Local UI.',
  plan: 'Stages: Design → Code → PR → Review → Merge & Close; the user approves the design, opening the PR, and merging from the AgentK Local UI.',
  quick: 'Stages: Code → PR → Review → Merge & Close; the user approves opening the PR and merging from the AgentK Local UI.',
};

/** Longest setInterval delay Node honours, in whole seconds (2^31-1 ms). */
export const MAX_TIMER_SECS = Math.floor((2 ** 31 - 1) / 1000);
const QUARTER_HOUR = 15 * 60 * 1000;
const HOUR = 60 * 60 * 1000;
/** How long stopping a session waits for its last turns to reach Meko. */
const FINAL_CAPTURE_MS = 15000;
/** How long a new session waits for its Meko conversation to be created before Claude starts anyway. */
const NAME_CONVERSATION_MS = 5000;
/** How long stopping a session waits for it to finish starting. */
const SETTLE_MS = 60000;
/** How long recovery waits before trying again after a failed restart (each try may fetch from the remote). */
const RECOVERY_BACKOFF_MS = 5 * 60 * 1000;
/**
 * Session variables the launcher clears when it doesn't set them, so a value inherited from
 * tmux's environment or the user's profile can't leak in. MEKO_CHECKPOINT_INTERVAL is only
 * set with capture off; inherited with capture on, it would silently turn capture off.
 */
const SESSION_ENV_UNSET = ['MEKO_CHECKPOINT_INTERVAL'];

/** The "created" event: where the branch started, and what happened to the local base. */
function startedText(branch: string, base: string, sync: SyncResult | null): string {
  switch (sync?.kind) {
    case 'updated': return `Started on ${branch} from ${base} (fast-forwarded ${base} to origin/${base})`;
    case 'current': return `Started on ${branch} from ${base} (already up to date with origin)`;
    case 'skipped': return `Started on ${branch} from origin/${base}; local ${base} wasn't updated: ${sync.reason}`;
    default: return `Started on ${branch} from ${base}`;
  }
}

function realpath(p: string): string {
  try {
    return realpathSync(p);
  } catch {
    return p;
  }
}

const within = (p: Promise<void>, ms: number) => Promise.race([p, new Promise<void>((r) => setTimeout(r, ms).unref())]);

export class SessionManager {
  private busy = new Set<string>();
  private diffs = new Map<string, { added: number; removed: number } | null>();
  private mergeInfo = new Map<string, { canMerge: boolean; blockers: string[] }>();
  /** Sessions showing Claude's plan-approval dialog. */
  readonly planPending = new Set<string>();
  /** Sessions being launched right now; reconcile leaves them alone. */
  private launching = new Set<string>();
  /** Sessions being created right now (row written, Claude not started yet); recovery leaves them alone. */
  private creating = new Set<string>();
  /** Sessions we are deliberately stopping; their exit isn't an event. */
  private stopping = new Set<string>();
  /** Sessions whose Claude was launched but hasn't sent SessionStart yet (it may be showing the folder-trust dialog). */
  private awaitingStart = new Set<string>();
  /** The tail of each session's prompt submissions, so two prompts are never pasted at once. */
  private submitting = new Map<string, Promise<void>>();
  /** After a failed recovery, when to try again (Infinity: not until the user resumes it). */
  private recoverRetryAt = new Map<string, number>();
  /** Token totals per session, refreshed after each transcript read. */
  private usage = new Map<string, TokenUsage>();
  private usageLoaded = false;
  private usageReadAt = new Map<string, number>();
  /** Startup progress, for the "Starting…" view. */
  private startup = new Map<string, StartupState>();

  setStartup(id: string, step: StartupStep, opts: { resumed?: boolean; hasPrompt?: boolean } = {}): void {
    const prev = this.startup.get(id);
    const now = Date.now();
    this.startup.set(id, {
      step,
      stepSince: now,
      startedAt: prev?.startedAt ?? now,
      resumed: opts.resumed ?? prev?.resumed ?? false,
      hasPrompt: opts.hasPrompt ?? prev?.hasPrompt ?? false,
    });
    this.core.changed();
  }

  clearStartup(id: string): void {
    if (this.startup.delete(id)) this.core.changed();
  }

  constructor(private core: Core) {}

  private get db() {
    return this.core.db;
  }

  // ---- reads ----

  runtime(id: string): SessionRuntime {
    const m = this.mergeInfo.get(id);
    return {
      usage: this.usage.get(id) ?? { ...ZERO_USAGE },
      startup: this.startup.get(id) ?? null,
      busy: this.busy.has(id),
      diffStat: this.diffs.get(id) ?? null,
      canMerge: m?.canMerge ?? false,
      mergeBlockers: m?.blockers ?? [],
    };
  }

  listForProject(projectId: string): Session[] {
    this.loadUsage();
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
    const settings = parseSettings(p.settings_json);
    const base = (req.baseBranch ?? '').trim() || p.default_branch;
    const task = (req.prompt ?? '').trim();
    const kind: SessionKind = req.kind ?? (req.skipDesign ? 'quick' : 'plan');
    if (!SESSION_KINDS.includes(kind)) throw new HttpError(400, `Pick a session type: ${SESSION_KINDS.join(', ')}.`);
    const stage: Stage = KIND_STAGES[kind][0];

    const slug = await this.uniqueSlug(projectId, p.repo_path, settings.branchPrefix, slugify(name));
    const branch = `${settings.branchPrefix}${slug}`;
    const worktree = join(this.core.paths.worktrees, projectId, slug);
    const id = `${projectId}--${slug}`;
    const now = Date.now();
    // The row goes in first so the tab (and its "Starting…" view) shows up right away.
    tx(this.db, () => {
      run(this.db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, worktree_path,
                      tmux_name, hook_token, desired, status, kind, stage, initial_prompt, created_at, last_event_at, plan_mode)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'running', 'starting', ?, ?, ?, ?, ?, 0)`,
        id, projectId, slug, name, randomUUID(), branch, base, worktree, tmuxName(projectId, slug),
        randomBytes(24).toString('base64url'), kind, stage, task || null, now, now);
      run(this.db, `INSERT INTO session_stages(session_id, stage, entered_at, moved_by) VALUES (?, ?, ?, 'you')`, id, stage, now);
    });
    // Until Claude is running, the row has no pane; reconcile must not mistake it for a lost session.
    this.creating.add(id);
    try {
      this.setStartup(id, 'worktree', { resumed: false, hasPrompt: !!task });
      const naming = this.nameConversation(this.row(id));
      let sync: SyncResult | null;
      try {
        sync = await addWorktree(p.repo_path, worktree, branch, base);
      } catch (e) {
        this.startup.delete(id);
        run(this.db, 'DELETE FROM sessions WHERE id = ?', id);
        this.core.changed();
        throw new HttpError(400, `Couldn't create the worktree: ${errMsg(e)}`);
      }
      this.event(id, 'created', startedText(branch, base, sync));

      const row = this.row(id);
      const prompt = task ? this.prompt(row, this.firstPrompt(row), { task }) : null;
      this.setStartup(id, 'launch');
      await within(naming, NAME_CONVERSATION_MS);
      try {
        await this.launch(row, { resume: false, prompt });
      } catch (e) {
        this.startup.delete(id);
        if (e instanceof HttpError && e.status === 409) throw e;
        this.setStatus(id, 'exited', `Launch failed: ${errMsg(e)}`);
        throw new HttpError(500, `Claude couldn't be started: ${errMsg(e)}`);
      }
      this.setStartup(id, 'loading');
      this.core.log.info(`session created: ${id}`);
      this.core.changed();
      return this.get(id);
    } finally {
      this.creating.delete(id);
    }
  }

  private async uniqueSlug(projectId: string, repo: string, prefix: string, base: string): Promise<string> {
    for (let i = 1; ; i++) {
      const slug = i === 1 ? base : `${base}-${i}`;
      const taken =
        get(this.db, 'SELECT 1 AS x FROM sessions WHERE (project_id = ? AND slug = ?) OR tmux_name = ?', projectId, slug, tmuxName(projectId, slug)) ||
        (await tryGit(repo, 'rev-parse', '--verify', '--quiet', `refs/heads/${prefix}${slug}`)) ||
        existsSync(join(this.core.paths.worktrees, projectId, slug));
      if (!taken) return slug;
    }
  }

  /** Where Claude writes a markdown design during the Design stage (outside the repo). */
  designFile(row: SessionRow): string {
    return join(this.core.paths.designs, row.id, 'design.md');
  }

  /** The prompt a session's task starts with, for the stage it's in. */
  firstPrompt(row: SessionRow): PromptName {
    if (row.stage === 'explore') return 'explore';
    if (row.stage !== 'design') return 'code';
    return row.plan_mode ? 'design-plan' : 'design';
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
      designFile: this.designFile(row),
      ...extra,
    });
  }

  /** The project's datapacks for a session to use: none while Meko is off, so nothing points it at Meko. */
  private sessionDatapacks(projectId: string) {
    return this.core.mekoStatus.configured ? projectDatapacks(this.db, projectId) : [];
  }

  private env(row: SessionRow): Record<string, string> {
    const dps = this.sessionDatapacks(row.project_id);
    const dedicated = dps.find((d) => d.role === 'dedicated');
    // Our status line replaces the user's; it runs theirs (resolved for this worktree) after reporting limits.
    // Set even when empty: empty means the user has none, unset means a session from before this.
    return {
      AK_USER_STATUSLINE: userStatusLine(row.worktree_path, statusLineScriptPath(this.core.paths))?.command ?? '',
      AK_SESSION: row.id,
      AK_HOOK_TOKEN: row.hook_token,
      AK_HOOK_SOCK: this.core.paths.hookSock,
      AK_DESIGN_FILE: this.designFile(row),
      MEKO_AGENT_ID: projectAgentId(row.project_id),
      MEKO_DATAPACK_ID: dedicated?.id ?? '',
      MEKO_DATAPACK_IDS: dps.map((d) => d.id).join(','),
      // With capture off, the Meko plugin's checkpoint timer (started by its SessionStart hook on a
      // resumed session) mustn't send transcripts either. It runs setInterval(tick, seconds * 1000),
      // and Node turns a delay past 2^31-1 ms into 1 ms, so this is the longest safe interval.
      ...(this.core.capture.enabled ? {} : { MEKO_CHECKPOINT_INTERVAL: String(MAX_TIMER_SECS) }),
    };
  }

  private systemPrompt(row: SessionRow): string {
    const p = projectRow(this.db, row.project_id);
    const dps = this.sessionDatapacks(row.project_id);
    const dedicated = dps.find((d) => d.role === 'dedicated');
    const shared = dps.filter((d) => d.role === 'shared');
    const lines = [
      `This Claude Code session is managed by AgentK Local. Project: ${p?.name ?? row.project_id}. Task: ${row.name}. Branch: ${row.branch} (base ${row.base_branch}). ${WORKFLOW[row.kind]}`,
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

  /**
   * Runs a resume or recovery of a session, marked as launching so reconcile, recovery and
   * archive leave it alone until Claude is up. Refuses a session that is already starting.
   */
  private async starting<T>(id: string, fn: () => Promise<T>): Promise<T> {
    if (this.creating.has(id) || this.launching.has(id)) throw new HttpError(409, 'This session is already starting.');
    this.launching.add(id);
    try {
      return await fn();
    } finally {
      this.launching.delete(id);
    }
  }

  /** Waits for a session's create, resume or recovery to finish, so stopping it never races Claude starting. */
  private async settle(id: string): Promise<void> {
    const busy = () => this.creating.has(id) || this.launching.has(id);
    for (let waited = 0; busy() && waited < SETTLE_MS; waited += 100) await sleep(100);
    if (busy()) throw new HttpError(409, 'This session is still starting. Try again in a moment.');
  }

  /** Pins the project's dedicated datapack for its Meko agent id again (it may have been changed or removed). */
  private pinDatapack(projectId: string): void {
    const dp = dedicatedDatapack(this.db, projectId);
    // Meko off: AgentK pins nothing. Claude Code's own Meko setup, if any, is left as it is.
    if (!dp || !this.core.mekoStatus.configured) return;
    try {
      this.core.projects.writePin(projectId, dp);
    } catch (e) {
      this.core.log.warn(`could not pin the Meko datapack for ${projectId}`, errMsg(e));
    }
  }

  /**
   * Starts (or restarts) Claude in the session's tmux session. Callers mark the session as
   * creating or launching for the whole start (see `starting()`).
   */
  async launch(row: SessionRow, opts: { resume: boolean; prompt: string | null }): Promise<void> {
    // Claude Code keys folder trust by the git root, which for a worktree is the main repo.
    const repo = projectRow(this.db, row.project_id)?.repo_path;
    if (repo) await this.trustRepo(repo);
    this.pinDatapack(row.project_id);
    const designDir = join(this.core.paths.designs, row.id);
    mkdirSync(designDir, { recursive: true });
    const script = writeLauncher(this.core.paths, {
      sessionId: row.id,
      worktree: row.worktree_path,
      claudeCommand: this.core.config.claude.command,
      claudeSessionId: row.claude_session_id,
      resume: opts.resume,
      name: row.name,
      planMode: row.stage === 'design' && !!row.plan_mode,
      addDirs: [designDir],
      appendSystemPrompt: this.systemPrompt(row),
      prompt: opts.prompt,
      env: this.env(row),
      unsetEnv: SESSION_ENV_UNSET,
    });
    const command = `exec sh ${shq(script)}`;
    const pane = (await this.core.tmux.panes()).get(row.tmux_name);
    // Archived (or its project deleted) while it was starting: Claude must not start behind its back.
    if (sessionRow(this.db, row.id)?.desired !== 'running') throw new HttpError(409, 'This session was archived while it was starting.');
    // Never replace a live Claude: respawn-pane -k would end it (or start this one twice).
    if (pane && !pane.dead) throw new HttpError(409, 'Claude is already running in this session.');
    this.awaitingStart.add(row.id);
    try {
      if (pane) await this.core.tmux.respawn(row.tmux_name, row.worktree_path, command);
      else await this.core.tmux.newSession(row.tmux_name, row.worktree_path, command);
    } catch (e) {
      this.awaitingStart.delete(row.id);
      throw e;
    }
    this.recoverRetryAt.delete(row.id);
  }

  /**
   * Makes sure Claude Code trusts the repo. Other Claude processes rewrite
   * ~/.claude.json with their own copy when they start, which can drop a
   * fresh entry, so a new entry is checked again after a moment.
   */
  async trustRepo(repo: string): Promise<void> {
    try {
      for (let i = 0; i < 3; i++) {
        if (ensureWorkspaceTrust(repo) !== 'added') return;
        await sleep(400);
      }
    } catch (e) {
      this.core.log.warn(`could not mark ${repo} as trusted in Claude Code`, errMsg(e));
    }
  }

  /**
   * Types a prompt into the session's Claude TUI and submits it. Submissions to one session
   * go one at a time; one that had to wait is refused if the one before it started a turn.
   */
  submit(id: string, text: string): Promise<void> {
    const prev = this.submitting.get(id);
    const next = (prev ?? Promise.resolve()).then(async () => {
      const r = this.row(id);
      if (r.desired !== 'running') throw new HttpError(409, 'This session is archived.');
      if (prev && r.status === 'working') throw new HttpError(409, 'AgentK is working on another prompt. Try again when the turn ends.');
      await this.core.tmux.submit(r.tmux_name, text);
      this.setStatus(id, 'working');
    });
    const tail = next.catch(() => undefined);
    this.submitting.set(id, tail);
    void tail.then(() => {
      if (this.submitting.get(id) === tail) this.submitting.delete(id);
    });
    return next;
  }

  // ---- hooks ----

  /**
   * Design-stage guard, asked before every file edit: nothing in the repo (worktree or main
   * checkout) changes until the design is approved. Returns Claude Code's deny decision, or
   * null to let the edit go ahead.
   */
  guard(token: string, p: HookPayload): GuardDecision {
    const row = this.byToken(token);
    if (!row || row.stage !== 'design' || row.plan_mode || !EDIT_TOOLS.includes(p.tool_name ?? '')) return null;
    const raw = p.tool_input?.file_path ?? p.tool_input?.notebook_path;
    if (typeof raw !== 'string' || !raw) return null;
    const file = resolve(p.cwd || row.worktree_path, raw);
    const repo = projectRow(this.db, row.project_id)?.repo_path;
    const roots = [row.worktree_path, repo].filter((d): d is string => !!d).flatMap((d) => [d, realpath(d)]);
    if (!roots.some((d) => file === d || file.startsWith(d + sep))) return null;
    return {
      hookSpecificOutput: {
        hookEventName: 'PreToolUse',
        permissionDecision: 'deny',
        permissionDecisionReason:
          `This session is in the Design stage: AgentK Local blocks changes to the repo until the user approves the design. ` +
          `Publish the design as an Artifact, or write it to ${this.designFile(row)}.`,
      },
    };
  }

  /** A status line run: only its plan usage limits are kept. */
  onStatusLine(token: string, input: unknown): void {
    const row = this.byToken(token);
    if (row?.desired === 'running') this.core.limits.report(input);
  }

  async onHook(token: string, p: HookPayload): Promise<void> {
    const row = this.byToken(token);
    if (!row || row.desired !== 'running') return;
    const id = row.id;
    const ev = p.hook_event_name ?? '';
    const now = Date.now();
    run(this.db, 'UPDATE sessions SET last_event_at = ? WHERE id = ?', now, id);

    switch (ev) {
      case 'SessionStart': {
        this.awaitingStart.delete(id);
        if (p.transcript_path) run(this.db, 'UPDATE sessions SET transcript_path = ? WHERE id = ?', p.transcript_path, id);
        // /clear (or /resume in the TUI) moves Claude to another conversation; resume that one from now on.
        if (typeof p.session_id === 'string' && /^[\w-]{1,100}$/.test(p.session_id) && p.session_id !== row.claude_session_id) {
          run(this.db, 'UPDATE sessions SET claude_session_id = ? WHERE id = ?', p.session_id, id);
          this.event(id, 'conversation', 'Claude moved to a new conversation; Resume continues that one');
        }
        const trustPrompt = row.status === 'needs_input' && /trust this folder/.test(row.status_detail ?? '');
        const st = this.startup.get(id);
        if ((row.status === 'starting' || trustPrompt) && st?.hasPrompt && !st.resumed) {
          // Claude is up; it submits the first prompt next (UserPromptSubmit ends startup).
          if (trustPrompt) this.setStatus(id, 'starting');
          this.setStartup(id, 'prompt');
        } else if (row.status === 'starting' || row.status === 'exited' || trustPrompt) {
          this.clearStartup(id);
          this.setStatus(id, 'idle');
        }
        break;
      }
      case 'UserPromptSubmit':
        this.clearStartup(id);
        this.planPending.delete(id);
        this.update(id, { interrupted: 0 });
        this.setStatus(id, 'working');
        break;
      case 'PreToolUse': {
        if (p.tool_name === 'ExitPlanMode') {
          const plan = typeof p.tool_input?.plan === 'string' ? p.tool_input.plan : '';
          if (plan) await this.core.lifecycle.saveDesign(id, { markdown: plan });
          void this.captureTranscript(id);
          this.planPending.add(id);
          this.core.lifecycle.designReady(id);
        }
        break;
      }
      case 'PostToolUse': {
        if (p.tool_name === 'ExitPlanMode') {
          this.planPending.delete(id);
          const resp = JSON.stringify(p.tool_response ?? '').toLowerCase();
          if (row.stage === 'design' && !/reject|denied|keep planning/.test(resp)) this.core.lifecycle.designApproved(id, 'you');
        }
        if (p.tool_name === 'Artifact' && row.stage === 'design' && !row.plan_mode) await this.core.lifecycle.captureArtifact(id, p);
        if (row.status !== 'working') this.setStatus(id, 'working');
        // Keep usage live during long turns without reading the transcript on every tool call.
        if (Date.now() - (this.usageReadAt.get(id) ?? 0) > 10_000) this.ingestUsage(id);
        break;
      }
      case 'PermissionRequest': {
        // The plan dialog is also a permission request; keep the clearer wording.
        if (p.tool_name === 'ExitPlanMode') break;
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
        void this.captureTranscript(id);
        if (this.planPending.has(id)) break;
        this.update(id, { interrupted: 0 });
        // A Design turn that wrote or republished the design leaves it waiting for approval.
        if (row.stage === 'design' && !row.plan_mode && (await this.core.lifecycle.collectDesign(this.row(id)))) {
          this.core.lifecycle.designReady(id);
        } else {
          this.setStatus(id, 'idle');
        }
        this.refreshDiff(id).catch(() => undefined);
        this.ingestUsage(id);
        await this.core.lifecycle.onTurnEnd(id);
        break;
      }
      case 'SessionEnd':
        this.ingestUsage(id);
        // /clear ends one conversation and starts the next (SessionStart follows); Claude keeps running.
        if (p.reason === 'clear') break;
        this.clearStartup(id);
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
    for (const r of sessionRows(this.db)) {
      if (r.desired !== 'running') continue;
      this.ingestUsage(r.id);
      void this.captureTranscript(r.id); // catches up on turns from before this server version
    }
    this.backfillUsage();
    let panes: Map<string, PaneInfo>;
    try {
      await this.core.tmux.ensureServer();
      panes = await this.core.tmux.panes();
    } catch (e) {
      // Without a reliable pane list, relaunching could start Claude twice; reconcile tries again.
      this.core.log.error('recovery skipped: tmux is not answering', e);
      return;
    }
    for (const row of sessionRows(this.db)) {
      if (!this.recoverable(row) || !projectRow(this.db, row.project_id)) continue;
      const pane = panes.get(row.tmux_name);
      if (pane && !pane.dead) continue; // server restart; Claude kept running
      if (pane && pane.dead) {
        if (row.status !== 'exited') this.setStatus(row.id, 'exited', 'Claude exited while the server was down');
        continue;
      }
      await this.tryRecover(row);
    }
  }

  /** Sessions recovery and reconcile may touch: running, and not being created, launched or stopped. */
  private recoverable(row: SessionRow): boolean {
    return row.desired === 'running' && !this.creating.has(row.id) && !this.launching.has(row.id) && !this.stopping.has(row.id);
  }

  /** Recovers a session whose pane is gone; after a failure, waits a while before trying again. */
  private async tryRecover(row: SessionRow): Promise<void> {
    if (Date.now() < (this.recoverRetryAt.get(row.id) ?? 0)) return;
    try {
      await this.recoverOne(row);
    } catch (e) {
      this.core.log.error(`recovery failed for ${row.id}`, e);
      if (e instanceof HttpError && e.status === 409) return; // archived, or started some other way meanwhile
      // A deleted branch won't come back by itself; anything else (a fetch, a lock) may.
      const gone = e instanceof BranchGoneError;
      this.recoverRetryAt.set(row.id, gone ? Infinity : Date.now() + RECOVERY_BACKOFF_MS);
      const now = sessionRow(this.db, row.id);
      if (now?.desired !== 'running') return;
      if (now.status === 'working') this.update(row.id, { interrupted: 1 });
      this.setStatus(row.id, 'exited', gone ? errMsg(e) : `Couldn't restart Claude: ${errMsg(e)}. AgentK Local tries again in 5 minutes, or Resume it now.`);
    }
  }

  /** Periodic check: dead panes become `exited`; missing panes (tmux died) are recovered. */
  async reconcile(): Promise<void> {
    if (!(await this.core.tmux.serverRunning())) {
      this.core.log.warn('tmux server is gone; recovering sessions');
      await this.recover();
      return;
    }
    // Throws when tmux fails for any reason but "no server": this tick is skipped rather than
    // every session looking lost.
    const panes = await this.core.tmux.panes();
    for (const row of sessionRows(this.db)) {
      if (!this.recoverable(row)) continue;
      const pane = panes.get(row.tmux_name);
      const trustShown = row.status === 'needs_input' && /trust this folder/.test(row.status_detail ?? '');
      if (!pane) {
        await this.tryRecover(row);
      } else if (!pane.dead && (row.status === 'starting' || this.awaitingStart.has(row.id)) && !trustShown && Date.now() - (row.last_event_at ?? 0) > 4000) {
        // Claude fires no hooks until the folder-trust dialog is answered (new, resumed or recovered).
        const screen = await this.core.tmux.capture(row.tmux_name).catch(() => '');
        if (TRUST_PROMPT_RE.test(screen)) {
          const repo = projectRow(this.db, row.project_id)?.repo_path;
          if (repo) await this.trustRepo(repo); // so the next session skips it
          this.setStatus(row.id, 'needs_input', 'Claude is asking whether you trust this folder. Answer in the terminal.');
          this.notifyUser(row.id, 'Needs you', 'Confirm folder trust in the terminal.');
        }
      } else if (pane.dead && row.status !== 'exited') {
        this.startup.delete(row.id);
        this.awaitingStart.delete(row.id);
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
  recoverOne(row: SessionRow): Promise<void> {
    return this.starting(row.id, () => this.recoverNow(row));
  }

  private async recoverNow(row: SessionRow): Promise<void> {
    const p = projectRow(this.db, row.project_id);
    if (!p) return;
    await restoreWorktree(p.repo_path, row.worktree_path, row.branch);
    const now = Date.now();
    const history = (JSON.parse(row.auto_continue_at) as number[]).filter((t) => now - t < HOUR);
    const wasWorking = row.status === 'working' || row.interrupted === 1;
    const autoContinue = parseSettings(p.settings_json).autoContinue;
    const tooSoon = history.some((t) => now - t < QUARTER_HOUR);
    const parked = wasWorking && autoContinue && history.length >= 2;
    const cont = wasWorking && autoContinue && !tooSoon && !parked;
    const resume = this.transcriptExists(row);

    let prompt: string | null = null;
    if (!resume) prompt = row.initial_prompt ? this.prompt(row, this.firstPrompt(row)) : null;
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
      case 'discard': await this.archive(row, { keepBranch: false, force: true }); break;
      default: throw new HttpError(400, `Unknown action ${String(action)}.`);
    }
    return this.get(id);
  }

  private resume(row: SessionRow): Promise<void> {
    return this.starting(row.id, () => this.resumeNow(row));
  }

  private async resumeNow(row: SessionRow): Promise<void> {
    // Claude's SessionEnd hook fires just before its process ends, so give a pane
    // that is still shutting down a moment before calling it "already running".
    let pane = (await this.core.tmux.panes()).get(row.tmux_name);
    for (let i = 0; pane && !pane.dead && row.status === 'exited' && i < 30; i++) {
      await sleep(100);
      pane = (await this.core.tmux.panes()).get(row.tmux_name);
    }
    if (pane && !pane.dead) throw new HttpError(409, 'Claude is already running in this session.');
    const p = projectRow(this.db, row.project_id);
    if (p) {
      await restoreWorktree(p.repo_path, row.worktree_path, row.branch).catch((e: unknown) => {
        throw e instanceof BranchGoneError ? new HttpError(409, e.message) : e;
      });
    }
    const resume = this.transcriptExists(row);
    const firstPrompt = resume ? null : row.initial_prompt ? this.prompt(row, this.firstPrompt(row)) : null;
    this.setStatus(row.id, 'starting', 'Resuming');
    this.startup.delete(row.id);
    this.setStartup(row.id, 'launch', { resumed: resume, hasPrompt: !!firstPrompt });
    await this.launch(row, { resume, prompt: firstPrompt });
    this.setStartup(row.id, 'loading');
    this.event(row.id, 'resumed', 'Resumed');
  }

  /** What Discard would throw away: uncommitted files and commits no remote has. */
  /** Every file changed on the session's branch vs. its base, committed or not. */
  async changes(id: string): Promise<SessionChanges> {
    const row = this.row(id);
    if (!existsSync(row.worktree_path)) throw new HttpError(409, 'This session\'s worktree is gone, so there are no changes to show.');
    const c = await changes(row.worktree_path, row.base_branch);
    if (!c) throw new HttpError(409, `Couldn't find where ${row.branch} branched off ${row.base_branch}.`);
    return c;
  }

  /** One side of a changed image, for the Changes panel's preview. */
  async changedImage(id: string, path: string, side: ChangeSide): Promise<{ type: string; data: Buffer }> {
    const row = this.row(id);
    if (!existsSync(row.worktree_path)) throw new HttpError(409, 'This session\'s worktree is gone, so there are no changes to show.');
    let img: { type: string; data: Buffer } | null;
    try {
      img = await changedImage(row.worktree_path, row.base_branch, path, side);
    } catch {
      throw new HttpError(400, 'Only image files inside this session\'s repo can be previewed.');
    }
    if (!img) throw new HttpError(404, side === 'old' ? 'This image isn\'t in the base branch.' : 'This image isn\'t in the worktree, or it is too large to preview.');
    return img;
  }

  async discardCheck(id: string): Promise<DiscardCheck> {
    const row = this.row(id);
    const p = projectRow(this.db, row.project_id);
    const [uncommittedFiles, unpushedCommits] = await Promise.all([
      uncommittedCount(row.worktree_path),
      p ? unpushedCount(p.repo_path, row.branch, row.base_branch) : 0,
    ]);
    return { worktreeExists: existsSync(row.worktree_path), uncommittedFiles, unpushedCommits };
  }

  /**
   * Stops Claude and removes the worktree if it's clean. The branch is kept unless asked.
   * With force (Discard), the worktree is removed even with uncommitted changes.
   */
  async archive(row: SessionRow, opts: { keepBranch: boolean; force?: boolean; stage?: Stage }): Promise<{ worktreeRemoved: boolean }> {
    // A session still starting would bring its worktree back, or start Claude after the kill.
    await this.settle(row.id);
    row = this.row(row.id);
    // Archiving again (a cleanup finishing after the user archived by hand) keeps the first time.
    const wasArchived = row.desired === 'archived';
    this.stopping.add(row.id);
    try {
      this.ingestUsage(row.id);
      run(this.db, `UPDATE sessions SET desired = 'archived', status = 'archived', archived_at = COALESCE(?, archived_at) WHERE id = ?`,
        wasArchived ? null : Date.now(), row.id);
      await within(this.captureTranscript(row.id), FINAL_CAPTURE_MS);
      await this.core.tmux.kill(row.tmux_name);
      const p = projectRow(this.db, row.project_id);
      let worktreeRemoved = false;
      if (p) {
        if (opts.force || !existsSync(row.worktree_path) || (await worktreeClean(row.worktree_path))) {
          worktreeRemoved = await removeWorktree(p.repo_path, row.worktree_path, opts.force);
        }
        if (worktreeRemoved && !opts.keepBranch) await tryGit(p.repo_path, 'branch', '-D', row.branch);
      }
      rmSync(join(this.core.paths.run, `${row.id}.sh`), { force: true });
      this.busy.delete(row.id);
      this.planPending.delete(row.id);
      this.awaitingStart.delete(row.id);
      this.recoverRetryAt.delete(row.id);
      this.mergeInfo.delete(row.id);
      this.diffs.delete(row.id);
      if (opts.force) {
        this.event(row.id, 'discarded', worktreeRemoved
          ? `Discarded; worktree and branch ${row.branch} deleted`
          : `Discarded, but the worktree at ${row.worktree_path} could not be removed, so branch ${row.branch} was kept`);
      } else if (!wasArchived) {
        this.event(row.id, 'archived', worktreeRemoved ? 'Archived; worktree removed' : 'Archived; worktree kept because it has uncommitted changes');
      }
      this.core.changed();
      return { worktreeRemoved };
    } finally {
      this.stopping.delete(row.id);
    }
  }

  /**
   * Used when a project is deleted: stop everything, remove clean worktrees, keep branches.
   * A worktree with uncommitted changes is kept, like Archive does. Returns the kept paths.
   */
  async teardownProject(projectId: string): Promise<string[]> {
    const p = projectRow(this.db, projectId);
    for (const row of sessionRows(this.db, projectId)) if (row.desired === 'running') await this.settle(row.id);
    const kept: string[] = [];
    for (const row of sessionRows(this.db, projectId)) {
      if (row.desired === 'running') {
        this.stopping.add(row.id);
        run(this.db, `UPDATE sessions SET desired = 'archived', status = 'archived', archived_at = ? WHERE id = ?`, Date.now(), row.id);
        await within(this.captureTranscript(row.id), FINAL_CAPTURE_MS);
        await this.core.tmux.kill(row.tmux_name);
        this.stopping.delete(row.id);
        this.awaitingStart.delete(row.id);
        this.recoverRetryAt.delete(row.id);
      }
      if (p && !(await removeWorktree(p.repo_path, row.worktree_path))) {
        kept.push(row.worktree_path);
        this.core.log.info(`kept the worktree of ${row.id} at ${row.worktree_path}: it has uncommitted changes`);
      }
      rmSync(join(this.core.paths.run, `${row.id}.sh`), { force: true });
    }
    return kept;
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
      if (r.desired !== 'running') continue;
      await this.refreshDiff(r.id).catch(() => undefined);
      this.ingestUsage(r.id);
    }
  }

  // ---- Meko conversation ----

  /** Sends the session's new turns to its Meko conversation. Never throws. */
  captureTranscript(id: string): Promise<void> {
    const r = sessionRow(this.db, id);
    if (!r || !this.core.mekoStatus.configured) return Promise.resolve();
    return this.core.capture.capture(id, sessionTranscript(r, transcriptPathFor));
  }

  /**
   * Creates the session's Meko conversation, titled after the session, before
   * Claude starts. Meko lists a conversation under the title of its first
   * conversation_create and ignores later creates and conversation_update, so
   * this has to come before the plugin's SessionStart hook, which titles every
   * conversation "Claude Code session (auto-captured)". Both key it by the
   * Claude session id, so the hook's create lands on this conversation. Never throws.
   */
  private async nameConversation(r: SessionRow): Promise<void> {
    const dp = dedicatedDatapack(this.db, r.project_id);
    if (!dp || !this.core.capture.enabled || !this.core.mekoStatus.configured) return;
    const title = `${r.name} (${projectRow(this.db, r.project_id)?.name ?? r.project_id})`;
    try {
      await this.core.meko.ensureConversation(projectAgentId(r.project_id), dp.id, r.claude_session_id, title);
    } catch (e) {
      this.core.log.warn(`naming the Meko conversation for ${r.id} failed`, errMsg(e));
    }
  }

  // ---- token usage ----

  /** Reads any new transcript lines for a session and updates its token totals. */
  ingestUsage(id: string): void {
    this.loadUsage();
    const r = sessionRow(this.db, id);
    if (!r) return;
    this.usageReadAt.set(id, Date.now());
    try {
      if (!ingestSessionUsage(this.db, id, sessionTranscript(r, transcriptPathFor), r.base_branch)) return;
    } catch (e) {
      this.core.log.warn(`reading token usage for ${id} failed`, errMsg(e));
      return;
    }
    this.usage.set(id, sessionUsage(this.db, id));
    this.core.changed();
    void this.countPushedCommits(r);
  }

  /** Fills in how many commits each newly found push carried; -1 when git can't tell. */
  private async countPushedCommits(r: SessionRow): Promise<void> {
    const pending = all<{ range: string }>(this.db, 'SELECT range FROM pushes WHERE session_id = ? AND commits IS NULL', r.id);
    if (!pending.length) return;
    // Deleted projects keep their row, and their repo usually still has the commits.
    const repo = get<{ repo_path: string }>(this.db, 'SELECT repo_path FROM projects WHERE id = ?', r.project_id)?.repo_path;
    for (const { range } of pending) {
      let n = -1;
      if (repo) {
        const out = await git(repo, 'rev-list', '--count', range).catch(() => '');
        if (/^\d+$/.test(out)) n = Number(out);
      }
      run(this.db, 'UPDATE pushes SET commits = ? WHERE session_id = ? AND range = ? AND commits IS NULL', n, r.id, range);
    }
  }

  /**
   * Reads the transcripts of sessions no one has read yet, such as archived ones after a migration
   * cleared the read offsets, one session per tick so the server stays responsive.
   */
  private backfillUsage(): void {
    const ids = all<{ id: string }>(this.db,
      'SELECT id FROM sessions s WHERE NOT EXISTS (SELECT 1 FROM usage_files f WHERE f.session_id = s.id)').map((x) => x.id);
    const next = () => {
      const id = ids.shift();
      if (!id) return;
      this.ingestUsage(id);
      setTimeout(next, 0);
    };
    next();
  }

  private loadUsage(): void {
    if (this.usageLoaded) return;
    this.usageLoaded = true;
    this.usage = usageBySession(this.db);
  }
}
