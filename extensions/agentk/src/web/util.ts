import { HARNESS_CAPS, harnessOf, type HarnessId } from '../shared/harnesses';
import type { SyntheticEvent } from 'react';
import { INSTRUCTIONS_MAX, LEARN_WARN_FILES, LEARN_WARN_PRS, STAGE_LABELS, type LearnScope } from '../shared/types';
import { mekoDatapackPath } from '../shared/mekourls';
import { isStrategySetting, reviewHolds, type CodeReviewFinding, type ReviewOverrides } from '../shared/codereview';
import { stepOf } from '../shared/stages';
import type { ClaudeCodeStatus, HarnessStatus, KiroPlan, LearnProgress, MekoFirstTurn, MekoGapCounts, MekoGapQuality, MekoLearning, MekoStatus, ModelUsage, Project, ProjectWithSessions, ReviewingPr, Session, SessionStatus, Stage, TokenUsage } from '../shared/types';

/**
 * What a status dot shows: the session's status, `learning` while AgentK saves what it learned to
 * Meko, `queued` while a gate waits to go between turns, or `meko` while Claude runs a Meko tool
 * call in a working turn.
 */
export type DotState = SessionStatus | 'learning' | 'queued' | 'meko';

/** Higher is more urgent. Used for the project's aggregate dot in the rail. */
const URGENCY: Record<DotState, number> = {
  needs_input: 8,
  exited: 7,
  // Above working, so the rail shows the m whenever any of the project's sessions calls Meko.
  meko: 6,
  working: 5,
  starting: 5,
  learning: 5,
  // Above idle and recovered, so a project whose only activity is a queued merge shows the ring.
  queued: 4,
  recovered: 3,
  idle: 2,
  archived: 0,
};

/**
 * A code-review session: a task's local review (`Session.parentId`). It never shows as a tab, a
 * rail row or a list entry of its own: it's the magnifier joined to its task's tab, which opens its
 * screen while it runs (`reviewOpens`).
 */
export function isReviewChild(s: Pick<Session, 'kind'>): boolean {
  return s.kind === 'code-review';
}

/** A project's open sessions, its tasks' code-review sessions left out (they show as part of their task). */
export function liveSessions(p: ProjectWithSessions): Session[] {
  return p.sessions.filter((s) => s.desired !== 'archived' && s.status !== 'archived' && !isReviewChild(s));
}

/** A project's open sessions with its tasks' code-review sessions: what a terminal or the route may show. */
export function liveWithReviews(p: ProjectWithSessions): Session[] {
  return p.sessions.filter((s) => s.desired !== 'archived' && s.status !== 'archived');
}

/**
 * Which part of a task's joined tab (or rail row) is on screen: the task's, its review's (the task
 * with its findings open, or its code-review session on screen), or neither.
 */
export function joinedPartSelected(task: Pick<Session, 'id' | 'codeReview'>, currentId: string | null | undefined, findingsOpen = false): 'task' | 'review' | null {
  if (!currentId) return null;
  if (task.id === currentId) return findingsOpen && task.codeReview ? 'review' : 'task';
  return task.codeReview?.sessionId === currentId ? 'review' : null;
}

/**
 * What opening a task's review shows (its magnifier in the tab or the rail, its row in the ⋯ sheet):
 * the review's own screen, its terminal with its findings beside it, while its session runs and
 * isn't idle waiting for the task's fixes; else its findings beside the task's terminal (a done
 * review's session is gone).
 */
export function reviewOpens(task: Pick<Session, 'codeReview'>): 'session' | 'findings' {
  const r = task.codeReview;
  return r?.sessionId && r.state !== 'fixing' ? 'session' : 'findings';
}

/** The task whose code-review session this is, among `sessions`; undefined for any other session. */
export function reviewParent(sessions: Session[], s: Pick<Session, 'kind' | 'parentId'>): Session | undefined {
  return isReviewChild(s) && s.parentId ? sessions.find((x) => x.id === s.parentId) : undefined;
}

/**
 * How far, in pixels, a wheel turn over the session tabs scrolls them sideways: the strip has no
 * scrollbar, and a mouse wheel only turns up and down. 0 for a sideways turn (a trackpad's swipe,
 * Shift with the wheel), which the browser already scrolls by. `deltaMode` 1 is lines, 2 pages.
 */
export function sidewaysWheel(e: { deltaX: number; deltaY: number; deltaMode: number }, pageWidth: number): number {
  if (Math.abs(e.deltaX) >= Math.abs(e.deltaY)) return 0;
  return e.deltaY * (e.deltaMode === 1 ? 40 : e.deltaMode === 2 ? pageWidth : 1);
}

/**
 * Whether the open sessions across all projects run on more than one harness: the session tabs and
 * the rail then show each session's logo (`HarnessLogo`), and none while they all run on one.
 */
export function harnessLogos(projects: ProjectWithSessions[]): boolean {
  return new Set(projects.flatMap((p) => liveSessions(p).map((s) => harnessOf(s.harness)))).size > 1;
}

/**
 * What the last `claude update` came to (`ClaudeCodeStatus.lastUpdate`), for its banner and, while
 * `[claude] mod` is off and the banners don't show, Settings › Harness's Claude Code panel.
 */
export function claudeUpdateText(c: ClaudeCodeStatus, serverName: string): { ok: boolean; text: string } | null {
  const last = c.lastUpdate;
  if (!last) return null;
  const text = !last.ok
    ? `Couldn’t update Claude Code${last.message ? `: ${last.message}` : ''}. Run claude update on ${serverName}, or update it the way it was installed.`
    : last.from && last.to && last.from !== last.to
      ? `Claude Code was updated to ${last.to} (from ${last.from}). Running sessions keep their version until Claude restarts in them.`
      : c.supportsMod === false
        ? `claude update found nothing newer than ${last.to ?? 'this version'}. Its release channel, or your organization’s settings, may be holding it back.`
        : `Claude Code is up to date${last.to ? ` (${last.to})` : ''}.`;
  return { ok: last.ok, text };
}

