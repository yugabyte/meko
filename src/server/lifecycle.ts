import { existsSync } from 'node:fs';
import { basename } from 'node:path';
import { mekoHost } from './config.js';
import type { Core } from './core.js';
import { get, run } from './db.js';
import { artifactUrlIn, isArtifactPublish, mekoDesignName, questionsFromMarkdown, readArtifactHtml, readDesignFile, readDesignQuestions } from './design.js';
import { commitsBetween, commitsBeyond, deleteBranch, fetchPrHead, git, moveReviewBranch, remoteFor, tryGit, uncommittedCount } from './git.js';
import * as github from './github.js';
import { countFileTypes } from './filetypes.js';
import { errMsg } from './log.js';
import { launchedAgentId } from './meko.js';
import type { HookPayload } from './hooks.js';
import { mekoPrFooter } from './prompts.js';
import { HttpError } from './projects.js';
import { KIND_LABELS, STAGE_LABELS, type DesignQuestion, type MoveSessionRequest, type ReviewerMode, type Stage } from '../shared/types.js';
import { MOVE_TARGETS, kindOf, moveBlockedIn, moveError, movesStep, offersPrompt, promptByDefault } from '../shared/stages.js';
import { parseSettings, projectRow, sessionDatapack, sessionRepo, sessionRow, sessionRows, type SessionRow } from './store.js';
import { workflowLine } from './sessions.js';

const POLL_MS = 15000;
const REVIEW_POLL_MS = 45000;
/** How long a requested merge may leave the PR open (a merge queue) before the session goes back to Review. */
const MERGE_GRACE_MS = 10 * 60 * 1000;
/** Archived sessions still in Merge & Close this recently are cleanups a restart cut short. */
const CLOSING_RESUME_MS = 24 * 60 * 60 * 1000;
/** How long a CI failure waits for the rest of the checks before Claude is asked to fix it. */
const CI_SETTLE_MS = 10 * 60 * 1000;
/** Heads in a row that get a CI-fix prompt before a failing CI is left to the user. */
const MAX_CI_FIXES = 3;
/** Heads in a row that get a prompt to resolve merge conflicts before the conflicts are left to the user. */
const MAX_CONFLICT_FIXES = 3;
/** How long Open PR waits for Meko to name the session's conversation for the PR description. */
const PR_CONVERSATION_TIMEOUT_MS = 10000;

/** The status detail while a markdown or Artifact design waits for approval. */
export const DESIGN_READY = 'Design ready for your approval';

/** Prompts queued for Claude in the Review stage, in the order they go out. */
const QUEUE_ORDER = ['review', 'review-people', 'conflicts', 'ci'] as const;
/** The one prompt queued in a Review a PR session: the summary after Update to latest. */
const REVIEWING_ORDER = ['review-pr-update'] as const;
type QueuedPrompt = (typeof QUEUE_ORDER)[number] | (typeof REVIEWING_ORDER)[number];

/** The prompts that may go out in a stage, in order: none outside Review and Reviewing PR. */
function queueOrder(stage: Stage): readonly QueuedPrompt[] {
  return stage === 'review' ? QUEUE_ORDER : stage === 'reviewing-pr' ? REVIEWING_ORDER : [];
}

/** Commits on the PR after <sha> (its commits oldest first), or null when <sha> isn't one of them (a force-push). */
export function commitsAfter(commits: string[], sha: string | null): number | null {
  const i = sha ? commits.indexOf(sha) : -1;
  return i < 0 ? null : commits.length - 1 - i;
}

const REVIEW_VERBS: Record<string, string> = { APPROVED: 'approved', CHANGES_REQUESTED: 'requested changes on', COMMENTED: 'commented on' };
/** `sessions.pending_prompts`: kept across restarts; a prompt is `sent` until the turn that got it ends. */
type Queue = Partial<Record<QueuedPrompt, { state: 'queued' | 'sent'; vars?: Record<string, string> }>>;

const REVIEW_REQUEST = /^\/agentk review\b/;

/** The reviews' authors, each once, as `a, b`. */
function loginList(reviews: github.PrReview[]): string {
  return [...new Set(reviews.map((r) => r.author?.login))].join(', ');
}

/**
 * True when the local branch has commits the PR's head lacks: committed but never pushed, or
 * pushed after the PR merged. False when the head is unknown or the branch is gone.
 */
async function lacksLocalCommits(repo: string, branch: string, st: github.PrStatus): Promise<boolean> {
  const head = st.headRefOid;
  if (!head) return false;
  const tip = await tryGit(repo, 'rev-parse', '--verify', '--quiet', `refs/heads/${branch}`);
  if (!tip || tip === head) return false;
  // The head can be a commit made on GitHub (Update branch); fetch it to compare.
  if ((await tryGit(repo, 'cat-file', '-e', `${head}^{commit}`)) === null) {
    await tryGit(repo, 'fetch', '--quiet', 'origin', `refs/pull/${st.number}/head`);
  }
  return (await tryGit(repo, 'merge-base', '--is-ancestor', tip, head)) === null;
}

/**
 * The task lifecycle: Design → Code → PR & Review → Merge & Close (merge, then cleanup).
 * Gates (approve design, open PR, merge) come from the user; everything else
 * is driven by hook events and by polling GitHub from the server, so waiting
 * on reviews and CI costs no Claude tokens.
 */
export class Lifecycle {
  private timer: NodeJS.Timeout | null = null;
  private lastPoll = new Map<string, number>();
  private polling = false;
  private warned = new Map<string, number>();
  /** Sessions whose design changed during the current turn. */
  private designChanged = new Set<string>();
  /** Sessions whose cleanup is running, so an overlapping poll doesn't start a second one. */
  private closing = new Set<string>();
  /** Sessions with a Merge request in flight. */
  private merging = new Set<string>();
  /** Per session, the tail of the GitHub work (polls, review requests, merge) queued for it. */
  private chains = new Map<string, Promise<void>>();
  /** Per session, the tail of its design sends to Meko. */
  private designSends = new Map<string, Promise<void>>();
  /** Per session, the failing head whose checks are still running, and when its failure was first seen. */
  private ciFailSince = new Map<string, { head: string; at: number }>();
  /** The GitHub login gh is signed in as, once known (whose reviews a Review a PR session logs). */
  private ghLogin: string | null = null;

  constructor(private core: Core) {}

  private get db() {
    return this.core.db;
  }

  private get sessions() {
    return this.core.sessions;
  }

  start(): void {
    this.timer = setInterval(() => {
      this.pollAll().catch((e) => this.core.log.warn('lifecycle poll failed', errMsg(e)));
    }, POLL_MS);
  }

  stop(): void {
    if (this.timer) clearInterval(this.timer);
    this.timer = null;
  }

  // ---- design ----

  /**
   * Stores the parts of a session's design that changed (markdown, and for an Artifact its
   * HTML and claude.ai link), and saves them to the project's dedicated datapack.
   */
  async saveDesign(id: string, d: { markdown?: string; html?: string; artifactUrl?: string }): Promise<void> {
    const prev = get<{ markdown: string; html: string | null; artifact_url: string | null }>(
      this.db, 'SELECT markdown, html, artifact_url FROM designs WHERE session_id = ?', id);
    const markdown = d.markdown ?? prev?.markdown ?? '';
    const html = d.html ?? prev?.html ?? null;
    const url = d.artifactUrl ?? prev?.artifact_url ?? null;
    // A part that changed hasn't been sent to Meko yet.
    run(this.db, `INSERT INTO designs(session_id, markdown, html, artifact_url, updated_at) VALUES (?, ?, ?, ?, ?)
                  ON CONFLICT(session_id) DO UPDATE SET markdown = excluded.markdown, html = excluded.html,
                    artifact_url = excluded.artifact_url, updated_at = excluded.updated_at,
                    md_sent = (designs.md_sent AND designs.markdown = excluded.markdown),
                    html_sent = (designs.html_sent AND designs.html IS excluded.html)`, id, markdown, html, url, Date.now());
    this.core.changed();
    void this.sendDesign(id);
  }

  /**
   * Sends the parts of a session's design that Meko doesn't have yet, one session's sends at a
   * time. Meko can't replace a file, so each one is a new version (see mekoDesignName), counted
   * only once it's sent: a failed send is tried again at the next save, turn end or approval.
   */
  private sendDesign(id: string): Promise<void> {
    const next = (this.designSends.get(id) ?? Promise.resolve()).then(() => this.sendDesignNow(id))
      .catch((e) => this.core.log.warn(`saving design to Meko failed for ${id}`, errMsg(e)));
    this.designSends.set(id, next);
    void next.then(() => {
      if (this.designSends.get(id) === next) this.designSends.delete(id);
    });
    return next;
  }

