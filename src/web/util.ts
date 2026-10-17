import { HARNESS_CAPS, harnessOf, type HarnessId } from '../shared/harnesses';
import { DEFAULT_MEKO_URL, mekoWebUrl } from '../shared/mekourls';
import type { SyntheticEvent } from 'react';
import { INSTRUCTIONS_MAX, STAGE_LABELS } from '../shared/types';
import type { HarnessStatus, MekoFirstTurn, MekoGapCounts, MekoGapQuality, MekoLearning, MekoSettings, Project, ProjectWithSessions, ReviewingPr, Session, SessionStatus, TokenUsage } from '../shared/types';

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

export function liveSessions(p: ProjectWithSessions): Session[] {
  return p.sessions.filter((s) => s.desired !== 'archived' && s.status !== 'archived');
}

/**
 * Whether the open sessions across all projects run on more than one harness: the session tabs and
 * the rail then show each session's logo (`HarnessLogo`), and none while they all run on one.
 */
export function harnessLogos(projects: ProjectWithSessions[]): boolean {
  return new Set(projects.flatMap((p) => liveSessions(p).map((s) => harnessOf(s.harness)))).size > 1;
}

/**
 * Settings' Default agent: the agents it offers (only available ones) and the one it shows picked,
 * or null while there's no choice (fewer than two available), when Settings leaves it out. A stored
 * default that isn't available shows as the first available (`AppState.harnesses` is in
 * `HARNESS_IDS` order), which the server starts on meanwhile; it stays stored, so it decides again
 * once it's available (unless Kiro is turned off in Settings, which resets it to Claude Code).
 */
export function defaultAgentChoice(harnesses: HarnessStatus[], stored: HarnessId): { agents: HarnessId[]; value: HarnessId } | null {
  const agents = harnesses.filter((h) => h.available).map((h) => h.id);
  return agents.length > 1 ? { agents, value: agents.includes(stored) ? stored : agents[0] } : null;
}

/** A project's open sessions in the order the rail and the phone's tabs list them: oldest first. */
export function openSessions(p: ProjectWithSessions): Session[] {
  return liveSessions(p).sort((a, b) => a.createdAt - b.createdAt);
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

/** Remembers the session open in a project, so switching back to the project reopens it. */
export function rememberOpenSession(projectId: string, sessionId: string) {
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

/**
 * What a session's dot shows: `Session.learning` over idle, recovered and working, then
 * `Session.queued` over idle and recovered, then `Session.mekoCall` over working.
 */
export function dotState(s: Pick<Session, 'status' | 'learning' | 'queued' | 'mekoCall'>): DotState {
  if (s.learning && LEARNING_SHOWS.has(s.status)) return 'learning';
  if (s.queued && QUEUED_SHOWS.has(s.status)) return 'queued';
  return s.mekoCall && s.status === 'working' ? 'meko' : s.status;
}

/** The project's dot in the rail: its most urgent session's. */
export function aggregateDot(p: ProjectWithSessions): DotState {
  let best: DotState = 'idle';
  for (const s of liveSessions(p)) {
    const dot = dotState(s);
    if (URGENCY[dot] > URGENCY[best]) best = dot;
  }
  return best;
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

export function duration(ms: number | null | undefined, now = Date.now()): string {
  if (!ms) return '';
  const s = Math.max(0, Math.round((now - ms) / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.floor(m / 60);
  return `${h}h ${m % 60}m`;
}

export function slugify(s: string): string {
  return s
    .toLowerCase()
    .replace(/[^a-z0-9-]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 60);
}

/** A datapack's page in the Meko web app, or null when the web app's address is unknown. */
export function mekoDatapackUrl(webUrl: string | null, datapackId: string): string | null {
  return webUrl ? `${webUrl}/datapacks/${encodeURIComponent(datapackId)}` : null;
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

/**
 * The Meko web app pages for getting an account and an API key (keys are under Settings →
 * Security). Signing in or up with `return_to` lands on Settings. Null when the web app is unknown.
 */
export function mekoKeyLinks(webUrl: string | null): { signup: string; signin: string; settings: string } | null {
  const base = webUrl?.trim().replace(/\/+$/, '') ?? '';
  if (!httpHost(base)) return null;
  const back = `return_to=${encodeURIComponent('/settings')}`;
  return { signup: `${base}/signup?${back}`, signin: `${base}/signin?${back}`, settings: `${base}/settings` };
}

/**
 * Which Meko web app the sign-up links point to, from the MCP and web app URLs as typed (saved or
 * not): worked out the way the server will once they're saved (`mekoWebUrl`), with an empty field
 * falling back to what the environment or Claude Code's settings give. Null when it isn't known
 * (your own Meko with no web app URL); `mekoKeyLinks` drops anything that isn't http(s).
 */
export function mekoKeyLinkBase(typed: { url: string; webUrl: string }, m: Pick<MekoSettings, 'fallbackUrl' | 'envWebUrl'>): string | null {
  // The server saves Meko Cloud's URL as empty, like a cleared field.
  const url = typed.url.trim().replace(/\/+$/, '');
  return mekoWebUrl(url && url !== DEFAULT_MEKO_URL ? url : m.fallbackUrl, typed.webUrl.trim() || m.envWebUrl);
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

/** The approve button's label: with open questions, it takes the user to answer them first. */
export function approveLabel(openQuestions: number): string {
  return openQuestions ? 'Answer & approve' : 'Approve design';
}

/**
 * Whether the design panel's header shows the approve button: for a design of a running session in
 * Design, as the questions' footer is. With open questions it only takes the user to them, so it's
 * left out while they're on screen (their footer approves, with the answers) and while they can't
 * be shown (the design is loading, or couldn't be read).
 */
export function headerApproves(session: Session, questions: 'shown' | 'hidden' | 'none'): boolean {
  if (session.stage !== 'design' || session.desired !== 'running' || !session.hasDesign) return false;
  return !session.designQuestions.open || questions === 'hidden';
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
  ['Waiting for the AgentK review.', 'waiting on the AgentK review'],
  ['AgentK is waiting on you in the terminal.', 'waiting on you in the terminal'],
];

/** The pill a queued gate shows in place of its button (`Session.queued`); null when nothing is queued. */
export function queuedText(session: Session): string | null {
  if (session.queued === 'open-pr') return '✓ Open PR queued: goes when the turn ends';
  if (session.queued !== 'merge') return null;
  const first = session.mergeBlockers[0];
  const waits = first ? (MERGE_WAITS.find(([text]) => text === first)?.[1] ?? 'waiting on AgentK') : 'merging soon';
  return `✓ Merge queued: ${waits}`;
}

/** A session tab's tooltip: name, what its dot means, stage and tokens. */
export function sessionTabTitle(s: Session): string {
  return [
    s.name,
    HARNESS_CAPS[harnessOf(s.harness)].label,
    dotTitle(s) + (s.statusDetail && s.status !== 'starting' ? `: ${s.statusDetail}` : ''),
    STAGE_LABELS[s.stage],
    `${formatTokens(totalTokens(s.usage))} tokens`,
  ].join(' · ');
}

/**
 * A session's dot in words, for its tab's tooltip: the long learning text, or what a queued gate
 * waits on. The queued dot shows between turns, when a queued Open PR is already going (the pill's
 * "goes when the turn ends" is for mid-turn).
 */
export function dotTitle(s: Session): string {
  const dot = dotState(s);
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
