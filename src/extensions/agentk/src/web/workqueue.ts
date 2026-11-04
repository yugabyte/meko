// The work queue's helpers: an item's session, its Start session request and project, and the queue's dot. Pure, so it's unit-tested.
import type { CreateSessionRequest, ProjectWithSessions, Session, WorkItem, WorkSourceResult, WorkSync } from '../shared/types';
import { liveSessions, relativeTime, urgentDot, type DotState } from './util';

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

/** The rail's work queue line: how many items are open and new, and its sessions' most urgent dot. */
export function queueSummary(items: WorkItem[], projects: ProjectWithSessions[]): { open: number; unplaced: number; dot: DotState } {
  const sessions = items.map((i) => itemSession(i, projects)).filter((s): s is Session => !!s);
  return { open: items.length, unplaced: items.filter((i) => i.unplaced).length, dot: urgentDot(sessions) };
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
    sync.at ? `Synced ${relativeTime(sync.at, now)}` : 'Not synced since AgentK started',
    sync.github && source('GitHub', sync.github),
    sync.slack ? source('Slack', sync.slack) : sync.sources.slack && !sync.slackAuto && sync.at ? 'Slack: only when you press Sync' : null,
  ].filter(Boolean).join(' · ');
}