  private async sendDesignNow(id: string): Promise<void> {
    const row = sessionRow(this.db, id);
    const dp = row && sessionDatapack(this.db, row);
    // No datapack, or Meko is off: the design stays here until it can be sent.
    if (!row || !dp || !this.core.mekoStatus.configured) return;
    const d = get<{ markdown: string; html: string | null; md_version: number; md_sent: number; html_version: number; html_sent: number }>(
      this.db, 'SELECT markdown, html, md_version, md_sent, html_version, html_sent FROM designs WHERE session_id = ?', id);
    if (!d) return;
    const parts: { name: string; text: string; type: string; sent: string; version: number }[] = [];
    if (!d.md_sent && d.markdown) {
      parts.push({ name: mekoDesignName(row.slug, 'md', d.md_version + 1), text: d.markdown, type: 'text/markdown',
        sent: 'UPDATE designs SET md_version = ?, md_sent = (markdown = ?) WHERE session_id = ?', version: d.md_version + 1 });
    }
    if (!d.html_sent && d.html !== null) {
      parts.push({ name: mekoDesignName(row.slug, 'html', d.html_version + 1), text: d.html, type: 'text/html',
        sent: 'UPDATE designs SET html_version = ?, html_sent = (html IS ?) WHERE session_id = ?', version: d.html_version + 1 });
    }
    if (!parts.length) return;
    const conv = await this.sessions.conversationId(row, dp);
    await Promise.all(parts.map(async (p) => {
      await this.core.meko.putArtifact(p.name, p.text, p.type, dp.id, launchedAgentId(row), conv);
      // The version is used; it's the current design only if nothing changed while it was sent.
      run(this.db, p.sent, p.version, p.text, id);
    }));
  }

  /** After an Artifact tool call during Design: keeps the published page and its link as the design. */
  async captureArtifact(id: string, p: HookPayload): Promise<void> {
    const input = p.tool_input ?? {};
    if (!isArtifactPublish(input)) return;
    const url = artifactUrlIn(JSON.stringify(p.tool_response ?? ''));
    const html = readArtifactHtml(input.file_path, p.cwd);
    if (!url && html === null) return;
    await this.saveDesign(id, { ...(url ? { artifactUrl: url } : {}), ...(html !== null ? { html } : {}) });
    this.designChanged.add(id);
  }

  /**
   * At the end of a Design turn: picks up the markdown design file if Claude wrote or
   * changed it. True when the design changed during this turn (file or Artifact).
   */
  async collectDesign(row: SessionRow): Promise<boolean> {
    const md = readDesignFile(this.sessions.designFile(row));
    if (md !== null) {
      const prev = get<{ markdown: string }>(this.db, 'SELECT markdown FROM designs WHERE session_id = ?', row.id);
      if (prev?.markdown !== md) {
        const url = artifactUrlIn(md);
        await this.saveDesign(row.id, { markdown: md, ...(url ? { artifactUrl: url } : {}) });
        this.designChanged.add(row.id);
      }
    }
    this.collectQuestions(row);
    void this.sendDesign(row.id); // anything an earlier send couldn't deliver
    return this.designChanged.delete(row.id);
  }

  /**
   * The design's questions for the user: the questions file Claude writes beside the design, or
   * else the "Open questions" list of a markdown design. A file that's too large, unreadable or
   * not questions is logged and leaves the ones stored alone. A change counts as a change to the
   * design, and drafts are matched to the new options (see DesignAnswerService.realign).
   */
  private collectQuestions(row: SessionRow): void {
    const d = get<{ markdown: string; questions: string; questions_source: string | null }>(
      this.db, 'SELECT markdown, questions, questions_source FROM designs WHERE session_id = ?', row.id);
    if (!d) return;
    let questions: DesignQuestion[] | null;
    try {
      questions = readDesignQuestions(this.sessions.questionsFile(row));
    } catch (e) {
      this.warn(row.id, `Reading the design's questions (${this.sessions.questionsFile(row)})`, e);
      return;
    }
    const source = questions ? 'file' : null;
    questions ??= d.markdown ? questionsFromMarkdown(d.markdown) : [];
    const from = source ?? (questions.length ? 'markdown' : null);
    const json = JSON.stringify(questions);
    if (json === d.questions && from === d.questions_source) return;
    run(this.db, 'UPDATE designs SET questions = ?, questions_source = ?, open_questions = ?, updated_at = ? WHERE session_id = ?',
      json, from, questions.filter((q) => q.status === 'open').length, Date.now(), row.id);
    this.core.answers.realign(row.id, questions);
    this.designChanged.add(row.id);
    this.core.changed();
  }

  /** The design is waiting for the user: say so in the activity log, the header and a notification. */
  designReady(id: string): void {
    this.sessions.event(id, 'design', 'Design ready for approval');
    this.sessions.setStatus(id, 'needs_input', DESIGN_READY);
    // Answers queued meanwhile go out right after this, so AgentK isn't waiting on the user.
    if (this.core.answers.queued(id)) return;
    const open = get<{ n: number }>(this.db, 'SELECT open_questions AS n FROM designs WHERE session_id = ?', id)?.n ?? 0;
    this.sessions.notifyUser(id, 'Design ready',
      open ? `Review the design and answer ${open === 1 ? 'its open question' : `${open} open questions`}.` : 'Review and approve the design.');
  }

  async approveDesign(row: SessionRow): Promise<void> {
    if (row.stage !== 'design') throw new HttpError(409, 'This session is past the Design stage.');
    if (row.plan_mode) {
      if (!this.sessions.planPending.has(row.id)) {
        throw new HttpError(409, "AgentK hasn't presented a design for approval yet. Ask it to finish the design, or approve it in the terminal.");
      }
      // Claude's plan dialog has "Yes" selected by default.
      await this.core.tmux.sendKeys(row.tmux_name, 'Enter');
      this.sessions.planPending.delete(row.id);
      this.designApproved(row.id, 'you');
      return;
    }
    if (!get(this.db, 'SELECT 1 AS x FROM designs WHERE session_id = ?', row.id)) {
      throw new HttpError(409, "AgentK hasn't written a design yet. Wait for it to finish, or ask it in the terminal.");
    }
    if (row.status === 'working' || row.status === 'starting') throw new HttpError(409, 'AgentK is still working. Wait for the turn to finish.');
    if (row.status === 'exited') throw new HttpError(409, 'Claude has exited. Resume the session first.');
    // Any other question (a permission prompt) would take the approval as its answer.
    if (row.status === 'needs_input' && row.status_detail !== DESIGN_READY) {
      throw new HttpError(409, 'AgentK is waiting on you in the terminal. Answer it first.');
    }
    // Answers the user hasn't sent go with the approval.
    const answers = this.core.answers.forApproval(row.id);
    this.designApproved(row.id, 'you');
    try {
      const cur = this.sessions.row(row.id);
      const lead = answers && this.sessions.prompt(cur, 'approved-answers', { answers: answers.text });
      await this.sessions.submit(row.id, [lead, this.sessions.prompt(cur, 'approved')].filter(Boolean).join('\n\n'));
      if (answers) {
        answers.sent();
        this.sessions.event(row.id, 'design', `Approved with ${answers.count === 1 ? '1 answer' : `${answers.count} answers`}`);
      }
    } catch (e) {
      if (this.sessions.row(row.id).stage === 'code') this.sessions.setStage(row.id, 'design', 'auto');
      throw new HttpError(502, `Couldn't send the approval to Claude: ${errMsg(e)}. Try again.`);
    }
  }

  designApproved(id: string, by: 'you' | 'auto'): void {
    const row = sessionRow(this.db, id);
    if (!row || row.stage !== 'design') return;
    this.sessions.setStage(id, 'code', by);
    void this.sendDesign(id); // the approved design, if Meko doesn't have it yet
  }

  // ---- PR ----

  async openPr(row: SessionRow): Promise<void> {
    const check = (r: SessionRow) => {
      if (r.stage !== 'code') throw new HttpError(409, 'Open PR is only available in the Code stage.');
      this.checkPromptable(r);
    };
    check(row);
    await this.sendPr(row, check);
  }

