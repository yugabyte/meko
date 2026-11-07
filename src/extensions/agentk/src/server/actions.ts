import { noOverrides, readOverrides, type ReviewOverrides } from '../shared/codereview.js';
import { REVIEW_TOOLS } from './review/runner.js';
import { basename } from 'node:path';
import { defaultProjectDatapackName, freeDatapackName } from '../shared/datapacks.js';
import { parseGithubRef } from '../shared/github-refs.js';
import { checkModel, HARNESS_CAPS, HARNESS_IDS, isHarnessId, modelLabel, type HarnessId } from '../shared/harnesses.js';
import { MOVE_TARGETS, moveBlockedIn, moveError } from '../shared/stages.js';
import {
  ACTIONS_MAX, INSTRUCTIONS_MAX, STAGE_LABELS, START_STAGES,
  type ActionResult, type AskAgentkAction, type CreateProjectRequest, type DatapackChoice, type MoveTarget, type Project, type Session, type StartStage,
} from '../shared/types.js';
import type { Core } from './core.js';
import { repoNameFromUrl } from './git.js';
import type { ModReply } from './hooks.js';
import { repoConfigTrusted } from './headtrust.js';
import { errMsg } from './log.js';
import { repoKey } from './paths.js';
import { HttpError } from './projects.js';
import { githubRemote, localClone } from './prreview.js';
import type { SessionRow } from './store.js';
import type { ToolName } from './tooldefs.js';

/**
 * The actions Ask AgentK proposes and AgentK's session tools take (`tools.ts`), and running them:
 * one vocabulary and one set of checks for both, each action run through the same core call the
 * UI's own route makes, so neither can do anything the user couldn't do in the UI.
 */

/** A project as the actions see it: its running sessions, archived ones left out, and its GitHub repo. */
export interface ActionProject {
  id: string;
  name: string;
  repoPath: string;
  /** `owner/name` of the repo on GitHub (its upstream for a fork), or null when it's not on GitHub or not read. */
  github?: string | null;
  sessions: Session[];
}

/** The most models `list` names for one agent. */
const LIST_MODELS_MAX = 50;

/** A started task's name and prompt, as the agent wrote them, are cut to these. */
const NAME_MAX = 80;
export const PROMPT_MAX = 8000;

const isObject = (v: unknown): v is Record<string, unknown> => !!v && typeof v === 'object' && !Array.isArray(v);
const live = (s: Session) => s.desired !== 'archived' && s.status !== 'archived';
const str = (v: unknown) => (typeof v === 'string' ? v.trim() : '');

/**
 * Whether a proposed start asks for Run to merge (`autoRun: true`), which only a task starting at
 * Code takes: asked for at any other stage, it's left out, with a note naming `what`.
 */
function autoRunFor(r: Record<string, unknown>, stage: string, what: string, notes: string[]): boolean {
  if (r.autoRun !== true) return false;
  if (stage === 'code') return true;
  notes.push(`Left out Run to merge for ${what}: it's only for a task that starts at Code.`);
  return false;
}
/**
 * The agent and model a proposed start asks for (`harness`, `model`: AgentK's session tools give
 * them), or null when either can't be used, with a note naming `what`. Whether the agent is
 * available is the start's to say (`Harnesses.pick`), as for the New session dialog.
 */
function agentFor(r: Record<string, unknown>, what: string, notes: string[]): { harness?: HarnessId; model?: string } | null {
  const harness = r.harness == null || r.harness === '' ? undefined : r.harness;
  if (harness !== undefined && !isHarnessId(harness)) {
    notes.push(`Left out ${what}: ${typeof harness === 'string' ? harness : 'that'} isn't an agent AgentK runs (${HARNESS_IDS.join(', ')}).`);
    return null;
  }
  const model = str(r.model);
  const bad = model && checkModel(model);
  if (bad) {
    notes.push(`Left out ${what}: ${bad}`);
    return null;
  }
  return { ...(harness ? { harness } : {}), ...(model ? { model } : {}) };
}

/** What a start's result line adds for the agent and model it was asked for: " on Kiro", " on Claude Code (Opus)". */
function onAgent(a: { harness?: HarnessId; model?: string }, s: Session): string {
  if (!a.harness && !a.model) return '';
  const label = HARNESS_CAPS[s.harness].label;
  return a.model ? ` on ${label} (${modelLabel(s.harness, a.model)})` : ` on ${label}`;
}

