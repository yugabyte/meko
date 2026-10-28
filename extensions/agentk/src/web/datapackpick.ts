// The Open project dialog's datapack rules: the datapacks it can offer, the projects Project name
// suggests, the Search all datapacks list, and which datapack a project gets from what's picked,
// typed and ticked (detectDatapack). Pure, so the rules are tested without the dialog.
import type { ClosedProject, DatapackChoice, DatapackInvite, MekoDatapack, Project } from '../shared/types';
import { PROJECT_DATAPACK_PREFIX, defaultProjectDatapackName, freeDatapackName, parseProjectDatapack, remoteFromDescription } from '../shared/datapacks';

/** One datapack the dialog can offer: one we can see, or an invite waiting in Meko. */
export interface DpOption {
  id: string;
  name: string;
  grant: string | null;
  owner: string | null;
  inviteId: string | null;
  remote: string | null;
  /** Projects here that already use it. */
  usedBy: string[];
  /** The closed project it was the dedicated datapack of: picking it opens that project again. */
  closed: ClosedProject | null;
  group: 'invites' | 'shared' | 'yours';
}

export const GROUP_LABEL: Record<DpOption['group'], string> = {
  invites: 'Invites for you',
  shared: 'Shared with you',
  yours: 'Yours',
};

const GROUP_ORDER: DpOption['group'][] = ['invites', 'shared', 'yours'];

export function tagFor(o: DpOption): string | null {
  if (o.inviteId) return `invite${o.grant ? ` · ${o.grant}` : ''}${o.owner ? ` · ${o.owner}` : ''}`;
  if (o.grant === 'viewer') return 'viewer · read only';
  if (o.grant && o.grant !== 'owner') return `${o.grant}${o.owner ? ` · ${o.owner}` : ''}`;
  return o.grant;
}

/** Sessions write to a project's own datapack, so one we can only read can't be picked. */
export const readOnly = (o: DpOption): boolean => o.grant === 'viewer';

/** Invites first, then datapacks shared with us, then ours; each with the ones a project here uses last. */
export function datapackOptions(
  datapacks: MekoDatapack[], invites: DatapackInvite[], projects: Project[], closedProjects: ClosedProject[],
): DpOption[] {
  const usedBy = new Map<string, string[]>();
  for (const p of projects) for (const d of p.datapacks) usedBy.set(d.id, [...(usedBy.get(d.id) ?? []), p.name]);
  const closed = new Map(closedProjects.map((c) => [c.datapackId, c]));
  const out: DpOption[] = [];
  const seen = new Set<string>();
  for (const i of invites) {
    if (!i.inviteId) continue;
    seen.add(i.datapackId);
    out.push({
      id: i.datapackId, name: i.name, grant: i.grant, owner: i.ownerName ?? i.ownerEmail, inviteId: i.inviteId,
      remote: i.repoRemote, usedBy: usedBy.get(i.datapackId) ?? [], closed: closed.get(i.datapackId) ?? null, group: 'invites',
    });
  }
  for (const d of datapacks) {
    if (seen.has(d.id)) continue;
    const mine = !d.grant || d.grant === 'owner';
    out.push({
      id: d.id, name: d.name, grant: d.grant, owner: mine ? null : (d.ownerName ?? d.ownerEmail), inviteId: null,
      remote: remoteFromDescription(d.description), usedBy: usedBy.get(d.id) ?? [], closed: closed.get(d.id) ?? null, group: mine ? 'yours' : 'shared',
    });
  }
  return out.sort(
    (a, b) =>
      GROUP_ORDER.indexOf(a.group) - GROUP_ORDER.indexOf(b.group) ||
      Number(a.usedBy.length > 0) - Number(b.usedBy.length > 0) ||
      a.name.localeCompare(b.name),
  );
}

/** The project a project datapack stands for: a closed project's own name, else the datapack's project part. */
export function projectNameOf(o: DpOption): string | null {
  return o.closed?.name ?? parseProjectDatapack(o.name)?.project ?? null;
}

