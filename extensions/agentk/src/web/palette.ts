// Command palette labels and filtering. Pure, so it's unit-tested.
import { shareLinkToken } from '../shared/mekourls';
import { STAGE_LABELS, type AskAgentkAction, type JoinByLinkResult, type LinkPlan, type LinkProject, type ProjectWithSessions, type SessionMatch } from '../shared/types';
import { stageWords } from './util';

/** A piece of a palette label: plain text, or a session's or project's name, drawn in its own style. */
export type LabelPart = string | { session: string } | { project: string };

/** The label as plain text: what the filter searches and what screen readers read. */
export function labelText(parts: LabelPart[]): string {
  return parts.map((p) => (typeof p === 'string' ? p : 'session' in p ? p.session : p.project)).join('');
}

/** The items whose label and keywords contain every word typed, in any order; items keep their order. */
export function filterItems<T extends { label: LabelPart[]; keywords: string }>(items: T[], q: string): T[] {
  const terms = q.toLowerCase().split(/\s+/).filter(Boolean);
  if (!terms.length) return items;
  return items.filter((it) => {
    const hay = `${labelText(it.label)} ${it.keywords}`.toLowerCase();
    return terms.every((t) => hay.includes(t));
  });
}

/**
 * What a pasted link's palette item does: go to a session, start one (an issue's at `start`) in
 * `projectId` (left out: in the project the plan opens), or nothing (it says why).
 */
export type LinkAction = { go: { projectId: string; sessionId: string } } | { start: 'design' | 'code' | null; projectId?: string } | null;

export interface LinkEntry {
  id: string;
  label: LabelPart[];
  hint: string;
  action: LinkAction;
  /** Starting it clones the repo first (for its Busy label). */
  clones: boolean;
}

/**
 * The palette items for a pasted PR or issue: sessions already on it first; then, for an open PR
 * nobody reviews yet, Review it; for an issue, work on it from Design or straight from Code; then the
 * same in each other project on the repo (an issue's at Design). Each names the project, and its hint
 * says why that project and what else starting does (a new project, a clone, a share).
 */
export function linkEntries(plan: LinkPlan): LinkEntry[] {
  const pr = plan.kind === 'pr';
  const out: LinkEntry[] = plan.existing.map((s) => ({
    id: `link-go-${s.sessionId}`,
    label: ['Go to ', { session: s.name }],
    hint: pr ? 'Already reviewing it' : 'Already working on it',
    action: { go: { projectId: s.projectId, sessionId: s.sessionId } },
    clones: false,
  }));
  if (pr && out.length) return out;
  if (pr && plan.state !== 'open') {
    return [{
      id: 'link-closed',
      label: [`PR #${plan.number} of ${plan.repo} is ${plan.state}, so there's nothing to review.`],
      hint: '',
      action: null,
      clones: false,
    }];
  }
  const np = plan.newProject;
  const why: Record<LinkProject['why'], string> = { datapack: 'Names its datapack', current: 'This project', repo: `On ${plan.repo}` };
  /** Where an item starts: the label's end, its hint's notes, whether it clones, and the project it sends. */
  const at = (p: LinkProject | null, notes: string[]) => {
    const clones = p ? !p.clone : !np?.repoPath;
    if (clones) notes.push(`Clones ${plan.repo}`);
    return {
      // A closed project that opens again is named as itself, not as a new one.
      where: (p ? [' in ', { project: p.name }] : [np?.reopens ? ' in ' : ' in a new project ', { project: np?.name ?? plan.repo }]) as LabelPart[],
      notes,
      clones,
      projectId: p?.id,
    };
  };
  const notes: string[] = [];
  if (plan.project) notes.push(why[plan.project.why]);
  if (np) notes.push(np.reopens ? 'Opens the project again' : 'Opens a new project');
  const main = at(plan.project, notes);
  if (np?.datapack === 'create') main.notes.push('New datapack');
  else if (np?.datapack?.invite) main.notes.push(`Accepts the share of ${np.datapack.name}`);
  const others = plan.others.map((p) => ({ p, ...at(p, [`Also on ${plan.repo}`]) }));
  const hint = (first: string[], rest: string[]) => [...first, ...rest].join(' · ');
  if (pr) {
    const review = (id: string, w: typeof main): LinkEntry => ({
      id, label: [`Review #${plan.number} ${plan.title}`, ...w.where], hint: hint([], w.notes), action: { start: null, projectId: w.projectId }, clones: w.clones,
    });
    out.push(review('link-review', main), ...others.map((o) => review(`link-review-${o.p.id}`, o)));
    return out;
  }
  const closed = plan.state !== 'open' ? ['Closed issue'] : [];
  const design = (id: string, w: typeof main): LinkEntry => ({
    id, label: [`Work on issue #${plan.number} ${plan.title}`, ...w.where], hint: hint([...closed, 'Starts at Design'], w.notes),
    action: { start: 'design', projectId: w.projectId }, clones: w.clones,
  });
  out.push(
    design('link-design', main),
    {
      id: 'link-code', label: [`Work on issue #${plan.number}`, ...main.where, ', straight to Code'], hint: hint([...closed, 'Starts at Code'], main.notes),
      action: { start: 'code', projectId: main.projectId }, clones: main.clones,
    },
    ...others.map((o) => design(`link-design-${o.p.id}`, o)),
  );
  return out;
}

