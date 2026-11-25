// The work queue's helpers: an item's session, its Start session request and project, and the queue's counts. Pure, so it's unit-tested.
import type { CreateSessionRequest, ProjectWithSessions, Session, WorkItem, WorkSourceResult, WorkStoreState, WorkSync } from '../shared/types';
import { liveSessions, relativeTime } from './util';

/** The open session of an item, from the state; null when it has none or only a closed one. */
export function itemSession(item: WorkItem, projects: ProjectWithSessions[]): Session | null {
  if (!item.sessionId) return null;
  for (const p of projects) {
    const s = liveSessions(p).find((x) => x.id === item.sessionId);
    if (s) return s;
  }
  return null;
}

/** `owner/name` and number of an item's first GitHub link. */
function githubRef(item: WorkItem): { repo: string; number: number } | null {
  for (const l of item.links) {
    const m = /^gh:(.+)#(\d+)$/.exec(l.key);
    if (m) return { repo: m[1], number: Number(m[2]) };
  }
  return null;
}

/** A task's first prompt from an item: its title, its notes, and its links, one a line. */
export function itemPrompt(item: WorkItem): string {
  const links = item.links.map((l) => l.url ?? l.label);
  return [item.title, item.notes.trim(), links.length ? `Links:\n${links.join('\n')}` : ''].filter(Boolean).join('\n\n');
}

/**
 * What Start session fills New session with: a review of the PR for a GitHub review request, else a
 * task at Design named after the item, with the item's prompt. Linked to the item as it's written.
 */
export function itemStartRequest(item: WorkItem): CreateSessionRequest {
  const gh = githubRef(item);
  if (item.source === 'github' && gh) return { name: '', kind: 'review-pr', pr: gh, workItemId: item.id };
  return { name: item.title.slice(0, 100), kind: 'task', start: 'design', prompt: itemPrompt(item), workItemId: item.id };
}

/**
 * The project Start session picks first: one with a session on a PR of the item's GitHub repo, else
 * the project on screen, else the first. Null with no project open.
 */
export function itemProject(item: WorkItem, projects: ProjectWithSessions[], currentProjectId: string | null): string | null {
  const gh = githubRef(item);
  const prefix = gh ? `https://github.com/${gh.repo}/`.toLowerCase() : null;
  const onRepo = prefix ? projects.find((p) => p.sessions.some((s) => s.prUrl?.toLowerCase().startsWith(prefix))) : undefined;
  return onRepo?.id ?? projects.find((p) => p.id === currentProjectId)?.id ?? projects[0]?.id ?? null;
}

/** The rail's work queue button: how many items are open and how many are new. */
export function queueSummary(items: WorkItem[]): { open: number; unplaced: number } {
  return { open: items.length, unplaced: items.filter((i) => i.unplaced).length };
}

/** The count on the rail's work queue button: up to 99, then 99+. */
export function queueBadge(open: number): string {
  return open > 99 ? '99+' : String(open);
}

/** The queue's ids with `id` moved to `to` (an index in the list without it), for a drop. */
export function movedOrder(ids: string[], id: string, to: number): string[] {
  const rest = ids.filter((x) => x !== id);
  rest.splice(Math.max(0, Math.min(rest.length, to)), 0, id);
  return rest;
}

/** The line beside Sync: when it last ran, and what each source came to. */
export function workSyncText(sync: WorkSync, now = Date.now()): string {
  const source = (name: string, r: WorkSourceResult) => (r.ok ? `${name}: ${r.added ? `${r.added} new` : 'nothing new'}` : `${name}: ${r.error ?? 'failed'}`);
  if (!sync.sources.github && !sync.sources.slack) return 'Sync is off (Settings › Work queue)';
  return [
    sync.at ? `Synced ${relativeTime(sync.at, now)}` : 'Not synced since Kode started',
    sync.github && source('GitHub', sync.github),
    sync.slack ? source('Slack', sync.slack) : sync.sources.slack && !sync.slackAuto && sync.at ? 'Slack: only when you press Sync' : null,
  ].filter(Boolean).join(' · ');
}

/**
 * The queue page's line about where the queue is kept (Meko Cloud): in place of the list before it's set
 * up on this computer, and above it while it can't be reached or a newer Kode set it up. Empty while it's ready.
 */
export function workStoreLine(w: WorkStoreState): string {
  switch (w.status) {
    case 'not-linked':
      return "Your work queue is kept in Meko Cloud. Link Meko to Meko Cloud in Meko's settings, then set it up in Settings › Work queue.";
    case 'not-set-up':
      return 'Your work queue is kept in Meko Cloud, so every computer you run Kode on shows the same one. Set it up once, from this computer.';
    case 'can-join':
      return 'Another of your computers set up your work queue in Meko Cloud. Join it from this computer.';
    case 'setting-up':
      return w.step ?? 'Setting up your work queue in Meko Cloud';
    case 'unreachable':
      return `Can't reach your work queue in Meko Cloud: ${w.error ?? 'no answer'}. This is the list as last read, and changes are refused until it's back. If this computer's address changed, let it in from Settings › Work queue.`;
    case 'ready':
      return w.readOnly ? 'A newer Kode set up your work queue in Meko Cloud. Update Kode on this computer to change it.' : '';
  }
}