/** The review settings a proposed start asks for, when it asks for any (`review`, the task's own). */
function reviewFor(r: Record<string, unknown>): { review?: ReviewOverrides } {
  const o = readOverrides(r.review);
  return noOverrides(o) ? {} : { review: o };
}

/** One line, no control characters (sessionName() refuses them), at most `max` characters. */
export const oneLine = (s: string, max: number) =>
  // eslint-disable-next-line no-control-regex
  [...s.replace(/\s+/g, ' ').replace(/[\u0000-\u001f\u007f-\u009f]/g, '').trim()].slice(0, max).join('').trim();
const cut = (s: string, max: number) => [...s].slice(0, max).join('');

/** The running sessions, by id. */
export function runningSessions(projects: ActionProject[]): Map<string, Session> {
  return new Map(projects.flatMap((p) => p.sessions.filter(live).map((s) => [s.id, s] as const)));
}

/** The GitHub repo of each project, read from its remotes. */
export async function withGithub(projects: ActionProject[]): Promise<ActionProject[]> {
  return Promise.all(projects.map(async (p) => ({ ...p, github: p.github !== undefined ? p.github : await githubRemote(p.repoPath).catch(() => null) })));
}

/**
 * Proposed actions, checked against the projects and sessions now: the ones that fit, in their
 * order and at most `max`, and a note for each one left out.
 */
export function checkActions(raw: unknown, projects: ActionProject[], max = ACTIONS_MAX): { actions: AskAgentkAction[]; notes: string[] } {
  const actions: AskAgentkAction[] = [];
  const notes: string[] = [];
  const sessions = runningSessions(projects);
  const project = (id: string) => projects.find((p) => p.id === id);
  for (const a of Array.isArray(raw) ? raw : []) {
    if (actions.length >= max) break;
    const r = (a && typeof a === 'object' ? a : {}) as Record<string, unknown>;
    const session = sessions.get(str(r.sessionId));
    const instructions = cut(str(r.instructions), INSTRUCTIONS_MAX);
    switch (r.type) {
      case 'start': {
        const p = project(str(r.projectId));
        const prompt = cut(str(r.prompt), PROMPT_MAX);
        const start = (START_STAGES as string[]).includes(str(r.stage) || str(r.start)) ? ((str(r.stage) || str(r.start)) as StartStage) : 'design';
        const name = oneLine(str(r.name), NAME_MAX) || oneLine(prompt, 60);
        const branch = oneLine(str(r.branch), NAME_MAX);
        const agent = p && prompt ? agentFor(r, `"${name}"`, notes) : null;
        if (!p) notes.push('Left out a task in a project that isn\'t open.');
        else if (!prompt) notes.push('Left out a task with no prompt.');
        else if (agent) {
          actions.push({
            type: 'start', projectId: p.id, start, name, prompt, ...(branch ? { branch } : {}), ...(autoRunFor(r, start, name, notes) ? { autoRun: true } : {}), ...reviewFor(r), ...agent,
          });
        }
        break;
      }
      case 'link': {
        const ref = str(r.ref);
        const projectId = str(r.projectId);
        const stage = str(r.stage) || str(r.start);
        const agent = parseGithubRef(ref) && !(projectId && !project(projectId)) ? agentFor(r, ref, notes) : null;
        if (!parseGithubRef(ref)) notes.push('Left out a link that isn\'t a GitHub PR or issue.');
        else if (projectId && !project(projectId)) notes.push(`Left out ${ref} in a project that isn't open.`);
        else if (agent) {
          actions.push({
            type: 'link', ref,
            ...(projectId ? { projectId } : {}),
            ...(stage === 'design' || stage === 'code' ? { start: stage } : {}),
            ...(autoRunFor(r, stage, ref, notes) ? { autoRun: true } : {}),
            ...reviewFor(r),
            ...agent,
          });
        }
        break;
      }
      case 'go': {
        const view = str(r.view);
        if (view === 'usage' || view === 'settings') actions.push({ type: 'go', view });
        else if (view === 'home') {
          if (project(str(r.projectId))) actions.push({ type: 'go', view, projectId: str(r.projectId) });
          else notes.push('Left out a project that isn\'t open.');
        } else if (view === 'session') {
          if (session) actions.push({ type: 'go', view, projectId: session.projectId, sessionId: session.id });
          else notes.push('Left out a session that isn\'t running.');
        } else notes.push('Left out a place AgentK can\'t open.');
        break;
      }
      case 'move': {
        const stage = str(r.stage) as MoveTarget;
        if (!session) {
          notes.push('Left out a move of a session that isn\'t running.');
          break;
        }
        const why = moveBlockedIn(session.stage) ?? (MOVE_TARGETS.includes(stage) ? moveError(session, stage) : 'That stage doesn\'t exist.');
        if (why) notes.push(`Left out moving ${session.name}: ${why}`);
        else actions.push({ type: 'move', sessionId: session.id, stage, instructions });
        break;
      }
      case 'openPr': {
        if (!session) notes.push('Left out an Open PR for a session that isn\'t running.');
        else if (session.stage !== 'code') notes.push(`Left out an Open PR for ${session.name}: it isn't in Code.`);
        else actions.push({ type: 'openPr', sessionId: session.id, instructions });
        break;
      }
      case 'openProject': {
        const repo = str(r.repo);
        const datapack = str(r.datapack) || 'new';
        if (!repo) notes.push('Left out opening a project with no repo.');
        else actions.push({ type: 'openProject', repo, name: oneLine(str(r.name), NAME_MAX), datapack });
        break;
      }
      default:
        notes.push('Left out an action AgentK doesn\'t have.');
    }
  }
  return { actions, notes };
}

