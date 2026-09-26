import type { Core } from './core.js';
import { get, run } from './db.js';
import { artifactUrlIn, isArtifactPublish, readArtifactHtml, readDesignFile } from './design.js';
import { git, tryGit } from './git.js';
import * as github from './github.js';
import { errMsg } from './log.js';
import { projectAgentId } from './meko.js';
import type { HookPayload } from './hooks.js';
import { HttpError } from './projects.js';
import { dedicatedDatapack, parseSettings, projectRow, sessionRow, sessionRows, type SessionRow } from './store.js';

const POLL_MS = 15000;
const REVIEW_POLL_MS = 45000;

/**
 * The task lifecycle: Design → Code → PR → Review → Merge → Cleanup.
 * Gates (approve design, open PR, merge) come from the user; everything else
 * is driven by hook events and by polling GitHub from the server, so waiting
 * on reviews and CI costs no Claude tokens.
 */
export class Lifecycle {
  private timer: NodeJS.Timeout | null = null;
  private lastPoll = new Map<string, number>();
  /** Sessions with a review prompt waiting for Claude to finish its turn. */
  private queuedReview = new Map<string, 'review' | 'review-people'>();
  private polling = false;
  private warned = new Map<string, number>();
  /** Sessions whose design changed during the current turn. */
  private designChanged = new Set<string>();
  /** Sessions whose cleanup is running, so an overlapping poll doesn't start a second one. */
  private closing = new Set<string>();

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
    this.sessions.setStatus(id, 'needs_input', 'Design ready for your approval');
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
    this.designApproved(row.id, 'you');
    await this.sessions.submit(row.id, this.sessions.prompt(this.sessions.row(row.id), 'approved'));
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
    this.sessions.setStage(row.id, 'pr', 'you');
    this.sessions.setBusy(row.id, true);
    await this.sessions.submit(row.id, this.sessions.prompt(row, 'pr'));
  }

  /** Called on every Stop hook: a Claude turn just finished. */
  async onTurnEnd(id: string): Promise<void> {
    const row = sessionRow(this.db, id);
    if (!row) return;
    this.sessions.setBusy(id, false);
    if (row.stage === 'pr') {
      await this.pollPrStage(row).catch((e) => this.warn(row.id, 'PR lookup failed', e));
    } else if (row.stage === 'review') {
      if (row.review_state === 'addressing') this.sessions.update(id, { review_state: 'addressed' });
      const queued = this.queuedReview.get(id);
      if (queued) {
        this.queuedReview.delete(id);
        await this.sendReviewPrompt(this.sessions.row(id), queued);
      } else {
        await this.pollReview(this.sessions.row(id)).catch((e) => this.warn(id, 'PR status failed', e));
      }
    }
    // Review comments go after anything above that already gave Claude its next turn.
    await this.core.comments.deliver(id).catch((e) => this.warn(id, 'Sending review comments', e));
  }

  // ---- polling ----

  private async pollAll(): Promise<void> {
    if (this.polling) return;
    this.polling = true;
    try {
      const now = Date.now();
      for (const row of sessionRows(this.db)) {
        if (row.desired !== 'running' || !projectRow(this.db, row.project_id)) continue;
        if (row.stage === 'pr') {
          await this.pollPrStage(row).catch((e) => this.warn(row.id, 'PR lookup failed', e));
        } else if (row.stage === 'review' || row.stage === 'merge') {
          if (now - (this.lastPoll.get(row.id) ?? 0) < REVIEW_POLL_MS) continue;
          await this.pollReview(row).catch((e) => this.warn(row.id, 'PR status failed', e));
        }
        // Comments queued before a restart, or while Claude was waiting on the user.
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

  async pollPrStage(row: SessionRow): Promise<void> {
    const pr = await github.findPr(row.worktree_path, row.branch);
    if (!pr) return;
    this.sessions.update(row.id, { pr_number: pr.number, pr_url: pr.url, review_state: 'waiting' });
    this.sessions.event(row.id, 'pr', `Opened PR #${pr.number}`, { url: pr.url });
    this.sessions.setStage(row.id, 'review', 'auto');
    await this.pollReview(this.sessions.row(row.id));
  }

  async pollReview(row: SessionRow): Promise<void> {
    if (!row.pr_number) return;
    this.lastPoll.set(row.id, Date.now());
    const st = await github.prStatus(row.worktree_path, row.pr_number);
    if (st.state === 'MERGED') {
      await this.merged(row, st);
      return;
    }
    const p = projectRow(this.db, row.project_id);
    if (!p) return;
    const settings = parseSettings(p.settings_json);
    const ci = github.ciState(st.statusCheckRollup);
    let reviewState = row.review_state;
    const seen = new Set(JSON.parse(row.seen_review_ids) as string[]);

    if (settings.reviewer === 'agentk') {
      const botReviews = st.reviews.filter((r) => github.isLogin(r.author, settings.reviewerLogin));
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
          run(this.db, 'UPDATE sessions SET seen_review_ids = ?, review_state = ? WHERE id = ?', JSON.stringify([...seen]), reviewState, row.id);
          await this.sendReviewPrompt(this.sessions.row(row.id), 'review');
        }
      } else if (reviewState === 'waiting' || reviewState === 'in_review' || reviewState === 'refused') {
        // AgentK talks through issue comments before the review: an ack, or a refusal.
        const latestReviewAt = Math.max(0, ...botReviews.map((r) => Date.parse(r.submittedAt)));
        const botComment = [...st.comments].reverse().find((c) => github.isLogin(c.author, settings.reviewerLogin) && Date.parse(c.createdAt) > latestReviewAt);
        if (botComment) {
          if (/no review/i.test(botComment.body)) reviewState = 'refused';
          else if (/started reviewing|reviewing/i.test(botComment.body)) reviewState = 'in_review';
        }
      }
    } else {
      const humans = st.reviews.filter((r) => !github.isLogin(r.author, settings.reviewerLogin) && r.author);
      const fresh = humans.filter((r) => !seen.has(r.id));
      if (fresh.length) {
        fresh.forEach((r) => seen.add(r.id));
        const needsWork = fresh.some((r) => r.state === 'CHANGES_REQUESTED' || (r.state === 'COMMENTED' && r.body.trim()));
        this.sessions.event(row.id, 'review', `Review from ${fresh.map((r) => r.author?.login).join(', ')}`);
        if (needsWork) {
          reviewState = 'addressing';
          run(this.db, 'UPDATE sessions SET seen_review_ids = ?, review_state = ? WHERE id = ?', JSON.stringify([...seen]), reviewState, row.id);
          await this.sendReviewPrompt(this.sessions.row(row.id), 'review-people');
        }
      }
      if (st.reviewDecision === 'APPROVED' && reviewState !== 'addressing') reviewState = 'approved';
      else if (reviewState === 'none') reviewState = 'waiting';
    }

    const current = this.sessions.row(row.id);
    const finalReview = current.review_state === 'addressing' && reviewState !== 'addressing' ? current.review_state : reviewState;
    this.sessions.update(row.id, {
      ci_state: ci,
      review_state: finalReview,
      merge_state: st.mergeStateStatus,
      seen_review_ids: JSON.stringify([...seen]),
    });
    this.sessions.setMergeInfo(row.id, this.mergeInfo(this.sessions.row(row.id), st, settings.reviewer));
    this.core.changed();
  }

  private async sendReviewPrompt(row: SessionRow, name: 'review' | 'review-people'): Promise<void> {
    const text = this.sessions.prompt(row, name);
    if (row.status === 'idle' || row.status === 'recovered') {
      await this.sessions.submit(row.id, text);
    } else {
      this.queuedReview.set(row.id, name);
    }
  }

  mergeInfo(row: SessionRow, st: github.PrStatus, reviewer: 'agentk' | 'people'): { canMerge: boolean; blockers: string[] } {
    const b: string[] = [];
    if (st.state !== 'OPEN') b.push('The PR is not open.');
    if (row.status === 'working' || this.sessions.runtime(row.id).busy) b.push('Claude is still working.');
    const ci = github.ciState(st.statusCheckRollup);
    if (ci === 'failure') b.push('CI is failing.');
    else if (ci === 'pending') b.push('CI is still running.');
    if (reviewer === 'agentk') {
      if (row.review_state === 'waiting' || row.review_state === 'in_review') b.push('Waiting for the AgentK review.');
      if (row.review_state === 'addressing') b.push('Claude is addressing review findings.');
    }
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
    await github.comment(row.worktree_path, row.pr_number, force ? '/agentk review force' : '/agentk review');
    this.sessions.update(row.id, { review_state: 'waiting' });
    this.sessions.event(row.id, 'review', force ? 'Forced an AgentK review' : 'Requested another AgentK review');
    this.lastPoll.delete(row.id);
  }

  // ---- merge / cleanup ----

  async merge(row: SessionRow): Promise<void> {
    if (row.stage !== 'review' || !row.pr_number || !row.pr_url) throw new HttpError(409, 'Merge is only available in the Review stage.');
    const p = projectRow(this.db, row.project_id);
    if (!p) throw new HttpError(404, 'Project not found.');
    const settings = parseSettings(p.settings_json);
    await this.pollReview(row);
    const fresh = this.sessions.row(row.id);
    if (fresh.stage !== 'review') return; // merged elsewhere and already cleaned up
    const info = this.sessions.runtime(row.id);
    if (!info.canMerge) throw new HttpError(409, `Not ready to merge: ${info.mergeBlockers.join(' ')}`);
    const method = await github.resolveMergeMethod(row.worktree_path, settings.mergeMethod);
    this.sessions.setBusy(row.id, true);
    try {
      this.sessions.setStage(row.id, 'merge', 'you');
      await github.merge(row.worktree_path, row.pr_url, method);
    } catch (e) {
      this.sessions.setStage(row.id, 'review', 'auto');
      throw new HttpError(502, `GitHub refused the merge: ${errMsg(e)}`);
    } finally {
      this.sessions.setBusy(row.id, false);
    }
    await this.pollReview(this.sessions.row(row.id));
  }

  /** The PR is merged (here, or on GitHub): finish Merge & Close. Safe to call again after a restart. */
  private async merged(row: SessionRow, st: github.PrStatus): Promise<void> {
    if (row.stage === 'done' || this.closing.has(row.id)) return;
    this.closing.add(row.id);
    try {
      this.sessions.event(row.id, 'merged', `PR #${st.number} merged`, { url: st.url });
      if (row.stage !== 'merge') this.sessions.setStage(row.id, 'merge', 'auto');
      await this.cleanup(this.sessions.row(row.id), st);
    } finally {
      this.closing.delete(row.id);
    }
  }

  /** Records what shipped in the datapack, then removes the worktree and branches. */
  async cleanup(row: SessionRow, st: github.PrStatus): Promise<void> {
    const p = projectRow(this.db, row.project_id);
    const dp = dedicatedDatapack(this.db, row.project_id);
    if (p && dp) {
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
      } catch (e) {
        this.core.log.warn(`saving the shipped summary to Meko failed for ${row.id}`, errMsg(e));
      }
    }
    const { worktreeRemoved } = await this.sessions.archive(row, { keepBranch: false });
    if (p) {
      await tryGit(p.repo_path, 'push', 'origin', '--delete', row.branch); // often already deleted by GitHub
      await git(p.repo_path, 'fetch', '--prune', '--quiet', 'origin').catch(() => undefined);
    }
    this.sessions.setStage(row.id, 'done', 'auto');
    this.sessions.event(row.id, 'cleanup', worktreeRemoved
      ? `Cleaned up: worktree and branch ${row.branch} removed`
      : `Cleaned up; worktree kept at ${row.worktree_path} because it has uncommitted changes`);
    this.core.changed();
  }
}