  /**
   * Types the PR prompt (with a moved session's `moved` line first, `lead`): the session goes to the
   * PR stage, and back to where it was if the prompt can't be typed. `check` runs again after
   * waiting on GitHub and Meko, which gave Claude and the user time to move on.
   */
  private async sendPr(row: SessionRow, check: (r: SessionRow) => void, lead?: string): Promise<void> {
    if (row.branch_reused) await this.followPrBase(row);
    const footer = await this.prFooter(row);
    const cur = this.sessions.row(row.id);
    check(cur);
    // The stage moves first: Claude's turn can end before submit returns.
    this.sessions.setStage(row.id, 'pr', 'you');
    this.sessions.setBusy(row.id, true);
    try {
      await this.sessions.submit(row.id, [lead, this.sessions.prompt(cur, 'pr'), footer].filter(Boolean).join('\n\n'));
    } catch (e) {
      this.sessions.setBusy(row.id, false);
      if (this.sessions.row(row.id).stage === 'pr') this.sessions.setStage(row.id, cur.stage, 'auto');
      throw new HttpError(502, `Couldn't send the PR prompt to Claude: ${errMsg(e)}. Try again.`);
    }
  }

  /** Refuses while Claude can't take a prompt: mid-turn, waiting on the user (a ready design aside), or exited. */
  private checkPromptable(row: SessionRow): void {
    this.checkBetweenTurns(row);
    if (row.status === 'needs_input' && row.status_detail !== DESIGN_READY) throw new HttpError(409, 'AgentK is waiting on you in the terminal. Answer it first.');
    if (row.status === 'exited') throw new HttpError(409, 'Claude has exited. Resume the session first.');
  }

  private checkBetweenTurns(row: SessionRow): void {
    if (row.status === 'working' || row.status === 'starting' || this.sessions.runtime(row.id).busy) {
      throw new HttpError(409, 'AgentK is still working. Wait for the turn to finish.');
    }
  }

  // ---- moves ----

  /**
   * Moves a task session to another stage at the user's request. The rules for which moves exist
   * are in src/shared/stages.ts, shared with the move dialog. A `kind` sent by a page from before
   * session kinds became start stages is ignored.
   */
  async move(row: SessionRow, req: MoveSessionRequest): Promise<void> {
    const { stage, prompt, task } = req ?? ({} as MoveSessionRequest);
    if (!MOVE_TARGETS.includes(stage)) throw new HttpError(400, `Pick a stage to move to: ${MOVE_TARGETS.join(', ')}.`);
    if (prompt != null && typeof prompt !== 'boolean') throw new HttpError(400, 'prompt must be true or false.');
    if (task != null && typeof task !== 'string') throw new HttpError(400, 'The task must be text.');
    await this.serial(row.id, () => this.doMove(row.id, stage, prompt, (task ?? '').trim()));
  }

  private async doMove(id: string, stage: MoveSessionRequest['stage'], prompt: boolean | undefined, task: string): Promise<void> {
    const cur = this.sessions.row(id);
    if (cur.desired !== 'running') throw new HttpError(409, 'This session is archived.');
    const blocked = moveBlockedIn(cur.stage);
    if (blocked) throw new HttpError(409, blocked);
    if (this.merging.has(id) || this.closing.has(id)) throw new HttpError(409, 'A merge is in progress.');
    const bad = moveError(cur, stage);
    if (bad) throw new HttpError(400, bad);
    if (cur.stage === 'design' && cur.plan_mode) {
      throw new HttpError(409, "This session designs in Claude's plan mode. Approve or reject the plan in the terminal first.");
    }
    this.checkBetweenTurns(cur);
    if (stage === 'review' && movesStep(cur.stage, stage)) return this.moveToReview(cur);

    const send = offersPrompt(cur.stage, stage) && (prompt ?? promptByDefault(cur.stage, stage));
    if (send) {
      this.checkPromptable(cur);
      if (!cur.initial_prompt && !task) throw new HttpError(400, 'Give AgentK the task for this session.');
    }
    if (task && !cur.initial_prompt) this.sessions.update(id, { initial_prompt: task });
    // Designs are written as markdown or an Artifact now; plan mode is only for older sessions.
    if (stage === 'design') this.sessions.update(id, { plan_mode: 0 });
    if (movesStep(cur.stage, stage)) {
      if (cur.stage === 'pr' || cur.stage === 'review') this.leaveReview(id);
      if (cur.stage === 'design') this.leaveDesign(id, stage === 'code');
      this.sessions.setStage(id, stage, 'you');
    }
    if (!send) return;
    const row = this.sessions.row(id);
    const body = this.sessions.prompt(row, this.sessions.firstPrompt(row));
    try {
      await this.sessions.submit(id, `${this.movedLine(row)}\n\n${body}`);
    } catch (e) {
      throw new HttpError(502, `Moved to ${STAGE_LABELS[stage]}, but couldn't send the ${STAGE_LABELS[stage]} prompt to Claude: ${errMsg(e)}. Ask AgentK in the terminal.`);
    }
  }

  /**
   * A move to PR & Review: picks up the branch's open PR when there's nothing to push (the branch
   * tip is in the PR and the worktree is clean), or else sends the PR prompt, as Open PR does.
   */
  private async moveToReview(row: SessionRow): Promise<void> {
    const cwd = this.ghCwd(row);
    const pr = await github.findPr(cwd, row.branch, row.created_at, true).catch(() => null);
    let adopt = false;
    if (pr?.state === 'OPEN') {
      const st = await github.prStatus(cwd, pr.number).catch(() => null);
      const repo = sessionRepo(this.db, row);
      adopt = !!st && !!repo && !(await lacksLocalCommits(repo, row.branch, st))
        && (!this.sessions.ownsWorktree(row) || (await uncommittedCount(row.worktree_path)) === 0);
    }
    // Recheck after the GitHub calls: Claude may have started a turn.
    const cur = this.sessions.row(row.id);
    const check = (r: SessionRow) => {
      if (r.desired !== 'running') throw new HttpError(409, 'This session is archived.');
      if (r.stage !== row.stage) throw new HttpError(409, `The session moved to ${STAGE_LABELS[r.stage]} meanwhile. Try again.`);
    };
    check(cur);
    this.checkBetweenTurns(cur);
    if (!adopt || !pr) {
      await this.sendPr(cur, (r) => {
        check(r);
        this.checkPromptable(r);
      }, cur.stage === 'code' ? undefined : this.movedLine(cur, 'review'));
      return;
    }
    if (cur.stage === 'design') this.leaveDesign(row.id, false);
    await this.adoptPr(row.id, pr, 'you');
  }

  /** The `moved` line: where the session is now, since Claude's system prompt still says where it was. */
  private movedLine(row: SessionRow, stage: Stage = row.stage): string {
    const kind = kindOf(row.kind);
    return this.sessions.prompt(row, 'moved', { kind: KIND_LABELS[kind], stage: STAGE_LABELS[stage], workflow: workflowLine(kind, stage) });
  }

  /**
   * Leaving Design by hand: a design waiting for approval no longer is, and the design (if any)
   * goes to Meko as an approval would send it. `skipped`: straight to Code, past the approval.
   */
  private leaveDesign(id: string, skipped: boolean): void {
    const row = this.sessions.row(id);
    if (row.status === 'needs_input' && row.status_detail === DESIGN_READY) this.sessions.setStatus(id, 'idle');
    if (skipped) this.sessions.event(id, 'design', 'Design approval skipped');
    void this.sendDesign(id);
  }

  /**
   * Leaving PR & Review by hand, between turns: the PR stays open and queued review, conflict and CI prompts
   * wait for the session to come back (deliverNext only sends in Review). Prompts marked sent
   * belong to a turn that's over, as onTurnEnd would have decided had the session stayed.
   */
  private leaveReview(id: string): void {
    const q = this.queue(id);
    for (const n of QUEUE_ORDER) if (q[n]?.state === 'sent') delete q[n];
    this.setQueue(id, q);
    if (!q.review && !q['review-people'] && this.sessions.row(id).review_state === 'addressing') {
      this.sessions.update(id, { review_state: 'addressed' });
    }
    this.ciFailSince.delete(id);
    this.lastPoll.delete(id);
  }

  /**
   * The PR prompt's Meko lines (see mekoPrFooter), or '' when the project has no dedicated
   * datapack or Meko is off. When Meko fails or takes longer than PR_CONVERSATION_TIMEOUT_MS
   * (connecting included), the PR goes out without the conversation line; a slow lookup is left
   * to finish on its own rather than cut short, which would close the Meko connection others share.
   */
  private async prFooter(row: SessionRow): Promise<string> {
    const dp = sessionDatapack(this.db, row);
    if (!dp || !this.core.mekoStatus.configured) return '';
    const lookup = this.sessions.conversationId(row, dp).catch((e: unknown): null => {
      this.core.log.warn(`looking up the Meko conversation for ${row.id} failed`, errMsg(e));
      return null;
    });
    let timer: NodeJS.Timeout | undefined;
    const late = new Promise<null>((r) => {
      timer = setTimeout(() => {
        this.core.log.warn(`Meko took too long to look up the conversation for ${row.id}; the PR goes out without it`);
        r(null);
      }, PR_CONVERSATION_TIMEOUT_MS);
    });
    const conv = await Promise.race([lookup, late]).finally(() => clearTimeout(timer));
    return mekoPrFooter(mekoHost(this.core.config), dp, conv);
  }