/**
 * Settings' Default agent: the agents it offers (only available ones) and the one it shows picked,
 * or null while there's no choice (fewer than two available), when Settings leaves it out. A stored
 * default that isn't available shows as the first available (`AppState.harnesses` is in
 * `HARNESS_IDS` order), which the server starts on meanwhile; it stays stored, so it decides again
 * once it's available (unless that agent is turned off in Settings, which moves the default to another
 * available agent).
 */
export function defaultAgentChoice(harnesses: HarnessStatus[], stored: HarnessId): { agents: HarnessId[]; value: HarnessId } | null {
  const agents = harnesses.filter((h) => h.available).map((h) => h.id);
  return agents.length > 1 ? { agents, value: agents.includes(stored) ? stored : agents[0] } : null;
}

/** A project's open sessions in the order the rail and the tabs list them: by position, newest and moved-to-top first. */
export function openSessions(p: ProjectWithSessions): Session[] {
  return liveSessions(p).sort((a, b) => a.position - b.position || a.createdAt - b.createdAt);
}

/**
 * Statuses where Claude is up, so the session still has the Meko datapack it was launched with.
 * `recovered` counts: it's set once Claude was relaunched after a restart.
 */
const RUNNING: ReadonlySet<SessionStatus> = new Set(['starting', 'working', 'needs_input', 'idle', 'recovered']);

/** Sessions whose Claude is running now (not exited or archived). */
export function runningSessions(sessions: Session[]): Session[] {
  return sessions.filter((s) => s.desired !== 'archived' && RUNNING.has(s.status));
}

/** The project on screen and how many open sessions it has, as App last rendered it. */
export interface LiveCount {
  projectId: string | null;
  live: number;
}

/**
 * The projects to show: the state's, plus a project just opened (as Open project returned it, with
 * no sessions yet) until a state push brings it, so the route, the rail and the New session dialog
 * don't wait on the events socket.
 */
export function withOpened(projects: ProjectWithSessions[], opened: Project | null): ProjectWithSessions[] {
  if (!opened || projects.some((p) => p.id === opened.id)) return projects;
  return [...projects, { ...opened, sessions: [], usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, messages: 0 } }];
}

/**
 * The projects with a session just created (as New session returned it) in its project until a
 * state push brings it: the reply comes before the push, and its tab shows the startup steps.
 */
export function withCreated(projects: ProjectWithSessions[], created: Session | null): ProjectWithSessions[] {
  if (!created || projects.some((p) => p.sessions.some((s) => s.id === created.id))) return projects;
  return projects.map((p) => (p.id === created.projectId ? { ...p, sessions: [...p.sessions, created] } : p));
}

/** The project on screen just lost its last open session (not a switch to a project without any). */
export function lastSessionClosed(prev: LiveCount, next: LiveCount): boolean {
  return !!next.projectId && prev.projectId === next.projectId && prev.live > 0 && next.live === 0;
}

const LAST_OPEN_KEY = 'ak-last-session';

/** The session last open in each project in this browser, by project id. */
function readLastOpen(): Record<string, string> {
  try {
    const v: unknown = JSON.parse(localStorage.getItem(LAST_OPEN_KEY) ?? '{}');
    return v && typeof v === 'object' ? (v as Record<string, string>) : {};
  } catch {
    return {};
  }
}

export function lastOpenSession(projectId: string): string | undefined {
  return readLastOpen()[projectId];
}

/**
 * Remembers the session open in a project, so switching back to the project reopens it. A task's
 * review session is remembered as its task: it has no tab to come back to, and it goes once the
 * review is done.
 */
export function rememberOpenSession(projectId: string, session: Pick<Session, 'id' | 'kind' | 'parentId'>) {
  const sessionId = isReviewChild(session) && session.parentId ? session.parentId : session.id;
  const all = readLastOpen();
  if (all[projectId] === sessionId) return;
  all[projectId] = sessionId;
  try {
    localStorage.setItem(LAST_OPEN_KEY, JSON.stringify(all));
  } catch {
    /* storage blocked or full: this session isn't remembered */
  }
}

/**
 * The session to open when switching to a project: the one last open there while it's still
 * open, else the one with the most recent activity.
 */
export function sessionToOpen(live: Session[], lastOpenId: string | undefined): Session | undefined {
  return (
    live.find((s) => s.id === lastOpenId) ??
    [...live].sort((a, b) => (b.lastEventAt ?? b.createdAt) - (a.lastEventAt ?? a.createdAt))[0]
  );
}

/**
 * The statuses a learn step shows over: Claude between turns (the mod's step) or in the typed
 * learn turn. Needs you, exited and starting win.
 */
const LEARNING_SHOWS: ReadonlySet<SessionStatus> = new Set(['idle', 'recovered', 'working']);

/** The statuses a queued gate shows over: Claude between turns. Anything else says more. */
const QUEUED_SHOWS: ReadonlySet<SessionStatus> = new Set(['idle', 'recovered']);

/** The statuses Review & PR waiting on the user shows Needs you over: Claude there, in a turn or not. */
const YOURS_SHOWS: ReadonlySet<SessionStatus> = new Set(['idle', 'recovered', 'working']);

/** The session is in Review & PR and a merge blocker only the user can clear stands (`Session.waitsOnYou`). */
function waitsOnYou(s: Pick<Session, 'stage' | 'waitsOnYou'>): boolean {
  return s.stage === 'review' && s.waitsOnYou.length > 0;
}

/**
 * What a session's dot shows: Needs you while Review & PR waits on the user, over idle, recovered
 * and working; then `Session.learning` over idle, recovered and working, then `Session.queued` over
 * idle and recovered, then `Session.mekoCall` over working.
 */
export function dotState(s: Pick<Session, 'status' | 'learning' | 'queued' | 'mekoCall' | 'stage' | 'waitsOnYou'>): DotState {
  if (waitsOnYou(s) && YOURS_SHOWS.has(s.status)) return 'needs_input';
  if (s.learning && LEARNING_SHOWS.has(s.status)) return 'learning';
  if (s.queued && QUEUED_SHOWS.has(s.status)) return 'queued';
  return s.mekoCall && s.status === 'working' ? 'meko' : s.status;
}

