import { basename } from 'node:path';
import { defaultProjectDatapackName, freeDatapackName } from '../shared/datapacks.js';
import { parseGithubRef } from '../shared/github-refs.js';
import { MOVE_TARGETS, moveBlockedIn, moveError } from '../shared/stages.js';
import {
  ACTIONS_MAX, INSTRUCTIONS_MAX, STAGE_LABELS, START_STAGES,
  type ActionResult, type AskAgentkAction, type CreateProjectRequest, type DatapackChoice, type MoveTarget, type Project, type Session, type StartStage,
} from '../shared/types.js';
import type { Core } from './core.js';
import { repoNameFromUrl } from './git.js';
import type { ModReply } from './hooks.js';
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

/** A started task's name and prompt, as the agent wrote them, are cut to these. */
const NAME_MAX = 80;
const PROMPT_MAX = 8000;

const isObject = (v: unknown): v is Record<string, unknown> => !!v && typeof v === 'object' && !Array.isArray(v);
const live = (s: Session) => s.desired !== 'archived' && s.status !== 'archived';
const str = (v: unknown) => (typeof v === 'string' ? v.trim() : '');
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
        if (!p) notes.push('Left out a task in a project that isn\'t open.');
        else if (!prompt) notes.push('Left out a task with no prompt.');
        else actions.push({ type: 'start', projectId: p.id, start, name: oneLine(str(r.name), NAME_MAX) || oneLine(prompt, 60), prompt });
        break;
      }
      case 'link': {
        const ref = str(r.ref);
        const projectId = str(r.projectId);
        const stage = str(r.stage) || str(r.start);
        if (!parseGithubRef(ref)) notes.push('Left out a link that isn\'t a GitHub PR or issue.');
        else if (projectId && !project(projectId)) notes.push(`Left out ${ref} in a project that isn't open.`);
        else {
          actions.push({
            type: 'link', ref,
            ...(projectId ? { projectId } : {}),
            ...(stage === 'design' || stage === 'code' ? { start: stage } : {}),
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
      const { session, done } = await core.sessions.begin(a.projectId, { name: a.name, prompt: a.prompt, start: a.start });
      done.catch((e: unknown) => core.log.warn(`session ${session.id} didn't start`, errMsg(e)));
      const p = core.projects.get(a.projectId);
      return { ok: true, text: `Started "${session.name}" in ${p.name} at ${STAGE_LABELS[a.start]}.`, projectId: p.id, sessionId: session.id };
    }
    case 'link': {
      const { project, session } = await core.githubLinks.start({ ref: a.ref, projectId: a.projectId, start: a.start }, { once: true });
      const what = session.kind === 'review-pr' ? 'a review of' : 'a task on';
      return { ok: true, text: `Started ${what} ${a.ref}, "${session.name}", in ${project.name}.`, projectId: project.id, sessionId: session.id };
    }
    case 'move': {
      const s = await core.sessions.move(a.sessionId, { stage: a.stage, instructions: a.instructions || undefined });
      return { ok: true, text: `Moved "${s.name}" to ${STAGE_LABELS[a.stage]}.`, projectId: s.projectId, sessionId: s.id };
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

// ---- AgentK's session tools: the calls `agentk mcp` makes (tools.ts, tooldefs.ts) ----

/** The answer to a tool call: the text the agent reads, marked as an error when nothing was done. */
function answer(text: string, isError = false): ModReply {
  return { status: 200, body: { text, isError } };
}

export class SessionTools {
  constructor(private core: Core) {}

  /** A tool call from `agentk mcp` in `row`'s session: `{ v, tool, input }`. */
  async call(row: SessionRow, body: Record<string, unknown>): Promise<ModReply> {
    if (!this.core.config.tools.enabled) return answer("AgentK's session tools are turned off (Settings → General in AgentK).", true);
    // A PR head from a public repo could be steering the agent: it gets no say over other sessions.
    if (row.untrusted === 1) {
      return answer("This session works on a PR from a public repo, so AgentK's session tools are off in it. Ask the user to do this in AgentK.", true);
    }
    const tool = str(body.tool) as ToolName;
    const input = isObject(body.input) ? body.input : {};
    const projects = await withGithub(this.core.actionProjects());
    try {
      switch (tool) {
        case 'list':
          return answer(listText(projects, row, str(input.projectId)));
        case 'start_sessions': {
          const list = Array.isArray(input.sessions) ? input.sessions : [];
          if (!list.length) return answer('Give at least one session to start.', true);
          if (list.length > ACTIONS_MAX) return answer(`Start at most ${ACTIONS_MAX} sessions in one call.`, true);
          const raw = list.map((e: unknown) => {
            const s = isObject(e) ? e : {};
            const projectId = str(s.projectId) || row.project_id;
            return str(s.ref)
              ? { type: 'link', ref: str(s.ref), projectId, stage: str(s.stage) }
              : { type: 'start', projectId, name: s.name, prompt: s.prompt, stage: str(s.stage) };
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
          return answer(`AgentK has no tool ${tool || '(none named)'}: this session may run a newer agentk mcp than the server. Update AgentK.`, true);
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

/** `list`'s answer: the open projects and their running sessions, as JSON. */
function listText(projects: ActionProject[], row: SessionRow, only: string): string {
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
  return JSON.stringify({ projects: out });
}