/**
 * Project name's dropdown: the projects our project datapacks stand for, and every closed project
 * whatever its datapack is called; all of them for an empty name.
 */
export function suggestProjects(text: string, options: DpOption[]): DpOption[] {
  const q = text.trim().toLowerCase();
  return options.filter((o) => {
    const project = projectNameOf(o);
    return project !== null && (!q || project.toLowerCase().includes(q));
  });
}

/** Search all datapacks: every datapack, by any part of its name. */
export function searchDatapacks(text: string, options: DpOption[]): DpOption[] {
  const q = text.trim().toLowerCase();
  return options.filter((o) => !q || o.name.toLowerCase().includes(q));
}

function existing(o: DpOption): DatapackChoice {
  return { mode: 'existing', id: o.id, name: o.name, ...(o.inviteId ? { inviteId: o.inviteId } : {}) };
}

/** Which datapack the project gets, and what the Datapack section says about it. `choice` null keeps Open project off. */
export type DetectedDatapack =
  | { kind: 'no-meko' | 'none'; choice: DatapackChoice }
  /** A datapack picked from the dropdown or the search (`picked`), or the one the name and repo name (`found`). */
  | { kind: 'picked' | 'found'; option: DpOption; choice: DatapackChoice }
  /**
   * A new datapack. `taken` is the name the project would have had, when it's a datapack we can
   * only read, or one that projects here already use (`usedBy`), which is only shared when picked.
   */
  | { kind: 'new'; name: string; taken: { name: string; usedBy: string[] } | null; choice: DatapackChoice }
  /** The datapack list is still loading. */
  | { kind: 'loading'; choice: null }
  | { kind: 'no-name'; choice: null }
  /** A name but no repo yet: `name` is the new datapack's name with `<repo>` in place of the repo part. */
  | { kind: 'no-repo'; name: string; choice: null };

/**
 * A picked datapack wins; else the one named `adlc/proj/<repo>/<project>` (defaultProjectDatapackName)
 * when we can write to it and no project here uses it, else a new one by that name (the next free
 * name, among the datapacks we can see, when one has it). So a project shares another one's
 * datapack only when it's picked. No Meko, or No datapack ticked, opens the project without one.
 * `options` is null while the list loads; a list that failed to load is passed as [], and Meko
 * refuses a taken name, as it does one held by a datapack we can't see.
 */
export function detectDatapack(o: {
  mekoUsable: boolean;
  noDatapack: boolean;
  picked: DpOption | null;
  name: string;
  repoName: string;
  options: readonly DpOption[] | null;
}): DetectedDatapack {
  if (!o.mekoUsable) return { kind: 'no-meko', choice: { mode: 'none' } };
  if (o.noDatapack) return { kind: 'none', choice: { mode: 'none' } };
  if (o.picked) return { kind: 'picked', option: o.picked, choice: existing(o.picked) };
  if (!o.name.trim()) return { kind: 'no-name', choice: null };
  if (!o.repoName.trim()) {
    const project = defaultProjectDatapackName('repo', o.name).slice(`${PROJECT_DATAPACK_PREFIX}repo/`.length);
    return { kind: 'no-repo', name: `${PROJECT_DATAPACK_PREFIX}<repo>/${project}`, choice: null };
  }
  if (!o.options) return { kind: 'loading', choice: null };
  const want = defaultProjectDatapackName(o.repoName, o.name);
  const named = o.options.find((d) => d.name === want);
  if (named && !readOnly(named) && !named.usedBy.length) return { kind: 'found', option: named, choice: existing(named) };
  const name = named ? freeDatapackName(want, new Set(o.options.map((d) => d.name))) : want;
  return { kind: 'new', name, taken: named ? { name: want, usedBy: named.usedBy } : null, choice: { mode: 'create', name } };
}