/** The most urgent dot of these sessions (idle with none). */
export function urgentDot(sessions: Session[]): DotState {
  let best: DotState = 'idle';
  for (const s of sessions) {
    const dot = dotState(s);
    if (URGENCY[dot] > URGENCY[best]) best = dot;
  }
  return best;
}

/** The project's dot in the rail: its most urgent session's. */
export function aggregateDot(p: ProjectWithSessions): DotState {
  return urgentDot(liveSessions(p));
}

export const STATUS_TEXT: Record<DotState, string> = {
  starting: 'starting',
  working: 'working',
  needs_input: 'needs you',
  idle: 'idle',
  exited: 'exited',
  recovered: 'recovered after restart',
  archived: 'archived',
  learning: 'learning',
  queued: 'queued',
  meko: 'calling Meko',
};

/** A learning dot's tooltip. */
export const LEARNING_TITLE = 'learning (saving what it learned to Meko)';

/**
 * Whether Save learnings is offered: AgentK saves to the project's datapack, so it needs one, Meko and
 * Claude there. Not while a Discard waits for its learn step: the Discard row already says so.
 */
export function offersLearn(s: Pick<Session, 'status' | 'discarding'>, datapack: boolean, mekoConfigured: boolean): boolean {
  return datapack && mekoConfigured && s.status !== 'exited' && !s.discarding;
}

export { agentName } from '../shared/harnesses';

/** Every agent that's turned on has answered, and none can run a session (`AppState.harnesses`). */
export function noAgent(harnesses: HarnessStatus[]): boolean {
  return harnesses.length > 0 && harnesses.every((h) => h.checked && !h.available);
}

/** Last segment of a repo path: /home/me/code/api → api. */
export function repoFolder(path: string): string {
  return path.split('/').filter(Boolean).pop() ?? path;
}

export function relativeTime(ms: number | null | undefined, now = Date.now()): string {
  if (!ms) return '—';
  const s = Math.round((now - ms) / 1000);
  if (s < 45) return 'just now';
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.round(m / 60);
  if (h < 24) return h === 1 ? '1 hour ago' : `${h} hours ago`;
  const d = Math.round(h / 24);
  if (d === 1) return 'Yesterday';
  if (d < 14) return `${d} days ago`;
  const w = Math.round(d / 7);
  if (d < 60) return `${w} weeks ago`;
  return new Date(ms).toLocaleDateString();
}

export function shortTime(ms: number, now = Date.now()): string {
  const d = new Date(ms);
  const today = new Date(now);
  if (d.toDateString() === today.toDateString()) {
    return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  }
  const y = new Date(now - 86400000);
  if (d.toDateString() === y.toDateString()) return 'Yesterday';
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' });
}

export function slugify(s: string): string {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 60);
}

/**
 * A datapack's page in the Meko web app. While AgentK uses the Meko on this computer, that Meko's
 * own page on this origin, root-absolute: a web URL set for it can only be on its own (loopback)
 * origin, which a browser using remote access can't open. Else the known web app's (Meko Cloud's);
 * null when there's none.
 */
export function mekoDatapackUrl(meko: Pick<MekoStatus, 'webUrl' | 'source'>, datapackId: string): string | null {
  if (meko.source === 'local') return mekoDatapackPath(datapackId);
  return meko.webUrl ? `${meko.webUrl}${mekoDatapackPath(datapackId)}` : null;
}

/** An http(s) URL's host, lowercased; null for anything else (never a `javascript:` link). */
export function httpHost(url: string): string | null {
  try {
    const u = new URL(url);
    return u.protocol === 'https:' || u.protocol === 'http:' ? u.host.toLowerCase() : null;
  } catch {
    return null;
  }
}

export function isMac(): boolean {
  return /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
}

export function modKey(e: KeyboardEvent): boolean {
  return isMac() ? e.metaKey : e.ctrlKey;
}

/**
 * Ctrl+Enter or ⌘+Enter, on any platform: sends a dialog's form from a text box, where plain Enter
 * adds a line. Not when a field already took the Enter (prevented its default).
 */
export function isSubmitKey(e: Pick<KeyboardEvent, 'key' | 'ctrlKey' | 'metaKey' | 'defaultPrevented'>): boolean {
  return e.key === 'Enter' && (e.ctrlKey || e.metaKey) && !e.defaultPrevented;
}

export function cx(...parts: (string | false | null | undefined)[]): string {
  return parts.filter(Boolean).join(' ');
}

/** For a button's pointer and mouse down: tapping it keeps focus (and the on-screen keyboard) where it is. */
export const keepFocus = (e: SyntheticEvent) => e.preventDefault();

/** From this full, the context ring shows in the warning colour. */
export const CONTEXT_WARN_PERCENT = 75;
/** From this full, the context ring shows in the danger colour. */
export const CONTEXT_BAD_PERCENT = 95;

/** How worried to look about a context window this full, in percent. */
export function contextLevel(percent: number): 'ok' | 'warn' | 'bad' {
  return percent >= CONTEXT_BAD_PERCENT ? 'bad' : percent >= CONTEXT_WARN_PERCENT ? 'warn' : 'ok';
}

/** The length of the context ring's arc, out of 100: the percent, kept to 0–100. */
export function contextArc(percent: number): number {
  return Math.min(100, Math.max(0, percent));
}

export function totalTokens(u: TokenUsage): number {
  return u.input + u.output + u.cacheRead + u.cacheWrite;
}

/**
 * One of a usage row's token counts, or a dash for a Kiro-only row (credits and no tokens: Kiro's
 * `auto` model counts none), so it doesn't read as zero use.
 */
export function rowTokens(u: TokenUsage, n: number): string {
  return u.credits && totalTokens(u) === 0 ? '—' : formatTokens(n);
}

/**
 * A by-model row's name: the model id, with the agent's name first when its ids don't say which
 * agent ran them (`HarnessCaps.labelModels`: "Kiro · auto", since Kiro's default model is just `auto`).
 */
export function modelLabel(m: ModelUsage): string {
  const caps = HARNESS_CAPS[m.harness];
  return caps.labelModels ? `${caps.short} · ${m.model}` : m.model;
}