  /**
   * A session started on an existing branch that already has an open PR works against that PR's
   * base, so the PR prompt, the diff and the Changes panel compare with what the PR merges into.
   */
  private async followPrBase(row: SessionRow): Promise<void> {
    const pr = await github.findPr(this.ghCwd(row), row.branch, row.created_at, true).catch(() => null);
    const base = pr?.state === 'OPEN' ? pr.baseRefName : undefined;
    if (!base || base === row.base_branch) return;
    this.sessions.update(row.id, { base_branch: base });
    this.sessions.event(row.id, 'base', `Base set to ${base}, the base of PR #${pr!.number}`);
  }

  /** Called on every Stop hook: a Claude turn just finished. */
  async onTurnEnd(id: string): Promise<void> {
    const row = sessionRow(this.db, id);
    if (!row) return;
    this.sessions.setBusy(id, false);
    if (row.stage === 'pr') {
      await this.pollPrStage(row).catch((e) => this.warn(row.id, 'PR lookup failed', e));
    } else if (row.stage === 'review') {
      await this.serial(id, async () => {
        // The turn that got a prompt is over. Only that turn addresses a review: one still queued isn't.
        const q = this.queue(id);
        for (const n of QUEUE_ORDER) if (q[n]?.state === 'sent') delete q[n];
        this.setQueue(id, q);
        const reviewQueued = !!(q.review || q['review-people']);
        if (!reviewQueued && this.sessions.row(id).review_state === 'addressing') {
          this.sessions.update(id, { review_state: 'addressed' });
        }
        // A queued review goes out now. Anything else waits for a fresh look at the PR, so a CI
        // prompt never goes out for a head this turn's push replaced.
        if (reviewQueued) await this.deliverNext(id);
        else await this.doPollReview(id);
      }).catch((e) => this.warn(id, 'PR status failed', e));
    } else if (row.stage === 'merge') {
      // A merge that landed while Claude was working: close now that the turn is over.
      await this.pollReview(row).catch((e) => this.warn(id, 'PR status failed', e));
    } else if (row.stage === 'reviewing-pr') {
      await this.serial(id, async () => {
        // The turn that got the update summary is over.
        const q = this.queue(id);
        if (q['review-pr-update']?.state === 'sent') {
          delete q['review-pr-update'];
          this.setQueue(id, q);
        }
        await this.doPollReviewedPr(id);
      }).catch((e) => this.warn(id, 'PR status failed', e));
    }
    // Review comments and design answers go after anything above that already gave Claude its next turn.
    await this.core.comments.deliver(id).catch((e) => this.warn(id, 'Sending review comments', e));
    await this.core.answers.deliver(id).catch((e) => this.warn(id, 'Sending answers to the design', e));
  }

  /**
   * Runs fn after the GitHub work already queued for the session, so polls, review requests
   * and merges never interleave and each one sees the others' writes.
   */
  private serial<T>(id: string, fn: () => Promise<T>): Promise<T> {
    const next = (this.chains.get(id) ?? Promise.resolve()).then(fn);
    const tail = next.then(() => undefined, () => undefined);
    this.chains.set(id, tail);
    void tail.then(() => {
      if (this.chains.get(id) === tail) this.chains.delete(id);
    });
    return next;
  }

  private queue(id: string): Queue {
    const r = get<{ q: string }>(this.db, 'SELECT pending_prompts AS q FROM sessions WHERE id = ?', id);
    try {
      return JSON.parse(r?.q ?? '{}') as Queue;
    } catch {
      return {};
    }
  }

  private setQueue(id: string, q: Queue): void {
    run(this.db, 'UPDATE sessions SET pending_prompts = ? WHERE id = ?', JSON.stringify(q), id);
  }

  private enqueue(id: string, name: QueuedPrompt, vars?: Record<string, string>): void {
    const q = this.queue(id);
    q[name] = { state: 'queued', ...(vars ? { vars } : {}) };
    this.setQueue(id, q);
  }

  /** Queues `review-people` naming its reviewers, adding to those of a prompt still waiting to go out. */
  private enqueuePeople(id: string, logins: string[]): void {
    const waiting = this.queue(id)['review-people'];
    const earlier = waiting?.state === 'queued' ? (waiting.vars?.reviewers ?? '').split(', ').filter(Boolean) : [];
    this.enqueue(id, 'review-people', { reviewers: [...new Set([...earlier, ...logins])].join(', ') });
  }

  /** gh needs a directory in the repo; the worktree is gone (or another session's) once cleanup let go of it. */
  private ghCwd(row: SessionRow): string {
    if (existsSync(row.worktree_path) && this.sessions.ownsWorktree(row)) return row.worktree_path;
    return sessionRepo(this.db, row) ?? row.worktree_path;
  }

  // ---- polling ----

  private async pollAll(): Promise<void> {
    if (this.polling) return;
    this.polling = true;
    try {
      const now = Date.now();
      for (const row of sessionRows(this.db)) {
        if (!projectRow(this.db, row.project_id)) continue;
        const due = now - (this.lastPoll.get(row.id) ?? 0) >= REVIEW_POLL_MS;
        if (row.desired !== 'running') {
          // A cleanup that a restart cut short after archiving: finish it.
          if (row.stage === 'merge' && due && now - (row.archived_at ?? 0) < CLOSING_RESUME_MS) {
            await this.pollReview(row).catch((e) => this.warn(row.id, 'PR status failed', e));
          }
          continue;
        }
        if (row.stage === 'pr') {
          await this.pollPrStage(row).catch((e) => this.warn(row.id, 'PR lookup failed', e));
        } else if ((row.stage === 'review' || row.stage === 'merge') && due) {
          await this.pollReview(row).catch((e) => this.warn(row.id, 'PR status failed', e));
        } else if (row.stage === 'reviewing-pr' && due && !this.sessions.isCreating(row.id)) {
          // Not while its PR head is still being fetched: a push meanwhile isn't news, the fetch has it.
          await this.pollReviewedPr(row).catch((e) => this.warn(row.id, 'PR status failed', e));
        }
        // Prompts and comments queued before a restart, or while Claude was busy, waiting on the user or exited.
        await this.serial(row.id, () => this.deliverNext(row.id)).catch((e) => this.warn(row.id, 'Sending a queued prompt', e));
        await this.core.comments.deliver(row.id).catch((e) => this.warn(row.id, 'Sending review comments', e));
        await this.core.answers.deliver(row.id).catch((e) => this.warn(row.id, 'Sending answers to the design', e));
      }
    } finally {
      this.polling = false;
    }
  }

  private warn(id: string, what: string, e: unknown): void {
    const last = this.warned.get(id) ?? 0;
    if (Date.now() - last > 10 * 60 * 1000) {
      this.core.log.warn(`${what} for ${id}`, errMsg(e));
      this.warned.set(id, Date.now());
    }
  }

  pollPrStage(row: SessionRow): Promise<void> {
    return this.serial(row.id, () => this.doPollPrStage(row.id));
  }

  private async doPollPrStage(id: string): Promise<void> {
    const before = sessionRow(this.db, id);
    if (!before || before.stage !== 'pr' || before.desired !== 'running') return;
    // A session on a branch that existed before it takes that branch's open PR, however old.
    const pr = await github.findPr(before.worktree_path, before.branch, before.created_at, !!before.branch_reused);
    if (!pr || sessionRow(this.db, id)?.stage !== 'pr') return;
    // The PR it had before a move back to Code, merged or closed since: wait for the new one.
    if (pr.number === before.pr_number && pr.state !== 'OPEN') return;
    await this.adoptPr(id, pr, 'auto');
  }

