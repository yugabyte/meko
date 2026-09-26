import { existsSync } from 'node:fs';
import type { Core } from './core.js';
import { get, run } from './db.js';
import { artifactUrlIn, isArtifactPublish, readArtifactHtml, readDesignFile } from './design.js';
import { deleteBranch, git, tryGit } from './git.js';
import * as github from './github.js';
import { errMsg } from './log.js';
import { projectAgentId } from './meko.js';
import type { HookPayload } from './hooks.js';
import { HttpError } from './projects.js';
import type { ReviewerMode } from '../shared/types.js';
import { dedicatedDatapack, parseSettings, projectRow, sessionRow, sessionRows, type SessionRow } from './store.js';

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

/** The status detail while a markdown or Artifact design waits for approval. */
export const DESIGN_READY = 'Design ready for your approval';

/** Prompts queued for Claude in the Review stage, in the order they go out. */
const QUEUE_ORDER = ['review', 'review-people', 'ci'] as const;
type QueuedPrompt = (typeof QUEUE_ORDER)[number];
/** `sessions.pending_prompts`: kept across restarts; a prompt is `sent` until the turn that got it ends. */
type Queue = Partial<Record<QueuedPrompt, { state: 'queued' | 'sent'; vars?: Record<string, string> }>>;

const REVIEW_REQUEST = /^\/agentk review\b/;

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
 * The task lifecycle: Design → Code → PR → Review → Merge → Cleanup.
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
  /** Per session, the failing head whose checks are still running, and when its failure was first seen. */
  private ciFailSince = new Map<string, { head: string; at: number }>();

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
    run(this.db, `INSERT INTO designs(session_id, markdown, html, artifact_url, updated_at) VALUES (?, ?, ?, ?, ?)
                  ON CONFLICT(session_id) DO UPDATE SET markdown = excluded.markdown, html = excluded.html,
                    artifact_url = excluded.artifact_url, updated_at = excluded.updated_at`, id, markdown, html, url, Date.now());
    this.core.changed();
    const row = sessionRow(this.db, id);
    const dp = row && dedicatedDatapack(this.db, row.project_id);
    if (!row || !dp) return;
    const files: [string, string, string][] = [];
    if (d.markdown !== undefined) files.push([`design-${row.slug}.md`, d.markdown, 'text/markdown']);
    if (d.html !== undefined) files.push([`design-${row.slug}.html`, d.html, 'text/html']);
    if (!files.length) return;
    this.core.projects.ensureConversation(row.project_id)
      .then((conv) => Promise.all(files.map(([name, text, type]) =>
        this.core.meko.putArtifact(name, text, type, dp.id, projectAgentId(row.project_id), conv))))
      .catch((e) => this.core.log.warn(`saving design to Meko failed for ${id}`, errMsg(e)));
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
    return this.designChanged.delete(row.id);
  }

  /** The design is waiting for the user: say so in the activity log, the header and a notification. */
  designReady(id: string): void {
    this.sessions.event(id, 'design', 'Design ready for approval');
    this.sessions.setStatus(id, 'needs_input', DESIGN_READY);
    this.sessions.notifyUser(id, 'Design ready', 'Review and approve the design.');
  }

  async approveDesign(row: SessionRow): Promise<void> {
    if (row.stage !== 'design') throw new HttpError(409, 'This session is past the Design stage.');
    if (row.plan_mode) {
      if (!this.sessions.planPending.has(row.id)) {
        throw new HttpError(409, "Claude hasn't presented a design for approval yet. Ask it to finish the design, or approve it in the terminal.");
      }
      // Claude's plan dialog has "Yes" selected by default.
      await this.core.tmux.sendKeys(row.tmux_name, 'Enter');
      this.sessions.planPending.delete(row.id);
      this.designApproved(row.id, 'you');
      return;
    }
    if (!get(this.db, 'SELECT 1 AS x FROM designs WHERE session_id = ?', row.id)) {
      throw new HttpError(409, "Claude hasn't written a design yet. Wait for it to finish, or ask it in the terminal.");
    }
    if (row.status === 'working' || row.status === 'starting') throw new HttpError(409, 'Claude is still working. Wait for the turn to finish.');
    if (row.status === 'exited') throw new HttpError(409, 'Claude has exited. Resume the session first.');
    // Any other question (a permission prompt) would take the approval as its answer.
    if (row.status === 'needs_input' && row.status_detail !== DESIGN_READY) {
      throw new HttpError(409, 'Claude is waiting on you in the terminal. Answer it first.');
    }
    this.designApproved(row.id, 'you');
    try {
      await this.sessions.submit(row.id, this.sessions.prompt(this.sessions.row(row.id), 'approved'));
    } catch (e) {
      if (this.sessions.row(row.id).stage === 'code') this.sessions.setStage(row.id, 'design', 'auto');
      throw new HttpError(502, `Couldn't type the approval into Claude's terminal: ${errMsg(e)}. Try again.`);
    }
  }

  designApproved(id: string, by: 'you' | 'auto'): void {
    const row = sessionRow(this.db, id);
    if (!row || row.stage !== 'design') return;
    this.sessions.setStage(id, 'code', by);
  }

  // ---- PR ----

  async openPr(row: SessionRow): Promise<void> {
    if (row.stage !== 'code') throw new HttpError(409, 'Open PR is only available in the Code stage.');
    if (row.status === 'working' || row.status === 'starting' || this.sessions.runtime(row.id).busy) {
      throw new HttpError(409, 'Claude is still working. Wait for the turn to finish.');
    }
    if (row.status === 'needs_input') throw new HttpError(409, 'Claude is waiting on you in the terminal. Answer it first.');
    if (row.status === 'exited') throw new HttpError(409, 'Claude has exited. Resume the session first.');
    // The stage moves first: Claude's turn can end before submit returns.
    this.sessions.setStage(row.id, 'pr', 'you');
    this.sessions.setBusy(row.id, true);
    try {
      await this.sessions.submit(row.id, this.sessions.prompt(row, 'pr'));
    } catch (e) {
      this.sessions.setBusy(row.id, false);
      if (this.sessions.row(row.id).stage === 'pr') this.sessions.setStage(row.id, 'code', 'auto');
      throw new HttpError(502, `Couldn't type the PR prompt into Claude's terminal: ${errMsg(e)}. Try again.`);
    }
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
    }
    // Review comments go after anything above that already gave Claude its next turn.
    await this.core.comments.deliver(id).catch((e) => this.warn(id, 'Sending review comments', e));
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

  /** gh needs a directory in the repo; the worktree is gone once cleanup removed it. */
  private ghCwd(row: SessionRow): string {
    if (existsSync(row.worktree_path)) return row.worktree_path;
    return projectRow(this.db, row.project_id)?.repo_path ?? row.worktree_path;
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
        }
        // Prompts and comments queued before a restart, or while Claude was busy, waiting on the user or exited.
        await this.serial(row.id, () => this.deliverNext(row.id)).catch((e) => this.warn(row.id, 'Sending a queued prompt', e));
        await this.core.comments.deliver(row.id).catch((e) => this.warn(row.id, 'Sending review comments', e));
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
    const pr = await github.findPr(before.worktree_path, before.branch, before.created_at);
    if (!pr || sessionRow(this.db, id)?.stage !== 'pr') return;
    this.sessions.update(id, { pr_number: pr.number, pr_url: pr.url, review_state: 'waiting' });
    this.sessions.event(id, 'pr', `Opened PR #${pr.number}`, { url: pr.url });
    this.sessions.setStage(id, 'review', 'auto');
    await this.doPollReview(id);
  }

  pollReview(row: SessionRow): Promise<void> {
    return this.serial(row.id, () => this.doPollReview(row.id));
  }

  private async doPollReview(id: string): Promise<void> {
    const before = sessionRow(this.db, id);
    if (!before?.pr_number || before.stage === 'done') return;
    if (before.desired !== 'running' && before.stage !== 'merge') return;
    const p = projectRow(this.db, before.project_id);
    if (!p) return;
    this.lastPoll.set(id, Date.now());
    const st = await github.prStatus(this.ghCwd(before), before.pr_number);
    const unpushed = st.state === 'OPEN' && (await lacksLocalCommits(p.repo_path, before.branch, st));
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
    const agentkSeen = !!row.agentk_seen || st.reviews.some((r) => isAgentk(r.author))
      || st.comments.some((c) => isAgentk(c.author) || REVIEW_REQUEST.test(c.body.trim()));

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
        const requestedAt = Math.max(0, ...st.comments.filter((c) => REVIEW_REQUEST.test(c.body.trim())).map((c) => Date.parse(c.createdAt)));
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
    const bots = others.length && mode !== 'all' ? await this.botLogins(before, st.number) : [];
    if (others.length && bots) {
      others.forEach((r) => seen.add(r.id));
      const counted = others.filter((r) => !bots.some((b) => github.isLogin(r.author, b)));
      // Replies on review threads (Claude's too: it posts as the user) are empty COMMENTED reviews.
      const worded = counted.filter((r) => r.state !== 'COMMENTED' || r.body.trim());
      if (worded.length) {
        const names = [...new Set(worded.map((r) => r.author?.login))].join(', ');
        this.sessions.event(row.id, 'review', `Review from ${names}`);
        if (worded.some((r) => r.state === 'CHANGES_REQUESTED' || r.state === 'COMMENTED')) {
          reviewState = 'addressing';
          this.sessions.notifyUser(row.id, 'Review arrived', `From ${names}.`);
          this.enqueue(row.id, 'review-people');
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
    this.watchCi(this.sessions.row(row.id), st, ci, unpushed);
    // A failed send stays queued; the next poll or turn end retries it.
    await this.deliverNext(row.id).catch((e) => this.warn(row.id, 'Sending a queued prompt', e));
    this.sessions.setMergeInfo(row.id, this.mergeInfo(this.sessions.row(row.id), st, mode, unpushed));
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
    const afterReview = !!(next.review || next['review-people']);
    this.sessions.notifyUser(id, 'CI failed', `${names}. ${afterReview ? 'Claude will fix it after the review.' : 'Claude is fixing it.'}`);
  }

  /** A requested merge that didn't happen: the PR was closed, or is still open well after the request. */
  private mergeStalled(id: string, st: github.PrStatus): boolean {
    if (st.state === 'CLOSED') return true;
    const since = get<{ entered_at: number }>(this.db,
      'SELECT entered_at FROM session_stages WHERE session_id = ? AND left_at IS NULL ORDER BY rowid DESC LIMIT 1', id)?.entered_at;
    return since !== undefined && Date.now() - since > MERGE_GRACE_MS;
  }

  /**
   * Types the first queued prompt (reviews first, then CI) into Claude when it's between turns
   * in the Review stage. One at a time: the next waits until the turn that got one has ended.
   */
  private async deliverNext(id: string): Promise<void> {
    const q = this.queue(id);
    const name = QUEUE_ORDER.find((n) => q[n]?.state === 'queued');
    if (!name) return;
    const row = this.sessions.row(id);
    if (row.desired !== 'running' || row.stage !== 'review' || (row.status !== 'idle' && row.status !== 'recovered')) return;
    if (this.sessions.runtime(id).busy) return;
    const sent = QUEUE_ORDER.filter((n) => q[n]?.state === 'sent');
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

  mergeInfo(row: SessionRow, st: github.PrStatus, reviewer: ReviewerMode, unpushed = false): { canMerge: boolean; blockers: string[] } {
    const b: string[] = [];
    if (st.state !== 'OPEN') b.push('The PR is not open.');
    if (row.status === 'working' || this.sessions.runtime(row.id).busy) b.push('Claude is still working.');
    else if (row.status === 'needs_input') b.push('Claude is waiting on you in the terminal.');
    if (unpushed) b.push("The branch has commits that aren't pushed to the PR.");
    const ci = github.ciState(st.statusCheckRollup);
    if (ci === 'failure') b.push('CI is failing.');
    else if (ci === 'pending') b.push('CI is still running.');
    // AgentK gates the merge only once it's active on the PR, so repos it doesn't watch never wait for it.
    if (reviewer !== 'people' && row.agentk_seen && (row.review_state === 'waiting' || row.review_state === 'in_review')) {
      b.push('Waiting for the AgentK review.');
    }
    if (row.review_state === 'addressing') b.push('Claude is addressing review findings.');
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
      if (!announced) this.sessions.event(row.id, 'merged', `PR #${st.number} merged; closing when Claude finishes`, { url: st.url });
      if (row.stage !== 'merge') this.sessions.setStage(row.id, 'merge', 'auto');
      return;
    }
    this.closing.add(row.id);
    try {
      if (!announced) this.sessions.event(row.id, 'merged', `PR #${st.number} merged`, { url: st.url });
      if (row.stage !== 'merge') this.sessions.setStage(row.id, 'merge', 'auto');
      await this.cleanup(this.sessions.row(row.id), st);
    } finally {
      this.closing.delete(row.id);
    }
  }

  /**
   * Records what shipped in the datapack, then removes the worktree and branches. Branches
   * with commits the merged PR lacks are kept.
   */
  async cleanup(row: SessionRow, st: github.PrStatus): Promise<void> {
    const p = projectRow(this.db, row.project_id);
    const dp = dedicatedDatapack(this.db, row.project_id);
    // Once per session: a cleanup a restart cut short, or one that waited for Claude's turn while
    // the user archived by hand, may run again.
    const shipped = !!get(this.db, "SELECT 1 AS x FROM session_events WHERE session_id = ? AND kind = 'shipped'", row.id);
    if (p && dp && !shipped) {
      try {
        const conv = await this.core.projects.ensureConversation(row.project_id);
        const body = st.body.trim().slice(0, 4000);
        this.sessions.ingestUsage(row.id);
        const u = this.sessions.runtime(row.id).usage;
        const tokens = `Token usage: ${u.output} output, ${u.input} input, ${u.cacheWrite} cache write, ${u.cacheRead} cache read tokens over ${u.messages} responses.`;
        await this.core.meko.addMemory(
          `Shipped in ${p.name}: "${st.title}" (PR #${st.number}, ${st.url}), branch ${row.branch}. ${tokens}${body ? `\n\n${body}` : ''}`,
          dp.id, projectAgentId(row.project_id), conv,
          { source: 'agent-k-local', kind: 'shipped', pr: st.url, session: row.id },
        );
        this.sessions.event(row.id, 'shipped', `Saved what shipped to ${dp.name}`);
      } catch (e) {
        this.core.log.warn(`saving the shipped summary to Meko failed for ${row.id}`, errMsg(e));
      }
    }
    const keep = !!p && (await lacksLocalCommits(p.repo_path, row.branch, st));
    const { worktreeRemoved } = await this.sessions.archive(row, { keepBranch: true });
    if (p) {
      if (worktreeRemoved && !keep) await deleteBranch(p.repo_path, row.branch);
      if (!keep && st.headRefOid) {
        // Often already deleted by GitHub. The lease deletes it only while it's still the PR's head.
        await tryGit(p.repo_path, 'push', `--force-with-lease=refs/heads/${row.branch}:${st.headRefOid}`, 'origin', '--delete', row.branch);
      }
      await git(p.repo_path, 'fetch', '--prune', '--quiet', 'origin').catch(() => undefined);
    }
    if (keep) this.core.log.info(`kept branch ${row.branch} of ${row.id}: it has commits PR #${st.number} doesn't have`);
    this.sessions.setStage(row.id, 'done', 'auto');
    this.sessions.event(row.id, 'cleanup', !worktreeRemoved
      ? `Cleaned up; worktree kept at ${row.worktree_path} because it has uncommitted changes`
      : keep
        ? `Cleaned up: worktree removed; branch ${row.branch} kept because it has commits PR #${st.number} doesn't have`
        : `Cleaned up: worktree and branch ${row.branch} removed`);
    this.core.changed();
  }
}