/** 950 → "950", 12_300 → "12.3k", 4_560_000 → "4.56M". */
export function formatTokens(n: number): string {
  if (n < 1000) return String(n);
  const [div, unit] = n >= 1e9 ? [1e9, 'B'] : n >= 1e6 ? [1e6, 'M'] : [1e3, 'k'];
  const v = n / div;
  return `${v >= 100 ? Math.round(v) : v >= 10 ? v.toFixed(1).replace(/\.0$/, '') : v.toFixed(2).replace(/\.?0+$/, '')}${unit}`;
}

/** Kiro's credits: "0.42 credits", "12.5 credits", "1 credit". */
export function formatCredits(n: number): string {
  const v = roundCredits(n);
  return `${v.toLocaleString()} ${v === 1 ? 'credit' : 'credits'}`;
}

/** Kiro's credits as a bare number, for a column or tile already labelled Credits: "0.42", "12.5", "1,234". */
export function creditsNumber(n: number): string {
  return roundCredits(n).toLocaleString();
}

function roundCredits(n: number): number {
  return n >= 100 ? Math.round(n) : n >= 10 ? Number(n.toFixed(1)) : Number(n.toFixed(2));
}

/** A plan bar's level in the rail (Claude's windows, Kiro's credits): amber from 80%, red from 95%. */
export function limitLevel(percent: number): 'warn' | 'bad' | null {
  return percent >= 95 ? 'bad' : percent >= 80 ? 'warn' : null;
}

/**
 * The extra lines under Kiro's credit ring (`KiroLimit` in Rail.tsx): `extra` is AgentK's own
 * reading of the bonus and overage lines, and `said` is Kiro's verbatim line quoted under them,
 * kept unless the `extra` line already states everything AgentK read from the line, so a fact
 * isn't shown twice and nothing AgentK read but doesn't show is hidden. The overage quote is
 * dropped only for the one form that's been seen, `Overages: Disabled` (its on/off read, no
 * credits), which `extra` fully states as `Overage off`; a line that also carries credits keeps
 * its quote, since `extra` shows only the credits (not the on/off) and the credits amount itself
 * is still a guess (`firstAmount` in kiro.ts could misread a cap or a date as the amount). The
 * bonus quote always stays while its format is unseen.
 */
export function kiroPlanLines(plan: Pick<KiroPlan, 'bonus' | 'overage'>): { extra: string[]; said: string[] } {
  const extra = [
    plan.bonus && `Bonus ${creditsNumber(Math.max(0, plan.bonus.total - plan.bonus.used))} of ${creditsNumber(plan.bonus.total)} left`,
    plan.overage && (plan.overage.credits !== null ? `Overage ${creditsNumber(plan.overage.credits)} billed` : plan.overage.enabled !== null ? `Overage ${plan.overage.enabled ? 'on' : 'off'}` : null),
  ].filter((l): l is string => !!l);
  // Kiro's own words, quoted unless `extra` says everything AgentK read: dropped only for the seen
  // `Overages: Disabled` shape (on/off, no credits), which `Overage off`/`Overage on` fully states.
  const overageShown = !!plan.overage && plan.overage.enabled !== null && plan.overage.credits === null;
  const said = [plan.bonus?.line, overageShown ? null : plan.overage?.line].filter((l): l is string => !!l);
  return { extra, said };
}

/** USD: "$3.06", "$241", "$1,234", "<$0.01", "−$2.75". */
export function formatCost(usd: number): string {
  const sign = usd < 0 ? '−' : '';
  const v = Math.abs(usd);
  if (v === 0) return '$0';
  if (v < 0.01) return `${sign}<$0.01`;
  return `${sign}$${v < 100 ? v.toFixed(2) : Math.round(v).toLocaleString('en-US')}`;
}

/** A cost that leaves out models with no known price is a lower bound: "≥ $3.06". */
export function formatCostOf(cost: { usd: number; unpricedMessages: number }): string {
  return `${cost.unpricedMessages > 0 ? '≥ ' : ''}${formatCost(cost.usd)}`;
}

/** One-line breakdown for tooltips. */
export function usageTitle(u: TokenUsage): string {
  const f = (n: number) => n.toLocaleString();
  return `Tokens: ${f(totalTokens(u))} total · ${f(u.output)} output · ${f(u.input)} input · ${f(u.cacheWrite)} cache write · ${f(u.cacheRead)} cache read · ${u.messages} responses`;
}

/**
 * The Meko section's 1st-turn tokens tile in words: its split line (shown, since tooltips aren't
 * seen on touch) and a tooltip that spells it out, including how many sessions AgentK's search at
 * start (the seed) ran in, since sessions from before it was added, or started with no task, count
 * but never had one.
 */
export function firstTurnText(t: MekoFirstTurn): { split: string | null; title: string } {
  const f = (n: number) => n.toLocaleString();
  if (t.sessions === 0) return { split: null, title: 'No session has finished its first turn yet.' };
  const sessions = (n: number) => `${f(n)} ${n === 1 ? 'session' : 'sessions'}`;
  const seed = t.startSearches === 0
    ? "AgentK's search at session start, the seed, hasn't run in any of them yet;"
    : `${f(t.seedTokens)} that AgentK's search at session start passed to Claude (the seed` +
      (t.startSearches < t.sessions ? `; it ran in ${f(t.startSearches)} of them, the others are from before it was added or had no task),` : '),');
  return {
    split: `${t.startSearches === 0 ? 'Seeded none yet' : `Seeded ≈ ${formatTokens(t.seedTokens)}`} · Searched ≈ ${formatTokens(t.searchedTokens)}`,
    title:
      `About ${f(t.seedTokens + t.searchedTokens)} tokens from Meko in the first turns of ${sessions(t.sessions)} (characters ÷ 4): ` +
      `${seed} and ${f(t.searchedTokens)} from Claude's own Meko searches before its first turn ended. ` +
      'Only sessions in projects with datapacks count.',
  };
}

/** A learn-step tile's text: the number, a line under its label (shown, since tooltips aren't seen on touch) and a tooltip. */
export interface LearningTile {
  num: string;
  line: string | null;
  title: string;
}