/** Who asked for a run: a session's agent through the session tools, or the user (Ask AgentK's Do all). */
export interface ActionCaller {
  session: SessionRow | null;
}

/**
 * Runs checked actions in order, each through the core call its UI route makes (which checks it
 * again), and says what each came to; one that fails doesn't stop the rest. `go` only navigates, so
 * it isn't run. An action a session's agent took is written as an event on that session and on the
 * session it changed.
 */
export async function runActions(core: Core, actions: AskAgentkAction[], by: ActionCaller): Promise<ActionResult[]> {
  const results: ActionResult[] = [];
  for (const a of actions) {
    if (a.type === 'go') continue;
    let r: ActionResult;
    try {
      r = await runOne(core, a, by);
    } catch (e) {
      r = { ok: false, text: `${describe(core, a)}: ${errMsg(e)}` };
    }
    if (by.session) {
      core.sessions.event(by.session.id, 'tools', r.text, { action: a.type, ok: r.ok });
      if (r.ok && r.sessionId && r.sessionId !== by.session.id) {
        core.sessions.event(r.sessionId, 'tools', `${verb(a)} by the agent in ${by.session.name}`, { by: by.session.id });
      }
    }
    results.push(r);
  }
  return results;
}

/**
 * Where a move left a session, in words: a move to Pull Request that a code review took over leaves it
 * in Code until the review sends the PR prompt (Lifecycle.reviewBeforePr).
 */
function movedTo(asked: MoveTarget, s: Pick<Session, 'stage'>): string {
  return asked === 'review' && s.stage === 'code' ? 'Code, for its code review before the PR opens' : STAGE_LABELS[asked];
}

/** What an action does, for an event on the session it changed. */
function verb(a: AskAgentkAction): string {
  switch (a.type) {
    case 'move': return `Moved to ${STAGE_LABELS[a.stage]}`;
    case 'openPr': return 'Asked to open its PR';
    default: return 'Started';
  }
}

/** An action in words, for a line that says why it failed. */
function describe(core: Core, a: AskAgentkAction): string {
  const name = (id: string) => {
    try {
      return `"${core.sessions.get(id).name}"`;
    } catch {
      return 'a session';
    }
  };
  switch (a.type) {
    case 'start': return `Couldn't start "${a.name}"`;
    case 'link': return `Couldn't start a session on ${a.ref}`;
    case 'move': return `Couldn't move ${name(a.sessionId)} to ${STAGE_LABELS[a.stage]}`;
    case 'openPr': return `Couldn't open the PR for ${name(a.sessionId)}`;
    case 'openProject': return `Couldn't open a project on ${a.repo}`;
    default: return "Couldn't do that";
  }
}

