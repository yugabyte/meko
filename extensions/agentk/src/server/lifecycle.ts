import { findMeAkReviewLine, withMeAkReviewLine } from '../shared/codereview.js';
import { existsSync, readFileSync, rmSync } from 'node:fs';
import { basename } from 'node:path';
import { mekoHost } from './config.js';
import type { Core } from './core.js';
import { get, kvGet, run } from './db.js';
import { designAnswers, type CheckedAnswers } from './answers.js';
import {
  artifactUrlIn, checkToolQuestions, designLine, isArtifactPublish, mekoDesignName, parseDesignLine, readArtifactHtml, readDesignFile, sha256Hex,
  withDesignLine, type PrDesignRef,
} from './design.js';
import { commitsBetween, commitsBeyond, deleteBranch, fetchPrHead, git, moveReviewBranch, remoteFor, remoteRepo, tryGit, uncommittedCount } from './git.js';
import * as github from './github.js';
import { countFileTypes } from './filetypes.js';
import { KIRO_PLAN_TURN_EVERY_MS } from './kiroplan.js';
import { errMsg } from './log.js';
import {
  andList, checkHeldRequest, decideHeld, dropOtherPrs, HELD_SINCE_KEY, heldBlocker, heldPromptLines, heldReviews, heldSummary, holdReview, reviewRoute,
  type ReviewDetails, type ReviewWho,
} from './heldreviews.js';
import { launchedAgentId, withMekoScope } from './meko.js';
import type { HookPayload, ModReply } from './hooks.js';
import { fetchPrDesign, markPrDesignTold, prDesignSource, savePrDesign } from './prdesign.js';
import { meAkReviewFooter, mekoDatapackLineId, mekoPrFooter, movePrompt } from './prompts.js';
import { HttpError } from './projects.js';
import {
  INSTRUCTIONS_MAX, KIND_LABELS, STAGE_LABELS, type CloseSessionRequest, type HeldReview, type HeldReviewAuthor, type HeldReviewsResult, type MoveSessionRequest, type QueuedGate,
  type ReviewerMode, type Stage,
} from '../shared/types.js';
import { MOVE_TARGETS, kindOf, moveBlockedIn, moveError, movesStep, offersPrompt, promptByDefault } from '../shared/stages.js';
import { parseSettings, projectRow, sessionDatapack, sessionRepo, sessionRow, sessionRows, type MergeInfo, type SessionRow } from './store.js';
import { pushLine, workflowLine } from './sessions.js';
import { agentName } from '../shared/harnesses.js';

/**
 * The `owner/name` a session's PR is in, from its URL, for gh's `--repo`: the clone's default repo
 * for gh isn't always the PR's (a fork's clone, or a PR picked in the upstream). Undefined when the
 * URL isn't a github.com PR's, and gh then uses its default.
 */
function prRepoOf(url: string | null): string | undefined {
  return github.prFromUrl(url)?.repo;
}

const POLL_MS = 15000;
/** The most of a learn session's summary its timeline keeps. */
const LEARNED_MAX = 2000;
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
/**
 * How long after a new head a queued merge waits for CI to show up while its checks show none or only
 * passed ones, and GitHub's BLOCKED counts as required checks still to report. After it, a head
 * without checks counts as one without CI, and BLOCKED is left to GitHub.
 */
const CI_START_MS = 2 * 60 * 1000;
/** How long Open PR waits for Meko to name the session's conversation for the PR description. */
const PR_CONVERSATION_TIMEOUT_MS = 10000;
/** How long a PR poll waits before trying again to update a design line it couldn't. */
const DESIGN_LINE_RETRY_MS = 10 * 60 * 1000;
/** How long a Review a PR poll waits before trying again to fetch a PR design it couldn't. */
const PR_DESIGN_RETRY_MS = 10 * 60 * 1000;
/** The stages after a design's approval, where a change to it is saved as a new version and named in the PR. */
const LATER_DESIGN_STAGES: readonly Stage[] = ['code', 'pr', 'review', 'merge'];
/** Where Run to merge can be turned on or off: from Design until the merge. */
const AUTO_RUN_STAGES: readonly Stage[] = ['design', 'code', 'pr', 'review'];

/** True when a PR's design line names `ref`. */
function sameRef(a: PrDesignRef | null, ref: PrDesignRef): boolean {
  return !!a && a.name === ref.name && a.sha256 === ref.sha256;
}

/** What mergeInfo learns from looking at the branch and the worktree (see there). */
interface MergeChecks {
  unpushed?: boolean;
  uncommitted?: number;
  turnAtStart?: boolean;
  newHead?: boolean;
}

/** What goes with a PR prompt (sendPr): a lead before it, the user's instructions after it, who sent it, the review's line. */
interface PrSend {
  lead?: string;
  instructions?: string;
  by?: 'you' | 'auto';
  reviewLine?: string | null;
}

/** A queued Open PR was withdrawn before its prompt went. */
class QueueWithdrawn extends Error {
  constructor() {
    super('The queued Open PR was withdrawn.');
  }
}

/** A queued gate as the activity log names it. */
const GATE_NAMES: Record<QueuedGate, string> = { 'open-pr': 'Open PR', merge: 'merge' };

/** The status detail while a markdown or Artifact design waits for approval. */
export const DESIGN_READY = 'Design ready for your approval';

/** Merge blockers AgentK works on by itself in Review & PR (the CI and merge conflict watchers). */
const CI_FAILING = 'CI is failing.';
const CONFLICTING = 'The branch has merge conflicts.';
/** The merge blocker for a question in the terminal, which already notified the user as Needs you. */
const IN_TERMINAL = 'AgentK is waiting on you in the terminal.';
/** GitHub's own BLOCKED on a new head when nothing else blocks: required checks not reported yet. */
const CHECKS_PENDING = "GitHub's required checks haven't reported on the latest push yet.";

/** Prompts queued for Claude in the Review stage, in the order they go out. */
const QUEUE_ORDER = ['review', 'review-people', 'review-held', 'conflicts', 'ci'] as const;
/** The prompts queued in a Review a PR session: the summary after Update to latest, or the PR's new design alone. */
const REVIEWING_ORDER = ['review-pr-update', 'review-pr-design'] as const;
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

/** A review prompt (AgentK's, people's or the held ones the user sent) is queued or out. */
const reviewQueued = (q: Queue) => !!(q.review || q['review-people'] || q['review-held']);

/** A review that asks for work: changes requested, or comments (an empty one, a thread reply, is skipped before this). */
const asksForWork = (r: github.PrReview) => r.state === 'CHANGES_REQUESTED' || r.state === 'COMMENTED';

/** An AgentK review with nothing to address: no inline comments, nothing unanchored or left open. */
function cleanAgentkReview(body: string): boolean {
  const m = body.match(/(\d+)\s+inline comments?/i);
  return !!m && Number(m[1]) === 0 && !/could not be anchored|still open/i.test(body);
}

const REVIEW_REQUEST = /^\/agentk review\b/;

/** The reviews' authors, each once, as `a, b`. */
function loginList(reviews: github.PrReview[]): string {
  return [...new Set(reviews.map((r) => r.author?.login))].join(', ');
}

/** The user's instructions in a move or Open PR request: trimmed, '' when there are none. */
export function checkInstructions(v: unknown): string {
  if (v === undefined || v === null) return '';
  if (typeof v !== 'string') throw new HttpError(400, 'Instructions must be text.');
  const text = v.trim();
  if (text.length > INSTRUCTIONS_MAX) {
    throw new HttpError(400, `Instructions can be at most ${INSTRUCTIONS_MAX.toLocaleString('en-US')} characters; these have ${text.length.toLocaleString('en-US')}.`);
  }
  return text;
}

/**
 * The instructions in an open-pr action's body (OpenPrRequest): '' for blank ones, null when the
 * request gives none (the plain gate's press), which leaves a queued Open PR's alone.
 */
