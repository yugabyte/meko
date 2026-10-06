import { randomBytes, randomUUID } from 'node:crypto';
import { existsSync, mkdirSync, realpathSync, rmSync, statSync } from 'node:fs';
import { basename, join, resolve, sep } from 'node:path';
import type { ChangeSide, CreateSessionRequest, DatapackRef, DiscardCheck, FailedStart, MoveSessionRequest, PastedImage, Session, SessionChanges, SessionAction, SessionKind, SessionStatus, Stage, StartStage, StartupState, StartupStep, TokenUsage } from '../shared/types.js';
import { START_STAGES, STAGE_LABELS } from '../shared/types.js';
import type { Core } from './core.js';
import { all, get, run, tx } from './db.js';
import { changedImage, changes, diffStat } from './changes.js';
import {
  BranchGoneError, addReviewWorktree, addWorktree, baseExists, checkedOutAt, commitsBeyond, defaultBranch, discardChanges, ensureFirstCommit, existingBranch, fetchPrHead, git,
  gitRaw, isIdleWorktree, prRef, remoteFor, removeWorktree, restoreWorktree, reuseWorktree, tryGit, uncommittedCount, unpushedCount,
  worktreeClean, type SyncResult,
} from './git.js';
import type { GuardDecision, HookContext, HookPayload, ModReply } from './hooks.js';
import { isRepoName, issueText, prInfo, type PrInfo } from './github.js';
import { HOOK_WAIT_MS, RecallStore, SEARCH_TIMEOUT_MS, recordPreload, runRecall, withoutMekoLines } from './recall.js';
import { countFileTypes, parseNumstatZ } from './filetypes.js';
import { errMsg } from './log.js';
import { launchedAgentId, sessionAgentId } from './meko.js';
import { designQuestionsLine, loadPrompt, mekoQuestionLine, mekoQuestionSystemLine, mekoRecallLine, mekoRecallSystemLine, render, type PromptName } from './prompts.js';
import { HttpError } from './projects.js';
import { removePastes, savePastedImage } from './pastes.js';
import { kindOf } from '../shared/stages.js';
import {
  EDIT_TOOLS, modDirPath, shq, statusLineScriptPath, transcriptPathFor, userStatusLine, writeLauncher,
} from './claude.js';
import { ModLinks, type DroppedPrompt } from './modlink.js';
import {
  STORED_TASK_KIND, dedicatedDatapack, parseSettings, projectDatapacks, projectRow, sessionRepo, sessionRow, sessionRows, slugify, toSession,
  type SessionRow, type SessionRuntime,
} from './store.js';
import { sleep, tmuxName, type PaneInfo } from './tmux.js';
import { ZERO_USAGE, ingestSessionUsage, sessionTranscript, sessionUsage, usageBySession } from './usage.js';
import { TRUST_PROMPT_RE, ensureWorkspaceTrust } from './trust.js';

/**
 * What the appended system prompt (and a moved session's prompt) says about the session's workflow:
 * a task's stages, with what Explore means while it's there, or what a Review a PR is.
 */
export function workflowLine(kind: SessionKind, stage: Stage): string {
  if (kind === 'review-pr') {
    return "This is a Review a PR session: the user is reviewing someone else's pull request and you assist them. Don't review it on your own; answer what they ask. Don't push to the PR's branch or post on GitHub unless they ask. The user ends the session from the AgentK Local UI.";
  }
  const stages = 'Stages: Explore → Design → Code → PR & Review → Merge & Close; the user moves the session between stages, and approves the design, opening the PR, and merging, from the AgentK Local UI.';
  if (stage !== 'explore') return stages;
  return `${stages} It's in Explore now: investigate, answer questions, build POCs and run experiments in this worktree, and push experimental branches when useful. AgentK Local runs no design approval, PR, review or merge until the user moves it on.`;
}

/** The old session types a create request may still name, and where each starts. */
const LEGACY_STARTS: Partial<Record<string, StartStage>> = { explore: 'explore', plan: 'design', quick: 'code' };

/** The PR a new Review a PR session checks out: its repo on GitHub, what GitHub says about it, and the clone's remote it's fetched from. */
interface ReviewTarget {
  repo: string;
  info: PrInfo;
  remote: string;
}

/** Longest setInterval delay Node honours, in whole seconds (2^31-1 ms). */
export const MAX_TIMER_SECS = Math.floor((2 ** 31 - 1) / 1000);
const QUARTER_HOUR = 15 * 60 * 1000;
const HOUR = 60 * 60 * 1000;
/**
 * A starting session this long past its last startup step, with a transcript written since, has
 * missed its hooks (reconcile's followTranscript).
 */