async function runOne(core: Core, a: Exclude<AskAgentkAction, { type: 'go' }>, by: ActionCaller): Promise<ActionResult> {
  switch (a.type) {
    case 'start': {
      const { session, done } = await core.sessions.begin(a.projectId, {
        name: a.name, prompt: a.prompt, start: a.start, autoRun: a.autoRun, review: a.review, harness: a.harness, model: a.model, ...(a.branch ? { branch: a.branch, existingBranch: true } : {}),
      });
      done.catch((e: unknown) => core.log.warn(`session ${session.id} didn't start`, errMsg(e)));
      const p = core.projects.get(a.projectId);
      return {
        ok: true, text: `Started "${session.name}" in ${p.name} at ${STAGE_LABELS[a.start]}${a.branch ? ` on ${a.branch}` : ''}${onAgent(a, session)}${a.autoRun ? ', with Run to merge' : ''}.`,
        projectId: p.id, sessionId: session.id,
      };
    }
    case 'link': {
      const { project, session } = await core.githubLinks.start({
        ref: a.ref, projectId: a.projectId, start: a.start, autoRun: a.autoRun, review: a.review, harness: a.harness, model: a.model,
      }, { once: true });
      const what = session.kind === 'review-pr' ? 'a review of' : 'a task on';
      return {
        ok: true, text: `Started ${what} ${a.ref}, "${session.name}", in ${project.name}${onAgent(a, session)}${a.autoRun ? ', with Run to merge' : ''}.`,
        projectId: project.id, sessionId: session.id,
      };
    }
    case 'move': {
      // The calling session is mid-turn (its agent is running the tool), and a move waits for a
      // turn's end, as the move dialog does: it goes when this turn ends (Lifecycle.onTurnEnd).
      if (by.session?.id === a.sessionId) {
        core.tools.queueSelfMove(a.sessionId, { stage: a.stage, instructions: a.instructions });
        return { ok: true, text: `Moving "${by.session.name}" to ${STAGE_LABELS[a.stage]} once this turn ends.`, projectId: by.session.project_id, sessionId: a.sessionId };
      }
      const s = await core.sessions.move(a.sessionId, { stage: a.stage, instructions: a.instructions || undefined });
      return { ok: true, text: `Moved "${s.name}" to ${movedTo(a.stage, s)}.`, projectId: s.projectId, sessionId: s.id };
    }
    case 'openPr': {
      // A session's own PR stays the user's gate.
      if (by.session?.id === a.sessionId) throw new HttpError(403, "a session doesn't open its own PR: the user does, with Open PR in AgentK");
      const s = await core.sessions.action(a.sessionId, 'open-pr', a.instructions ? { instructions: a.instructions } : undefined);
      return { ok: true, text: `Opening the PR for "${s.name}".`, projectId: s.projectId, sessionId: s.id };
    }
    case 'openProject': {
      const p = await openProject(core, a);
      return { ok: true, text: `Opened ${p.name} on ${p.repoPath}.`, projectId: p.id };
    }
  }
}

/**
 * Opens a project for an `openProject` action. The repo is a scanned repo (by its path), GitHub's
 * `owner/name` (its clone here, else cloned) or a clone URL; never any other folder, which the Open
 * project dialog would make a repo. The datapack: a new one (while Meko is on), none, or one by id.
 */
async function openProject(core: Core, a: Extract<AskAgentkAction, { type: 'openProject' }>): Promise<Project> {
  const { repos } = await core.projects.repos();
  const gh = parseGithubRef(`${a.repo}#1`)?.repo ?? null;
  let where: Pick<CreateProjectRequest, 'repoPath' | 'cloneUrl'>;
  let repoName: string;
  const scanned = repos.find((r) => r.path === a.repo || repoKey(r.path) === a.repo);
  if (scanned) {
    where = { repoPath: scanned.path };
    repoName = basename(scanned.path);
  } else if (gh) {
    const clone = localClone(repos, gh);
    where = clone ? { repoPath: clone } : { cloneUrl: `https://github.com/${gh}.git` };
    repoName = gh.split('/')[1];
  } else if (/^(https?|ssh|git):\/\/|^[\w.-]+@[\w.-]+:/.test(a.repo)) {
    where = { cloneUrl: a.repo };
    repoName = repoNameFromUrl(a.repo);
  } else {
    throw new HttpError(400, `${a.repo} isn't a repo in your repos folders, a GitHub owner/name or a clone URL`);
  }
  const name = a.name || repoName;
  let datapack: DatapackChoice = { mode: 'none' };
  if (a.datapack === 'new') {
    if (core.mekoStatus.configured) {
      const taken = new Set((await core.meko.listDatapacks().catch(() => [])).map((d) => d.name));
      datapack = { mode: 'create', name: freeDatapackName(defaultProjectDatapackName(repoName, name), taken) };
    }
  } else if (a.datapack !== 'none') {
    const dp = (await core.meko.listDatapacks()).find((d) => d.id === a.datapack);
    if (!dp) throw new HttpError(404, `Meko has no datapack ${a.datapack} that you can use`);
    datapack = { mode: 'existing', id: dp.id, name: dp.name };
  }
  try {
    return await core.projects.create({ ...where, name, datapack });
  } catch (e) {
    if (!(e instanceof HttpError && e.code === 'DATAPACK_LIMIT') || datapack.mode !== 'create') throw e;
    core.log.warn(`opening a project on ${a.repo} without a datapack: ${e.message}`);
    return core.projects.create({ ...where, name, datapack: { mode: 'none' } });
  }
}