  /** The session works on this PR from now on: it goes to Review and the PR is polled right away. */
  private async adoptPr(id: string, pr: github.PrRef, by: 'you' | 'auto'): Promise<void> {
    const before = this.sessions.row(id);
    if (before.pr_number === pr.number) {
      // Back on a PR it had before a move: its review state carries on, and it's not another PR for the stats.
      if (before.review_state === 'none') this.sessions.update(id, { review_state: 'waiting' });
      this.sessions.event(id, 'pr-again', `Back on PR #${pr.number}`, { url: pr.url });
    } else {
      this.sessions.update(id, { pr_number: pr.number, pr_url: pr.url, review_state: 'waiting', agentk_seen: 0, ci_fix_head: null, ci_fixes: 0, conflict_head: null, conflict_fixes: 0 });
      const older = by === 'you' || Date.parse(pr.createdAt) < before.created_at - github.CLOCK_SKEW_MS;
      this.sessions.event(id, 'pr', older ? `Picked up PR #${pr.number}` : `Opened PR #${pr.number}`, { url: pr.url });
    }
    // Opening the PR and its review are one step in the UI: "Opened PR #7" already says the session moved on.
    this.sessions.setStage(id, 'review', by, { event: before.stage !== 'pr' });
    await this.doPollReview(id);
  }

  pollReview(row: SessionRow): Promise<void> {
    return this.serial(row.id, () => this.doPollReview(row.id));
  }

  private async doPollReview(id: string): Promise<void> {
    const before = sessionRow(this.db, id);
    // Only Review and Merge & Close follow the PR: a session moved back to Code keeps its PR unwatched.
    if (!before?.pr_number || (before.stage !== 'review' && before.stage !== 'merge')) return;
    if (before.desired !== 'running' && before.stage !== 'merge') return;
    const p = projectRow(this.db, before.project_id);
    if (!p) return;
    this.lastPoll.set(id, Date.now());
    const st = await github.prStatus(this.ghCwd(before), before.pr_number);
    const unpushed = st.state === 'OPEN' && (await lacksLocalCommits(sessionRepo(this.db, before) ?? p.repo_path, before.branch, st));
    // Merge & Close ends in an archive, which would keep a dirty worktree on the branch.
    const uncommitted = this.sessions.ownsWorktree(before) ? await uncommittedCount(before.worktree_path) : 0;
    // Read the row again: a turn may have ended or a review been requested during the calls above.
    let row = sessionRow(this.db, id);
    if (!row || row.stage === 'done') return;
    if (st.state === 'MERGED') {
      await this.merged(row, st);
      return;
    }
    if (row.desired !== 'running') return;
    if (row.stage === 'merge' && !this.merging.has(id) && this.mergeStalled(id, st)) {
      this.sessions.event(id, 'merge', st.state === 'CLOSED'
        ? `PR #${st.number} was closed without merging`
        : `PR #${st.number} still isn't merged (it may have left the merge queue)`);
      this.sessions.setStage(id, 'review', 'auto');
      row = this.sessions.row(id);
    }
    const settings = parseSettings(p.settings_json);
    const mode = settings.reviewer;
    const ci = github.ciSummary(st.statusCheckRollup);
    let reviewState = row.review_state;
    const seen = new Set(JSON.parse(row.seen_review_ids) as string[]);
    const isAgentk = (a: { login: string } | null) => github.isLogin(a, settings.reviewerLogin);
    // On a public repo anyone can comment: only the user and people with a role on it can ask for AgentK.
    const asked = st.comments.filter((c) => REVIEW_REQUEST.test(c.body.trim()));
    const me = asked.some((c) => !github.isTrustedAuthor(c, settings.reviewerLogin)) ? await this.viewerLogin(this.ghCwd(before)) : null;
    const requests = asked.filter((c) => github.isTrustedAuthor(c, settings.reviewerLogin) || (!!me && github.isLogin(c.author, me)));
    const agentkSeen = !!row.agentk_seen || st.reviews.some((r) => isAgentk(r.author))
      || st.comments.some((c) => isAgentk(c.author)) || requests.length > 0;

    if (mode !== 'people') {
      const botReviews = st.reviews.filter((r) => isAgentk(r.author));
      const fresh = botReviews.filter((r) => !seen.has(r.id));
      if (fresh.length) {
        fresh.forEach((r) => seen.add(r.id));
        const latest = fresh[fresh.length - 1];
        const m = latest.body.match(/(\d+)\s+inline comments?/i);
        const findings = m ? Number(m[1]) : null;
        const firstLine = latest.body.split('\n').find((l) => l.trim())?.replace(/\*/g, '').trim() ?? 'Review posted';
        this.sessions.event(row.id, 'review', `AgentK review: ${firstLine}`);
        if (findings === 0 && !/could not be anchored|still open/i.test(latest.body)) {
          reviewState = 'approved';
          this.sessions.notifyUser(row.id, 'AgentK review', 'No findings.');
        } else {
          reviewState = 'addressing';
          this.sessions.notifyUser(row.id, 'AgentK review arrived', firstLine);
          this.enqueue(row.id, 'review');
        }
      } else if (reviewState === 'waiting' || reviewState === 'in_review' || reviewState === 'refused') {
        // AgentK talks through issue comments before the review: an ack, or a refusal. Only
        // comments after the last review and the last request count (a forced review follows a refusal).
        const latestReviewAt = Math.max(0, ...botReviews.map((r) => Date.parse(r.submittedAt)));
        const requestedAt = Math.max(0, ...requests.map((c) => Date.parse(c.createdAt)));
        const since = Math.max(latestReviewAt, requestedAt);
        const botComment = [...st.comments].reverse().find((c) => isAgentk(c.author) && Date.parse(c.createdAt) > since);
        if (botComment) {
          if (/no review/i.test(botComment.body)) reviewState = 'refused';
          else if (/started reviewing|reviewing/i.test(botComment.body)) reviewState = 'in_review';
        }
      }
    }

    // People, and in All mode other bots too. AgentK's reviews are never counted here.
    const others = st.reviews.filter((r) => r.author && !isAgentk(r.author) && !seen.has(r.id));
    const bots = others.length ? await this.botLogins(before, st.number) : [];
    const isBot = (r: github.PrReview) => !!bots?.some((b) => github.isLogin(r.author, b));
    // Replies on review threads (Claude's too: it posts as the user) are empty COMMENTED reviews.
    const worded = others.filter((r) => (mode === 'all' || !isBot(r)) && (r.state !== 'COMMENTED' || r.body.trim()));
    // Anyone can review a public repo's PR. Only people with a role on it, bots (apps installed
    // on it) and the user get to start a turn; the rest are shown to the user, not to Claude.
    const outside = bots ? worded.filter((r) => !isBot(r) && !github.isTrustedAuthor(r, settings.reviewerLogin)) : [];
    const user = outside.length ? await this.viewerLogin(this.ghCwd(before)) : null;
    // Like the bot lookup, an unknown login leaves the reviews unseen for the next poll, so the
    // user's own review is never dropped as a stranger's.
    if (others.length && bots && (user || !outside.length)) {
      others.forEach((r) => seen.add(r.id));
      const ignored = outside.filter((r) => !user || !github.isLogin(r.author, user));
      const trusted = worded.filter((r) => !ignored.includes(r));
      if (ignored.length) {
        const names = loginList(ignored);
        this.sessions.event(row.id, 'review', `Ignored review from ${names} (not a collaborator on the repo)`);
        this.sessions.notifyUser(row.id, 'Review ignored', `From ${names}, not a collaborator on the repo. AgentK won't act on it; read it on GitHub.`);
      }
      if (trusted.length) {
        const names = loginList(trusted);
        this.sessions.event(row.id, 'review', `Review from ${names}`);
        if (trusted.some((r) => r.state === 'CHANGES_REQUESTED' || r.state === 'COMMENTED')) {
          reviewState = 'addressing';
          this.sessions.notifyUser(row.id, 'Review arrived', `From ${names}.`);
          this.enqueuePeople(row.id, trusted.map((r) => r.author!.login));
        }
      }
    }
    const awaitingAgentk = mode !== 'people' && agentkSeen && (reviewState === 'waiting' || reviewState === 'in_review');
    if (st.reviewDecision === 'APPROVED' && reviewState !== 'addressing' && !awaitingAgentk) reviewState = 'approved';
    else if (reviewState === 'none') reviewState = 'waiting';

    const current = this.sessions.row(row.id);
    const finalReview = current.review_state === 'addressing' && reviewState !== 'addressing' ? current.review_state : reviewState;
    this.sessions.update(row.id, {
      ci_state: ci.state,
      review_state: finalReview,
      merge_state: st.mergeStateStatus,
      seen_review_ids: JSON.stringify([...seen]),
      agentk_seen: agentkSeen ? 1 : 0,
    });
    this.watchConflicts(this.sessions.row(row.id), st, unpushed);
    this.watchCi(this.sessions.row(row.id), st, ci, unpushed);
    // A failed send stays queued; the next poll or turn end retries it.
    await this.deliverNext(row.id).catch((e) => this.warn(row.id, 'Sending a queued prompt', e));
    this.sessions.setMergeInfo(row.id, this.mergeInfo(this.sessions.row(row.id), st, mode, unpushed, uncommitted));
    this.core.changed();
  }