export function openPrInstructions(body: unknown): string | null {
  if (body === undefined || body === null) return null;
  if (typeof body !== 'object' || Array.isArray(body)) throw new HttpError(400, 'The Open PR request must be {instructions}.');
  const v = (body as Record<string, unknown>).instructions;
  return v === undefined || v === null ? null : checkInstructions(v);
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
 * The task lifecycle: Design → Code → Review & PR → Merge & Close (merge, then cleanup).
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
  /**
   * Approvals given while AgentK was busy (with the mod live), waiting for the turn to end: the
   * user's answers to the design's questions, or null. In memory only, like the answers.
   */
  private queuedApprovals = new Map<string, CheckedAnswers | null>();
  /**
   * PR prompts Claude took but exited before running (prLost), to send again once Claude is back:
   * the stage it went back to and what went with it. In memory only: after a restart, Open PR sends it.
   */
  private lostPrs = new Map<string, PrSend & { back: Stage }>();
  /** Sessions whose cleanup is running, so an overlapping poll doesn't start a second one. */
  private closing = new Set<string>();
  /** Sessions with a Merge request in flight. */
  private merging = new Set<string>();
  /** Per session, the tail of the GitHub work (polls, review requests, merge) queued for it. */
  private chains = new Map<string, Promise<void>>();
  /** Per session, the tail of its design sends to Meko. */
  private designSends = new Map<string, Promise<void>>();
  /** Per task, when updating its PR's design line last failed. */
  private designLineFailedAt = new Map<string, number>();
  /** Per review, the PR design it last failed to fetch (its hash) and when. */
  private prDesignFailed = new Map<string, { sha256: string; at: number; missing: boolean }>();
  /** Per session, the failing head whose checks are still running, and when its failure was first seen. */
  private ciFailSince = new Map<string, { head: string; at: number }>();
  /** Per session, the PR head at the last look and when it was first seen there (CI_START_MS). */
  private headSeen = new Map<string, { head: string; at: number }>();
  /** Per Run to merge session, the blockers waiting on the user it was last told about (so it's told once per new set). */
  private autoRunTold = new Map<string, string>();
  /**
   * Run to merge sessions whose queued merge waits on a blocker that doesn't clear by itself
   * (approvals can take days): their PR is looked at every REVIEW_POLL_MS, not at every pass.
   */
  private autoRunBlocked = new Set<string>();

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
    // No datapack, or Meko is off: the design stays here until it can be sent. A review's design
    // is its PR's, already in Meko.
    if (!row || !dp || !this.core.mekoStatus.configured || row.kind === 'review-pr') return;
    const d = get<{ markdown: string; html: string | null; md_version: number; md_sent: number; html_version: number; html_sent: number }>(
      this.db, 'SELECT markdown, html, md_version, md_sent, html_version, html_sent FROM designs WHERE session_id = ?', id);
    if (!d) return;
    const parts: { name: string; text: string; type: string; sent: string; version: number }[] = [];
    if (!d.md_sent && d.markdown) {
      parts.push({ name: mekoDesignName(row.slug, 'md', d.md_version + 1), text: d.markdown, type: 'text/markdown',
        sent: 'UPDATE designs SET md_version = ?, md_hash = ?, md_sent = (markdown = ?) WHERE session_id = ?', version: d.md_version + 1 });
    }
    if (!d.html_sent && d.html !== null) {
      parts.push({ name: mekoDesignName(row.slug, 'html', d.html_version + 1), text: d.html, type: 'text/html',
        sent: 'UPDATE designs SET html_version = ?, html_hash = ?, html_sent = (html IS ?) WHERE session_id = ?', version: d.html_version + 1 });
    }
    if (!parts.length) return;
    const conv = await this.sessions.conversationId(row, dp);
    await Promise.all(parts.map(async (p) => {
      const hash = await withMekoScope(row, () => this.core.meko.putArtifact(p.name, p.text, p.type, dp.id, launchedAgentId(row), conv));
      // The version is used, and is the one the PR names; it's the current design only if nothing
      // changed while it was sent.
      run(this.db, p.sent, p.version, hash, p.text, id);
    }));
    await this.syncPrDesign(id);
  }

  /**
   * The design a task's PR names (see designLine): its first version's Meko name and the hash of
   * the latest version sent, the Artifact's page for an Artifact design. Null without a design, or
   * before a version reached Meko. A version sent before hashes were kept is named by the hash of
   * the design as it is, when that's the one sent.
   */
  prDesignRef(row: SessionRow): PrDesignRef | null {
    if (row.kind === 'review-pr') return null;
    const d = get<{ markdown: string; html: string | null; md_version: number; md_sent: number; md_hash: string | null; html_version: number; html_sent: number; html_hash: string | null }>(
      this.db, 'SELECT markdown, html, md_version, md_sent, md_hash, html_version, html_sent, html_hash FROM designs WHERE session_id = ?', row.id);
    if (!d) return null;
    const [ext, text, version, sent, hash] = d.html !== null
      ? ['html', d.html, d.html_version, d.html_sent, d.html_hash] as const
      : ['md', d.markdown, d.md_version, d.md_sent, d.md_hash] as const;
    const sha256 = hash ?? (version > 0 && sent && text ? sha256Hex(text) : null);
    return sha256 ? { name: mekoDesignName(row.slug, ext, 1), sha256 } : null;
  }

  /**
   * Points the task's PR at the latest version of its design: rewrites the `meko_design:` line of
   * the PR's description (adding it after the Meko datapack line when it's missing) and nothing
   * else. `body` is the description as just polled, read from GitHub when not given. A failure is
   * logged and tried again at a poll after DESIGN_LINE_RETRY_MS. Never throws.
   */
  private async syncPrDesign(id: string, body?: string): Promise<void> {
    const row = sessionRow(this.db, id);
    if (!row?.pr_number || row.kind === 'review-pr' || !LATER_DESIGN_STAGES.includes(row.stage)) return;
    const ref = this.prDesignRef(row);
    if (!ref) return;
    if (body !== undefined && sameRef(parseDesignLine(body), ref)) return;
    if (Date.now() - (this.designLineFailedAt.get(id) ?? 0) < DESIGN_LINE_RETRY_MS && body !== undefined) return;
    const repo = prRepoOf(row.pr_url);
    const cwd = this.ghCwd(row);
    try {
      const cur = await github.prBody(cwd, row.pr_number, repo);
      if (sameRef(parseDesignLine(cur), ref)) return;
      await github.setPrBody(cwd, row.pr_number, withDesignLine(cur, designLine(ref), (l) => mekoDatapackLineId(l) !== null), repo);
      this.designLineFailedAt.delete(id);
      this.sessions.event(id, 'design', `Pointed PR #${row.pr_number} at the latest version of the design in Meko`);
    } catch (e) {
      this.designLineFailedAt.set(id, Date.now());
      this.warn(id, `Updating the design's line in PR #${row.pr_number}`, e);
    }
  }

  /**
   * After an Artifact tool call: keeps the published page and its link as the design. During
   * Design any publish counts; later (`sameOnly`), only a republish of the design's own Artifact,
   * so a page Claude publishes while coding isn't taken for the design.
   */
  async captureArtifact(id: string, p: HookPayload, sameOnly = false): Promise<void> {
    const input = p.tool_input ?? {};
    if (!isArtifactPublish(input)) return;
    const url = artifactUrlIn(JSON.stringify(p.tool_response ?? ''));
    if (sameOnly) {
      const d = get<{ artifact_url: string | null }>(this.db, 'SELECT artifact_url FROM designs WHERE session_id = ?', id);
      if (!url || !d?.artifact_url || d.artifact_url !== url) return;
    }
    const html = readArtifactHtml(input.file_path, p.cwd);
    if (!url && html === null) return;
    if (sameOnly) {
      const prev = get<{ html: string | null }>(this.db, 'SELECT html FROM designs WHERE session_id = ?', id);
      if (html === null || prev?.html === html) return;
      await this.saveDesign(id, { html });
      this.sessions.event(id, 'design', 'Design updated');
      return;
    }
    await this.saveDesign(id, { ...(url ? { artifactUrl: url } : {}), ...(html !== null ? { html } : {}) });
    this.designChanged.add(id);
  }

  /**
   * At the end of a turn after the design was approved (Code, Review & PR, Merge & Close): picks up
   * a change to the markdown design file, which saves a new version to Meko and points the PR at
   * it. Nothing waits for the user: it's only noted in the activity log.
   */
  async collectLaterDesign(row: SessionRow): Promise<void> {
    if (row.kind === 'review-pr' || row.plan_mode || !LATER_DESIGN_STAGES.includes(row.stage)) return;
    const prev = get<{ markdown: string; html: string | null }>(this.db, 'SELECT markdown, html FROM designs WHERE session_id = ?', row.id);
    if (!prev) return;
    // An Artifact design is kept by its republishes (captureArtifact); the file is a markdown design's.
    const md = prev.html === null ? readDesignFile(this.sessions.designFile(row)) : null;
    if (md !== null && md !== prev.markdown) {
      await this.saveDesign(row.id, { markdown: md });
      this.sessions.event(row.id, 'design', 'Design updated');
      return;
    }
    void this.sendDesign(row.id); // anything an earlier send couldn't deliver
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
   * The design's questions for the user, read again (see DesignQuestionStore). A file that's too
   * large, unreadable or not questions is logged and leaves the ones kept alone. A change counts
   * as a change to the design, and the design's time moves on so the browser fetches them again.
   */
  private collectQuestions(row: SessionRow): void {
    let changed: boolean;
    try {
      changed = this.core.questions.refresh(row);
    } catch (e) {
      this.warn(row.id, `Reading the design's questions (${this.sessions.questionsFile(row)})`, e);
      return;
    }
    if (!changed) return;
    run(this.db, 'UPDATE designs SET updated_at = ? WHERE session_id = ?', Date.now(), row.id);
    this.designChanged.add(row.id);
    this.core.changed();
  }

  /**
   * The design is waiting for the user: say so in the activity log, the header and a notification.
   * `approvalDropped`: the user approved it during the turn, but the turn changed it. No
   * notification while Claude holds answers the user sent during the turn: it revises the design
   * again right away.
   */
  designReady(id: string, approvalDropped = false): void {
    this.sessions.event(id, 'design', 'Design ready for approval');
    this.sessions.setStatus(id, 'needs_input', DESIGN_READY);
    const row = sessionRow(this.db, id);
    const open = row ? this.core.questions.get(row).open : 0;
    if (approvalDropped) {
      this.sessions.notifyUser(id, 'Design changed', "AgentK changed the design during its turn, so your approval wasn't sent. Review it and approve again.");
      return;
    }
    if (this.sessions.promptsHeld(id) > 0) return;
    this.sessions.notifyUser(id, 'Design ready',
      open ? `Review the design and answer ${open === 1 ? 'its open question' : `${open} open questions`}.` : 'Review and approve the design.');
  }

  /**
   * The design's questions from the mod's design_questions tool: checked, shown at once, and saved
   * to the questions file (so a restart, and Claude's later edits to the file, start from them).
   * A bad call is answered with what's wrong, for Claude to fix in the same turn.
   */
  questionsFromTool(row: SessionRow, input: unknown): ModReply {
    const fail = (error: string): ModReply => ({ status: 400, body: { error } });
    if (row.stage !== 'design' || row.plan_mode) return fail("The session isn't in the Design stage, so there's no design to ask about.");
    const checked = checkToolQuestions(input, this.core.questions.get(row).questions);
    if ('error' in checked) return fail(checked.error);
    let changed: boolean;
    try {
      changed = this.core.questions.set(row, checked.questions);
    } catch (e) {
      return fail(`AgentK couldn't save the questions (${errMsg(e)}). Write them to ${this.sessions.questionsFile(row)} instead.`);
    }
    // The same questions again change nothing. Before there's a design they're only saved, to
    // show with it when it comes.
    if (changed && get(this.db, 'SELECT 1 AS x FROM designs WHERE session_id = ?', row.id)) {
      run(this.db, 'UPDATE designs SET updated_at = ? WHERE session_id = ?', Date.now(), row.id);
      this.designChanged.add(row.id);
      this.core.changed();
    }
    const asked = checked.questions.filter((q) => q.kind === 'question');
    const open = asked.filter((q) => q.status === 'open').length;
    const n = asked.length;
    const d = checked.questions.length - n;
    const cited = checked.questions.filter((q) => q.meko.length).length;
    const decisions = d ? ` and ${d === 1 ? '1 decision' : `${d} decisions`}` : '';
    const meko = cited ? `, ${cited} citing Meko,` : '';
    return {
      status: 200,
      body: {
        text: `Saved ${n === 1 ? '1 question' : `${n} questions`} (${open} open)${decisions}${meko} to ${this.sessions.questionsFile(row)}. The user sees them beside the design in AgentK.`,
      },
    };
  }

  /**
   * Approves the design: Claude's plan dialog in plan mode, otherwise the approval prompt, with
   * the user's answers to the design's questions (`body`, a DesignAnswersRequest) ahead of it.
   */
  async approveDesign(row: SessionRow, body?: unknown): Promise<void> {
    if (row.stage !== 'design') throw new HttpError(409, 'This session is past the Design stage.');
    const answers = designAnswers(body, this.core.questions.get(row).questions);
    if (row.plan_mode) {
      if (answers) throw new HttpError(409, 'AgentK is in plan mode, so the approval is a choice in its terminal. Type your answers there.');
      if (!this.sessions.planPending.has(row.id)) {
        throw new HttpError(409, "AgentK hasn't presented a design for approval yet. Ask it to finish the design, or approve it in the terminal.");
      }
      // Claude's plan dialog has "Yes" selected by default.
      await this.core.host.sendKeys(row.tmux_name, 'Enter');
      this.sessions.planPending.delete(row.id);
      this.designApproved(row.id, 'you');
      return;
    }
    if (!get(this.db, 'SELECT 1 AS x FROM designs WHERE session_id = ?', row.id)) {
      throw new HttpError(409, "AgentK hasn't written a design yet. Wait for it to finish, or ask it in the terminal.");
    }
    if (row.status === 'exited') throw new HttpError(409, `${agentName(row)} has exited. Resume the session first.`);
    if (row.status === 'starting') throw new HttpError(409, 'AgentK is still working. Wait for the turn to finish.');
    // Answers sent during a turn wait in Claude, and run before anything sent now: the approval
    // would move to Code while AgentK revises the design.
    if (this.sessions.promptsHeld(row.id) > 0) {
      throw new HttpError(409, 'AgentK has answers from you to take first. Approve once it has revised the design.');
    }
    // Mid-turn, a turn may still change the design: with the mod, the approval waits for its end.
    if (this.sessions.midTurn(row.id)) {
      if (!this.sessions.modLive(row.id)) {
        throw new HttpError(409, row.status === 'working' ? 'AgentK is still working. Wait for the turn to finish.' : 'AgentK is waiting on you in the terminal. Answer it first.');
      }
      this.queuedApprovals.set(row.id, answers);
      this.sessions.event(row.id, 'design', 'Approval queued: it goes when AgentK finishes its turn, if the design stays the same');
      this.core.changed();
      return;
    }
    // Any other question (a permission prompt, a parked recovery) would take a typed approval as its answer.
    if (row.status === 'needs_input' && row.status_detail !== DESIGN_READY) {
      throw new HttpError(409, 'AgentK is waiting on you in the terminal. Answer it first.');
    }
    this.queuedApprovals.delete(row.id);
    await this.sendApproval(row.id, answers);
  }

  /** Moves the session to Code and sends Claude the approval, with the user's answers ahead of it. */
  private async sendApproval(id: string, answers: CheckedAnswers | null): Promise<void> {
    this.designApproved(id, 'you');
    try {
      const cur = this.sessions.row(id);
      const lead = answers && this.sessions.prompt(cur, 'approved-answers', { answers: answers.text });
      await this.sessions.submit(id, [lead, this.sessions.prompt(cur, 'approved')].filter(Boolean).join('\n\n'));
      if (answers) this.sessions.event(id, 'design', `Approved with ${answers.count}`);
      if (cur.auto_run) this.sessions.event(id, 'auto-run', 'Approved with Run to merge: AgentK opens the PR when this turn ends, and merges it by itself');
    } catch (e) {
      if (this.sessions.row(id).stage === 'code') this.sessions.setStage(id, 'design', 'auto');
      throw new HttpError(502, `Couldn't send the approval to Claude: ${errMsg(e)}. Try again.`);
    }
    this.recordAnswers(id, answers, true);
  }

  /**
   * Once the user's answers have gone to Claude, marks the questions they answered resolved, with
   * their answers (`all`: every open one, see DesignQuestionStore.record), so the Questions tab
   * shows what they sent. It isn't a change AgentK made in its turn. A questions file that can't be
   * read or written is logged and left alone: the answers went, and AgentK is asked to resolve them.
   */
  private recordAnswers(id: string, answers: CheckedAnswers | null, all: boolean): void {
    this.questionsUpdate(id, 'Recording the user\'s answers in', (row) => this.core.questions.record(row, answers?.answers ?? [], answers?.questions ?? [], all));
  }

  /**
   * Changes the design's questions with `fn` (true when they changed): the design's time moves on
   * so the browser fetches them again. A failure is logged.
   */
  private questionsUpdate(id: string, what: string, fn: (row: SessionRow) => boolean): void {
    const row = sessionRow(this.db, id);
    if (!row) return;
    try {
      if (!fn(row)) return;
    } catch (e) {
      this.warn(id, `${what} the design's questions (${this.sessions.questionsFile(row)})`, e);
      return;
    }
    run(this.db, 'UPDATE designs SET updated_at = ? WHERE session_id = ?', Date.now(), id);
    this.core.changed();
  }

  /** True while an approval waits for the session's turn to end. */
  approvalQueued(id: string): boolean {
    return this.queuedApprovals.has(id);
  }

  /** Withdraws an approval waiting for the turn to end. */
  cancelApproval(row: SessionRow): void {
    if (!this.queuedApprovals.delete(row.id)) throw new HttpError(409, 'There is no approval waiting for AgentK.');
    this.sessions.event(row.id, 'design', 'Queued approval withdrawn');
    this.core.changed();
  }

  /** Drops an approval waiting for the turn to end, saying why (Claude exited, the session was archived). */
  dropApproval(id: string, why: string): void {
    if (!this.queuedApprovals.delete(id)) return;
    this.sessions.event(id, 'design', `Queued approval dropped: ${why}`);
    this.core.changed();
  }

  /**
   * At the end of a turn: sends an approval queued during it (`sent`), unless the turn changed the
   * design or its questions (`dropped`: the user sees the new design first) or the session left
   * Design meanwhile. `failed`: it couldn't be sent, and the design waits for approval again (the
   * user is told). Null when none was queued.
   */
  async takeQueuedApproval(id: string, changed: boolean): Promise<'sent' | 'dropped' | 'failed' | null> {
    if (!this.queuedApprovals.has(id)) return null;
    const answers = this.queuedApprovals.get(id) ?? null;
    this.queuedApprovals.delete(id);
    this.core.changed();
    const row = sessionRow(this.db, id);
    if (!row || row.desired !== 'running' || row.stage !== 'design') return null;
    if (changed) {
      this.sessions.event(id, 'design', "Queued approval dropped: AgentK changed the design during its turn");
      return 'dropped';
    }
    // Claude is at its prompt now.
    this.sessions.setStatus(id, 'idle');
    try {
      await this.sendApproval(id, answers);
    } catch (e) {
      this.warn(id, 'Sending the queued approval', e);
      this.sessions.setStatus(id, 'needs_input', DESIGN_READY);
      this.sessions.notifyUser(id, 'Approval not sent', `Your approval couldn't be sent to Claude (${errMsg(e)}). Approve the design again.`);
      return 'failed';
    }
    return 'sent';
  }

  /**
   * Sends the user's answers to the design's questions (`body`, a DesignAnswersRequest) to Claude
   * for it to revise the design. Nothing is kept on the server: they go now, while Claude is at its
   * prompt (a design waiting for approval included), or, with the mod live, mid-turn too, held by
   * Claude until the turn ends.
   */
  async reviseDesign(row: SessionRow, body: unknown): Promise<void> {
    if (row.desired !== 'running') throw new HttpError(409, 'This session is archived.');
    if (row.stage !== 'design' || row.plan_mode) throw new HttpError(409, 'Answers to the design can only be sent while the session is in Design.');
    if (this.queuedApprovals.has(row.id)) throw new HttpError(409, 'You approved the design: the approval goes when this turn ends. Cancel it to send answers instead.');
    // With the mod, answers sent mid-turn wait in Claude for the turn to end.
    if (this.sessions.modLive(row.id) && this.sessions.midTurn(row.id)) {
      if (this.sessions.runtime(row.id).busy) throw new HttpError(409, 'AgentK is still working. Wait for the turn to finish.');
    } else {
      this.checkPromptable(row);
    }
    const answers = designAnswers(body, this.core.questions.get(row).questions);
    if (!answers) throw new HttpError(400, 'Answer a question or write feedback first.');
    try {
      await this.sessions.submit(row.id, this.sessions.prompt(row, 'design-answers', { count: answers.count, answers: answers.text }));
    } catch (e) {
      throw new HttpError(502, `Couldn't send your answers to Claude: ${errMsg(e)}. Try again.`);
    }
    this.sessions.event(row.id, 'design', `Sent ${answers.count} to AgentK`);
    // Mid-turn, AgentK may have changed the questions already: read them first, so that counts as
    // its change and the answers go on top of it.
    this.collectQuestions(row);
    this.recordAnswers(row.id, answers, false);
  }

  designApproved(id: string, by: 'you' | 'auto'): void {
    const row = sessionRow(this.db, id);
    if (!row || row.stage !== 'design') return;
    this.sessions.setStage(id, 'code', by);
    void this.sendDesign(id); // the approved design, if Meko doesn't have it yet
  }

  // ---- PR ----

  /**
   * Sends the PR prompt, or, while AgentK is mid-turn (or Claude is starting), queues it for the
   * turn's end (takeQueuedPr). A queued Open PR is cleared once the prompt is about to go. Pressed
   * again with instructions (blank ones included) while one is queued, they replace its own.
   */
  async openPr(row: SessionRow, body?: unknown): Promise<void> {
    const given = openPrInstructions(body);
    if (row.stage !== 'code') throw new HttpError(409, 'Open PR is only available in the Code stage.');
    if (row.desired === 'running' && !this.sessions.runtime(row.id).busy && this.turnRunning(row)) {
      if (row.queued_gate !== 'open-pr') {
        const text = given ? 'Open PR queued with your instructions: it goes when AgentK finishes its turn' : 'Open PR queued: it goes when AgentK finishes its turn';
        this.queueGate(row.id, 'open-pr', text, given ?? '');
      } else if (given !== null && given !== (row.queued_instructions ?? '')) {
        // Pressed with instructions, Run to merge's Open PR is the user's now: stopping it keeps it.
        this.sessions.update(row.id, { queued_instructions: given || null, queued_auto: 0 });
        this.sessions.event(row.id, 'queued', given ? 'Your instructions for the queued Open PR changed' : 'Your instructions for the queued Open PR were removed');
      } else if (row.queued_auto) {
        this.sessions.update(row.id, { queued_auto: 0 });
      }
      return;
    }
    // Pressed between a turn's end and the queued Open PR going: it goes now, with the queued
    // instructions unless this press gives its own. Read before the check, which clears them.
    const instructions = given ?? (row.queued_gate === 'open-pr' ? row.queued_instructions ?? '' : '');
    const check = this.prCheck(row.id, false);
    check(row);
    // A local review goes first (review/runner.ts): it sends the PR prompt itself when it's done.
    if (await this.core.reviews.openPr(row, { instructions, by: 'you' })) return;
    await this.sendPr(row, check, { instructions });
  }

  /**
   * The check before the PR prompt goes: still in Code, and Claude can take it. Clears a queued
   * Open PR as it passes. For a queued one (`queued`), a withdrawal meanwhile (Cancel, a move,
   * Claude exiting) stops it with QueueWithdrawn: sendPr waits on GitHub and Meko before the check.
   */
  private prCheck(id: string, queued: boolean): (r: SessionRow) => void {
    return (r) => {
      if (queued && r.queued_gate !== 'open-pr') throw new QueueWithdrawn();
      if (r.stage !== 'code') throw new HttpError(409, 'Open PR is only available in the Code stage.');
      this.checkPromptable(r);
      if (this.sessions.promptsHeld(id) > 0) throw new HttpError(409, 'AgentK has prompts from you to take first. Open the PR once it has.');
      if (r.queued_gate === 'open-pr') this.setQueued(id, null);
    };
  }

  /** A turn is running, or Claude is starting and will take its first prompt. */
  private turnRunning(row: SessionRow): boolean {
    return row.status === 'working' || row.status === 'starting' || this.sessions.midTurn(row.id);
  }

  /** Sends an Open PR queued during a turn, once Claude is between turns; it stays queued until then. */
  private takeQueuedPr(id: string): Promise<void> {
    return this.serial(id, async () => {
      const row = sessionRow(this.db, id);
      if (row?.queued_gate !== 'open-pr' || row.desired !== 'running') return;
      if (row.stage !== 'code') {
        this.dropQueued(id, `the session moved to ${STAGE_LABELS[row.stage]}`);
        return;
      }
      if ((row.status !== 'idle' && row.status !== 'recovered') || this.sessions.runtime(id).busy || this.sessions.promptsHeld(id) > 0) return;
      // Cleared with the gate before the prompt is typed: if it can't be, the activity log keeps them.
      const instructions = row.queued_instructions;
      try {
        // A local review takes the queued Open PR, and sends the PR prompt itself when it's done.
        if (await this.core.reviews.openPr(row, { instructions: instructions ?? '', by: row.auto_run ? 'auto' : 'you' })) {
          this.setQueued(id, null);
          return;
        }
        await this.sendPr(row, this.prCheck(id, true), { by: row.auto_run ? 'auto' : 'you' });
        this.sessions.event(id, 'queued', row.auto_run ? 'Run to merge: PR prompt sent' : 'Queued Open PR sent');
      } catch (e) {
        // Withdrawn while sendPr waited: whatever withdrew it said so in the activity log.
        if (e instanceof QueueWithdrawn) return;
        const cur = sessionRow(this.db, id);
        // Still queued: Claude took another prompt meanwhile, and the PR goes after that turn.
        if (cur?.queued_gate === 'open-pr') return;
        this.warn(id, 'Sending the queued Open PR', e);
        this.sessions.event(id, 'queued', `Queued Open PR not sent: ${errMsg(e)}${instructions ? `\nYour instructions with it:\n${instructions}` : ''}`);
        this.sessions.notifyUser(id, 'Open PR not sent', `The queued PR prompt couldn't be sent (${errMsg(e)}). Press Open PR again${instructions ? '; your instructions are in the activity log' : ''}.`);
      }
    });
  }

  /**
   * Types the PR prompt (with a moved session's `moved` line first, `lead`, and the user's
   * `instructions` last): the session goes to the PR stage, and back to where it was if the prompt
   * can't be typed. `check` runs again after waiting on GitHub and Meko, which gave Claude and the
   * user time to move on. Without `instructions` (a queued Open PR, from takeQueuedPr), those kept
   * with the queued gate go.
   */
  private async sendPr(row: SessionRow, check: (r: SessionRow) => void, opts: PrSend = {}): Promise<void> {
    if (row.branch_reused) await this.followPrBase(row);
    // A branch whose PR head is on another remote (a fork's) is pushed there, not to origin.
    const footer = [row.push_remote ? pushLine(row) : '', opts.reviewLine ? meAkReviewFooter(opts.reviewLine) : '', await this.prFooter(row)].filter(Boolean).join('\n\n');
    const cur = this.sessions.row(row.id);
    // Read before the check, which clears a queued Open PR with its instructions; the latest, if a
    // press replaced them while this waited.
    const instructions = opts.instructions ?? (cur.queued_gate === 'open-pr' ? cur.queued_instructions ?? '' : '');
    check(cur);
    // The stage moves first: Claude's turn can end before submit returns.
    this.sessions.setStage(row.id, 'pr', opts.by ?? 'you');
    this.sessions.setBusy(row.id, true);
    // This one replaces a PR prompt the last Claude lost.
    this.lostPrs.delete(row.id);
    const sent = { ...opts, instructions, back: cur.stage };
    try {
      await this.sessions.submit(row.id, [opts.lead, this.sessions.prompt(cur, 'pr'), footer, this.instructionsPrompt(cur, instructions)].filter(Boolean).join('\n\n'), () => this.prLost(row.id, sent));
    } catch (e) {
      this.sessions.setBusy(row.id, false);
      if (this.sessions.row(row.id).stage === 'pr') this.sessions.setStage(row.id, cur.stage, 'auto');
      throw new HttpError(502, `Couldn't send the PR prompt to Claude: ${errMsg(e)}. Try again.`);
    }
  }

  /**
   * Claude went after taking the PR prompt but before running it (sessions.submit's `lost`): the
   * session goes back to where it was, as when the prompt can't be typed, a local review's PR
   * counts as not sent (so Open PR sends it with the findings), and the prompt goes again once
   * Claude is back (claudeBack, or the end of its next turn).
   */
  private prLost(id: string, sent: PrSend & { back: Stage }): void {
    const row = sessionRow(this.db, id);
    if (!row || row.desired !== 'running' || row.stage !== 'pr') return;
    this.sessions.setBusy(id, false);
    this.sessions.setStage(id, sent.back, 'auto');
    // Only a review's own PR prompt (sendReviewedPr): another one's loss mustn't bring back an older review's findings.
    if (sent.reviewLine !== undefined) this.core.reviews.markUnsent(id);
    this.lostPrs.set(id, sent);
    const name = agentName(row);
    const press = sent.back === 'code' ? 'Open PR' : 'a move to Review & PR';
    this.sessions.event(id, 'pr', `${name} exited before taking the PR prompt; it goes again once ${name} is back`);
    this.sessions.notifyUser(id, 'PR prompt not taken', `${name} exited before taking the PR prompt. It goes again once ${name} is back (Resume the session if it shows as exited), or ${press} sends it.`);
  }

  /** Claude is back between turns after a launch with no prompt (a resume): a PR prompt the last one lost goes now. */
  async claudeBack(id: string): Promise<void> {
    await this.resendLostPr(id);
  }

  /**
   * Sends again a PR prompt the last Claude lost, once Claude can take it in the stage the session
   * went back to; dropped when the session moved on meanwhile. True when it went.
   */
  private async resendLostPr(id: string): Promise<boolean> {
    const sent = this.lostPrs.get(id);
    const row = sent && sessionRow(this.db, id);
    if (!sent || !row) return false;
    if (row.desired !== 'running' || row.stage !== sent.back) {
      this.lostPrs.delete(id);
      return false;
    }
    // An Open PR the user pressed since (with their latest instructions) goes instead: takeQueuedPr sends it.
    if (row.queued_gate === 'open-pr' && !row.queued_auto) {
      this.lostPrs.delete(id);
      return false;
    }
    // Run to merge's PR prompt goes only while Run to merge is on (stopping it drops it too: autoRunOff).
    if (sent.by === 'auto' && !row.auto_run) {
      this.lostPrs.delete(id);
      return false;
    }
    try {
      this.checkPromptable(row);
    } catch {
      return false; // not yet: the next turn's end tries again
    }
    if (this.sessions.promptsHeld(id) > 0) return false;
    this.lostPrs.delete(id);
    try {
      await this.serial(id, () => this.sendPr(row, (r) => {
        if (r.stage !== sent.back) throw new HttpError(409, `The session moved to ${STAGE_LABELS[r.stage]} meanwhile.`);
        this.checkPromptable(r);
        if (r.queued_gate === 'open-pr' && !r.queued_auto) throw new QueueWithdrawn();
        // Run to merge's Open PR, queued at a turn's start meanwhile, is this one.
        if (r.queued_gate === 'open-pr') this.setQueued(id, null);
      }, sent));
      if (sent.reviewLine !== undefined) this.core.reviews.markSent(id);
      this.sessions.event(id, 'pr', 'PR prompt sent again');
      return true;
    } catch (e) {
      // The user pressed Open PR while this waited on GitHub and Meko: theirs goes, with its instructions.
      if (e instanceof QueueWithdrawn) return false;
      const how = sent.back === 'code' ? 'Press Open PR to send it.' : 'Move the session to Review & PR to send it.';
      this.warn(id, 'Sending the PR prompt again', e);
      this.sessions.event(id, 'pr', `The PR prompt didn't go again: ${errMsg(e)}. ${how}`);
      this.sessions.notifyUser(id, 'Open PR not sent', `The PR prompt couldn't be sent again (${errMsg(e)}). ${how}`);
      return false;
    }
  }

  /** Refuses while Claude can't take a prompt: mid-turn, waiting on the user (a ready design aside), or exited. */
  private checkPromptable(row: SessionRow): void {
    this.checkBetweenTurns(row);
    if (row.status === 'needs_input' && row.status_detail !== DESIGN_READY) throw new HttpError(409, 'AgentK is waiting on you in the terminal. Answer it first.');
    if (row.status === 'exited') throw new HttpError(409, `${agentName(row)} has exited. Resume the session first.`);
  }

  /**
   * A task's local review asks it to fix a round's findings before the next round (review/runner.ts).
   * The task stays in Code, and its turn's end starts the next round. `moved`: the `moved` line of a
   * move to Review & PR that started the review from Explore or Design (reviewBeforePr), first.
   */
  async sendReviewFix(task: SessionRow, o: { strategy: string; round: number; findings: string; outcomeHow: string; moved?: string | null }): Promise<void> {
    const row = this.sessions.row(task.id);
    if (row.stage !== 'code' || row.desired !== 'running') throw new Error('the task left Code');
    const fix = this.sessions.prompt(row, 'code-review-fix', { strategy: o.strategy, round: o.round, findings: o.findings, outcomeHow: o.outcomeHow });
    await this.sessions.submit(row.id, [o.moved, fix].filter(Boolean).join('\n\n'));
  }

  /**
   * A task's local review is done (or the user opened the PR past a stopped one): the PR prompt goes,
   * led by the last round's findings when it had some, with the `me_ak_review:` line (none past a
   * stopped review), after the `moved` line of a move that started the review, when that line hasn't gone
   * with the review's fixes. False when it couldn't go; the user's Open PR sends it later.
   */
  async sendReviewedPr(task: SessionRow, o: { line: string | null; findings: string; outcomeHow: string; strategy: string; instructions: string; by: 'you' | 'auto'; moved?: string | null }): Promise<boolean> {
    const row = this.sessions.row(task.id);
    if (row.stage !== 'code' || row.desired !== 'running') return false;
    const findings = o.findings ? this.sessions.prompt(row, 'code-review-findings', { strategy: o.strategy, findings: o.findings, outcomeHow: o.outcomeHow }) : '';
    const lead = [o.moved, findings].filter(Boolean).join('\n\n') || undefined;
    try {
      await this.serial(row.id, () => this.sendPr(row, this.prCheck(row.id, false), { lead, instructions: o.instructions, by: o.by, reviewLine: o.line }));
      this.core.reviews.markSent(row.id);
      return true;
    } catch (e) {
      this.warn(row.id, 'Sending the PR prompt after the local review', e);
      this.sessions.event(row.id, 'review', `The PR prompt didn't go after the local review: ${errMsg(e)}. Press Open PR to send it.`);
      this.sessions.notifyUser(row.id, 'Open PR not sent', `The local review is done, but the PR prompt couldn't be sent (${errMsg(e)}). Press Open PR to send it.`);
      return false;
    }
  }

  /** A task's local review failed or was cancelled: its Open PR waits for Retry or Open anyway, Run to merge's too. */
  reviewStopped(id: string): void {
    const row = sessionRow(this.db, id);
    if (row?.queued_gate === 'open-pr') this.setQueued(id, null);
  }

  /**
   * After the PR is found: a task whose local review finished has the review's `me_ak_review:` line
   * in the description, written in by AgentK when Claude left it out. Never throws.
   */
  private async syncReviewLine(id: string): Promise<void> {
    const row = sessionRow(this.db, id);
    const line = row?.pr_number ? this.core.reviews.prLine(id) : null;
    if (!row?.pr_number || !line) return;
    try {
      const repo = prRepoOf(row.pr_url);
      const cwd = this.ghCwd(row);
      const body = await github.prBody(cwd, row.pr_number, repo);
      if (findMeAkReviewLine(body) === line) return;
      await github.setPrBody(cwd, row.pr_number, withMeAkReviewLine(body, line), repo);
      this.sessions.event(id, 'review', `Put the local review's line in PR #${row.pr_number}: ${line}`);
    } catch (e) {
      this.warn(id, `Putting the local review's line in PR #${row.pr_number}`, e);
    }
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
    const instructions = checkInstructions(req?.instructions);
    await this.serial(row.id, () => this.doMove(row.id, stage, prompt, (task ?? '').trim(), instructions));
  }

  /**
   * `task`: the session's first task, for one that has none ('' leaves it without). `instructions`:
   * the user's, sent after the stage's prompt, or after the `moved` line alone when the move sends
   * no stage prompt; '' for none.
   */
  private async doMove(id: string, stage: MoveSessionRequest['stage'], prompt: boolean | undefined, task: string, instructions: string): Promise<void> {
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
    // A gate queued in the stage it leaves (Open PR, a merge) isn't wanted where it goes, and a move
    // takes the gates back from Run to merge.
    const moved = `the session moved to ${STAGE_LABELS[stage]}`;
    const dropQueued = () => {
      this.stopAutoRun(id, moved);
      this.dropQueued(id, moved);
    };
    if (instructions) this.checkPromptable(cur);
    // A move within Review & PR was refused above (moveError).
    if (stage === 'review') {
      // Off before the PR is picked up, which would queue its merge; back on if the move is refused.
      const wasOn = this.autoRunOff(id);
      this.dropQueued(id, moved);
      try {
        await this.moveToReview(this.sessions.row(id), instructions);
      } catch (e) {
        if (wasOn && this.sessions.row(id).stage === cur.stage) this.sessions.update(id, { auto_run: 1 });
        throw e;
      }
      // A move a local review took over leaves the session in Code until the review sends the PR prompt.
      if (wasOn) this.sessions.event(id, 'auto-run', `Run to merge stopped: ${this.sessions.row(id).stage === 'code' ? 'the session moved to Code for its local review' : moved}`);
      return;
    }

    const send = offersPrompt(cur.stage, stage) && (prompt ?? promptByDefault(cur.stage, stage));
    if (send) this.checkPromptable(cur);
    // A task given with the move is the session's first (the dialog offers it only to one without).
    const newTask = !!task && !cur.initial_prompt;
    if (newTask) this.sessions.update(id, { initial_prompt: task });
    // Designs are written as markdown or an Artifact now; plan mode is only for older sessions.
    if (stage === 'design') this.sessions.update(id, { plan_mode: 0 });
    if (movesStep(cur.stage, stage)) {
      dropQueued();
      if (cur.stage === 'pr' || cur.stage === 'review') this.leaveReview(id);
      if (cur.stage === 'code') this.core.reviews.leftCode(id);
      if (cur.stage === 'design') this.leaveDesign(id, stage === 'code');
      this.sessions.setStage(id, stage, 'you');
      // Back in Design, what an approval left to AgentK is open again.
      if (stage === 'design') this.questionsUpdate(id, 'Reopening', (row) => this.core.questions.reopen(row));
    }
    if (!send && !instructions) return;
    const row = this.sessions.row(id);
    // The stage's start prompt names the task; that's right only for one just given. Otherwise AgentK
    // carries on from the conversation, which may have moved on from the first prompt.
    const body = send ? this.sessions.prompt(row, newTask ? this.sessions.firstPrompt(row) : movePrompt(stage)) : '';
    try {
      await this.sessions.submit(id, [this.movedLine(row), body, this.instructionsPrompt(row, instructions)].filter(Boolean).join('\n\n'));
    } catch (e) {
      const what = send ? `the ${STAGE_LABELS[stage]} prompt` : 'your instructions';
      throw new HttpError(502, `Moved to ${STAGE_LABELS[stage]}, but couldn't send ${what} to Claude: ${errMsg(e)}. Ask AgentK in the terminal.`);
    }
  }

  /**
   * A move to Review & PR: picks up the branch's open PR when there's nothing to push (the branch
   * tip is in the PR and the worktree is clean), or else does what Open PR does: the local review
   * first, when it runs (reviewBeforePr), else the PR prompt. The user's `instructions` go after the
   * PR prompt, or with the `moved` line once a PR is picked up.
   */
  private async moveToReview(row: SessionRow, instructions: string): Promise<void> {
    const cwd = this.ghCwd(row);
    const pr = await this.branchPr(row, true).catch(() => null);
    let adopt = false;
    if (pr?.state === 'OPEN') {
      const st = await github.prStatus(cwd, pr.number, prRepoOf(pr.url)).catch(() => null);
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
      // As for Open PR: the user's prompts go first, and a stopped local review holds the PR. Outside
      // Code an earlier review holds nothing (its Retry and Open anyway are Code's).
      if (cur.stage !== 'code') this.core.reviews.leftCode(cur.id);
      if (this.sessions.promptsHeld(cur.id) > 0) throw new HttpError(409, 'AgentK has prompts from you to take first. Move the session once it has.');
      this.core.reviews.checkNotHeld(cur.id);
    }
    if ((!adopt || !pr) && this.core.reviews.applies(cur)) {
      await this.reviewBeforePr(cur, instructions);
      return;
    }
    if (!adopt || !pr) {
      await this.sendPr(cur, (r) => {
        check(r);
        this.checkPromptable(r);
      }, { lead: cur.stage === 'code' ? undefined : this.movedLine(cur, 'review'), instructions });
      return;
    }
    if (instructions) this.checkPromptable(cur);
    if (cur.stage === 'design') this.leaveDesign(row.id, false);
    // As sendPr does, the stage moves first and the session is busy until the turn ends: review
    // prompts waiting for the session go after the instructions' turn, not before or during it.
    if (instructions) this.sessions.setBusy(row.id, true);
    try {
      await this.adoptPr(row.id, pr, 'you');
    } catch (e) {
      if (instructions) this.sessions.setBusy(row.id, false);
      throw e;
    }
    if (!instructions) return;
    try {
      await this.sessions.submit(row.id, `${this.movedLine(this.sessions.row(row.id))}\n\n${this.instructionsPrompt(cur, instructions)}`);
    } catch (e) {
      this.sessions.setBusy(row.id, false);
      throw new HttpError(502, `Moved to Review & PR, but couldn't send your instructions to Claude: ${errMsg(e)}. Ask AgentK in the terminal.`);
    }
  }

  /** The `instructions` prompt with the user's text, or '' when they gave none. */
  private instructionsPrompt(row: SessionRow, instructions: string): string {
    return instructions ? this.sessions.prompt(row, 'instructions', { instructions }) : '';
  }

  /**
   * A move to Review & PR with work to push, on a task whose local review runs: as with Open PR, the
   * review goes first, in Code (from Explore or Design too, past the design approval), and sends the
   * PR prompt when it's done. moveToReview has checked what Open PR checks.
   */
  private async reviewBeforePr(row: SessionRow, instructions: string): Promise<void> {
    this.checkPromptable(row);
    // Claude's system prompt still says where it was (Explore: no PR): the `moved` line goes before the
    // review's first prompt to the task, its fixes or the PR prompt.
    let lead: string | undefined;
    if (row.stage !== 'code') {
      if (row.stage === 'design') this.leaveDesign(row.id, true);
      this.sessions.setStage(row.id, 'code', 'you', { event: false });
      this.sessions.event(row.id, 'stage', 'Moved to Code: AgentK reviews the change locally before it opens the PR');
      lead = this.movedLine(this.sessions.row(row.id));
    }
    await this.core.reviews.openPr(this.sessions.row(row.id), { instructions, by: 'you', lead });
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
   * Leaving Review & PR by hand, between turns: the PR stays open and queued review, conflict and CI prompts
   * wait for the session to come back (deliverNext only sends in Review). Prompts marked sent
   * belong to a turn that's over, as onTurnEnd would have decided had the session stayed.
   */
  private leaveReview(id: string): void {
    const q = this.queue(id);
    for (const n of QUEUE_ORDER) if (q[n]?.state === 'sent') delete q[n];
    this.setQueue(id, q);
    if (!reviewQueued(q) && this.sessions.row(id).review_state === 'addressing') {
      this.sessions.update(id, { review_state: 'addressed' });
    }
    this.ciFailSince.delete(id);
    this.headSeen.delete(id);
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
    const design = this.prDesignRef(row);
    return mekoPrFooter(mekoHost(this.core.config), dp, conv, design && designLine(design));
  }

  /**
   * A session started on an existing branch that already has an open PR works against that PR's
   * base, so the PR prompt, the diff and the Changes panel compare with what the PR merges into.
   */
  private async followPrBase(row: SessionRow): Promise<void> {
    const pr = await this.branchPr(row, true).catch(() => null);
    const base = pr?.state === 'OPEN' ? pr.baseRefName : undefined;
    if (!base || base === row.base_branch) return;
    this.sessions.update(row.id, { base_branch: base });
    this.sessions.event(row.id, 'base', `Base set to ${base}, the base of PR #${pr!.number}`);
  }

  /**
   * Called at every turn's end: its Stop hook, or the mod reporting a turn you interrupted
   * (`interrupted`). An interrupted turn didn't do what it was sent for: the review, CI or
   * conflicts prompt it got stays sent (the next turn to end counts as the one that took it) and
   * the findings stay being addressed, so a queued merge doesn't go on findings nobody addressed.
   */
  async onTurnEnd(id: string, interrupted = false): Promise<void> {
    const row = sessionRow(this.db, id);
    if (!row) return;
    this.sessions.setBusy(id, false);
    // A code-review session's turn moves its review on, and nothing else acts on it.
    if (row.kind === 'code-review') {
      await this.core.reviews.onTurnEnd(row, interrupted);
      return;
    }
    // A Kiro turn just ran: re-read the plan so the rail's credits stay fresh, at most every
    // KIRO_PLAN_TURN_EVERY_MS so a run of turns reads once. Never awaited; never blocks the turn.
    if (row.harness === 'kiro') void this.core.kiroPlan.refreshIfStale(KIRO_PLAN_TURN_EVERY_MS);
    // A typed learn step's turn (no mod) is over; at Merge & Close the poll below then cleans up.
    this.core.learn.turnEnded(id);
    // A Discard that waited for it goes now, and nothing else for the session does.
    if (this.sessions.row(id).discarding) return this.afterLearn(id);
    // Esc on a turn is the user stepping in: Run to merge stops before a queued Open PR could go.
    if (interrupted && (row.stage === 'code' || row.stage === 'pr' || row.stage === 'review')) this.stopAutoRun(id, 'you interrupted AgentK');
    // A move the session's own agent asked for (AgentK's session tools) goes now, and gives it its next turn.
    if (await this.core.tools.turnEnded(id, interrupted)) return;
    // A PR prompt the last Claude lost goes now (a recovery's continue turn came first), unless the user stepped in.
    if (!interrupted && (await this.resendLostPr(id))) return;
    // A learn session that wrote its summary is done, and closes.
    if (row.stage === 'explore' && row.purpose === 'learn' && !interrupted) {
      const closed = await this.learnDone(row).catch((e: unknown) => {
        this.warn(id, 'Closing the learn session', e);
        return false;
      });
      if (closed) return;
    }
    if (row.stage === 'code') {
      // A turn that fixed a local review's findings starts the review's next round, which opens the PR itself.
      if (await this.core.reviews.onTurnEnd(row, interrupted)) {
        if (this.sessions.row(id).queued_gate === 'open-pr') this.setQueued(id, null);
        return;
      }
      await this.takeQueuedPr(id).catch((e) => this.warn(id, 'Sending the queued Open PR', e));
    } else if (row.stage === 'pr') {
      await this.pollPrStage(row).catch((e) => this.warn(row.id, 'PR lookup failed', e));
    } else if (row.stage === 'review') {
      await this.serial(id, async () => {
        // The turn that got a prompt is over. Only that turn addresses a review: one still queued isn't.
        const q = this.queue(id);
        if (!interrupted) {
          for (const n of QUEUE_ORDER) if (q[n]?.state === 'sent') delete q[n];
          this.setQueue(id, q);
        }
        const review = reviewQueued(q);
        if (!review && this.sessions.row(id).review_state === 'addressing') {
          this.sessions.update(id, { review_state: 'addressed' });
        }
        // A queued review goes out now. Anything else waits for a fresh look at the PR, so a CI
        // prompt never goes out for a head this turn's push replaced.
        if (review) await this.deliverNext(id);
        else await this.doPollReview(id);
      }).catch((e) => this.warn(id, 'PR status failed', e));
    } else if (row.stage === 'merge') {
      // A merge that landed while Claude was working: close now that the turn is over.
      await this.pollReview(row).catch((e) => this.warn(id, 'PR status failed', e));
    } else if (row.stage === 'reviewing-pr') {
      await this.serial(id, async () => {
        // The turn that got the update summary (or the PR's new design) is over, unless interrupted.
        const q = this.queue(id);
        const sent = interrupted ? [] : REVIEWING_ORDER.filter((n) => q[n]?.state === 'sent');
        if (sent.length) {
          sent.forEach((n) => delete q[n]);
          this.setQueue(id, q);
        }
        await this.doPollReviewedPr(id);
      }).catch((e) => this.warn(id, 'PR status failed', e));
    }
    // Review comments go after anything above that already gave Claude its next turn.
    await this.core.comments.deliver(id).catch((e) => this.warn(id, 'Sending review comments', e));
  }

  /**
   * After a learn session's turn: when Claude wrote its summary (`learnedPath`), it's done, so the
   * summary goes on the timeline and the session is discarded, with no learn step (it saved as it
   * went). Not when its worktree has changes or commits, which Discard would throw away: the timeline
   * says so and it stays. A Save learnings step still running holds the close, as it holds a
   * Discard: the session goes when the step ends (`discarding`, afterLearn). The file is removed,
   * and the user told, only once the session is on its way out (or kept), so a close that fails
   * leaves the file for the next turn's end to try again. True when the session is closed or closing.
   */
  private async learnDone(row: SessionRow): Promise<boolean> {
    const file = this.sessions.learnedPath(row);
    let summary: string;
    try {
      summary = readFileSync(file, 'utf8').trim();
    } catch {
      return false;
    }
    const text = summary.length > LEARNED_MAX ? `${summary.slice(0, LEARNED_MAX).trimEnd()}…` : summary || 'Done learning the project.';
    const check = await this.sessions.discardCheck(row.id);
    const changed = [
      check.uncommittedFiles ? `${check.uncommittedFiles} uncommitted file${check.uncommittedFiles === 1 ? '' : 's'}` : '',
      check.unpushedCommits ? `${check.unpushedCommits} commit${check.unpushedCommits === 1 ? '' : 's'} not pushed` : '',
    ].filter(Boolean).join(' and ');
    this.sessions.event(row.id, 'learn-done', text);
    if (changed) {
      rmSync(file, { force: true });
      const why = `Done learning, but not closed: the worktree has ${changed}, which closing would throw away. Look at them, then discard the session.`;
      this.sessions.event(row.id, 'learn-kept', why);
      this.sessions.notifyUser(row.id, 'Done learning', why);
      return false;
    }
    if (this.core.learn.running(row.id)) {
      this.sessions.update(row.id, { discarding: 1 });
    } else {
      await this.sessions.discardNow(row.id);
    }
    rmSync(file, { force: true });
    this.sessions.notifyUser(row.id, 'Learned the project', text.split('\n').find((l) => l.trim())?.trim() ?? text);
    return true;
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

  /** Queues `review-held` with the user's sent reviews, adding to those of a prompt still waiting to go out. */
  private enqueueHeld(id: string, lines: string[]): void {
    const waiting = this.queue(id)['review-held'];
    const earlier = waiting?.state === 'queued' ? (waiting.vars?.reviews ?? '').split('\n').filter(Boolean) : [];
    this.enqueue(id, 'review-held', { reviews: [...new Set([...earlier, ...lines])].join('\n') });
  }

  /**
   * The branch's PR: the one linked when the session started on an existing branch or a PR, while
   * it's open (found by number in the PR's own repo, so a fork's PR, or one whose head branch has another name, counts),
   * else the one findPr finds by the branch's name (<anyOpen> as there).
   */
  private async branchPr(row: SessionRow, anyOpen: boolean, cwd = this.ghCwd(row)): Promise<github.PrRef | null> {
    if (row.branch_reused && row.pr_number) {
      const linked = await github.prByNumber(cwd, row.pr_number, prRepoOf(row.pr_url)).catch(() => null);
      if (linked?.state === 'OPEN') return linked;
    }
    // Only a PR from where the branch is pushed: a fork's branch of the same name isn't this one's.
    return github.findPr(cwd, row.branch, row.created_at, anyOpen, await remoteRepo(cwd, row.push_remote ?? 'origin'));
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
        // A queued merge is checked at every pass (every POLL_MS), so it goes soon after CI does.
        const due = (row.queued_gate === 'merge' && !this.autoRunBlocked.has(row.id)) || now - (this.lastPoll.get(row.id) ?? 0) >= REVIEW_POLL_MS;
        if (row.desired !== 'running') {
          // A cleanup that a restart cut short after archiving: finish it.
          if (row.stage === 'merge' && due && now - (row.archived_at ?? 0) < CLOSING_RESUME_MS) {
            await this.pollReview(row).catch((e) => this.warn(row.id, 'PR status failed', e));
          }
          continue;
        }
        if (row.stage === 'code' && row.queued_gate === 'open-pr') {
          // A turn that ended while the server was down, or a prompt that came between.
          await this.takeQueuedPr(row.id).catch((e) => this.warn(row.id, 'Sending the queued Open PR', e));
        } else if (row.stage === 'pr') {
          await this.pollPrStage(row).catch((e) => this.warn(row.id, 'PR lookup failed', e));
        } else if ((row.stage === 'review' || row.stage === 'merge') && (due || (row.stage === 'review' && row.auto_run && !row.queued_gate))) {
          // Run to merge's merge, after a restart that cut it short: queued again, and checked at every pass.
          if (row.stage === 'review') this.armMerge(row.id);
          await this.pollReview(row).catch((e) => this.warn(row.id, 'PR status failed', e));
        } else if (row.stage === 'reviewing-pr' && due && !this.sessions.isCreating(row.id)) {
          // Not while its PR head is still being fetched: a push meanwhile isn't news, the fetch has it.
          await this.pollReviewedPr(row).catch((e) => this.warn(row.id, 'PR status failed', e));
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
    // A session on a branch that existed before it takes that branch's open PR, however old.
    const pr = await this.branchPr(before, !!before.branch_reused, before.worktree_path);
    if (!pr || sessionRow(this.db, id)?.stage !== 'pr') return;
    // The PR it had before a move back to Code, merged or closed since: wait for the new one.
    if (pr.number === before.pr_number && pr.state !== 'OPEN') return;
    await this.adoptPr(id, pr, 'auto');
  }

  /** The session works on this PR from now on: it goes to Review and the PR is polled right away. */
  private async adoptPr(id: string, pr: github.PrRef, by: 'you' | 'auto'): Promise<void> {
    const before = this.sessions.row(id);
    // The PR's base is the session's from now on, whatever base the PR prompt named (it may have
    // changed since, from the Changes panel, before the PR was found).
    if (pr.baseRefName && pr.baseRefName !== before.base_branch) {
      this.sessions.update(id, { base_branch: pr.baseRefName });
      this.sessions.event(id, 'base', `Base set to ${pr.baseRefName}, the base of PR #${pr.number}`);
    }
    if (before.pr_number === pr.number) {
      // Back on a PR it had before a move: its review state carries on, and it's not another PR for the stats.
      if (before.review_state === 'none') this.sessions.update(id, { review_state: 'waiting' });
      this.sessions.event(id, 'pr-again', `Back on PR #${pr.number}`, { url: pr.url });
    } else {
      this.sessions.update(id, { pr_number: pr.number, pr_url: pr.url, review_state: 'waiting', agentk_seen: 0, ci_fix_head: null, ci_fixes: 0, conflict_head: null, conflict_fixes: 0 });
      // A session on an existing branch now works on a PR's head: whether it's trusted, before its prompts.
      await this.sessions.checkHead(id);
      dropOtherPrs(this.db, id, pr.number);
      // A review-held prompt still waiting names the old PR's reviews.
      const q = this.queue(id);
      if (q['review-held']?.state === 'queued') {
        delete q['review-held'];
        this.setQueue(id, q);
      }
      const older = by === 'you' || Date.parse(pr.createdAt) < before.created_at - github.CLOCK_SKEW_MS;
      this.sessions.event(id, 'pr', older ? `Picked up PR #${pr.number}` : `Opened PR #${pr.number}`, { url: pr.url });
    }
    // Opening the PR and its review are one step in the UI: "Opened PR #7" already says the session moved on.
    this.sessions.setStage(id, 'review', by, { event: before.stage !== 'pr' });
    void this.syncReviewLine(id);
    this.armMerge(id);
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
    // Mid-turn as the branch and worktree are measured: a turn that pushes and ends during this
    // poll leaves unpushed commits measured before its push. Its Stop polls again after this one.
    const turnAtStart = this.inTurn(before);
    const st = await github.prStatus(this.ghCwd(before), before.pr_number, prRepoOf(before.pr_url));
    if (st.headRefOid && this.headSeen.get(id)?.head !== st.headRefOid) this.headSeen.set(id, { head: st.headRefOid, at: Date.now() });
    // The PR names the latest version of the design, even after Claude rewrote the description.
    if (st.state === 'OPEN') void this.syncPrDesign(id, st.body ?? '');
    const unpushed = st.state === 'OPEN' && (await lacksLocalCommits(sessionRepo(this.db, before) ?? p.repo_path, before.branch, st));
    // Merge & Close ends in an archive, which would keep a dirty worktree on the branch.
    const uncommitted = this.sessions.ownsWorktree(before) ? await uncommittedCount(before.worktree_path) : 0;
    // Read the row again: a turn may have ended or a review been requested during the calls above.
    let row = sessionRow(this.db, id);
    if (!row || row.stage === 'done') return;
    // For finding the session by its PR's title (sessionsearch.ts), merged ones included.
    if (st.title && st.title !== row.pr_title && st.number === row.pr_number) this.sessions.update(id, { pr_title: st.title });
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
      this.giveUpMerge(id);
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

    const botReviews = st.reviews.filter((r) => isAgentk(r.author));
    // Reviews held for the user this poll, kept once GitHub has told their details.
    const toHold: { review: github.PrReview; kind: HeldReviewAuthor }[] = [];
    if (reviewRoute(mode, 'agentk') === 'claude') {
      const fresh = botReviews.filter((r) => !seen.has(r.id));
      if (fresh.length) {
        fresh.forEach((r) => seen.add(r.id));
        const latest = fresh[fresh.length - 1];
        const firstLine = latest.body.split('\n').find((l) => l.trim())?.replace(/\*/g, '').trim() ?? 'Review posted';
        this.sessions.event(row.id, 'review', `AgentK review: ${firstLine}`);
        if (cleanAgentkReview(latest.body)) {
          reviewState = 'approved';
          this.notifyProgress(row.id, 'AgentK review', 'No findings.');
        } else {
          reviewState = 'addressing';
          this.notifyProgress(row.id, 'AgentK review arrived', firstLine);
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
    } else {
      // AgentK's findings wait for the user. A clean review has nothing to send, and People only
      // never marked AgentK's reviews seen before reviews were held, so its earlier rounds aren't news.
      const since = Number(kvGet(this.db, HELD_SINCE_KEY) ?? 0);
      for (const r of botReviews.filter((b) => !seen.has(b.id))) {
        if (cleanAgentkReview(r.body) || Date.parse(r.submittedAt) < since) seen.add(r.id);
        else toHold.push({ review: r, kind: 'agentk' });
      }
    }

    // Everyone else's. AgentK's reviews are never counted here.
    const others = st.reviews.filter((r) => r.author && !isAgentk(r.author) && !seen.has(r.id));
    // False when GitHub couldn't say something the reviews' route needs: they stay unseen for the next poll.
    let ready = true;
    const sendable: github.PrReview[] = [];
    // Reviews that ask for nothing are only logged: someone's with a role on the repo, and the rest's.
    const told: github.PrReview[] = [];
    const toldOutside: github.PrReview[] = [];
    if (others.length) {
      const bots = await this.botLogins(before, st.number);
      const isBot = (r: github.PrReview) => !!bots?.some((b) => github.isLogin(r.author, b));
      // Replies on review threads (Claude's too: it posts as the user) are empty COMMENTED reviews.
      // Another bot's review that asks for nothing is news only where bots' reviews go to Claude.
      const worded = others.filter((r) => (r.state !== 'COMMENTED' || r.body.trim()) && (mode === 'all' || !isBot(r) || asksForWork(r)));
      // Anyone can review a public repo's PR: a review from someone with no role on it is held, and
      // in AgentK only so is a person's, unless it's the user's own (e.g. on a fork's PR upstream).
      const trusted = (r: github.PrReview) => github.isTrustedAuthor(r, settings.reviewerLogin);
      const needUser = worded.some((r) => !isBot(r) && (!trusted(r) || mode === 'agentk'));
      const user = bots && needUser ? await this.viewerLogin(this.ghCwd(before)) : null;
      // Like the bot lookup, an unknown login leaves the reviews unseen, so the user's own review is
      // never held as a stranger's.
      if (!bots || (needUser && !user)) ready = false;
      else {
        const who = (r: github.PrReview): ReviewWho =>
          isBot(r) ? 'bot' : user && github.isLogin(r.author, user) ? 'user' : trusted(r) ? 'person' : 'outsider';
        for (const r of worded) {
          const w = who(r);
          if (!asksForWork(r)) (w === 'outsider' ? toldOutside : told).push(r);
          else if (reviewRoute(mode, w) === 'claude') {
            sendable.push(r);
            told.push(r);
            // reviewRoute never holds the user's own.
          } else toHold.push({ review: r, kind: w as HeldReviewAuthor });
        }
      }
    }
    const details = ready && toHold.length ? await this.reviewDetails(before, toHold.map((h) => h.review.id)) : new Map<string, ReviewDetails>();
    if (ready && details) {
      others.forEach((r) => seen.add(r.id));
      toHold.forEach((h) => seen.add(h.review.id));
      if (told.length) this.sessions.event(row.id, 'review', `Review from ${loginList(told)}`);
      if (toldOutside.length) this.sessions.event(row.id, 'review', `Review from ${loginList(toldOutside)} (not a collaborator on the repo)`);
      if (sendable.length) {
        reviewState = 'addressing';
        this.notifyProgress(row.id, 'Review arrived', `From ${loginList(sendable)}.`);
        this.enqueuePeople(row.id, sendable.map((r) => r.author!.login));
      }
      if (toHold.length) this.hold(row, toHold, details);
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
    const headAt = this.headSeen.get(row.id)?.at;
    const info = this.mergeInfo(this.sessions.row(row.id), st, mode, {
      unpushed, uncommitted, turnAtStart, newHead: headAt !== undefined && Date.now() - headAt < CI_START_MS,
    });
    this.sessions.setMergeInfo(row.id, info);
    this.core.changed();
    await this.takeQueuedMerge(row.id, info);
  }

  /**
   * A queued merge, at the end of a look at the PR: it goes once nothing blocks it, waits while
   * only blockers that clear by themselves are left, and is dropped (the user is told) when another
   * one appears. Runs in the session's serial queue.
   */
  private async takeQueuedMerge(id: string, info: MergeInfo): Promise<void> {
    const row = this.sessions.row(id);
    if (row.queued_gate !== 'merge' || this.merging.has(id) || row.stage !== 'review' || row.desired !== 'running') return;
    if (info.canMerge) {
      this.sessions.event(id, 'queued', 'Queued merge: nothing blocks it now; merging');
      this.notifyProgress(id, 'Merging', `PR #${row.pr_number} is ready: merging it now, as you queued.`);
      this.autoRunTold.delete(id);
      this.autoRunBlocked.delete(id);
      this.merging.add(id);
      try {
        await this.doMerge(id, row.auto_run ? 'auto' : 'you');
      } catch (e) {
        this.sessions.event(id, 'queued', `Queued merge failed: ${errMsg(e)}`);
        // Its merge would be queued again at the next pass, and GitHub asked again: a refusal stops it.
        if (row.auto_run) this.stopAutoRun(id, 'GitHub refused the merge');
        this.sessions.notifyUser(id, 'Merge failed', `${errMsg(e)} ${row.auto_run ? 'Run to merge is off now: press Merge & Close' : 'Press Merge & Close again'} once that's fixed.`);
      } finally {
        this.merging.delete(id);
      }
      return;
    }
    if (info.queueable || row.auto_run) {
      if (row.auto_run) this.autoRunWaits(id, info);
      return;
    }
    const why = info.blockers.join(' ');
    this.dropQueued(id, why);
    this.sessions.notifyUser(id, 'Queued merge dropped', `${why} Press Merge & Close again once that's fixed.`);
  }

  /**
   * Run to merge keeps its merge queued through blockers that don't clear by themselves. AgentK works
   * on failing CI and merge conflicts (their watchers tell the user when they hand them over), and a
   * question in the terminal was told as Needs you; for the rest, the user is told once per new set.
   */
  private autoRunWaits(id: string, info: MergeInfo): void {
    if (info.hard.length) this.autoRunBlocked.add(id);
    else this.autoRunBlocked.delete(id);
    // Failing CI and conflicts AgentK handed over had their watchers' notification (a question in
    // the terminal, its own Needs you, is never in the list).
    const yours = info.yoursWithAutoRun.filter((b) => b !== CI_FAILING && !(b === CONFLICTING && this.core.config.pr.resolveConflicts));
    const key = yours.join('\n');
    if (key === (this.autoRunTold.get(id) ?? '')) return;
    this.autoRunTold.set(id, key);
    if (!yours.length) return;
    this.sessions.event(id, 'auto-run', `Run to merge waits on you: ${yours.join(' ')}`);
    this.sessions.notifyUser(id, 'Run to merge waits on you', `${yours.join(' ')} AgentK merges the PR once that's cleared.`);
  }

  /** Reviews' REST ids, links and inline comments, or null (retry at the next poll) when GitHub can't say. */
  private async reviewDetails(row: SessionRow, ids: string[]): Promise<Map<string, ReviewDetails> | null> {
    try {
      return await github.reviewDetails(this.ghCwd(row), ids);
    } catch (e) {
      this.warn(row.id, 'Reading held reviews', e);
      return null;
    }
  }

  /** Keeps reviews for the user to send to AgentK or dismiss, logs them and tells the user once. */
  private hold(row: SessionRow, found: { review: github.PrReview; kind: HeldReviewAuthor }[], details: Map<string, ReviewDetails>): void {
    // One GitHub has no review for any more (deleted since the poll) has nothing to send or show.
    const held = found.filter((h) => details.has(h.review.id));
    if (held.length < found.length) this.core.log.warn(`GitHub no longer has ${found.length - held.length} of the reviews to hold for ${row.id}; skipped`);
    if (!held.length) return;
    for (const { review: r, kind } of held) {
      holdReview(this.db, row.id, row.pr_number!, {
        id: r.id, author: r.author?.login ?? 'ghost', kind, state: r.state, body: r.body, submittedAt: r.submittedAt,
      }, details.get(r.id)!, row.pr_url ?? '');
    }
    const names = (kinds: (k: HeldReviewAuthor) => boolean) => loginList(held.filter((h) => kinds(h.kind)).map((h) => h.review));
    const inside = names((k) => k !== 'outsider');
    const outside = names((k) => k === 'outsider');
    if (inside) this.sessions.event(row.id, 'review', `Review from ${inside} (held for you)`);
    if (outside) this.sessions.event(row.id, 'review', `Review from ${outside} (held for you; not a collaborator on the repo)`);
    const one = held.length === 1;
    this.sessions.notifyUser(row.id, 'Review held', `From ${loginList(held.map((h) => h.review))}. Send ${one ? 'it' : 'them'} to AgentK or dismiss ${one ? 'it' : 'them'}.`);
  }

  /** The reviews on the session's PR held for the user, oldest first. */
  heldReviews(id: string): HeldReview[] {
    const row = this.sessions.row(id);
    return heldReviews(this.db, id, row.pr_number);
  }

  /**
   * The user's decision on held reviews: send them to Claude with the `review-held` prompt, which
   * only Review & PR does, or dismiss them. Then a fresh look
   * at the PR, so the merge blocker they were goes.
   */
  async decideHeldReviews(id: string, body: unknown): Promise<HeldReviewsResult> {
    const req = checkHeldRequest(body);
    return this.serial(id, async () => {
      const row = this.sessions.row(id);
      if (!row.pr_number) throw new HttpError(409, 'This session has no PR, so it has no held reviews.');
      if (req.action === 'send' && (row.stage !== 'review' || row.desired !== 'running')) {
        throw new HttpError(409, 'Held reviews go to AgentK in Review & PR. Move the session back there first, or dismiss them.');
      }
      const done = decideHeld(this.db, id, row.pr_number, req.ids, req.action === 'send' ? 'sent' : 'dismissed');
      if (!done.length) throw new HttpError(409, 'Those reviews were already sent or dismissed.');
      const what = `the held ${done.length === 1 ? 'review' : 'reviews'} from ${andList([...new Set(done.map((r) => r.author))])}`;
      if (req.action === 'send') {
        // AgentK's too: `review-held` names each review, where `review` has Claude read every round.
        this.enqueueHeld(id, heldPromptLines(done));
        this.sessions.update(id, { review_state: 'addressing' });
        this.sessions.event(id, 'review', `Sent ${what} to AgentK`);
        // Before the look at GitHub, so the prompt goes even while GitHub can't be reached.
        await this.deliverNext(id).catch((e) => this.warn(id, 'Sending a queued prompt', e));
      } else {
        this.sessions.event(id, 'review', `Dismissed ${what}`);
      }
      await this.doPollReview(id).catch((e) => this.warn(id, 'PR status failed', e));
      this.core.changed();
      return { done: done.length };
    });
  }

  /** The PR's bot reviewers, or null (retry at the next poll) when GitHub can't say. */
  private async botLogins(row: SessionRow, pr: number): Promise<string[] | null> {
    try {
      return await github.botLogins(this.ghCwd(row), pr, prRepoOf(row.pr_url));
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
    if (ci.state !== 'failure' || !head || row.stage !== 'review' || st.state !== 'OPEN') return;
    // AgentK's last attempt ended on this head without a fix (no push): handed over like a new head.
    const lastTried = row.ci_fixes === MAX_CI_FIXES && !q.ci;
    if (row.ci_fix_head === head && !lastTried) return;
    let since = this.ciFailSince.get(id);
    if (since?.head !== head) this.ciFailSince.set(id, (since = { head, at: Date.now() }));
    if ((ci.pending && Date.now() - since.at < CI_SETTLE_MS) || unpushed) return;
    this.ciFailSince.delete(id);
    const names = ci.failed.map((c) => c.name).join(', ');
    if (row.ci_fixes >= MAX_CI_FIXES) {
      // Past MAX_CI_FIXES: handed over on ci_fix_head (mergeInfo's `yours`), and told once.
      this.sessions.update(id, { ci_fix_head: head, ci_fixes: MAX_CI_FIXES + 1 });
      this.sessions.event(id, 'ci', `CI failed: ${names}. Still failing after ${MAX_CI_FIXES} fix attempts; over to you`, { sha: head, checks: ci.failed });
      this.sessions.notifyUser(id, 'CI still failing', `${names} failed after ${MAX_CI_FIXES} fix attempts. Take a look.`);
      return;
    }
    this.sessions.update(id, { ci_fix_head: head, ci_fixes: row.ci_fixes + 1 });
    this.sessions.event(id, 'ci', `CI failed: ${names}`, { sha: head, checks: ci.failed });
    const checks = ci.failed.map((c) => `- ${c.name}: ${c.conclusion.toLowerCase()}${c.url ? ` (${c.url})` : ''}`).join('\n');
    this.enqueue(id, 'ci', { sha: head, checks });
    const next = this.queue(id);
    const later = reviewQueued(next) ? 'AgentK will fix it after the review.'
      : next.conflicts ? 'AgentK will look at it after resolving the merge conflicts.' : 'AgentK is fixing it.';
    this.notifyProgress(id, 'CI failed', `${names}. ${later}`);
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
    if (!on || st.mergeable !== 'CONFLICTING' || !head || row.stage !== 'review' || st.state !== 'OPEN' || unpushed) return;
    // AgentK's last attempt ended on this head and it still conflicts: handed over like a new head.
    if (row.conflict_head === head && !(row.conflict_fixes === MAX_CONFLICT_FIXES && !q.conflicts)) return;
    const base = row.base_branch;
    if (row.conflict_fixes >= MAX_CONFLICT_FIXES) {
      // Past MAX_CONFLICT_FIXES: handed over on conflict_head (mergeInfo's `yours`), and told once.
      this.sessions.update(id, { conflict_head: head, conflict_fixes: MAX_CONFLICT_FIXES + 1 });
      this.sessions.event(id, 'conflicts', `Merge conflicts with ${base}. Still there after ${MAX_CONFLICT_FIXES} attempts; over to you`, { sha: head });
      this.sessions.notifyUser(id, 'Merge conflicts remain', `PR #${st.number} still conflicts with ${base} after ${MAX_CONFLICT_FIXES} attempts. Take a look.`);
      return;
    }
    this.sessions.update(id, { conflict_head: head, conflict_fixes: row.conflict_fixes + 1 });
    this.sessions.event(id, 'conflicts', `Merge conflicts with ${base}`, { sha: head });
    this.enqueue(id, 'conflicts', { sha: head });
    const next = this.queue(id);
    const afterReview = reviewQueued(next);
    this.notifyProgress(id, 'Merge conflicts', `PR #${st.number} conflicts with ${base}. ${afterReview ? 'AgentK will resolve them after the review.' : 'AgentK is resolving them.'}`);
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

  /**
   * What blocks the merge, in plain words. A blocker that clears by itself (`waits`: AgentK at
   * work, CI running, AgentK's review to come) lets the merge be queued; any other one doesn't,
   * and drops a queued merge, mid-turn or not. Unpushed commits and uncommitted files wait only
   * mid-turn (`turnAtStart`: when they were measured, or now), where they're AgentK's work in
   * progress; a question in the terminal only during a turn; findings being addressed only while
   * Claude runs. A queued merge also waits up to CI_START_MS for CI to show up on a new head
   * (`newHead`), even when a fast check already passed, so it never goes before CI has started on a
   * fresh push. GitHub's BLOCKED, when nothing else blocks, waits on a new head (required checks not
   * reported yet); after that it's left to GitHub, as it can't be told from a required merge queue.
   *
   * The hard blockers only the user can clear, for the Needs you dot (`Session.waitsOnYou`) and Run
   * to merge's notification: `yours`, a held review, and failing CI or merge conflicts AgentK handed
   * over on this head (watchCi, watchConflicts) or, for conflicts, doesn't resolve; and
   * `yoursWithAutoRun`, those and, while Run to merge is on, every other one but a question in the
   * terminal (already Needs you) and changes requested while AgentK addresses a review. Both are
   * kept so that turning Run to merge off or on shows at once, without a look at the PR.
   */
  mergeInfo(row: SessionRow, st: github.PrStatus, reviewer: ReviewerMode, opts: MergeChecks = {}): MergeInfo {
    const { unpushed = false, uncommitted = 0 } = opts;
    const b: string[] = [];
    const hard: string[] = [];
    const yours: string[] = [];
    const yoursWithAutoRun: string[] = [];
    const block = (text: string, waits = false, mine: 'always' | 'auto-run' | 'never' = 'auto-run') => {
      b.push(text);
      if (waits) return;
      hard.push(text);
      if (mine === 'always') yours.push(text);
      if (mine !== 'never') yoursWithAutoRun.push(text);
    };
    const handedOver = (fixes: number, max: number, at: string | null) => fixes > max && !!st.headRefOid && at === st.headRefOid;
    const working = row.status === 'working' || this.sessions.runtime(row.id).busy;
    const turn = this.inTurn(row);
    if (st.state !== 'OPEN') block('The PR is not open.');
    if (working) block('AgentK is still working.', true);
    else if (row.status === 'needs_input') block(IN_TERMINAL, turn, 'never');
    if (unpushed) block("The branch has commits that aren't pushed to the PR.", turn || !!opts.turnAtStart);
    if (uncommitted) block(`The worktree has ${uncommitted === 1 ? '1 uncommitted file' : `${uncommitted} uncommitted files`}: commit or discard them.`, turn || !!opts.turnAtStart);
    const ci = github.ciState(st.statusCheckRollup);
    // AgentK's to fix until it hands it over.
    if (ci === 'failure') block(CI_FAILING, false, handedOver(row.ci_fixes, MAX_CI_FIXES, row.ci_fix_head) ? 'always' : 'never');
    else if (ci === 'pending') block('CI is still running.', true);
    // A fast check (another workflow's) can pass before CI's own jobs show up, so passed checks hold too.
    else if ((ci === 'none' || ci === 'success') && opts.newHead && row.queued_gate === 'merge') block("CI hasn't started on the latest push yet.", true);
    // AgentK gates the merge only once it's active on the PR, so repos it doesn't watch never wait for it.
    if (reviewer !== 'people' && row.agentk_seen && (row.review_state === 'waiting' || row.review_state === 'in_review')) {
      block('Waiting for the AgentK review.', true);
    }
    // With Claude exited, the turn addressing them is gone until a Resume.
    if (row.review_state === 'addressing') block('AgentK is addressing review findings.', row.status !== 'exited');
    const held = heldSummary(this.db, row.id, row.pr_number);
    if (held.count) block(heldBlocker(held.authors), false, 'always');
    if (st.reviewDecision === 'CHANGES_REQUESTED') block('Changes were requested.', false, row.review_state === 'addressing' ? 'never' : 'auto-run');
    // Only on a repo that requires approvals: people's approvals can take days, so the merge isn't queued for them.
    if (st.reviewDecision === 'REVIEW_REQUIRED') block('Required approvals are missing.');
    if (st.mergeable === 'CONFLICTING') {
      const handed = !this.core.config.pr.resolveConflicts || handedOver(row.conflict_fixes, MAX_CONFLICT_FIXES, row.conflict_head);
      block(CONFLICTING, false, handed ? 'always' : 'never');
    }
    if (st.mergeStateStatus === 'BEHIND') block('The branch is behind its base.');
    if (st.mergeStateStatus === 'DRAFT') block('The PR is a draft.');
    // GitHub's own BLOCKED, only when nothing else blocks and only on a new head. Past that it's left to
    // GitHub: a PR not yet in a required merge queue reads BLOCKED too (`gh pr merge` is what queues it),
    // and so does any PR for a moment after an edit reruns a check.
    if (st.mergeStateStatus === 'BLOCKED' && !b.length && opts.newHead) block(CHECKS_PENDING, true);
    return { canMerge: b.length === 0, blockers: b, queueable: b.length > 0 && !hard.length, hard, yours, yoursWithAutoRun };
  }

  async requestReview(row: SessionRow, force: boolean): Promise<void> {
    if (row.stage !== 'review' || !row.pr_number) throw new HttpError(409, 'There is no open PR to review.');
    if (force && row.review_state !== 'refused') throw new HttpError(409, 'Force review is only for PRs AgentK declined.');
    const pr = row.pr_number;
    await this.serial(row.id, async () => {
      await github.comment(row.worktree_path, pr, force ? '/agentk review force' : '/agentk review', prRepoOf(row.pr_url));
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
    // Last, as it's the slowest: it checks the session is still open before keeping what it fetched.
    await this.refreshPrDesign(id, st.body ?? '');
    await this.deliverNext(id).catch((e) => this.warn(id, 'Sending a queued prompt', e));
    this.core.changed();
  }

  /**
   * A Review a PR session's copy of its PR's design: fetched when the review starts, and again by
   * its polls when the PR's `meko_design:` line names a version it doesn't have (the author changed
   * the design, or Meko couldn't be reached before). A version Meko said it doesn't have isn't asked for again until the line
   * changes; one Meko couldn't be asked for is tried again after PR_DESIGN_RETRY_MS. What's fetched
   * is kept only while the review is still open. Claude hears of it when the user presses Update to
   * latest (offered for a design Claude hasn't read, without new commits too).
   */
  async refreshPrDesign(id: string, body: string): Promise<void> {
    const ref = parseDesignLine(body);
    if (!ref || prDesignSource(this.core, id)?.ref.sha256 === ref.sha256) return;
    const failed = this.prDesignFailed.get(id);
    if (failed?.sha256 === ref.sha256 && (failed.missing || Date.now() - failed.at < PR_DESIGN_RETRY_MS)) return;
    const before = sessionRow(this.db, id);
    if (!before) return;
    const had = !!prDesignSource(this.core, id);
    let d: Awaited<ReturnType<typeof fetchPrDesign>>;
    try {
      d = await fetchPrDesign(this.core, before, body);
    } catch (e) {
      this.core.log.warn(`fetching PR #${before.pr_number}'s design failed for ${id}`, errMsg(e));
      this.prDesignFailed.set(id, { sha256: ref.sha256, at: Date.now(), missing: false });
      return;
    }
    if (!d) {
      this.prDesignFailed.set(id, { sha256: ref.sha256, at: Date.now(), missing: true });
      return;
    }
    this.prDesignFailed.delete(id);
    const row = sessionRow(this.db, id);
    if (!row || row.stage !== 'reviewing-pr' || row.desired !== 'running') return;
    savePrDesign(this.core, row, d);
    this.sessions.event(id, 'design', had ? `PR #${row.pr_number}'s design changed: the Design panel shows the new version` : `Loaded PR #${row.pr_number}'s design (${ref.name}) from Meko`);
  }

  /**
   * How Claude is to hear of the review's PR design with the next Update to latest: 'new' when it
   * hasn't read any version (the design came after its first prompt), 'changed' when it read an
   * earlier one, null when it has read this one (or there's none). Marks it read.
   */
  private takePrDesignNews(id: string): 'new' | 'changed' | null {
    const design = prDesignSource(this.core, id);
    if (!design || design.told === design.ref.sha256) return null;
    markPrDesignTold(this.core, id, design.ref.sha256);
    return design.told === null ? 'new' : 'changed';
  }

  /** gh's login, looked up once; null (and asked again next time) when gh can't say. */
  private viewerLogin(cwd: string): Promise<string | null> {
    return this.core.ghLogin(cwd);
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
      if (cur.status === 'exited') throw new HttpError(409, `${agentName(cur)} has exited. Resume the session first.`);
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
        // No new commits, but a design AgentK hasn't read: it reads that alone.
        const news = this.takePrDesignNews(cur.id);
        if (news) {
          this.enqueue(cur.id, 'review-pr-design', { design: news });
          await this.deliverNext(cur.id);
        }
        return;
      }
      const count = await commitsBetween(repo, old, sha);
      await moveReviewBranch(cur.worktree_path, sha);
      this.sessions.update(cur.id, { review_sha: sha, pr_head_sha: sha, pr_new_commits: 0 });
      this.sessions.event(cur.id, 'pr-update', `Updated to PR #${n} at ${sha.slice(0, 7)}`, { sha });
      // Claude is told of a design it hasn't read (this version, or any).
      const news = this.takePrDesignNews(cur.id);
      this.enqueue(cur.id, 'review-pr-update', {
        oldSha: old,
        sha,
        count: count === null ? 'its history was rewritten, a force-push' : `${count === 1 ? '1 new commit' : `${count} new commits`}`,
        ...(news ? { design: news } : {}),
      });
      await this.deliverNext(cur.id);
      void this.sessions.refreshDiff(cur.id).catch(() => undefined);
    });
  }

  // ---- merge / cleanup ----

  /**
   * Merge & Close: merges now when nothing blocks it, or queues the merge when only blockers that
   * clear by themselves are left (takeQueuedMerge sends it). Any other blocker is refused.
   */
  async merge(row: SessionRow, req: CloseSessionRequest = {}): Promise<void> {
    if (row.stage !== 'review' || !row.pr_number || !row.pr_url) throw new HttpError(409, 'Merge is only available in the Review stage.');
    if (!projectRow(this.db, row.project_id)) throw new HttpError(404, 'Project not found.');
    if (this.merging.has(row.id)) throw new HttpError(409, 'A merge is already in progress.');
    // Written only once the merge is requested or queued, and kept until it lands, when the learn
    // step runs or doesn't (Learner.beforeClose); a merge given up on clears it (giveUpMerge).
    const skip = { skip_learn: req.skipLearn ? 1 : 0 };
    this.merging.add(row.id);
    try {
      await this.serial(row.id, async () => {
        await this.doPollReview(row.id);
        const fresh = this.sessions.row(row.id);
        if (fresh.stage !== 'review') return; // merged elsewhere and already cleaned up
        const info = this.sessions.runtime(row.id);
        if (info.canMerge) {
          this.sessions.update(row.id, skip);
          return this.doMerge(row.id);
        }
        if (!info.mergeQueueable) throw new HttpError(409, `Not ready to merge: ${info.mergeBlockers.join(' ')}`);
        this.sessions.update(row.id, skip);
        if (fresh.queued_gate !== 'merge') {
          this.queueGate(row.id, 'merge', `Merge queued: it goes once nothing blocks it (now: ${info.mergeBlockers.join(' ')})`);
        } else if (fresh.queued_auto) {
          // Pressed while Run to merge's merge waited: the user's now, so stopping Run to merge keeps it.
          this.sessions.update(row.id, { queued_auto: 0 });
        }
      });
    } finally {
      this.merging.delete(row.id);
    }
  }

  /** A turn is running (or a gate action is in flight): AgentK working, or Claude asking during a turn. */
  private inTurn(row: SessionRow): boolean {
    return row.status === 'working' || this.sessions.runtime(row.id).busy || this.sessions.midTurn(row.id);
  }

  /** Asks GitHub to merge the PR, then looks at it again (which closes the session once it's merged). In the serial queue, under `merging`. */
  private async doMerge(id: string, by: 'you' | 'auto' = 'you'): Promise<void> {
    const row = this.sessions.row(id);
    const p = projectRow(this.db, row.project_id);
    if (!p || !row.pr_url) throw new HttpError(409, 'Merge is only available in the Review stage.');
    if (row.queued_gate) this.setQueued(id, null);
    const method = await github.resolveMergeMethod(row.worktree_path, parseSettings(p.settings_json).mergeMethod);
    this.sessions.setBusy(id, true);
    try {
      this.sessions.setStage(id, 'merge', by);
      await github.merge(row.worktree_path, row.pr_url, method);
    } catch (e) {
      if (this.sessions.row(id).stage === 'merge') this.sessions.setStage(id, 'review', 'auto');
      this.giveUpMerge(id);
      throw new HttpError(502, `GitHub refused the merge: ${errMsg(e)}`);
    } finally {
      this.sessions.setBusy(id, false);
    }
    await this.doPollReview(id);
    if (this.sessions.row(id).stage === 'merge') {
      this.sessions.event(id, 'merge', `Merge requested; waiting for GitHub to merge PR #${row.pr_number}`);
    }
  }

  // ---- Run to merge ----

  /**
   * Turns Run to merge on or off (`PATCH /api/sessions/:id`). On in Design waits for the approval;
   * in Code it queues Open PR (sent now when Claude is at its prompt), and in Review & PR the merge.
   * Off drops the gate it queued.
   */
  async setAutoRun(row: SessionRow, on: boolean): Promise<void> {
    if (row.desired !== 'running') throw new HttpError(409, 'This session is archived.');
    if (kindOf(row.kind) !== 'task') throw new HttpError(409, 'Run to merge is for tasks, not reviews of PRs.');
    if (!AUTO_RUN_STAGES.includes(row.stage)) {
      throw new HttpError(409, row.stage === 'explore' ? 'Run to merge starts at Design or later. Move the session on first.' : 'This session is already past its merge.');
    }
    if (row.stage === 'design' && row.plan_mode) throw new HttpError(409, "This session designs in Claude's plan mode, so Run to merge isn't available for it.");
    if (!!row.auto_run === on) return;
    if (!on) {
      if (this.merging.has(row.id)) throw new HttpError(409, 'The merge is already being requested.');
      this.stopAutoRun(row.id, 'you turned it off');
      return;
    }
    this.sessions.update(row.id, { auto_run: 1 });
    this.sessions.event(row.id, 'auto-run', row.stage === 'design'
      ? 'Run to merge on: once you approve the design, AgentK opens the PR and merges it by itself'
      : 'Run to merge on: AgentK opens the PR and merges it by itself');
    this.core.changed();
    if (row.stage === 'code' && row.status !== 'exited') {
      // Mid-turn it goes when the turn ends; between turns, now (takeQueuedPr checks Claude can take it).
      if (!row.queued_gate) this.queueGate(row.id, 'open-pr', 'Run to merge: Open PR queued', '', true);
      await this.takeQueuedPr(row.id).catch((e) => this.warn(row.id, 'Sending the queued Open PR', e));
    } else if (row.stage === 'review') {
      this.armMerge(row.id);
      await this.pollReview(this.sessions.row(row.id)).catch((e) => this.warn(row.id, 'PR status failed', e));
    }
  }

  /** Turns Run to merge off, dropping the gate it keeps queued, and says why in the activity log. */
  private stopAutoRun(id: string, why: string): void {
    const row = sessionRow(this.db, id);
    if (!this.autoRunOff(id)) return;
    // Only the gate it queued: one the user pressed themselves stays, as it would have without it.
    if (row?.queued_gate && row.queued_auto && !this.merging.has(id)) this.dropQueued(id, 'Run to merge stopped');
    this.sessions.event(id, 'auto-run', `Run to merge stopped: ${why}`);
  }

  /**
   * A notification of progress AgentK handles by itself (a review arrived, CI failed, merging): left
   * out while Run to merge is on, which tells the user only when it's done or needs them.
   */
  private notifyProgress(id: string, title: string, body: string): void {
    if (!sessionRow(this.db, id)?.auto_run) this.sessions.notifyUser(id, title, body);
  }

  /** Run to merge's notification that it's done: its PR merged (here, or on GitHub). */
  private autoRunDone(row: SessionRow, st: github.PrStatus): void {
    if (row.auto_run) this.sessions.notifyUser(row.id, 'Merged', `PR #${st.number} is merged: Run to merge is done.`);
  }

  /** Turns Run to merge off, leaving its queued gate alone; false when it wasn't on. */
  private autoRunOff(id: string): boolean {
    if (!sessionRow(this.db, id)?.auto_run) return false;
    this.sessions.update(id, { auto_run: 0 });
    // The PR prompt it sent that Claude never took goes with it: the user takes the gates back.
    if (this.lostPrs.get(id)?.by === 'auto') {
      this.lostPrs.delete(id);
      this.sessions.event(id, 'pr', `The PR prompt ${agentName(this.sessions.row(id))} didn't take won't go again: Run to merge stopped`);
    }
    this.autoRunTold.delete(id);
    this.autoRunBlocked.delete(id);
    this.core.changed();
    return true;
  }

  /**
   * A turn started (UserPromptSubmit). In Code with Run to merge on, Open PR is queued for its end,
   * so a relaunch (which drops a queued Open PR) is armed again by the next turn.
   */
  onTurnStart(id: string): void {
    const row = sessionRow(this.db, id);
    if (!row?.auto_run || row.stage !== 'code' || row.queued_gate || row.desired !== 'running') return;
    this.queueGate(id, 'open-pr', 'Run to merge: the PR opens when this turn ends', '', true);
  }

  /** In Review & PR with Run to merge on, the merge stays queued until it goes (takeQueuedMerge). */
  private armMerge(id: string): void {
    const row = sessionRow(this.db, id);
    if (!row?.auto_run || row.stage !== 'review' || row.queued_gate || this.merging.has(id) || row.desired !== 'running') return;
    this.queueGate(id, 'merge', 'Run to merge: the PR merges once nothing blocks it', '', true);
  }

  // ---- queued gates ----

  /**
   * Sets or clears the queued gate; its instructions (a queued Open PR's) go with it, and whether
   * Run to merge queued it (`auto`) rather than the user.
   */
  private setQueued(id: string, gate: QueuedGate | null, instructions = '', auto = false): void {
    this.sessions.update(id, { queued_gate: gate, queued_instructions: gate && instructions ? instructions : null, queued_auto: gate && auto ? 1 : 0 });
    this.core.changed();
  }

  private queueGate(id: string, gate: QueuedGate, text: string, instructions = '', auto = false): void {
    this.setQueued(id, gate, instructions, auto);
    this.sessions.event(id, 'queued', text);
  }

  /** Withdraws a queued Open PR or merge at the user's request. */
  cancelQueued(row: SessionRow): void {
    const gate = row.queued_gate;
    if (!gate) throw new HttpError(409, 'Nothing is queued.');
    if (gate === 'merge' && this.merging.has(row.id)) throw new HttpError(409, 'The merge is already being requested.');
    this.setQueued(row.id, null);
    if (gate === 'merge') this.giveUpMerge(row.id);
    this.sessions.event(row.id, 'queued', `Queued ${GATE_NAMES[gate]} withdrawn`);
    // Or the next turn (or poll) would queue it again.
    this.stopAutoRun(row.id, `you withdrew the queued ${GATE_NAMES[gate]}`);
  }

  /**
   * A requested or queued merge that won't happen (withdrawn, dropped, refused by GitHub, or back
   * in Review): its Don't save learnings goes with it, so a PR merged on GitHub later runs the learn step.
   */
  private giveUpMerge(id: string): void {
    if (sessionRow(this.db, id)?.skip_learn) this.sessions.update(id, { skip_learn: 0 });
  }

  /** Drops a queued gate (only `only`, when given), saying why in the activity log. */
  dropQueued(id: string, why: string, only?: QueuedGate): void {
    const gate = sessionRow(this.db, id)?.queued_gate;
    if (!gate || (only && gate !== only)) return;
    this.setQueued(id, null);
    if (gate === 'merge') this.giveUpMerge(id);
    this.sessions.event(id, 'queued', `Queued ${GATE_NAMES[gate]} dropped: ${why}`);
  }

  /**
   * A learn step ended (or Claude went away during it). A Discard waiting for it goes now. In Merge
   * & Close it held the cleanup back, whether it ran for the merge or was Save learnings already
   * running: look at the PR again, which finishes Merge & Close.
   */
  afterLearn(id: string): void {
    const row = sessionRow(this.db, id);
    if (row?.discarding) {
      if (row.desired === 'running' && !this.core.learn.running(id)) {
        void this.sessions.discardNow(id).catch((e) => this.core.log.warn(`discarding ${id} failed`, errMsg(e)));
      }
      return;
    }
    if (row?.stage !== 'merge') return;
    void this.pollReview(row).catch((e) => this.warn(id, 'PR status failed', e));
  }

  /** The PR is merged (here, or on GitHub): finish Merge & Close. Safe to call again after a restart. */
  private async merged(row: SessionRow, st: github.PrStatus): Promise<void> {
    if (row.stage === 'done' || this.closing.has(row.id)) return;
    // Merged on GitHub while a merge waited here: nothing is left for it to do.
    if (row.queued_gate) this.setQueued(row.id, null);
    const announced = !!get(this.db, `SELECT 1 AS x FROM session_events WHERE project_id = ? AND session_id = ? AND kind = 'merged'`,
      row.project_id, row.id);
    if (row.desired === 'running' && (row.status === 'working' || row.status === 'starting' || this.sessions.runtime(row.id).busy)) {
      // Stopping Claude mid-turn could lose its work: onTurnEnd closes the session when the turn ends.
      if (!announced) {
        this.sessions.event(row.id, 'merged', `PR #${st.number} merged; closing when AgentK finishes`, { url: st.url });
        await this.recordPrStats(row, st);
        this.autoRunDone(row, st);
      }
      if (row.stage !== 'merge') this.sessions.setStage(row.id, 'merge', 'auto');
      return;
    }
    this.closing.add(row.id);
    try {
      if (!announced) {
        this.sessions.event(row.id, 'merged', `PR #${st.number} merged`, { url: st.url });
        await this.recordPrStats(row, st);
        this.autoRunDone(row, st);
      }
      if (row.stage !== 'merge') this.sessions.setStage(row.id, 'merge', 'auto');
      // What the session learned goes to Meko first, while Claude is still there; this runs again
      // when the learn step ends (afterLearn).
      if (await this.core.learn.beforeClose(this.sessions.row(row.id))) return;
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
   * Lets go of the worktree (idle for the next session, or removed) and removes the branches.
   * Branches with commits the merged PR lacks are kept.
   */
  async cleanup(row: SessionRow, st: github.PrStatus): Promise<void> {
    const p = projectRow(this.db, row.project_id);
    const repo = p ? (sessionRepo(this.db, row) ?? p.repo_path) : null;
    const keep = !!repo && (await lacksLocalCommits(repo, row.branch, st));
    const { worktreeFreed, worktree } = await this.sessions.archive(row, { keepBranch: true });
    if (repo) {
      if (worktreeFreed && !keep) await deleteBranch(repo, row.branch);
      // A head on another remote (a fork's) is that fork owner's to delete.
      if (!keep && st.headRefOid && !row.push_remote) {
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