// ---- AgentK's session tools: the calls `me_kode mcp` makes (tools.ts, tooldefs.ts) ----

/** The answer to a tool call: the text the agent reads, marked as an error when nothing was done. */
function answer(text: string, isError = false): ModReply {
  return { status: 200, body: { text, isError } };
}

/** The answer to a call still running when the hook socket closes (a stop, a restart, a rebind). */
const CLOSING_TEXT = "AgentK is restarting, so it couldn't wait for this. What it started may still finish: call list in a minute to see what's there now.";

/** A promise and what settles it. */
function closingSignal(): { promise: Promise<void>; resolve: () => void } {
  let resolve = () => {};
  const promise = new Promise<void>((r) => (resolve = r));
  return { promise, resolve };
}

/** A move a session's own agent asked for, waiting for its turn to end. */
interface SelfMove {
  stage: MoveTarget;
  instructions: string;
}

export class SessionTools {
  /** Settles when the hook socket closes (`pause`): calls still running are answered then, so none holds the socket open. */
  private closing = closingSignal();
  /** Moves of a session by its own agent, by session id, for its turn's end (in memory: a restart drops them). */
  private selfMoves = new Map<string, SelfMove>();

  constructor(private core: Core) {}

  /** The agents new sessions can start on now, the default first marked, with the models each is known to take. */
  private agents(): ListedAgent[] {
    const cfg = this.core.config;
    const def = this.core.harnesses.defaultFor(cfg)?.id;
    return this.core.harnesses.status(cfg).filter((h) => h.available).map((h) => {
      const fixed = HARNESS_CAPS[h.id].models;
      // At most LIST_MODELS_MAX of a CLI's list (OpenCode's can name hundreds): any other id still works.
      const models = (fixed === 'cli' ? this.core.harnesses.modelsKept(cfg, h.id) : fixed.map((m) => m.id))?.slice(0, LIST_MODELS_MAX);
      return { id: h.id, name: HARNESS_CAPS[h.id].label, ...(h.id === def ? { default: true as const } : {}), ...(models?.length ? { models } : {}) };
    });
  }

  /**
   * A tool call from `me_kode mcp` in `row`'s session: `{ v, tool, input }`. Answered at once if the
   * hook socket closes first: a call that clones a repo can take minutes, and the socket's close
   * (a stop, a restart, a rebind) waits for every request still open.
   */
  call(row: SessionRow, body: Record<string, unknown>): Promise<ModReply> {
    return Promise.race([this.callNow(row, body), this.closing.promise.then(() => answer(CLOSING_TEXT, true))]);
  }

  /** The hook socket is closing: answers every call still running. Calls after it wait on the next close. */
  pause(): void {
    this.closing.resolve();
    this.closing = closingSignal();
  }

  queueSelfMove(id: string, move: SelfMove): void {
    this.selfMoves.set(id, move);
  }

  /**
   * The session's turn ended (Lifecycle.onTurnEnd): a move its own agent asked for goes now, with
   * the move dialog's checks, and its outcome is written to the session's log. An interrupted turn
   * drops it. True when one was waiting.
   */
  async turnEnded(id: string, interrupted: boolean): Promise<boolean> {
    const move = this.selfMoves.get(id);
    if (!move) return false;
    this.selfMoves.delete(id);
    if (interrupted) {
      this.core.sessions.event(id, 'tools', `Didn't move to ${STAGE_LABELS[move.stage]}: the turn that asked for it was interrupted`);
      return true;
    }
    try {
      const s = await this.core.sessions.move(id, { stage: move.stage, instructions: move.instructions || undefined });
      this.core.sessions.event(id, 'tools', `Moved to ${movedTo(move.stage, s)} by its agent, at the turn's end`);
    } catch (e) {
      this.core.sessions.event(id, 'tools', `Couldn't move to ${STAGE_LABELS[move.stage]}: ${errMsg(e)}`);
    }
    return true;
  }