  /** The PR's bot reviewers, or null (retry at the next poll) when GitHub can't say. */
  private async botLogins(row: SessionRow, pr: number): Promise<string[] | null> {
    try {
      return await github.botLogins(this.ghCwd(row), pr);
    } catch (e) {
      this.warn(row.id, 'Looking up bot reviewers', e);
      return null;
    }
  }

  /**
   * Queues a CI-fix prompt when CI fails on the PR's head: once per head, after the checks
   * finish (or the failure has waited CI_SETTLE_MS), never while the branch has unpushed
   * commits, and for at most MAX_CI_FIXES heads in a row before it's left to the user.
   */
  private watchCi(row: SessionRow, st: github.PrStatus, ci: github.CiSummary, unpushed: boolean): void {
    const id = row.id;
    const head = st.headRefOid;
    const q = this.queue(id);
    // A queued fix for a head that moved on, or for CI that's no longer failing, is stale.
    if (q.ci?.state === 'queued' && (ci.state !== 'failure' || q.ci.vars?.sha !== head)) {
      delete q.ci;
      this.setQueue(id, q);
    }
    if (ci.state === 'success') {
      this.ciFailSince.delete(id);
      if (row.ci_fixes) this.sessions.update(id, { ci_fixes: 0 });
      return;
    }
    if (ci.state !== 'failure' || !head || row.ci_fix_head === head || row.stage !== 'review' || st.state !== 'OPEN') return;
    let since = this.ciFailSince.get(id);
    if (since?.head !== head) this.ciFailSince.set(id, (since = { head, at: Date.now() }));
    if ((ci.pending && Date.now() - since.at < CI_SETTLE_MS) || unpushed) return;
    this.ciFailSince.delete(id);
    const names = ci.failed.map((c) => c.name).join(', ');
    if (row.ci_fixes >= MAX_CI_FIXES) {
      this.sessions.update(id, { ci_fix_head: head });
      this.sessions.event(id, 'ci', `CI failed: ${names}. Still failing after ${MAX_CI_FIXES} fix attempts; over to you`, { sha: head, checks: ci.failed });
      this.sessions.notifyUser(id, 'CI still failing', `${names} failed after ${MAX_CI_FIXES} fix attempts. Take a look.`);
      return;
    }
    this.sessions.update(id, { ci_fix_head: head, ci_fixes: row.ci_fixes + 1 });
    this.sessions.event(id, 'ci', `CI failed: ${names}`, { sha: head, checks: ci.failed });
    const checks = ci.failed.map((c) => `- ${c.name}: ${c.conclusion.toLowerCase()}${c.url ? ` (${c.url})` : ''}`).join('\n');
    this.enqueue(id, 'ci', { sha: head, checks });
    const next = this.queue(id);
    const later = next.review || next['review-people'] ? 'AgentK will fix it after the review.'
      : next.conflicts ? 'AgentK will look at it after resolving the merge conflicts.' : 'AgentK is fixing it.';
    this.sessions.notifyUser(id, 'CI failed', `${names}. ${later}`);
  }

  /**
   * Queues a prompt to resolve merge conflicts when GitHub says the PR's head conflicts with its
   * base: once per head, never while the branch has unpushed commits, and for at most
   * MAX_CONFLICT_FIXES heads in a row before it's left to the user. `[pr] resolve_conflicts` (Settings
   * → General) turns it off: the conflicts stay a merge blocker, and deliverNext drops a queued prompt.
   */
  private watchConflicts(row: SessionRow, st: github.PrStatus, unpushed: boolean): void {
    const id = row.id;
    const head = st.headRefOid;
    const on = this.core.config.pr.resolveConflicts;
    const q = this.queue(id);
    // Stale: turned off, the head moved on, or the PR merges cleanly now. UNKNOWN isn't an answer
    // (GitHub works it out after a push or a base change), so it keeps a queued prompt.
    if (q.conflicts?.state === 'queued' && (!on || st.mergeable === 'MERGEABLE' || q.conflicts.vars?.sha !== head)) {
      delete q.conflicts;
      this.setQueue(id, q);
    }
    if (st.mergeable === 'MERGEABLE') {
      // A later conflict on this head (the base moved again) is new, and starts the count again.
      if (row.conflict_fixes || row.conflict_head) this.sessions.update(id, { conflict_head: null, conflict_fixes: 0 });
      return;
    }
    if (!on || st.mergeable !== 'CONFLICTING' || !head || row.conflict_head === head || row.stage !== 'review' || st.state !== 'OPEN' || unpushed) return;
    const base = row.base_branch;
    if (row.conflict_fixes >= MAX_CONFLICT_FIXES) {
      this.sessions.update(id, { conflict_head: head });
      this.sessions.event(id, 'conflicts', `Merge conflicts with ${base}. Still there after ${MAX_CONFLICT_FIXES} attempts; over to you`, { sha: head });
      this.sessions.notifyUser(id, 'Merge conflicts remain', `PR #${st.number} still conflicts with ${base} after ${MAX_CONFLICT_FIXES} attempts. Take a look.`);
      return;
    }
    this.sessions.update(id, { conflict_head: head, conflict_fixes: row.conflict_fixes + 1 });
    this.sessions.event(id, 'conflicts', `Merge conflicts with ${base}`, { sha: head });
    this.enqueue(id, 'conflicts', { sha: head });
    const next = this.queue(id);
    const afterReview = !!(next.review || next['review-people']);
    this.sessions.notifyUser(id, 'Merge conflicts', `PR #${st.number} conflicts with ${base}. ${afterReview ? 'AgentK will resolve them after the review.' : 'AgentK is resolving them.'}`);
  }

  /** A requested merge that didn't happen: the PR was closed, or is still open well after the request. */
  private mergeStalled(id: string, st: github.PrStatus): boolean {
    if (st.state === 'CLOSED') return true;
    const since = get<{ entered_at: number }>(this.db,
      'SELECT entered_at FROM session_stages WHERE session_id = ? AND left_at IS NULL ORDER BY rowid DESC LIMIT 1', id)?.entered_at;
    return since !== undefined && Date.now() - since > MERGE_GRACE_MS;
  }

  /**
   * Types the first queued prompt (reviews first, then merge conflicts, then CI) into Claude when it's between turns
   * in the Review stage, or a Review a PR session's update summary in Reviewing PR. One at a time:
   * the next waits until the turn that got one has ended.
   */
  private async deliverNext(id: string): Promise<void> {
    const q = this.queue(id);
    // Turned off since it was queued: never sent, even when the poll that would drop it failed.
    if (q.conflicts?.state === 'queued' && !this.core.config.pr.resolveConflicts) {
      delete q.conflicts;
      this.setQueue(id, q);
    }
    const row = this.sessions.row(id);
    const order = queueOrder(row.stage);
    const name = order.find((n) => q[n]?.state === 'queued');
    if (!name) return;
    if (row.desired !== 'running' || (row.status !== 'idle' && row.status !== 'recovered')) return;
    if (this.sessions.runtime(id).busy) return;
    const sent = order.filter((n) => q[n]?.state === 'sent');
    if (sent.length) {
      // The status goes idle a moment before the turn's Stop is handled, and that clears it.
      if (row.status !== 'recovered') return;
      // Claude was relaunched after a restart: the turn that got it is over.
      sent.forEach((n) => delete q[n]);
    }
    const entry = q[name]!;
    // Marked first: the turn can end before submit returns, and its Stop must not send it again.
    q[name] = { ...entry, state: 'sent' };
    this.setQueue(id, q);
    try {
      await this.sessions.submit(id, this.sessions.prompt(row, name, entry.vars ?? {}));
    } catch (e) {
      const back = this.queue(id);
      if (back[name]?.state === 'sent') {
        back[name] = entry;
        this.setQueue(id, back);
      }
      throw e;
    }
  }