/**
 * The Meko section's Learned and Learning cost tiles in words: what the learn steps saved to the
 * datapack (memories added, with corrections and steps on the line under it), and what their model
 * calls cost.
 */
export function learningText(l: MekoLearning): { learned: LearningTile; cost: LearningTile } {
  const f = (n: number) => n.toLocaleString();
  const n = (k: number, one: string, many: string) => `${f(k)} ${k === 1 ? one : many}`;
  const skipped = l.skipped > 0 ? ` ${n(l.skipped, 'session was', 'sessions were')} closed without one, since Claude was gone or busy.` : '';
  if (l.runs === 0) {
    const none = 'No learn step has run yet. One runs at Merge & Close and Discard, or when you choose Save learnings.';
    return { learned: { num: '—', line: null, title: none + skipped }, cost: { num: '—', line: null, title: none } };
  }
  const failed = l.failed > 0 ? `; ${f(l.failed)} didn't finish` : '';
  return {
    learned: {
      num: f(l.saves),
      line: `${f(l.updates)} corrected · ${n(l.runs, 'step', 'steps')}`,
      title: `${n(l.runs, 'learn step', 'learn steps')} saved ${n(l.saves, 'memory', 'memories')} to the datapack, corrected ${f(l.updates)} and ` +
        `found ${n(l.skips, 'fact', 'facts')} already known${failed}.${skipped}`,
    },
    cost: {
      num: l.usd !== null ? formatCost(l.usd) : `≈ ${formatTokens(l.tokens)}`,
      line: l.usd !== null ? `≈ ${formatTokens(l.tokens)} tokens` : null,
      title: `What the learn steps' model calls cost: the look back over each session, the duplicate check and typed learn turns ` +
        `(${f(l.tokens)} tokens${l.usd !== null ? ', at Anthropic API list prices' : ', on models without a known price'}).`,
    },
  };
}

/** How a question's latest ask went, as What Meko doesn't know names it. */
export const GAP_QUALITY_LABEL: Record<MekoGapQuality, string> = { empty: 'Found nothing', weak: 'Nothing relevant' };

/** "Asked once", "Asked 3 times". */
export function askedText(asks: number): string {
  return asks === 1 ? 'Asked once' : `Asked ${asks.toLocaleString()} times`;
}

/**
 * The line under the Meko stats on the project home: how many questions Meko couldn't answer, and
 * the split (a part with none is left out); null when there are none.
 */
export function gapsLine(c: MekoGapCounts): { count: string; split: string } | null {
  const total = c.empty + c.weak;
  if (total === 0) return null;
  const parts = [c.empty > 0 && `${c.empty.toLocaleString()} found nothing`, c.weak > 0 && `${c.weak.toLocaleString()} nothing relevant`].filter(Boolean);
  return { count: `${total.toLocaleString()} ${total === 1 ? 'question' : 'questions'}`, split: parts.join(' · ') };
}

/** The status detail the server sets when a design waits for approval (`DESIGN_READY` in lifecycle.ts). */
const DESIGN_READY = 'Design ready for your approval';

/**
 * The Design gate in the stage bar: Design Review, which opens the design (`reviewTarget`) and
 * never approves itself, since only the panel can send the answers with the approval.
 */
export function designGate(session: Session): GateButton {
  const reason = session.hasDesign ? null : 'AgentK hasn’t proposed a design yet.';
  const title = session.designQuestions.open
    ? 'Review the design and answer its open questions, then approve it there'
    : 'Review the design, then approve it there';
  return { label: 'Design Review', reason, title: reason ?? title };
}

/**
 * What Design Review does: open the design panel on its questions (open questions, or a change to
 * one of AgentK's decisions waiting) or on the design, or, with the panel already open, bring the
 * panel's approve button into view (`approve`).
 */
export function reviewTarget(openQuestions: number, changing: boolean, panelOpen: boolean): 'questions' | 'design' | 'approve' {
  if (panelOpen) return 'approve';
  return openQuestions > 0 || changing ? 'questions' : 'design';
}

/** The panel header's approve button's label: with open questions, it takes the user to answer them first. */
export function approveLabel(openQuestions: number): string {
  return openQuestions ? 'Answer & approve' : 'Approve design';
}

/**
 * Whether the design panel's header shows the approve button: for a design of a running session in
 * Design, as the questions' footer is. With open questions, or a change to one of AgentK's decisions
 * waiting (`changing`), it only takes the user to them, so it's left out while they're on screen
 * (their footer approves, with the answers) and while they can't be shown (the design is loading,
 * or couldn't be read).
 */
export function headerApproves(session: Session, questions: 'shown' | 'hidden' | 'none', changing = false): boolean {
  if (session.stage !== 'design' || session.desired !== 'running' || !session.hasDesign) return false;
  return !(session.designQuestions.open || changing) || questions === 'hidden';
}

/** The status detail while Claude asks the user something (`askDetail()` in sessions.ts). */
const ASKING = 'AgentK asks: ';

/** Claude is waiting on a question or permission prompt in the terminal, not on the design. */
function otherPrompt(session: Session): boolean {
  if (session.status !== 'needs_input' || !session.statusDetail || session.statusDetail === DESIGN_READY) return false;
  // Claude's own question, whatever it says.
  if (session.statusDetail.startsWith(ASKING)) return true;
  // (Plan mode's approval dialog can carry Claude's own wording, so any mention of a plan is allowed.)
  return !/plan/i.test(session.statusDetail);
}

/**
 * Why Claude can't take a prompt typed by a move (its stage's, or the user's instructions), or null.
 * Mirrors checkPromptable in lifecycle.ts; the move dialog checks the turn itself.
 */
export function promptBlocker(session: Session): string | null {
  if (session.status === 'exited') return 'Claude has exited. Resume the session first.';
  if (session.status === 'needs_input' && session.statusDetail !== DESIGN_READY) return 'AgentK is waiting on you in the terminal. Answer it first.';
  return null;
}

/** Why the user's instructions for a move or Open PR can't go: too long (INSTRUCTIONS_MAX), or null. */
export function instructionsBlocker(text: string): string | null {
  const n = text.trim().length;
  if (n <= INSTRUCTIONS_MAX) return null;
  return `Instructions can be at most ${INSTRUCTIONS_MAX.toLocaleString('en-US')} characters; these have ${n.toLocaleString('en-US')}.`;
}