  private async callNow(row: SessionRow, body: Record<string, unknown>): Promise<ModReply> {
    // A code review's reports: from its steps and its task, whatever `[tools] enabled` says. They
    // only report on the session's own review, so they're answered for an untrusted head too.
    const reviewTool = REVIEW_TOOLS.find((t) => t === body.tool);
    if (reviewTool) {
      const r = this.core.reviews.onTool(row, reviewTool, isObject(body.input) ? body.input : {});
      return answer(r.text, r.error);
    }
    if (!this.core.config.tools.enabled) return answer("AgentK's session tools are turned off (Settings → General in AgentK).", true);
    // A PR head from a public repo could be steering the agent: it gets no say over other sessions.
    // Not checked yet (a PR linked since the launch) counts as untrusted, as the launch has it.
    if (!repoConfigTrusted(row)) {
      return answer("This session works on a PR from a public repo, or one AgentK hasn't checked yet, so AgentK's session tools are off in it. Ask the user to do this in AgentK.", true);
    }
    const tool = str(body.tool) as ToolName;
    const input = isObject(body.input) ? body.input : {};
    const projects = await withGithub(this.core.actionProjects());
    try {
      switch (tool) {
        case 'list':
          return answer(listText(projects, row, str(input.projectId), this.agents()));
        case 'start_sessions': {
          const list = Array.isArray(input.sessions) ? input.sessions : [];
          if (!list.length) return answer('Give at least one session to start.', true);
          if (list.length > ACTIONS_MAX) return answer(`Start at most ${ACTIONS_MAX} sessions in one call.`, true);
          const raw = list.map((e: unknown) => {
            const s = isObject(e) ? e : {};
            const projectId = str(s.projectId) || row.project_id;
            return str(s.ref)
              ? { type: 'link', ref: str(s.ref), projectId, stage: str(s.stage), autoRun: s.autoRun, review: s.review, harness: s.agent, model: s.model }
              : { type: 'start', projectId, name: s.name, prompt: s.prompt, stage: str(s.stage), branch: s.branch, autoRun: s.autoRun, review: s.review, harness: s.agent, model: s.model };
          });
          return this.run(row, raw, projects);
        }
        case 'move_session':
          return this.run(row, [{ type: 'move', sessionId: input.sessionId, stage: input.stage, instructions: input.instructions }], projects);
        case 'open_pr':
          return this.run(row, [{ type: 'openPr', sessionId: input.sessionId, instructions: input.instructions }], projects);
        case 'open_project':
          return this.run(row, [{ type: 'openProject', repo: input.repo, name: input.name, datapack: input.datapack }], projects);
        default:
          return answer(`AgentK has no tool ${tool || '(none named)'}: this session may run a newer me_kode mcp than the server. Update AgentK.`, true);
      }
    } catch (e) {
      return answer(`AgentK couldn't do that: ${errMsg(e)}`, true);
    }
  }

  /** Checks and runs the actions a call came to; one line per action, an error when none was done. */
  private async run(row: SessionRow, raw: unknown[], projects: ActionProject[]): Promise<ModReply> {
    const { actions, notes } = checkActions(raw, projects);
    const results = await runActions(this.core, actions, { session: row });
    const lines = [...notes, ...results.map((r) => r.text)];
    return answer(lines.join('\n') || 'Nothing to do.', !results.some((r) => r.ok));
  }
}

/** An agent new sessions can start on, as `list` names it for `start_sessions`' `agent` and `model`. */
interface ListedAgent {
  id: HarnessId;
  name: string;
  /** Sessions start on it when no agent is named. */
  default?: true;
  /** Model names it takes besides its own default (any other id works too), when known. */
  models?: string[];
}

/** `list`'s answer: the agents new sessions can start on, and the open projects and their running sessions, as JSON. */
function listText(projects: ActionProject[], row: SessionRow, only: string, agents: ListedAgent[] = []): string {
  const running = runningSessions(projects);
  const out = projects.filter((p) => !only || p.id === only).map((p) => ({
    id: p.id,
    name: p.name,
    repo: p.repoPath,
    ...(p.github ? { github: p.github } : {}),
    sessions: p.sessions.filter((s) => running.has(s.id)).map((s) => ({
      id: s.id, name: s.name, stage: s.stage, status: s.status, branch: s.branch, harness: s.harness,
      ...(s.prNumber ? { pr: s.prNumber } : {}),
      ...(s.id === row.id ? { self: true } : {}),
    })),
  }));
  return JSON.stringify({ ...(agents.length ? { agents } : {}), projects: out });
}