  mergeInfo(row: SessionRow, st: github.PrStatus, reviewer: ReviewerMode, unpushed = false, uncommitted = 0): { canMerge: boolean; blockers: string[] } {
    const b: string[] = [];
    if (st.state !== 'OPEN') b.push('The PR is not open.');
    if (row.status === 'working' || this.sessions.runtime(row.id).busy) b.push('AgentK is still working.');
    else if (row.status === 'needs_input') b.push('AgentK is waiting on you in the terminal.');
    if (unpushed) b.push("The branch has commits that aren't pushed to the PR.");
    if (uncommitted) b.push(`The worktree has ${uncommitted === 1 ? '1 uncommitted file' : `${uncommitted} uncommitted files`}: commit or discard them.`);
    const ci = github.ciState(st.statusCheckRollup);
    if (ci === 'failure') b.push('CI is failing.');
    else if (ci === 'pending') b.push('CI is still running.');
    // AgentK gates the merge only once it's active on the PR, so repos it doesn't watch never wait for it.
    if (reviewer !== 'people' && row.agentk_seen && (row.review_state === 'waiting' || row.review_state === 'in_review')) {
      b.push('Waiting for the AgentK review.');
    }
    if (row.review_state === 'addressing') b.push('AgentK is addressing review findings.');
    if (st.reviewDecision === 'CHANGES_REQUESTED') b.push('Changes were requested.');
    if (st.reviewDecision === 'REVIEW_REQUIRED') b.push('Required approvals are missing.');
    if (st.mergeable === 'CONFLICTING') b.push('The branch has merge conflicts.');
    if (st.mergeStateStatus === 'BEHIND') b.push('The branch is behind its base.');
    if (st.mergeStateStatus === 'DRAFT') b.push('The PR is a draft.');
    return { canMerge: b.length === 0, blockers: b };
  }

  async requestReview(row: SessionRow, force: boolean): Promise<void> {
    if (row.stage !== 'review' || !row.pr_number) throw new HttpError(409, 'There is no open PR to review.');
    if (force && row.review_state !== 'refused') throw new HttpError(409, 'Force review is only for PRs AgentK declined.');
    const pr = row.pr_number;
    await this.serial(row.id, async () => {
      await github.comment(row.worktree_path, pr, force ? '/agentk review force' : '/agentk review');
      this.sessions.update(row.id, { review_state: 'waiting', agentk_seen: 1 });
      this.sessions.event(row.id, 'review', force ? 'Forced an AgentK review' : 'Requested another AgentK review');
      this.lastPoll.delete(row.id);
    });
  }

  // ---- Review a PR ----

  pollReviewedPr(row: SessionRow): Promise<void> {
    return this.serial(row.id, () => this.doPollReviewedPr(row.id));
  }

  /**
   * A Review a PR session's look at its PR: new commits on it (once per head, with a notification),
   * the PR merged, closed or reopened, and reviews the user posted. Nothing goes to Claude by
   * itself; the summary after Update to latest goes out from here once Claude is between turns.
   */
  private async doPollReviewedPr(id: string): Promise<void> {
    const before = sessionRow(this.db, id);
    if (!before || before.kind !== 'review-pr' || before.stage !== 'reviewing-pr' || before.desired !== 'running') return;
    if (!before.pr_number || !before.pr_repo) return;
    this.lastPoll.set(id, Date.now());
    const cwd = this.ghCwd(before);
    const st = await github.prWatch(cwd, before.pr_repo, before.pr_number);
    const me = await this.viewerLogin(cwd);
    // Read the row again: Update to latest runs in the same serial queue, but an archive or Close
    // review doesn't, and may have ended the session during the calls above.
    const row = sessionRow(this.db, id);
    if (!row || row.stage !== 'reviewing-pr' || row.desired !== 'running') return;
    const n = row.pr_number;
    const fields: Partial<Record<keyof SessionRow, string | number | null>> = {};
    if (st.state !== row.pr_state) {
      fields.pr_state = st.state;
      if (st.state === 'MERGED' || st.state === 'CLOSED') {
        const what = st.state === 'MERGED' ? 'merged' : 'closed';
        this.sessions.event(id, 'pr-state', `PR #${n} was ${what}`);
        this.sessions.notifyUser(id, `PR ${what}`, `PR #${n} was ${what}. Close the review when you're done with it.`);
      } else if (row.pr_state) {
        this.sessions.event(id, 'pr-state', `PR #${n} was reopened`);
      }
    }
    if (st.headRefOid && st.headRefOid !== row.pr_head_sha) {
      fields.pr_head_sha = st.headRefOid;
      if (st.headRefOid === row.review_sha) {
        fields.pr_new_commits = 0;
      } else {
        const count = commitsAfter(st.commits, row.review_sha);
        fields.pr_new_commits = count;
        const what = count === null ? 'was force-pushed' : `has ${count === 1 ? 'a new commit' : `${count} new commits`}`;
        this.sessions.event(id, 'pr-update', `PR #${n} ${what}`, { sha: st.headRefOid });
        this.sessions.notifyUser(id, 'New commits', `PR #${n} ${what}. Update to latest when you're ready.`);
      }
    }
    const seen = new Set(JSON.parse(row.seen_review_ids) as string[]);
    for (const r of me ? st.reviews.filter((r) => github.isLogin(r.author, me) && !seen.has(r.id)) : []) {
      seen.add(r.id);
      const verb = REVIEW_VERBS[r.state];
      // Replies on threads are empty COMMENTED reviews; reviews from before the session aren't news.
      if (!verb || (r.state === 'COMMENTED' && !r.body.trim()) || Date.parse(r.submittedAt) < row.created_at - github.CLOCK_SKEW_MS) continue;
      this.sessions.event(id, 'review', `You ${verb} PR #${n}`);
    }
    fields.seen_review_ids = JSON.stringify([...seen]);
    this.sessions.update(id, fields);
    await this.deliverNext(id).catch((e) => this.warn(id, 'Sending a queued prompt', e));
    this.core.changed();
  }

  /** gh's login, looked up once; null (and asked again next time) when gh can't say. */
  private async viewerLogin(cwd: string): Promise<string | null> {
    if (this.ghLogin) return this.ghLogin;
    try {
      this.ghLogin = (await github.viewerLogin(cwd)) || null;
    } catch (e) {
      this.core.log.warn('looking up the GitHub login failed', errMsg(e));
    }
    return this.ghLogin;
  }

  /**
   * Update to latest: checks out the PR's head in a Review a PR session and has AgentK sum up what
   * changed since the commit reviewed so far (queued until Claude is between turns). Refused while
   * AgentK is busy, the worktree has uncommitted files, or the branch has commits of the user's own.
   */
  async updatePr(row: SessionRow): Promise<void> {
    if (row.kind !== 'review-pr' || row.stage !== 'reviewing-pr' || !row.pr_number || !row.pr_repo || !row.review_sha) {
      throw new HttpError(409, "Only a Review a PR session can update to its PR's latest commits.");
    }
    await this.serial(row.id, async () => {
      const cur = this.sessions.row(row.id);
      if (cur.desired !== 'running') throw new HttpError(409, 'This session is archived.');
      if (cur.status === 'working' || cur.status === 'starting' || this.sessions.runtime(cur.id).busy) throw new HttpError(409, 'Wait for AgentK to finish its turn.');
      if (cur.status === 'needs_input') throw new HttpError(409, 'AgentK is waiting on you in the terminal. Answer it first.');
      if (cur.status === 'exited') throw new HttpError(409, 'Claude has exited. Resume the session first.');
      const n = cur.pr_number!;
      const old = cur.review_sha!;
      const repo = sessionRepo(this.db, cur);
      if (!repo || !this.sessions.ownsWorktree(cur) || !existsSync(cur.worktree_path)) throw new HttpError(409, "This session's worktree is gone.");
      const remote = await remoteFor(repo, cur.pr_repo!);
      if (!remote) throw new HttpError(409, `${basename(repo)} has no remote for ${cur.pr_repo} any more, so PR #${n} can't be fetched.`);
      const dirty = await uncommittedCount(cur.worktree_path);
      if (dirty) {
        throw new HttpError(409, `The worktree has ${dirty === 1 ? '1 uncommitted file' : `${dirty} uncommitted files`}. Commit or discard ${dirty === 1 ? 'it' : 'them'}, then update.`);
      }
      const own = await commitsBeyond(repo, cur.branch, old);
      if (own === null) throw new HttpError(409, `Couldn't tell whether ${cur.branch} has commits of yours on top of the PR, so it wasn't moved.`);
      if (own) {
        throw new HttpError(409, `${cur.branch} has ${own === 1 ? 'a commit' : `${own} commits`} of yours on top of the PR. Move ${own === 1 ? 'it' : 'them'} to another branch, or drop ${own === 1 ? 'it' : 'them'}, then update.`);
      }
      let sha: string;
      try {
        sha = await fetchPrHead(repo, remote, n, cur.base_branch);
        if (remote !== cur.pr_remote) this.sessions.update(cur.id, { pr_remote: remote });
      } catch (e) {
        throw new HttpError(502, `Couldn't fetch PR #${n} from ${remote}: ${errMsg(e)}`);
      }
      if (sha === old) {
        this.sessions.update(cur.id, { pr_head_sha: sha, pr_new_commits: 0 });
        return;
      }
      const count = await commitsBetween(repo, old, sha);
      await moveReviewBranch(cur.worktree_path, sha);
      this.sessions.update(cur.id, { review_sha: sha, pr_head_sha: sha, pr_new_commits: 0 });
      this.sessions.event(cur.id, 'pr-update', `Updated to PR #${n} at ${sha.slice(0, 7)}`, { sha });
      this.enqueue(cur.id, 'review-pr-update', {
        oldSha: old,
        sha,
        count: count === null ? 'its history was rewritten, a force-push' : `${count === 1 ? '1 new commit' : `${count} new commits`}`,
      });
      await this.deliverNext(cur.id);
      void this.sessions.refreshDiff(cur.id).catch(() => undefined);
    });
  }