export const MISSED_START_MS = 20_000;
/** A transcript written this recently means the turn is still going. */
const TRANSCRIPT_ACTIVE_MS = 15_000;
/** How long stopping a session waits for its last turns to reach Meko. */
const FINAL_CAPTURE_MS = 15000;
/** How long a new session waits for its Meko conversation to be created before Claude starts anyway. */
const NAME_CONVERSATION_MS = 5000;
/** How many failed starts are kept for Try again. */
const MAX_FAILED_STARTS = 20;
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
function startedText(branch: string, base: string, sync: SyncResult | null, reused: boolean): string {
  if (reused) return `Started on the existing branch ${branch} (base ${base})`;
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

/**
 * What happened to a session's worktree when it ended: idle (kept for the next session), removed,
 * earlier (let go of before), uncommitted (kept, still the session's), or stuck (git refused).
 */
export type WorktreeFate = 'idle' | 'removed' | 'earlier' | 'uncommitted' | 'stuck';
/** Fates after which the session's branch is no longer checked out in its worktree. */
const FREED: WorktreeFate[] = ['idle', 'removed', 'earlier'];

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
  /**
   * Sessions that started without their hooks reaching the server (followTranscript): until a hook
   * arrives, reconcile sets working or idle from the transcript.
   */
  private hookless = new Set<string>();
  /** New sessions that couldn't start, oldest first (see `failStart`). */
  private failed = new Map<string, FailedStart>();
  /** Each new session's Meko search on its task, until its SessionStart hook takes what it found. */
  private recall = new RecallStore((id, r) => recordPreload(this.db, id, r));
  /** Each session's link to AgentK's mod in its Claude (modlink.ts): prompts go through it while it's live. */
  readonly mod = new ModLinks((id, dropped) => this.promptDropped(id, dropped));
  /** How full each session's context window is (percent), from the mod's measurements. */
  private contextFill = new Map<string, number>();

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
      contextPercent: this.contextFill.get(id) ?? null,
    };
  }

  listForProject(projectId: string): Session[] {
    this.loadUsage();
    return sessionRows(this.db, projectId)
      .filter((r) => r.desired === 'running')
      .map((r) => toSession(this.db, r, this.runtime(r.id), this.core.questions.get(r).open));
  }

  get(id: string): Session {
    const row = this.row(id);
    return toSession(this.db, row, this.runtime(id), this.core.questions.get(row).open);
  }

  row(id: string): SessionRow {
    const r = sessionRow(this.db, id);
    if (!r) throw new HttpError(404, 'That session no longer exists.');
    return r;
  }

  /** Saves an image pasted into the session's terminal, for the browser to paste its path. */
  pasteImage(id: string, data: unknown): PastedImage {
    const row = this.row(id);
    if (row.desired !== 'running') throw new HttpError(409, 'This session is archived.');
    return { path: savePastedImage(this.core.paths.pastes, row.id, data) };
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

  setStage(id: string, stage: Stage, by: 'you' | 'auto', opts: { event?: boolean } = {}): void {
    const now = Date.now();
    tx(this.db, () => {
      run(this.db, 'UPDATE session_stages SET left_at = ? WHERE session_id = ? AND left_at IS NULL', now, id);
      run(this.db, 'INSERT INTO session_stages(session_id, stage, entered_at, moved_by) VALUES (?, ?, ?, ?)', id, stage, now, by);
      run(this.db, 'UPDATE sessions SET stage = ? WHERE id = ?', stage, id);
    });
    if (opts.event !== false) this.event(id, 'stage', stage === 'done' ? 'Done' : `Moved to ${STAGE_LABELS[stage]}`);
    this.core.changed();
  }

  notifyUser(id: string, title: string, body: string): void {
    const r = sessionRow(this.db, id);
    const p = r && projectRow(this.db, r.project_id);
    if (!r || !p || !parseSettings(p.settings_json).notify) return;
    this.core.notify({ projectId: r.project_id, sessionId: id, title: `${r.name}: ${title}`, body });
  }

  // ---- create / launch ----

  /** Creates a session and waits for Claude to be launched in it (see `begin`). */
  async create(projectId: string, req: CreateSessionRequest): Promise<Session> {
    return (await this.begin(projectId, req)).done;
  }

  /**
   * Starts a new session in two parts. What's quick to check is checked first, so a bad request
   * starts nothing; then the row goes in and this returns `session`, still starting, so the New
   * session dialog can close and the session's tab show its startup steps. `done` does the slow
   * part (on a big repo, minutes): fetching a review's PR head, making the worktree and launching
   * Claude. When the worktree or the PR head can't be had there, the session is removed and kept
   * as a failed start (`failedStarts()`) for the user to try again.
   */
  async begin(projectId: string, req: CreateSessionRequest): Promise<{ session: Session; done: Promise<Session> }> {
    const p = projectRow(this.db, projectId);
    if (!p) throw new HttpError(404, 'That project no longer exists.');
    for (const k of ['name', 'prompt', 'baseBranch', 'repoPath', 'branch'] as const) {
      if (req[k] != null && typeof req[k] !== 'string') throw new HttpError(400, `The session's ${k} must be text.`);
    }
    const settings = parseSettings(p.settings_json);
    const asked = req.kind ?? 'task';
    const legacy = typeof asked === 'string' && Object.hasOwn(LEGACY_STARTS, asked) ? LEGACY_STARTS[asked] : undefined;
    if (asked !== 'task' && asked !== 'review-pr' && !legacy) throw new HttpError(400, 'Pick a session kind: task or review-pr.');
    const kind: SessionKind = asked === 'review-pr' ? 'review-pr' : 'task';
    // A review starts in Reviewing PR, so its `start` is ignored rather than checked.
    if (kind === 'task' && req.start != null && !START_STAGES.includes(req.start)) {
      throw new HttpError(400, `Pick where the session starts: ${START_STAGES.join(', ')}.`);
    }
    const start: StartStage = req.start ?? legacy ?? (req.skipDesign ? 'code' : 'design');
    const repo = this.core.projects.sessionRepo(p, req.repoPath);
    // A review must be of an open PR on GitHub; its head is fetched with the worktree.
    const review = kind === 'review-pr' ? await this.reviewTarget(repo, req.pr) : null;
    const name = (req.name ?? '').trim() || (review ? `Review #${review.info.number} ${review.info.title}`.slice(0, 100).trim() : '');
    if (!name) throw new HttpError(400, 'Give the session a name.');
    const notes = review ? (req.prompt ?? '').trim() : '';
    const stage: Stage = review ? 'reviewing-pr' : start;
    // A task with no first prompt works on its name, unless it starts at Explore, where it waits
    // for you. A review's task is the PR (the user's notes are kept apart, for its prompt).
    const task = review
      ? `Review PR #${review.info.number} of ${review.repo}: ${review.info.title}`
      : (req.prompt ?? '').trim() || (stage === 'explore' ? '' : name);
    // A repo with no commits yet (one opened before AgentK gave it a first commit) gets one now.
    if (!review) {
      try {
        await ensureFirstCommit(repo);
      } catch (e) {
        throw new HttpError(400, errMsg(e));
      }
    }
    const wanted = review ? '' : (req.branch ?? '').trim();
    const where = wanted ? await this.checkWantedBranch(repo, wanted) : null;
    const base = review?.info.baseRefName || (req.baseBranch ?? '').trim() || (repo === p.repo_path ? p.default_branch : await defaultBranch(repo));
    // Checked now: once the dialog has closed, a typo would only show up as a failed start. A
    // review's base comes from GitHub and is fetched with the PR.
    if (!review) {
      let found: boolean;
      try {
        found = await baseExists(repo, base);
      } catch (e) {
        throw new HttpError(400, errMsg(e));
      }
      if (!found) throw new HttpError(400, `The base branch ${base} isn't in ${basename(repo)} or on its origin. Pick another base branch.`);
    }
    // What Try again fills the New session dialog with, should the start fail.
    const request: CreateSessionRequest = {
      name, prompt: (req.prompt ?? '').trim() || undefined, kind, start: review ? undefined : start,
      baseBranch: review ? undefined : base, repoPath: req.repoPath, branch: wanted || undefined, pr: review ? { repo: review.repo, number: review.info.number } : undefined,
    };

    const slug = await this.uniqueSlug(projectId, repo, wanted ? null : settings.branchPrefix, review ? `review-pr-${review.info.number}` : slugify(name));
    const branch = wanted || `${settings.branchPrefix}${slug}`;
    const id = `${projectId}--${slug}`;
    const now = Date.now();
    const pool = this.core.worktrees;
    // GitHub's head until the fetch says otherwise (the PR may move in between).
    const sha = review?.info.headRefOid || null;
    // The row goes in first so the tab (and its "Starting…" view) shows up right away. The
    // repo's most recently idled worktree is claimed in the same write, so two sessions starting
    // at once (in any projects on the repo) never get the same one, and two starts on one typed
    // branch can't both pass the in-use check.
    const { worktree, reused } = tx(this.db, () => {
      if (wanted) this.checkBranchFree(repo, wanted);
      const claimed = pool.claim(repo, id);
      const path = claimed ?? pool.add(repo, id);
      run(this.db, `INSERT INTO sessions(id, project_id, slug, name, claude_session_id, branch, base_branch, repo_path, branch_reused,
                      worktree_path, tmux_name, hook_token, desired, status, kind, stage, initial_prompt, created_at, last_event_at, plan_mode, meko_agent_id,
                      pr_number, pr_url, pr_repo, pr_title, pr_author, review_sha, pr_head_sha, pr_new_commits, pr_state, pr_remote, review_notes)
                    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'running', 'starting', ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
        id, projectId, slug, name, randomUUID(), branch, base, repo, where ? 1 : 0, path, tmuxName(projectId, slug),
        randomBytes(24).toString('base64url'), review ? 'review-pr' : STORED_TASK_KIND, stage, task || null, now, now, sessionAgentId({ id, slug }),
        review?.info.number ?? null, review?.info.url ?? null, review?.repo ?? null, review?.info.title ?? null, review?.info.author ?? null,
        sha, sha, review ? 0 : null, review ? 'OPEN' : null, review?.remote ?? null, notes || null);
      run(this.db, `INSERT INTO session_stages(session_id, stage, entered_at, moved_by) VALUES (?, ?, ?, 'you')`, id, stage, now);
      return { worktree: path, reused: !!claimed };
    });
    // Until Claude is running, the row has no pane; reconcile must not mistake it for a lost session.
    this.creating.add(id);
    // Starting again under the same name replaces the failed start.
    this.failed.delete(id);
    this.setStartup(id, 'worktree', { resumed: false, hasPrompt: !!task });
    const done = this.finishCreate(id, { request, repo, worktree, reused, branch, base, where, review, task, notes })
      .finally(() => this.creating.delete(id));
    return { session: this.get(id), done };
  }

  /** The slow part of `begin`: the PR head for a review, the worktree, then Claude. */
  private async finishCreate(id: string, c: {
    request: CreateSessionRequest; repo: string; worktree: string; reused: boolean; branch: string; base: string;
    where: 'local' | 'origin' | null; review: ReviewTarget | null; task: string; notes: string;
  }): Promise<Session> {
    const { repo, review, branch, base, task } = c;
    // A review searches on the PR: its title and description (where the author's design is
    // summed up), and the user's notes.
    this.startRecall(this.row(id), review ? [review.info.title, withoutMekoLines(review.info.body), c.notes].filter(Boolean).join('\n') : task);
    const naming = this.nameConversation(this.row(id));
    let sha: string | undefined;
    if (review) {
      try {
        sha = await fetchPrHead(repo, review.remote, review.info.number, review.info.baseRefName);
      } catch (e) {
        const pool = this.core.worktrees;
        throw this.failStart(id, c.request, `Couldn't fetch PR #${review.info.number} from ${review.remote}: ${errMsg(e)}`,
          () => (c.reused ? pool.unclaim(c.worktree, id) : pool.drop(c.worktree)));
      }
      if (sha !== review.info.headRefOid) this.update(id, { review_sha: sha, pr_head_sha: sha, pr_new_commits: 0 });
    }
    const made = await this.makeWorktree(id, c.request, repo, c.worktree, c.reused, branch, base, !!c.where, sha);
    const started = review && sha
      ? `Checked out PR #${review.info.number} of ${review.repo} at ${sha.slice(0, 7)} on ${branch} (base ${base})`
      : startedText(branch, base, made.sync, !!c.where);
    this.event(id, 'created', started + (made.reused ? ` in a reused worktree (${basename(made.path)})` : ''), { reused: made.reused });

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
  }

  /**
   * Puts the new session's branch in its worktree: the idle one it claimed (build outputs kept)
   * or a new one. A claimed worktree that can't be reused is dropped from the pool and the
   * session gets a new one, so reuse never makes a start fail. On failure the session is removed
   * and kept as a failed start.
   */
  private async makeWorktree(
    id: string, request: CreateSessionRequest, repo: string, path: string, claimed: boolean, branch: string, base: string, fromOrigin: boolean, at?: string,
  ): Promise<{ sync: SyncResult | null; reused: boolean; path: string }> {
    const pool = this.core.worktrees;
    let current = path;
    // A review's branch starts at the PR's head; any other at the base (or is the existing branch).
    const add = async (wt: string, reuse: boolean): Promise<SyncResult | null> => {
      if (at) {
        await addReviewWorktree(repo, wt, branch, at, reuse);
        return null;
      }
      return reuse ? reuseWorktree(repo, wt, branch, base, fromOrigin) : addWorktree(repo, wt, branch, base, fromOrigin);
    };
    try {
      if (claimed) {
        try {
          return { sync: await add(path, true), reused: true, path };
        } catch (e) {
          if (await isIdleWorktree(repo, path)) {
            // The worktree is fine; what failed was the branch or base. It goes back to the pool,
            // and a new worktree reports the error if it's the same.
            pool.unclaim(path, id);
          } else {
            this.core.log.warn(`couldn't reuse the worktree at ${path} for ${id}; creating a new one`, errMsg(e));
            await pool.discard(path, id);
          }
          current = pool.add(repo, id);
          this.update(id, { worktree_path: current });
        }
      }
      return { sync: await add(current, false), reused: false, path: current };
    } catch (e) {
      throw this.failStart(id, request, `Couldn't create the worktree: ${errMsg(e)}`, () => pool.drop(current));
    }
  }

  /**
   * A new session whose worktree (or PR head) couldn't be had: it's removed, <release> frees its
   * worktree, and it's kept as a failed start, with a notification, since the New session dialog
   * has closed by now. Returns the error for the caller to throw.
   */
  private failStart(id: string, request: CreateSessionRequest, message: string, release: () => void): HttpError {
    const row = sessionRow(this.db, id);
    if (row) {
      this.notifyUser(id, "Couldn't start", message);
      this.failed.set(id, { id, projectId: row.project_id, name: row.name, error: message, at: Date.now(), request });
      // The oldest go first; nobody needs dozens.
      for (const old of [...this.failed.keys()].slice(0, Math.max(0, this.failed.size - MAX_FAILED_STARTS))) this.failed.delete(old);
    }
    this.startup.delete(id);
    this.recall.drop(id);
    release();
    run(this.db, 'DELETE FROM sessions WHERE id = ?', id);
    this.core.changed();
    return new HttpError(400, message);
  }

  /** New sessions that couldn't start, newest first, until dismissed. */
  failedStarts(): FailedStart[] {
    return [...this.failed.values()].reverse();
  }

  /** The session is being created (row written, Claude not launched yet): its worktree may not exist yet. */
  isCreating(id: string): boolean {
    return this.creating.has(id);
  }

  dismissFailedStart(id: string): void {
    if (this.failed.delete(id)) this.core.changed();
  }

  /**
   * A slug no session of the project has. With a <prefix> (AgentK names the branch), the branch
   * `<prefix><slug>` mustn't exist in <repo> either.
   */
  private async uniqueSlug(projectId: string, repo: string, prefix: string | null, base: string): Promise<string> {
    for (let i = 1; ; i++) {
      const slug = i === 1 ? base : `${base}-${i}`;
      const taken =
        get(this.db, 'SELECT 1 AS x FROM sessions WHERE (project_id = ? AND slug = ?) OR tmux_name = ?', projectId, slug, tmuxName(projectId, slug)) ||
        (prefix !== null && (await tryGit(repo, 'rev-parse', '--verify', '--quiet', `refs/heads/${prefix}${slug}`)));
      if (!taken) return slug;
    }
  }

  /**
   * Checks a branch the user typed: a valid name that no running session uses and no checkout
   * has. Returns where it already exists (local, or only on origin), or null for a new branch.
   */
  private async checkWantedBranch(repo: string, branch: string): Promise<'local' | 'origin' | null> {
    let where: 'local' | 'origin' | null;
    try {
      where = await existingBranch(repo, branch);
    } catch (e) {
      throw new HttpError(400, errMsg(e));
    }
    this.checkBranchFree(repo, branch);
    const at = where === 'local' ? await checkedOutAt(repo, branch) : null;
    if (at) {
      // git reports the real path; the stored one may go through a symlink.
      const kept = all<{ name: string; worktree_path: string }>(this.db,
        `SELECT name, worktree_path FROM sessions WHERE desired = 'archived' AND worktree_released = 0`)
        .find((s) => realpath(s.worktree_path) === realpath(at));
      throw new HttpError(409, kept
        ? `Branch ${branch} is still checked out at ${at}, the worktree kept for the archived session "${kept.name}" because it has uncommitted changes. Remove that worktree (git worktree remove ${at}), or pick another branch.`
        : `Branch ${branch} is checked out at ${at}. Switch that checkout to another branch, or pick another branch.`);
    }
    return where;
  }

  /** Throws a 409 when a running session (in any project) works on <branch> of <repo>. */
  private checkBranchFree(repo: string, branch: string): void {
    const s = get<{ name: string; project: string }>(this.db,
      `SELECT s.name, p.name AS project FROM sessions s JOIN projects p ON p.id = s.project_id
        WHERE s.desired = 'running' AND s.repo_path = ? AND s.branch = ?`, repo, branch);
    if (s) throw new HttpError(409, `Branch ${branch} is in use by the session "${s.name}" in ${s.project}. Archive that session first, or pick another branch.`);
  }

  /**
   * The PR a new Review a PR session is for: it must be open, and <repo> a clone of its repo (its
   * upstream, for a fork). Its head is fetched later, with the worktree (`finishCreate`).
   */
  private async reviewTarget(repo: string, pr: CreateSessionRequest['pr']): Promise<ReviewTarget> {
    if (!pr || typeof pr !== 'object' || !isRepoName(pr.repo) || !Number.isInteger(pr.number) || pr.number <= 0) {
      throw new HttpError(400, 'Pick the pull request to review.');
    }
    const remote = await remoteFor(repo, pr.repo);
    if (!remote) {
      throw new HttpError(400, `${basename(repo)} isn't a clone of ${pr.repo}, where PR #${pr.number} is. Pick a clone of ${pr.repo} under Advanced, or clone it first.`);
    }
    let info: PrInfo;
    try {
      info = await prInfo(repo, pr.repo, pr.number);
    } catch (e) {
      throw new HttpError(400, `Couldn't read PR #${pr.number} of ${pr.repo} from GitHub: ${errMsg(e)}`);
    }
    if (info.state !== 'OPEN') {
      throw new HttpError(400, `PR #${pr.number} of ${pr.repo} is ${info.state === 'MERGED' ? 'merged' : 'closed'}, so there's nothing to review.`);
    }
    return { repo: pr.repo, info, remote };
  }

  /** Where Claude writes a markdown design during the Design stage (outside the repo). */
  designFile(row: SessionRow): string {
    return join(this.core.paths.designs, row.id, 'design.md');
  }

  /** Where Claude lists the design's open questions, beside the markdown design. */
  questionsFile(row: SessionRow): string {
    return join(this.core.paths.designs, row.id, 'questions.json');
  }

  /** The prompt a session's task starts with, for the stage it's in. */
  firstPrompt(row: SessionRow): PromptName {
    if (row.stage === 'explore') return 'explore';
    if (row.stage === 'reviewing-pr') return 'review-pr';
    if (row.stage !== 'design') return 'code';
    return row.plan_mode ? 'design-plan' : 'design';
  }

  prompt(row: SessionRow, name: PromptName, extra: Record<string, string | number | null> = {}): string {
    const p = projectRow(this.db, row.project_id);
    const settings = p ? parseSettings(p.settings_json) : null;
    const meko = this.sessionDatapacks(row.project_id).length ? [mekoRecallLine(name), mekoQuestionLine(name)].filter(Boolean) : [];
    // A review names its PR's repo outright: the clone may be a fork, where gh's {owner}/{repo} is the
    // fork. Its base is the remote's branch the review fetched, since the local one may follow a
    // fork, lag behind, or not exist.
    const review = row.kind === 'review-pr'
      ? {
        repo: row.pr_repo ?? '{owner}/{repo}',
        base: row.pr_remote ? `${row.pr_remote}/${row.base_branch}` : row.base_branch,
        title: row.pr_title ?? '',
        author: row.pr_author ?? 'its author',
        sha: row.review_sha ?? '',
        notes: row.review_notes ? `The user's notes for you: ${row.review_notes}\n` : '',
      }
      : {};
    const text = render(loadPrompt(this.db, name, row.worktree_path), {
      task: row.initial_prompt ?? '',
      branch: row.branch,
      base: row.base_branch,
      pr: row.pr_number,
      prUrl: row.pr_url,
      reviewer: settings?.reviewerLogin ?? 'the reviewer',
      // A review-people prompt queued before it named its reviewers.
      reviewers: 'its reviewers',
      repo: '{owner}/{repo}',
      designFile: this.designFile(row),
      questionsFile: this.questionsFile(row),
      ...review,
      ...extra,
    });
    const lines = [...(name === 'design' ? [designQuestionsLine(this.questionsFile(row))] : []), ...meko];
    return lines.length ? `${text.trimEnd()}\n\n${lines.join('\n\n')}` : text;
  }

  /** The project's datapacks for a session to use: none while Meko is off, so nothing points it at Meko. */
  private sessionDatapacks(projectId: string) {
    return this.core.mekoStatus.configured ? projectDatapacks(this.db, projectId) : [];
  }

  private env(row: SessionRow, mod: boolean): Record<string, string> {
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
      MEKO_AGENT_ID: launchedAgentId(row),
      MEKO_DATAPACK_ID: dedicated?.id ?? '',
      MEKO_DATAPACK_IDS: dps.map((d) => d.id).join(','),
      // With capture off, the Meko plugin's checkpoint timer (started by its SessionStart hook on a
      // resumed session) mustn't send transcripts either. It runs setInterval(tick, seconds * 1000),
      // and Node turns a delay past 2^31-1 ms into 1 ms, so this is the longest safe interval.
      ...(this.core.capture.enabled ? {} : { MEKO_CHECKPOINT_INTERVAL: String(MAX_TIMER_SECS) }),
      // Plugins' hooks modules are early access in Claude Code, off unless this says otherwise.
      ...(mod ? { CLAUDE_CODE_ENABLE_FUNCTION_HOOKS: '1' } : {}),
    };
  }

  /**
   * The folder of AgentK's mod for a launch (`--plugin-dir`), or null when it's off: turned off in
   * the config, its files missing, or the Claude Code too old (or its version unreadable).
   */
  async modDir(): Promise<string | null> {
    const dir = modDirPath(this.core.paths);
    if (!this.core.config.claude.mod || !existsSync(join(dir, 'hooks', 'register.ts'))) return null;
    return (await this.core.claudeCode.supportsMod()) ? dir : null;
  }


  private systemPrompt(row: SessionRow): string {
    const p = projectRow(this.db, row.project_id);
    const dps = this.sessionDatapacks(row.project_id);
    const dedicated = dps.find((d) => d.role === 'dedicated');
    const shared = dps.filter((d) => d.role === 'shared');
    const lines = [
      `This Claude Code session is managed by AgentK Local. Project: ${p?.name ?? row.project_id}. Task: ${row.name}. ${row.repo_path && p && row.repo_path !== p.repo_path ? `Repo: ${row.repo_path}. ` : ''}${row.kind === 'review-pr' && row.pr_repo ? `PR: ${row.pr_repo}#${row.pr_number} (${row.pr_url}). ` : ''}Branch: ${row.branch} (base ${row.base_branch}). ${workflowLine(kindOf(row.kind), row.stage)}`,
    ];
    if (dedicated) {
      lines.push(`Meko: this project's dedicated datapack is "${dedicated.name}" (datapack_id ${dedicated.id}). Pass that datapack_id and agent_id "${launchedAgentId(row)}" on Meko calls that write.`);
    }
    if (shared.length) {
      lines.push(`Shared datapacks you may search (read-only, pass datapack_id explicitly): ${shared.map((d) => `"${d.name}" (${d.id})`).join(', ')}.`);
    }
    if (dps.length) lines.push(mekoRecallSystemLine());
    if (dps.length && row.kind === 'review-pr') lines.push(mekoQuestionSystemLine());
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
    if (busy()) throw new HttpError(409, 'This session is still starting: on a big repo its worktree can take a few minutes. Try again once its startup steps are done.');
  }

  /**
   * Pins the project's dedicated datapack for the session's Meko agent id (the project may have
   * switched to another since the last launch), and records it as the one this launch uses
   * (sessionDatapack), so AgentK's writes for the session stay with it after a switch, until the
   * session is launched again. A session from before sessions had their own agent id gets one now.
   */
  private pinDatapack(row: SessionRow): void {
    if (row.meko_agent_id === null) {
      row.meko_agent_id = sessionAgentId(row);
      run(this.db, 'UPDATE sessions SET meko_agent_id = ? WHERE id = ?', row.meko_agent_id, row.id);
    }
    const dp = this.core.mekoStatus.configured ? dedicatedDatapack(this.db, row.project_id) : null;
    row.meko_datapack_id = dp?.id ?? null;
    row.meko_datapack_name = dp?.name ?? null;
    run(this.db, 'UPDATE sessions SET meko_datapack_id = ?, meko_datapack_name = ? WHERE id = ?', row.meko_datapack_id, row.meko_datapack_name, row.id);
    // Meko off: AgentK pins nothing. Claude Code's own Meko setup, if any, is left as it is.
    if (!dp) return;
    try {
      this.core.projects.writePin(launchedAgentId(row), dp);
    } catch (e) {
      this.core.log.warn(`could not pin the Meko datapack for ${row.id}`, errMsg(e));
    }
  }

  /**
   * Starts (or restarts) Claude in the session's tmux session. Callers mark the session as
   * creating or launching for the whole start (see `starting()`).
   */
  async launch(row: SessionRow, opts: { resume: boolean; prompt: string | null }): Promise<void> {
    // Claude Code keys folder trust by the git root, which for a worktree is the main repo.
    const repo = sessionRepo(this.db, row);
    if (repo) await this.trustRepo(repo);
    this.pinDatapack(row);
    const designDir = join(this.core.paths.designs, row.id);
    mkdirSync(designDir, { recursive: true });
    const mod = await this.modDir();
    const script = writeLauncher(this.core.paths, {
      sessionId: row.id,
      worktree: row.worktree_path,
      claudeCommand: this.core.config.claude.command,
      claudeSessionId: row.claude_session_id,
      resume: opts.resume,
      name: row.name,
      planMode: row.stage === 'design' && !!row.plan_mode,
      addDirs: [designDir],
      pluginDirs: mod ? [mod] : [],
      appendSystemPrompt: this.systemPrompt(row),
      prompt: opts.prompt,
      env: this.env(row, !!mod),
      unsetEnv: SESSION_ENV_UNSET,
    });
    const command = `exec sh ${shq(script)}`;
    const pane = (await this.core.tmux.panes()).get(row.tmux_name);
    // Archived (or its project deleted) while it was starting: Claude must not start behind its back.
    if (sessionRow(this.db, row.id)?.desired !== 'running') throw new HttpError(409, 'This session was archived while it was starting.');
    // Never replace a live Claude: respawn-pane -k would end it (or start this one twice).
    if (pane && !pane.dead) throw new HttpError(409, 'Claude is already running in this session.');
    this.awaitingStart.add(row.id);
    // A new Claude process: the last one's mod and measurements are gone with it.
    this.mod.reset(row.id);
    this.contextFill.delete(row.id);
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
   * Submits a prompt to the session's Claude: through AgentK's mod while it's live and `[claude] mod`
   * is on (Claude takes it when it's next idle), else typed into the TUI. Submissions to one session go one at a time; one
   * that had to wait is refused if the one before it started a turn.
   */
  submit(id: string, text: string): Promise<void> {
    const prev = this.submitting.get(id);
    const next = (prev ?? Promise.resolve()).then(async () => {
      const r = this.row(id);
      if (r.desired !== 'running') throw new HttpError(409, 'This session is archived.');
      if (prev && r.status === 'working') throw new HttpError(409, 'AgentK is working on another prompt. Try again when the turn ends.');
      if (this.core.config.claude.mod && this.mod.live(id)) await this.mod.send(id, text);
      else await this.core.tmux.submit(r.tmux_name, text);
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
    const repo = sessionRepo(this.db, row);
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

  /**
   * Context for a new session's SessionStart hook: what AgentK's Meko search found for the task,
   * waiting briefly for a search still running. Only for a fresh start: on a resume it's already
   * in the transcript, and after /clear the user has moved on.
   */
  async context(token: string, p: HookPayload): Promise<HookContext> {
    const row = this.byToken(token);
    if (!row || row.desired !== 'running' || p.source !== 'startup') return null;
    const text = await this.recall.take(row.id, HOOK_WAIT_MS);
    return text ? { hookSpecificOutput: { hookEventName: 'SessionStart', additionalContext: text } } : null;
  }

  /**
   * AgentK's mod in the session's Claude (src/mod/agentk), on the hook socket's `/mod/*` routes:
   * hello when it loads, polls for prompts, acks them, and reports measurements after each turn.
   */
  async onMod(token: string, route: string, body: Record<string, unknown>, signal: AbortSignal): Promise<ModReply> {
    const row = this.byToken(token);
    if (!row || row.desired !== 'running') return { status: 403 };
    switch (route) {
      case '/mod/hello':
        if (this.mod.hello(row.id)) this.core.log.info(`AgentK's mod is running in ${row.id}; its prompts go through the mod`);
        return { status: 204 };
      case '/mod/poll': {
        // The socket is closing (a rebind or a stop), or the mod was turned off in the config: the
        // mod waits a few seconds and polls again. Sessions keep the mod until they're relaunched.
        if (!this.mod.accepting || !this.core.config.claude.mod) return { status: 503 };
        const answer = await this.mod.poll(row.id, signal);
        return answer ? { status: 200, body: answer } : { status: 204 };
      }
      case '/mod/ack':
        if (body.state === 'dropped') {
          this.mod.dropped(row.id, body.id, body.reason, body.filled);
          return { status: 204 };
        }
        // 204: submit it. 410: AgentK gave up on it (it reported the send failed), so don't.
        return { status: body.state === 'accepted' && this.mod.accept(row.id, body.id) ? 204 : 410 };
      case '/mod/measure':
        this.measured(row.id, body);
        return { status: 204 };
    }
    return { status: 404 };
  }

  /** A measurement from the mod: the context window's fill, and the account's plan limits. */
  private measured(id: string, m: Record<string, unknown>): void {
    this.core.limits.reportMeasured(m.rateLimits);
    const ctx = m.context && typeof m.context === 'object' ? (m.context as { percent?: unknown }) : null;
    const pct = typeof ctx?.percent === 'number' && Number.isFinite(ctx.percent) ? Math.round(Math.min(100, Math.max(0, ctx.percent))) : null;
    if (pct === (this.contextFill.get(id) ?? null)) return;
    if (pct === null) this.contextFill.delete(id);
    else this.contextFill.set(id, pct);
    this.core.changed();
  }

  /**
   * Another plugin in Claude refused a prompt AgentK sent through the mod, after the send had
   * resolved (its caller counts it delivered): the user has to step in, so the detail names the
   * prompt and says how to send it.
   */
  private promptDropped(id: string, d: DroppedPrompt): void {
    const line = d.text?.split('\n').find((l) => l.trim())?.trim() ?? '';
    const name = line ? ` "${line.length > 80 ? `${line.slice(0, 79)}…` : line}"` : '';
    const detail = `A plugin in Claude refused AgentK's prompt${name}: ${d.reason}. ` + (d.filled
      ? "Its text is in Claude's prompt box: press Enter in the terminal to send it, or edit it first."
      : 'Ask AgentK for it in the terminal.');
    this.core.log.warn(`${id}: ${detail}`);
    if (sessionRow(this.db, id)?.desired !== 'running') return;
    this.event(id, 'mod', detail);
    this.notifyUser(id, 'Needs you', detail);
    this.setStatus(id, 'needs_input', detail);
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
    this.hookless.delete(id);
    const ev = p.hook_event_name ?? '';
    const now = Date.now();
    run(this.db, 'UPDATE sessions SET last_event_at = ? WHERE id = ?', now, id);

    switch (ev) {
      case 'SessionStart': {
        this.awaitingStart.delete(id);
        if (p.transcript_path) run(this.db, 'UPDATE sessions SET transcript_path = ? WHERE id = ?', p.transcript_path, id);
        // /clear (or /resume in the TUI) moves Claude to another conversation; resume that one from now on.
        // After /clear the context is a new one; the mod measures it after the next turn.
        if (p.source === 'clear') this.contextFill.delete(id);
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
        this.mod.reset(id, 'Claude exited before taking the prompt');
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
      if (pane && !pane.dead) {
        // Server restart; Claude kept running. One still starting lost its progress with the old
        // server, and its startup hooks may have come while no server was listening.
        if (row.status === 'starting' && !this.startup.has(row.id)) this.restoreStartup(row);
        continue;
      }
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
      } else if (!pane.dead && this.hookless.has(row.id) && (row.status === 'working' || row.status === 'idle')) {
        this.syncFromTranscript(row);
      } else if (!pane.dead && (row.status === 'starting' || this.awaitingStart.has(row.id)) && !trustShown && Date.now() - (row.last_event_at ?? 0) > 4000) {
        if (this.followTranscript(row)) continue;
        // Claude fires no hooks until the folder-trust dialog is answered (new, resumed or recovered).
        const screen = await this.core.tmux.capture(row.tmux_name).catch(() => '');
        if (TRUST_PROMPT_RE.test(screen)) {
          const repo = sessionRepo(this.db, row);
          if (repo) await this.trustRepo(repo); // so the next session skips it
          this.setStatus(row.id, 'needs_input', 'Claude is asking whether you trust this folder. Answer in the terminal.');
          this.notifyUser(row.id, 'Needs you', 'Confirm folder trust in the terminal.');
        }
      } else if (pane.dead && row.status !== 'exited') {
        this.startup.delete(row.id);
        this.hookless.delete(row.id);
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
   * A starting session whose Claude is up but whose startup hooks never arrived (say the hook
   * socket was gone): once its transcript has been written since the start, Claude has taken a
   * prompt, so the session leaves "Starting" and follows the transcript (syncFromTranscript)
   * until a hook arrives. True when it moved the session on.
   */
  private followTranscript(row: SessionRow): boolean {
    const st = this.startup.get(row.id);
    if (row.status !== 'starting' || !st || (st.step !== 'loading' && st.step !== 'prompt') || Date.now() - st.stepSince < MISSED_START_MS) return false;
    if (this.transcriptWrittenAt(row) <= st.startedAt) return false;
    this.core.log.warn(`no startup hooks arrived from ${row.id}; following its transcript instead`);
    this.clearStartup(row.id);
    this.awaitingStart.delete(row.id);
    this.hookless.add(row.id);
    this.event(row.id, 'started', "Claude started, but its hooks never reached AgentK; its status follows the transcript until they do");
    this.syncFromTranscript(row);
    return true;
  }

  /**
   * Working while the transcript is being written, idle otherwise. Only a stand-in for the hooks:
   * a long tool call writes nothing and shows as idle, and permission prompts don't show at all.
   */
  private syncFromTranscript(row: SessionRow): void {
    const next = Date.now() - this.transcriptWrittenAt(row) < TRANSCRIPT_ACTIVE_MS ? 'working' : 'idle';
    if (row.status !== next) this.setStatus(row.id, next);
  }

  /** When the session's transcript was last written (ms), or 0 when there is none yet. */
  private transcriptWrittenAt(row: SessionRow): number {
    const files = [row.transcript_path, transcriptPathFor(row.worktree_path, row.claude_session_id)];
    return Math.max(0, ...files.map((f) => {
      try {
        return f ? statSync(f).mtimeMs : 0;
      } catch {
        return 0;
      }
    }));
  }

  /**
   * Startup progress for a session a previous server was starting: at the loading step, timed from
   * when it began starting, so the Starting view counts on and followTranscript can move it on.
   */
  private restoreStartup(row: SessionRow): void {
    const since = row.last_event_at ?? row.created_at;
    const resumed = row.status_detail === 'Resuming';
    this.startup.set(row.id, { step: 'loading', stepSince: since, startedAt: since, resumed, hasPrompt: !resumed && !!row.initial_prompt });
    this.core.changed();
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
    await restoreWorktree(sessionRepo(this.db, row) ?? p.repo_path, row.worktree_path, row.branch);
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

  /** `body` is the request's: approve-design takes the user's answers to the design (DesignAnswersRequest). */
  async action(id: string, action: SessionAction, body?: unknown): Promise<Session> {
    const row = this.row(id);
    if (row.desired !== 'running') throw new HttpError(409, 'This session is archived.');
    const lc = this.core.lifecycle;
    switch (action) {
      case 'approve-design': await lc.approveDesign(row, body); break;
      case 'open-pr': await lc.openPr(row); break;
      case 'merge': await lc.merge(row); break;
      case 'request-review': await lc.requestReview(row, false); break;
      case 'force-review': await lc.requestReview(row, true); break;
      case 'resume': await this.resume(row); break;
      case 'archive':
        await this.refuseUncommitted(row, 'Ask AgentK to commit, or Discard the session to throw the changes away.');
        await this.archive(row, { keepBranch: true });
        break;
      case 'discard': await this.archive(row, { keepBranch: false, force: true }); break;
      case 'update-pr': await lc.updatePr(row); break;
      case 'close-review': await this.closeReview(row); break;
      default: throw new HttpError(400, `Unknown action ${String(action)}.`);
    }
    return this.get(id);
  }

  /** Changes the session's type, stage or both, at the user's request (see Lifecycle.move). */
  async move(id: string, req: MoveSessionRequest): Promise<Session> {
    await this.core.lifecycle.move(this.row(id), req);
    return this.get(id);
  }

  /**
   * Ends a review: archives the session (which drops the PR's head ref) and deletes its review
   * branch, kept when it has commits of the user's own. Refused while the worktree has uncommitted files.
   */
  private async closeReview(row: SessionRow): Promise<void> {
    if (row.kind !== 'review-pr' || !row.pr_number) throw new HttpError(409, 'Only a Review a PR session can close a review.');
    await this.refuseUncommitted(row, 'Commit them, or Discard the session to throw them away.');
    const repo = sessionRepo(this.db, row);
    // Measured from the commit the branch was put at, which is always here (the PR's newer head may
    // not be fetched). When git can't tell, the branch is kept.
    const own = repo && row.review_sha ? await commitsBeyond(repo, row.branch, row.review_sha) : null;
    const keep = own !== 0;
    const { worktreeFreed } = await this.archive(row, { keepBranch: keep });
    this.setStage(row.id, 'done', 'you', { event: false });
    this.event(row.id, 'review-closed', keep && worktreeFreed
      ? `Closed the review of PR #${row.pr_number}; branch ${row.branch} kept, since it ${own ? `has ${own === 1 ? 'a commit' : `${own} commits`} of yours` : 'may have commits of yours'}`
      : `Closed the review of PR #${row.pr_number}`);
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
      await restoreWorktree(sessionRepo(this.db, row) ?? p.repo_path, row.worktree_path, row.branch).catch((e: unknown) => {
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

  /**
   * Throws a 409 while the session's own worktree has uncommitted or untracked files: Archive and
   * Merge would leave the branch checked out there, where no later session could have it.
   */
  async refuseUncommitted(row: SessionRow, fix: string): Promise<void> {
    if (!this.ownsWorktree(row)) return;
    const n = await uncommittedCount(row.worktree_path);
    if (n) throw new HttpError(409, `This session's worktree has ${n === 1 ? '1 uncommitted file' : `${n} uncommitted files`}. ${fix}`);
  }

  /** What Discard would throw away: uncommitted files and commits no remote has. */
  /** Every file changed on the session's branch vs. its base, committed or not. */
  async changes(id: string): Promise<SessionChanges> {
    const row = this.row(id);
    if (!existsSync(row.worktree_path) || !this.ownsWorktree(row)) throw new HttpError(409, 'This session\'s worktree is gone, so there are no changes to show.');
    const c = await changes(row.worktree_path, row.base_branch);
    if (!c) throw new HttpError(409, `Couldn't find where ${row.branch} branched off ${row.base_branch}.`);
    return c;
  }

  /** One side of a changed image, for the Changes panel's preview. */
  async changedImage(id: string, path: string, side: ChangeSide): Promise<{ type: string; data: Buffer }> {
    const row = this.row(id);
    if (!existsSync(row.worktree_path) || !this.ownsWorktree(row)) throw new HttpError(409, 'This session\'s worktree is gone, so there are no changes to show.');
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
    const owned = this.ownsWorktree(row);
    const repo = p ? (sessionRepo(this.db, row) ?? p.repo_path) : null;
    const [uncommittedFiles, unpushedCommits] = await Promise.all([
      owned ? uncommittedCount(row.worktree_path) : 0,
      // A review branch holds the author's commits, which are safe on GitHub: only the user's own count.
      repo && row.kind === 'review-pr' && row.review_sha
        ? commitsBeyond(repo, row.branch, row.review_sha).then((n) => n ?? unpushedCount(repo, row.branch, row.base_branch))
        : repo ? unpushedCount(repo, row.branch, row.base_branch) : 0,
    ]);
    return { worktreeExists: owned && existsSync(row.worktree_path), uncommittedFiles, unpushedCommits };
  }

  /**
   * Stops Claude and lets go of the worktree if it's clean (see freeWorktree). The branch is kept
   * unless asked. With force (Discard), uncommitted changes are thrown away first.
   */
  async archive(row: SessionRow, opts: { keepBranch: boolean; force?: boolean; stage?: Stage }): Promise<{ worktreeFreed: boolean; worktree: WorktreeFate }> {
    // A session still starting would bring its worktree back, or start Claude after the kill.
    await this.settle(row.id);
    row = this.row(row.id);
    this.recall.drop(row.id);
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
      const repo = p ? (sessionRepo(this.db, row) ?? p.repo_path) : null;
      const worktree: WorktreeFate = repo ? await this.freeWorktree(row, repo, !!opts.force) : 'stuck';
      const worktreeFreed = FREED.includes(worktree);
      if (repo && worktreeFreed && !opts.keepBranch) await tryGit(repo, 'branch', '-D', row.branch);
      // A review's copy of the PR's head (fetched again by any later review of it).
      if (repo && row.kind === 'review-pr' && row.pr_number) await tryGit(repo, 'update-ref', '-d', prRef(row.pr_number));
      rmSync(join(this.core.paths.run, `${row.id}.sh`), { force: true });
      removePastes(this.core.paths.pastes, row.id);
      this.core.questions.forget(row.id);
      this.busy.delete(row.id);
      this.planPending.delete(row.id);
      this.awaitingStart.delete(row.id);
      this.recoverRetryAt.delete(row.id);
      this.mergeInfo.delete(row.id);
      this.diffs.delete(row.id);
      this.mod.reset(row.id, 'The session was archived');
      this.contextFill.delete(row.id);
      if (opts.force) {
        this.event(row.id, 'discarded', {
          idle: `Discarded; changes and branch ${row.branch} deleted, worktree kept for the next session`,
          removed: `Discarded; worktree and branch ${row.branch} deleted`,
          earlier: `Discarded; branch ${row.branch} deleted`,
          uncommitted: `Discarded, but the worktree at ${row.worktree_path} could not be removed, so branch ${row.branch} was kept`,
          stuck: `Discarded, but the worktree at ${row.worktree_path} could not be removed, so branch ${row.branch} was kept`,
        }[worktree]);
      } else if (!wasArchived) {
        this.event(row.id, 'archived', {
          idle: 'Archived; worktree kept for the next session',
          removed: 'Archived; worktree removed',
          earlier: 'Archived',
          uncommitted: 'Archived; worktree kept because it has uncommitted changes',
          stuck: `Archived; git couldn't remove the worktree at ${row.worktree_path}, so it was kept`,
        }[worktree]);
      }
      this.core.changed();
      return { worktreeFreed, worktree };
    } finally {
      this.stopping.delete(row.id);
    }
  }

  /**
   * Lets go of an ending session's worktree. A clean one goes idle for the next session on the
   * repo (build outputs stay) or, with the pool off, is removed. One with uncommitted changes
   * stays the session's, unless force (Discard) throws the changes away first; ignored files
   * survive that. A worktree the session no longer owns is left alone. Once it's let go of
   * (idle, removed or earlier), the session is marked so, and its branch is free.
   */
  private async freeWorktree(row: SessionRow, repo: string, force: boolean): Promise<WorktreeFate> {
    const pool = this.core.worktrees;
    const path = row.worktree_path;
    if (!pool.owns(row)) {
      // Already let go of (an archive finishing a second time), or a pre-pool folder that's gone:
      // make sure git no longer counts it as checking out the branch.
      if (!existsSync(path)) await tryGit(repo, 'worktree', 'prune');
      pool.released(row.id);
      return 'earlier';
    }
    let fate: WorktreeFate;
    if (!existsSync(path)) {
      fate = (await removeWorktree(repo, path)) ? 'removed' : 'stuck'; // prunes git's record of it
    } else {
      if (force) await discardChanges(path);
      const clean = await worktreeClean(path);
      if (clean && (await pool.park(row, repo))) fate = 'idle';
      else if (!clean && !force) fate = 'uncommitted';
      else fate = (await removeWorktree(repo, path, force)) ? 'removed' : 'stuck';
    }
    if (fate === 'removed') pool.drop(path);
    if (FREED.includes(fate)) pool.released(row.id);
    return fate;
  }

  /** True while the session's worktree is its own (not idle or handed to another session). */
  ownsWorktree(row: SessionRow): boolean {
    return this.core.worktrees.owns(row);
  }

  /**
   * Used when a project is deleted: stop everything, let go of clean worktrees, keep branches.
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
        this.mod.reset(row.id, 'The project was deleted');
        this.contextFill.delete(row.id);
      }
      // A worktree that went idle or to another session (maybe another project's) isn't this one's to remove.
      if (p && this.ownsWorktree(row) && !FREED.includes(await this.freeWorktree(row, sessionRepo(this.db, row) ?? p.repo_path, false))) {
        kept.push(row.worktree_path);
        this.core.log.info(`kept the worktree of ${row.id} at ${row.worktree_path}: it has uncommitted changes`);
      }
      rmSync(join(this.core.paths.run, `${row.id}.sh`), { force: true });
      removePastes(this.core.paths.pastes, row.id);
      this.core.questions.forget(row.id);
    }
    // Idle worktrees stay for the repo's other projects; the last project on the repo takes them along.
    if (p) {
      const repos = new Set([p.repo_path, ...sessionRows(this.db, projectId).map((r) => r.repo_path).filter((r): r is string => !!r)]);
      for (const repo of repos) await this.core.worktrees.releaseRepo(repo, projectId);
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

  /**
   * Starts searching the project's datapacks for a new session's task, in the background: Claude
   * gets what it finds through its SessionStart hook (context()). Skipped when Meko is off, the
   * project has no datapacks or there's no task; never throws, and nothing waits for it. The
   * project's Meko conversation (needed for the search) is looked up while the issues are read.
   */
  private startRecall(r: SessionRow, task: string): void {
    // A session that reuses the id of one that failed to start mustn't get that one's results.
    this.recall.drop(r.id);
    if (!task || !this.sessionDatapacks(r.project_id).length) return;
    const pid = r.project_id;
    const cwd = sessionRepo(this.db, r) ?? r.worktree_path;
    this.core.projects.ensureConversation(pid, SEARCH_TIMEOUT_MS).catch(() => undefined); // searchDatapacks reports a failure
    const work = runRecall(r.name, task, {
      issue: (n) => issueText(cwd, n),
      search: async (q) => {
        const s = await this.core.projects.searchDatapacks(pid, q, SEARCH_TIMEOUT_MS);
        if (s.searched && s.failed === s.searched) throw s.error;
        return s.results;
      },
    });
    work.catch((e) => this.core.log.warn(`the Meko search at start failed for ${r.id}`, errMsg(e)));
    this.recall.start(r.id, work);
  }

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
    try {
      await this.conversationId(r, dp);
    } catch (e) {
      this.core.log.warn(`naming the Meko conversation for ${r.id} failed`, errMsg(e));
    }
  }

  /**
   * The id of the session's Meko conversation in its project's dedicated datapack (keyed by the
   * Claude session id, so the plugin's hooks and AgentK share it), titled after the session if
   * Meko doesn't have it yet. Callers check that Meko is on.
   */
  conversationId(r: SessionRow, dp: DatapackRef): Promise<string> {
    const title = `${r.name} (${projectRow(this.db, r.project_id)?.name ?? r.project_id})`;
    return this.core.meko.ensureConversation(launchedAgentId(r), dp.id, r.claude_session_id, title);
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

  /**
   * Fills in how many commits each newly found push carried (-1 when git can't tell) and what it
   * changed per file extension (NULL when git can't tell), for the usage page and the usage stats.
   */
  private async countPushedCommits(r: SessionRow): Promise<void> {
    const pending = all<{ range: string }>(this.db, 'SELECT range FROM pushes WHERE session_id = ? AND commits IS NULL', r.id);
    if (!pending.length) return;
    // Deleted projects keep their row, and their repo usually still has the commits.
    const repo = sessionRepo(this.db, r);
    for (const { range } of pending) {
      let n = -1;
      let fileTypes: string | null = null;
      if (repo) {
        const out = await git(repo, 'rev-list', '--count', range).catch(() => '');
        if (/^\d+$/.test(out)) n = Number(out);
        const [from, to] = range.split('..');
        const stat = await gitRaw(repo, 'diff', '--numstat', '-z', from, to).catch(() => null);
        if (stat !== null) fileTypes = JSON.stringify(countFileTypes(parseNumstatZ(stat)));
      }
      run(this.db, 'UPDATE pushes SET commits = ?, file_types = ? WHERE session_id = ? AND range = ? AND commits IS NULL', n, fileTypes, r.id, range);
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