/**
 * A turn is running and AgentK's mod is too: an approval now waits for the turn to end, and
 * answers sent now are taken when it does.
 */
export function heldForTurnEnd(session: Session): boolean {
  return session.modLive && session.midTurn;
}

/** Why Approve design can't be pressed right now, or null when it can. Mirrors the server's checks. */
export function designApprovalBlocker(session: Session): string | null {
  if (!session.hasDesign) return 'AgentK hasn’t proposed a design yet.';
  if (session.approvalQueued) return 'You approved the design: it goes to AgentK when this turn ends.';
  if (session.status === 'exited') return 'Claude has exited. Resume the session first.';
  if (session.status === 'starting') return 'Wait for AgentK to finish its turn.';
  if (session.promptsHeld > 0) return 'AgentK has answers from you to take first. Approve once it has revised the design.';
  // With the mod, an approval made mid-turn waits for the turn to end.
  if (heldForTurnEnd(session)) return null;
  if (session.status === 'working') return 'Wait for AgentK to finish its turn.';
  // A question or permission prompt from Claude isn't the design; the server refuses to approve over it.
  if (otherPrompt(session)) return 'AgentK is waiting on you in the terminal. Answer it first.';
  return null;
}

/** Why the design's approve buttons wait while the server moves the session on (`Session.busy`). */
export const SESSION_BUSY = 'AgentK is moving the session on. Wait a moment.';

/** A gate button: its label, why it's disabled (null when it isn't), and its tooltip. */
export interface GateButton {
  label: string;
  reason: string | null;
  title: string;
}

/** The Open PR gate in Code. Mid-turn, a press queues the PR prompt for the turn's end. Mirrors the server's checks. */
export function openPrGate(session: Session): GateButton {
  const gate = (reason: string | null, title = 'Commit, push and open a pull request'): GateButton => ({ label: 'Open PR', reason, title: reason ?? title });
  if (session.status === 'exited') return gate('Claude has exited. Resume the session first.');
  if (!session.busy && (session.status === 'working' || session.status === 'starting' || session.midTurn)) {
    return { label: 'Open PR after this turn', reason: null, title: 'AgentK is mid-turn: the PR prompt goes when the turn ends' };
  }
  if (session.status === 'needs_input') return gate('AgentK is waiting on you in the terminal. Answer it first.');
  if (session.promptsHeld > 0) return gate('AgentK has prompts from you to take first. Open the PR once it has.');
  return gate(null);
}

/**
 * An Update to latest that went through still waits on the state to show it: its answer comes before
 * the state does. It always moves the commit checked out or the head, or marks the design read.
 */
export function updateShowing(before: ReviewingPr, now: ReviewingPr | null): boolean {
  return !now || now.reviewSha !== before.reviewSha || now.headSha !== before.headSha || now.designUnread !== before.designUnread;
}

/** The Merge & Close gate in Review. With only blockers that clear by themselves left, a press queues the merge. */
export function mergeGate(session: Session): GateButton {
  if (session.canMerge) return { label: 'Merge & Close', reason: null, title: 'Merge the pull request, then remove the worktree and branch' };
  const blockers = session.mergeBlockers.join('\n');
  if (session.mergeQueueable) {
    return { label: 'Merge when ready', reason: null, title: `Merge the pull request once nothing blocks it. Waiting on:\n${blockers}` };
  }
  const reason = blockers || 'Not ready to merge yet.';
  return { label: 'Merge & Close', reason, title: reason };
}

/** What a queued merge waits on, from its first blocker, for the gate's pill. */
const MERGE_WAITS: [string, string][] = [
  ['CI is still running.', 'waiting on CI'],
  ["CI hasn't started on the latest push yet.", 'waiting on CI'],
  ["GitHub's required checks haven't reported on the latest push yet.", 'waiting on CI'],
  ['Waiting for the AgentK review.', 'waiting on the AgentK review'],
  ['AgentK is waiting on you in the terminal.', 'waiting on you in the terminal'],
  // Only Run to merge's merge stays queued through these, which AgentK works on.
  ['CI is failing.', 'waiting on a CI fix'],
  ['The branch has merge conflicts.', 'waiting on the merge conflicts'],
];

/**
 * The pill a queued gate shows in place of its button (`Session.queued`); null when nothing is
 * queued. Run to merge's merge also waits on blockers that don't clear by themselves.
 */
export function queuedText(session: Session): string | null {
  if (session.queued === 'open-pr') return session.autoRun ? '✓ Run to merge: opens the PR when this turn ends' : '✓ Open PR queued: goes when the turn ends';
  if (session.queued !== 'merge') return null;
  const first = session.mergeBlockers[0];
  const waits = first ? (MERGE_WAITS.find(([text]) => text === first)?.[1] ?? (session.autoRun && !session.mergeQueueable ? 'waiting on you' : 'waiting on AgentK')) : 'merging soon';
  return session.autoRun ? `✓ Run to merge: ${waits}` : `✓ Merge queued: ${waits}`;
}

/**
 * What Run to merge does from the stage the session is in: its checkbox's tooltip, and the line
 * under the gate in the phone's ⋯ sheet, which has no tooltips.
 */
export function runToMergeTitle(session: Session): string {
  if (session.stage === 'design') return 'once you approve the design, AgentK opens the PR when it has made the change, then merges it once nothing blocks it';
  if (session.stage === 'review') return 'AgentK merges the PR once nothing blocks it, and tells you when something only you can clear blocks it';
  return 'AgentK opens the PR when its turn ends (at once, between turns), then merges it once nothing blocks it';
}

/**
 * Whether a session shows Run to merge's mark after its stage (`RunToMergeMark`): on, and not yet
 * Done. The server leaves `autoRun` set after the merge, so it alone would mark finished sessions.
 */
export function runsToMerge(s: Session): boolean {
  return s.autoRun && s.stage !== 'done';
}

