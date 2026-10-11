// Command palette labels and filtering. Pure, so it's unit-tested.
import type { LinkPlan } from '../shared/types';

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

/** What a pasted link's palette item does: go to a session, start one (an issue's at `start`), or nothing (it says why). */
export type LinkAction = { go: { projectId: string; sessionId: string } } | { start: 'design' | 'code' | null } | null;

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
 * nobody reviews yet, Review it; for an issue, work on it from Design or straight from Code. Each
 * names the project, and its hint says what else starting does (a new project, a clone, a share).
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
  // A closed project that opens again is named as itself, not as a new one.
  const where: LabelPart[] = plan.project
    ? [' in ', { project: plan.project.name }]
    : [np?.reopens ? ' in ' : ' in a new project ', { project: np?.name ?? plan.repo }];
  const clones = plan.project ? !plan.project.clone : !np?.repoPath;
  const notes: string[] = [];
  if (plan.project) notes.push(plan.project.why === 'datapack' ? 'Names its datapack' : 'Same repo');
  if (np) notes.push(np.reopens ? 'Opens the project again' : 'Opens a new project');
  if (clones) notes.push(`Clones ${plan.repo}`);
  if (np?.datapack === 'create') notes.push('New datapack');
  else if (np?.datapack?.invite) notes.push(`Accepts the share of ${np.datapack.name}`);
  const hint = (first: string[]) => [...first, ...notes].join(' · ');
  if (pr) {
    out.push({ id: 'link-review', label: [`Review #${plan.number} ${plan.title}`, ...where], hint: hint([]), action: { start: null }, clones });
    return out;
  }
  const closed = plan.state !== 'open' ? ['Closed issue'] : [];
  out.push(
    { id: 'link-design', label: [`Work on issue #${plan.number} ${plan.title}`, ...where], hint: hint([...closed, 'Starts at Design']), action: { start: 'design' }, clones },
    { id: 'link-code', label: [`Work on issue #${plan.number}`, ...where, ', straight to Code'], hint: hint([...closed, 'Starts at Code']), action: { start: 'code' }, clones },
  );
  return out;
}