/** The palette input's placeholder, one at a time, so it shows everything the palette takes. */
export const PALETTE_HINTS = ['Ask AgentK to start a task…', 'Type a command…', 'Find a session by what it’s about…', 'Paste a GitHub PR or issue link…'];
/** The placeholder with reduced motion, which doesn't rotate. */
export const PALETTE_HINT_STILL = 'Ask AgentK, run a command, find a session by what it’s about or paste a GitHub link…';

/**
 * The palette item for a Meko share link pasted on its own: only one on this Meko's web app, so a
 * look-alike link is never offered. Picking it joins; pasting doesn't.
 */
export function joinEntry(text: string, webUrl: string | null): { id: string; label: LabelPart[]; hint: string } | null {
  return shareLinkToken(text, webUrl) ? { id: 'join-link', label: ['Join the datapack this link shares'], hint: 'Meko share link' } : null;
}

/** What joining by a share link came to, when there's no share to open or project to go to. */
export function joinedText(r: JoinByLinkResult): string {
  const what = r.name ?? 'the datapack';
  const did = r.alreadyMember ? `You already have access to ${what}` : `You joined ${what}${r.grant ? ` as ${r.grant}` : ''}`;
  if (!r.name) return `${did}. It shows in Open project once Meko lists it.`;
  if (r.grant === 'viewer') return `${did}. A viewer can’t open it as a project: attach it to one from the project’s Datapacks box.`;
  return `${did}. Pick it in Open project, or attach it to a project from the project’s Datapacks box.`;
}

/** A palette item for an action AgentK proposed; `edit` opens New session with the task instead of starting it. */
export interface AskEntry {
  id: string;
  label: LabelPart[];
  hint: string;
  action: AskAgentkAction;
  edit?: boolean;
}

/** The first line of `s`, cut to `max` characters with an ellipsis. */
function preview(s: string, max: number): string {
  const line = s.split('\n').find((l) => l.trim())?.trim() ?? '';
  return line.length > max ? `${line.slice(0, max - 1)}…` : line;
}

/**
 * The palette items for AgentK's proposed actions, named as the rest of the palette names them; a
 * task also gets an item that opens it in New session to change it first.
 */