/**
 * The task's local review holds its Open PR: a review runs (or waits for the task's fixes between
 * rounds) before the PR prompt goes, or failed or was cancelled and waits for Retry or Open anyway
 * (not one cancelled because the task left Code: the next Open PR starts a new review).
 */
export function reviewGatesPr(s: Pick<Session, 'stage' | 'codeReview'>): boolean {
  const r = s.codeReview;
  return s.stage === 'code' && !!r && r.state !== 'done' && (r.state !== 'cancelled' || reviewHolds(r));
}

/**
 * The step a session shows in the stage bar and its chip: Review & PR while its local review holds
 * the Open PR (the session stays in Code, where the review runs: docs/design/code-review.md), else
 * its stage's step.
 */
export function shownStep(s: Pick<Session, 'stage' | 'codeReview'>): Stage {
  return reviewGatesPr(s) ? 'review' : stepOf(s.stage);
}

/** A session's stage in its chip: the step it shows (shownStep). */
export function stageChipText(s: Pick<Session, 'stage' | 'codeReview'>): string {
  return STAGE_LABELS[shownStep(s)];
}

/** A session's stage in words, for tooltips and hints that can't hold the mark: "Code · Run to merge". */
export function stageWords(s: Session): string {
  return runsToMerge(s) ? `${STAGE_LABELS[s.stage]} · Run to merge` : STAGE_LABELS[s.stage];
}

/** Whether the stage bar's gate has Run to merge's box (StageBar in ProjectView.tsx). */
export function runToMergeShown(session: Session): boolean {
  return session.stage === 'code' || session.stage === 'review' || (session.stage === 'design' && session.hasDesign);
}

const facts = (n: number) => `${n} ${n === 1 ? 'fact' : 'facts'}`;

/** What a running learn step is doing, in words (`Session.learnProgress`). */
export function learnStepText(p: LearnProgress): string {
  switch (p.step) {
    case 'extract': return 'Finding what it learned';
    case 'search': return `Looking up ${facts(p.facts)} in Meko`;
    case 'check': return `Checking ${facts(p.facts)} against Meko`;
    case 'save': return `Saving ${Math.min(p.done + 1, p.facts)} of ${p.facts} to Meko`;
    case 'turn': return 'AgentK is saving what it learned in a turn';
  }
}

/** What a Discard waits on while its learn step runs. */
export function discardingText(s: Pick<Session, 'learnProgress'>): string {
  return s.learnProgress ? `${learnStepText(s.learnProgress)}, then discarding` : 'Saving learnings, then discarding';
}

/** Where Merge & Close is, for the gate's pill (`Session.closeStep`). */
export function closingText(s: Pick<Session, 'closeStep' | 'learnProgress' | 'prNumber'>): string {
  const pr = s.prNumber ? `PR #${s.prNumber}` : 'the PR';
  switch (s.closeStep) {
    case 'merge': return `Waiting for GitHub to merge ${pr}`;
    case 'turn': return 'Merged. Closing when AgentK finishes its turn';
    case 'learn': return `Merged. ${s.learnProgress ? learnStepText(s.learnProgress) : 'Saving learnings'}`;
    case 'cleanup': return 'Merged. Cleaning up';
    default: return 'Merging and closing';
  }
}