  // ---- merge / cleanup ----

  async merge(row: SessionRow): Promise<void> {
    if (row.stage !== 'review' || !row.pr_number || !row.pr_url) throw new HttpError(409, 'Merge is only available in the Review stage.');
    const p = projectRow(this.db, row.project_id);
    if (!p) throw new HttpError(404, 'Project not found.');
    const settings = parseSettings(p.settings_json);
    const url = row.pr_url;
    if (this.merging.has(row.id)) throw new HttpError(409, 'A merge is already in progress.');
    this.merging.add(row.id);
    try {
      await this.serial(row.id, async () => {
        await this.doPollReview(row.id);
        const fresh = this.sessions.row(row.id);
        if (fresh.stage !== 'review') return; // merged elsewhere and already cleaned up
        const info = this.sessions.runtime(row.id);
        if (!info.canMerge) throw new HttpError(409, `Not ready to merge: ${info.mergeBlockers.join(' ')}`);
        const method = await github.resolveMergeMethod(row.worktree_path, settings.mergeMethod);
        this.sessions.setBusy(row.id, true);
        try {
          this.sessions.setStage(row.id, 'merge', 'you');
          await github.merge(row.worktree_path, url, method);
        } catch (e) {
          if (this.sessions.row(row.id).stage === 'merge') this.sessions.setStage(row.id, 'review', 'auto');
          throw new HttpError(502, `GitHub refused the merge: ${errMsg(e)}`);
        } finally {
          this.sessions.setBusy(row.id, false);
        }
        await this.doPollReview(row.id);
        if (this.sessions.row(row.id).stage === 'merge') {
          this.sessions.event(row.id, 'merge', `Merge requested; waiting for GitHub to merge PR #${fresh.pr_number}`);
        }
      });
    } finally {
      this.merging.delete(row.id);
    }
  }

  /** The PR is merged (here, or on GitHub): finish Merge & Close. Safe to call again after a restart. */
  private async merged(row: SessionRow, st: github.PrStatus): Promise<void> {
    if (row.stage === 'done' || this.closing.has(row.id)) return;
    const announced = !!get(this.db, `SELECT 1 AS x FROM session_events WHERE project_id = ? AND session_id = ? AND kind = 'merged'`,
      row.project_id, row.id);
    if (row.desired === 'running' && (row.status === 'working' || row.status === 'starting' || this.sessions.runtime(row.id).busy)) {
      // Stopping Claude mid-turn could lose its work: onTurnEnd closes the session when the turn ends.
      if (!announced) {
        this.sessions.event(row.id, 'merged', `PR #${st.number} merged; closing when AgentK finishes`, { url: st.url });
        await this.recordPrStats(row, st);
      }
      if (row.stage !== 'merge') this.sessions.setStage(row.id, 'merge', 'auto');
      return;
    }
    this.closing.add(row.id);
    try {
      if (!announced) {
        this.sessions.event(row.id, 'merged', `PR #${st.number} merged`, { url: st.url });
        await this.recordPrStats(row, st);
      }
      if (row.stage !== 'merge') this.sessions.setStage(row.id, 'merge', 'auto');
      await this.cleanup(this.sessions.row(row.id), st);
    } finally {
      this.closing.delete(row.id);
    }
  }

  /**
   * Adds the PR's size to its `merged` event, for the usage stats: commits, lines, and files per
   * extension (never the paths). Best-effort: a failure leaves the event as it was.
   */
  private async recordPrStats(row: SessionRow, st: github.PrStatus): Promise<void> {
    try {
      const cwd = sessionRepo(this.db, row) ?? row.worktree_path;
      const pr = await github.prStats(cwd, st.url);
      run(this.db, `UPDATE session_events SET payload_json = json_set(COALESCE(payload_json, '{}'),
                      '$.commits', ?, '$.additions', ?, '$.deletions', ?, '$.fileTypes', json(?))
                    WHERE session_id = ? AND kind = 'merged'`,
        pr.commits, pr.additions, pr.deletions, JSON.stringify(countFileTypes(pr.files)), row.id);
    } catch (e) {
      this.core.log.warn(`reading the size of PR #${st.number} failed for ${row.id}`, errMsg(e));
    }
  }

  /**
   * Records what shipped in the datapack, then lets go of the worktree (idle for the next session,
   * or removed) and removes the branches. Branches with commits the merged PR lacks are kept.
   */
  async cleanup(row: SessionRow, st: github.PrStatus): Promise<void> {
    const p = projectRow(this.db, row.project_id);
    const dp = sessionDatapack(this.db, row);
    // Once per session: a cleanup a restart cut short, or one that waited for Claude's turn while
    // the user archived by hand, may run again.
    const shipped = !!get(this.db, "SELECT 1 AS x FROM session_events WHERE session_id = ? AND kind = 'shipped'", row.id);
    if (p && dp && !shipped && this.core.mekoStatus.configured) {
      try {
        const conv = await this.sessions.conversationId(row, dp);
        const body = st.body.trim().slice(0, 4000);
        this.sessions.ingestUsage(row.id);
        const u = this.sessions.runtime(row.id).usage;
        const tokens = `Token usage: ${u.output} output, ${u.input} input, ${u.cacheWrite} cache write, ${u.cacheRead} cache read tokens over ${u.messages} responses.`;
        await this.core.meko.addMemory(
          `Shipped in ${p.name}: "${st.title}" (PR #${st.number}, ${st.url}), branch ${row.branch}. ${tokens}${body ? `\n\n${body}` : ''}`,
          dp.id, launchedAgentId(row), conv,
          { source: 'agent-k-local', kind: 'shipped', pr: st.url, session: row.id },
        );
        this.sessions.event(row.id, 'shipped', `Saved what shipped to ${dp.name}`);
      } catch (e) {
        this.core.log.warn(`saving the shipped summary to Meko failed for ${row.id}`, errMsg(e));
      }
    }
    const repo = p ? (sessionRepo(this.db, row) ?? p.repo_path) : null;
    const keep = !!repo && (await lacksLocalCommits(repo, row.branch, st));
    const { worktreeFreed, worktree } = await this.sessions.archive(row, { keepBranch: true });
    if (repo) {
      if (worktreeFreed && !keep) await deleteBranch(repo, row.branch);
      if (!keep && st.headRefOid) {
        // Often already deleted by GitHub. The lease deletes it only while it's still the PR's head.
        await tryGit(repo, 'push', `--force-with-lease=refs/heads/${row.branch}:${st.headRefOid}`, 'origin', '--delete', row.branch);
      }
      await git(repo, 'fetch', '--prune', '--quiet', 'origin').catch(() => undefined);
    }
    if (keep) this.core.log.info(`kept branch ${row.branch} of ${row.id}: it has commits PR #${st.number} doesn't have`);
    this.sessions.setStage(row.id, 'done', 'auto');
    const wt = worktree === 'idle' ? 'worktree kept for the next session' : worktree === 'removed' ? 'worktree removed' : '';
    this.sessions.event(row.id, 'cleanup', !worktreeFreed
      ? `Cleaned up; worktree kept at ${row.worktree_path} because it has uncommitted changes`
      : keep
        ? `Cleaned up: ${wt ? `${wt}; ` : ''}branch ${row.branch} kept because it has commits PR #${st.number} doesn't have`
        : worktree === 'removed'
          ? `Cleaned up: worktree and branch ${row.branch} removed`
          : `Cleaned up: branch ${row.branch} removed${wt ? `; ${wt}` : ''}`);
    this.core.changed();
  }
}