export function askEntries(actions: AskAgentkAction[], projects: ProjectWithSessions[]): AskEntry[] {
  const projectName = (id: string) => projects.find((p) => p.id === id)?.name ?? 'a closed project';
  const session = (id: string) => projects.flatMap((p) => p.sessions).find((s) => s.id === id);
  const sessionName = (id: string) => session(id)?.name ?? 'a session';
  return actions.flatMap((a, i): AskEntry[] => {
    const id = `ask-${i}`;
    switch (a.type) {
      case 'start': {
        // The prompt as a hint, unless it only says the name again.
        const said = preview(a.prompt, 80);
        const same = said.replace(/[.!\s]+$/, '').toLowerCase() === a.name.toLowerCase();
        return [
          { id, label: ['Start ', { session: a.name }, ' in ', { project: projectName(a.projectId) }, ` at ${STAGE_LABELS[a.start]}${a.autoRun ? ', run to merge' : ''}`], hint: same ? 'Action' : said, action: a },
          { id: `${id}-edit`, label: ['Edit ', { session: a.name }, ' in New session…'], hint: 'Change it before it starts', action: a, edit: true },
        ];
      }
      case 'link':
        return [{ id, label: [`Work on ${a.ref}`, ...(a.projectId ? [' in ', { project: projectName(a.projectId) }] : [])], hint: 'Looks up the GitHub link', action: a }];
      case 'go': {
        if (a.view === 'session') {
          const s = session(a.sessionId);
          return [{ id, label: ['Go to ', { session: sessionName(a.sessionId) }], hint: s ? `${projectName(a.projectId)} · ${stageWords(s)}` : '', action: a }];
        }
        if (a.view === 'home') return [{ id, label: [{ project: projectName(a.projectId) }, ': project home'], hint: 'View', action: a }];
        return [{ id, label: [a.view === 'usage' ? 'Usage' : 'Settings'], hint: 'View', action: a }];
      }
      case 'move':
        return [{ id, label: ['Move ', { session: sessionName(a.sessionId) }, ` to ${STAGE_LABELS[a.stage]}`], hint: a.instructions ? preview(a.instructions, 80) : 'Action', action: a }];
      case 'openPr':
        return [{ id, label: ['Open PR for ', { session: sessionName(a.sessionId) }], hint: a.instructions ? preview(a.instructions, 80) : 'Action', action: a }];
      case 'openProject':
        return [{
          id, label: [`Open a project on ${a.repo}`, ...(a.name ? [' named ', { project: a.name }] : [])],
          hint: a.datapack === 'none' ? 'No datapack' : a.datapack === 'new' ? 'With a new datapack' : 'With that datapack', action: a,
        }];
    }
  });
}

/**
 * The steps **Do all** runs (POST /api/actions/run), when AgentK's answer is the steps of one
 * instruction (`together`) and two or more of them do something: every action but `go`, which only
 * navigates. Empty when there's no Do all to offer.
 */
export function doAllActions(actions: AskAgentkAction[], together: boolean): AskAgentkAction[] {
  const steps = actions.filter((a) => a.type !== 'go');
  return together && steps.length > 1 ? steps : [];
}

/** What a found session's palette item does: go to it, open its PR (a closed one's), or nothing (closed, with no PR). */
export type FindAction = { go: { projectId: string; sessionId: string } } | { open: string } | null;

export interface FindEntry {
  id: string;
  label: LabelPart[];
  hint: string;
  action: FindAction;
}

const FIELD_TEXT: Record<SessionMatch['field'], string> = { name: 'Name', pr: 'PR', task: 'Task', branch: 'Branch', design: 'Design' };

/**
 * The palette items for sessions a search found: each says where it is (its project, when not the one
 * on screen), its stage or that it's closed, and where the words were, with the text around them.
 */
export function findEntries(matches: SessionMatch[], projects: ProjectWithSessions[], currentProjectId: string | null): FindEntry[] {
  return matches.map((m) => {
    const where = m.projectId !== currentProjectId ? projects.find((p) => p.id === m.projectId)?.name : undefined;
    const state = m.closed ? (m.prNumber != null ? `Closed · PR #${m.prNumber}` : 'Closed') : STAGE_LABELS[m.stage];
    const found = m.snippet ? `${FIELD_TEXT[m.field]}: ${m.snippet}` : '';
    return {
      id: `find-${m.sessionId}`,
      label: [{ session: m.name }],
      hint: [where, state, found].filter(Boolean).join(' · '),
      action: !m.closed ? { go: { projectId: m.projectId, sessionId: m.sessionId } } : m.prUrl ? { open: m.prUrl } : null,
    };
  });
}