/** A running time as `42s` or `3m 05s`. */
export function elapsedText(ms: number): string {
  const s = Math.max(0, Math.floor(ms / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, '0')}s`;
}

/** A session tab's tooltip: name, what its dot means, stage and tokens. */
export function sessionTabTitle(s: Session): string {
  return [
    s.name,
    HARNESS_CAPS[harnessOf(s.harness)].label,
    dotTitle(s) + (s.statusDetail && s.status !== 'starting' ? `: ${s.statusDetail}` : ''),
    stageWords(s),
    `${formatTokens(totalTokens(s.usage))} tokens`,
  ].join(' · ');
}

/**
 * A session's dot in words, for its tab's tooltip: the long learning text, what the user is to
 * clear in Review & PR, or what a queued gate waits on. The queued dot shows between turns, when a
 * queued Open PR is already going (the pill's "goes when the turn ends" is for mid-turn).
 */
export function dotTitle(s: Session): string {
  const dot = dotState(s);
  if (dot === 'needs_input' && s.status !== 'needs_input') return `${STATUS_TEXT.needs_input}: ${s.waitsOnYou.join(' ')}`;
  if (dot === 'learning') return LEARNING_TITLE;
  if (dot === 'queued') return s.queued === 'open-pr' ? 'Open PR queued: going now' : (queuedText(s)?.replace(/^✓ /, '') ?? STATUS_TEXT.queued);
  return STATUS_TEXT[dot];
}

/** Why answers can't be sent for AgentK to revise the design right now, or null when they can. */
export function designReviseBlocker(session: Session): string | null {
  if (session.approvalQueued) return 'You approved the design. Cancel the approval to send answers instead.';
  if (session.status === 'exited') return 'Claude has exited. Resume the session first.';
  if (session.status === 'starting') return 'Wait for AgentK to finish its turn.';
  if (heldForTurnEnd(session)) return null;
  if (session.status === 'working') return 'Wait for AgentK to finish its turn.';
  if (otherPrompt(session)) return 'AgentK is waiting on you in the terminal. Answer it first.';
  return null;
}

/** How long after Send for revision the design panel shows AgentK at work before Claude reports the turn. */
export const REVISION_GRACE_MS = 10_000;

/**
 * A Send for revision from this page: when it was clicked, and whether its turn has been seen
 * (AgentK working on it, or Claude holding the answers for the turn's end) within the grace.
 */
export interface RevisionSent {
  at: number;
  started: boolean;
}

/** Whether a session can take or run a turn: not exited or starting, and not waiting on the user. */
function canWork(session: Session): boolean {
  return session.status !== 'exited' && session.status !== 'starting' && !otherPrompt(session);
}

/** AgentK is in a turn, or Claude holds answers for the next one (through the mod). */
function busyWithTurn(session: Session): boolean {
  return session.status === 'working' || (session.promptsHeld > 0 && canWork(session));
}

/**
 * Whether the design panel's header shows AgentK at work on the design (its edge loader), and how
 * it's worded: 'revising' for the turn of a Send for revision from this page (`rev`), 'working' for
 * any other turn, null when it doesn't show. It follows the session's status, so a reload or another
 * screen shows it too; `rev` covers the moment between the click and Claude reporting the turn, and
 * the wording. Never while Claude waits on the user, nor before there's a design (the panel has its
 * own line then).
 */
export function designWorking(session: Session, rev: RevisionSent | null, now: number): 'revising' | 'working' | null {
  if (session.stage !== 'design' || session.desired !== 'running' || !session.hasDesign || session.approvalQueued) return null;
  const inGrace = rev !== null && !rev.started && now - rev.at < REVISION_GRACE_MS;
  if (busyWithTurn(session)) return rev?.started || inGrace ? 'revising' : 'working';
  return inGrace && canWork(session) ? 'revising' : null;
}

/**
 * A revision sent from this page, brought up to date with its session: started once its turn is
 * seen within the grace, and null once it's done with (its turn over, or never seen within the
 * grace, so a later turn isn't taken for it).
 */
export function settleRevision(session: Session, rev: RevisionSent, now: number): RevisionSent | null {
  if (session.stage !== 'design' || session.desired !== 'running') return null;
  const busy = busyWithTurn(session) || session.midTurn;
  if (rev.started) return busy ? rev : null;
  if (now - rev.at >= REVISION_GRACE_MS) return null;
  return busy ? { ...rev, started: true } : rev;
}

/** How long after the working line stops a newly written design still counts as the revision landing. */
export const LATE_MS = 3_000;

/**
 * Where the design panel's working line is, for its landing flash: `before` is the design's
 * `designUpdatedAt` as the line started (undefined while it isn't showing), `stoppedAt` when it last
 * stopped with the design unchanged (0 when that's done with).
 */
export interface Landing {
  before?: number | null;
  stoppedAt: number;
}

/**
 * The next `Landing` as the working line turns on or off (`on`) or the design is written (`at`), and
 * whether the revised design just landed: written while the line showed, or within `LATE_MS` after
 * it stopped.
 */
export function designLanding(prev: Landing, on: boolean, at: number | null, now: number): [Landing, boolean] {
  if (on) return [prev.before === undefined ? { before: at, stoppedAt: 0 } : prev, false];
  if (prev.before !== undefined) {
    const changed = prev.before !== at;
    return [{ stoppedAt: changed ? 0 : now }, changed];
  }
  if (prev.stoppedAt && now - prev.stoppedAt < LATE_MS) return [{ stoppedAt: 0 }, true];
  return [prev, false];
}

/**
 * A review's findings grouped for its panel: by round, then by the sweep that first raised them,
 * in that order. A round or sweep is named only when the review had more than one.
 */
export function findingGroups(findings: CodeReviewFinding[]): { key: string; label: string; findings: CodeReviewFinding[] }[] {
  const rounds = new Set(findings.map((f) => f.round)).size > 1;
  const sweeps = new Set(findings.map((f) => f.sweep)).size > 1;
  const groups = new Map<string, { key: string; label: string; round: number; sweep: number; findings: CodeReviewFinding[] }>();
  for (const f of findings) {
    const key = `${f.round}:${f.sweep}`;
    let g = groups.get(key);
    if (!g) {
      const label = [rounds ? `Round ${f.round}` : '', sweeps ? `sweep ${f.sweep}` : ''].filter(Boolean).join(', ');
      g = { key, label: label ? label[0].toUpperCase() + label.slice(1) : 'Findings', round: f.round, sweep: f.sweep, findings: [] };
      groups.set(key, g);
    }
    g.findings.push(f);
  }
  return [...groups.values()]
    .sort((a, b) => a.round - b.round || a.sweep - b.sweep)
    .map(({ key, label, findings: fs }) => ({ key, label, findings: fs }));
}

/**
 * Review overrides with one setting changed from its select: '' (Default) leaves it out, so a
 * level with nothing set is `{}`; a strategy that isn't one is left out too.
 */
export function withOverride(value: ReviewOverrides, key: keyof ReviewOverrides, raw: string): ReviewOverrides {
  const next: ReviewOverrides = {};
  if (value.strategy != null) next.strategy = value.strategy;
  if (value.rounds != null) next.rounds = value.rounds;
  if (value.depth != null) next.depth = value.depth;
  delete next[key];
  if (raw === '') return next;
  if (key === 'strategy') {
    if (isStrategySetting(raw)) next.strategy = raw;
  } else {
    const n = Number(raw);
    if (Number.isInteger(n)) next[key] = n;
  }
  return next;
}

const counted = (n: number, word: string) => `${n.toLocaleString('en-US')} ${word}${n === 1 ? '' : 's'}`;

/**
 * The New session dialog's estimate for a learn session (`LearnScope`), under its months field:
 * how many merged PRs and files it covers and where, and, past `LEARN_WARN_PRS` or
 * `LEARN_WARN_FILES`, a warning with what would make it smaller.
 */
export function learnScopeHint(scope: LearnScope, months: number): { text: string; warn: boolean } {
  const { paths } = scope;
  const where = !paths.length ? 'the whole repo'
    : paths.length <= 3 ? paths.join(paths.length === 2 ? ' and ' : ', ').replace(/, ([^,]*)$/, ' and $1')
    : `${paths.slice(0, 2).join(', ')} and ${paths.length - 2} more`;
  const prs = months > 0 ? `About ${counted(scope.prs, 'merged PR')}` : 'No PRs';
  const text = `${prs} and ${counted(scope.files, 'file')} in ${where}.`;
  const manyPrs = months > 0 && scope.prs > LEARN_WARN_PRS;
  const manyFiles = scope.files > LEARN_WARN_FILES;
  if (!manyPrs && !manyFiles) return { text, warn: false };
  const fixes = [paths.length ? 'name fewer folders' : 'name the folders to learn', ...(manyPrs ? ['read fewer months'] : [])];
  const fix = fixes.join(', or ');
  return { text: `${text} That's a lot to read: it can take hours and use millions of tokens. ${fix[0].toUpperCase()}${fix.slice(1)}.`, warn: true };
}
