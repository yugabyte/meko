import { useCallback, useEffect, useMemo, useRef, useState, type CSSProperties } from 'react';
import type { AppState, CreateSessionRequest, DatapackInvite, FailedStart, Project, ProjectWithSessions, Session, WorkItem } from '../../shared/types';
import type { HarnessId } from '../../shared/harnesses';
import { api, ApiError, errorMessage } from '../api';
import { appUrl } from '../base';
import { approveDesign, settleApprovals, settleRevisions } from '../design-answers';
import { useEvents, type InviteEvent, type NotifyEvent } from '../hooks/useEvents';
import { useIsMobile } from '../hooks/useMedia';
import { useReload } from '../hooks/useReload';
import { installPrompting } from '../installapp';
import { navigate, useRoute } from '../router';
import { notifyLeader } from '../notifyLeader';
import { railHiddenSaved, railWidthSaved, saveRailHidden, saveRailWidth, sessionTabsShown, useRailSessions } from '../rail';
import type { MekoAuth } from '../signin';
import { harnessLogosToggle } from '../toggle';
import { pruneTerminals } from '../terminals';
import { useTheme } from '../theme';
import {
  cx,
  harnessLogos,
  lastOpenSession,
  lastSessionClosed,
  liveSessions,
  liveWithReviews,
  openSessions,
  modKey,
  withCreated,
  withOpened,
  rememberOpenSession,
  sessionToOpen,
  type LiveCount,
} from '../util';
import { Banners } from './Banners';
import { CloseProjectDialog } from './CloseProjectDialog';
import { CommandPalette } from './CommandPalette';
import { Busy } from './common';
import { NewProjectDialog } from './NewProjectDialog';
import { MoveSessionDialog, type MovePreset } from './MoveSessionDialog';
import { OpenPrDialog } from './OpenPrDialog';
import { RenameSessionDialog } from './RenameSessionDialog';
import { NewSessionDialog } from './NewSessionDialog';
import { DesignPanel } from './DesignPanel';
import { ProjectHomeView } from './ProjectHome';
import { ProjectView } from './ProjectView';
import { PaneIcon, Rail } from './Rail';
import { SettingsDialog, type SettingsTab } from './SettingsDialog';
import { TopBar } from './TopBar';
import { UsageView } from './UsageView';
import { MekoCallsView } from './MekoCallsView';
import { MekoGapsView } from './MekoGapsView';
import { WhatsNewDialog, whatsNewBack, type WhatsNewOpener } from './WhatsNewDialog';
import { LinkWorkItemDialog } from './LinkWorkItemDialog';
import { WorkItemDialog } from './WorkItemDialog';
import { WorkQueueView } from './WorkQueueView';
import { itemProject, itemStartRequest, queueSummary } from '../workqueue';

type Modal =
  | { kind: 'none' }
  /** `invite`: a share to open as the project (chosen as its datapack). */
  | { kind: 'new-project'; invite?: DatapackInvite }
  /**
   * `fresh`: the project was just opened, so a Review a PR is offered first when it has PRs to review.
   * `learn`: start on Learn the project (the project was just opened on an empty datapack, or its home offered it).
   * `retry`: a failed start to try again, filling the dialog in. `initial`: a task Ask AgentK proposed, to change before it starts.
   */
  | { kind: 'new-session'; projectId: string; fresh?: boolean; learn?: boolean; retry?: FailedStart; initial?: CreateSessionRequest; pickProject?: boolean }
  /** Add a work item, or edit `item`. */
  | { kind: 'work-item'; item?: WorkItem }
  /** Link a session to the item `itemId`, or the session `sessionId` to an item. */
  | { kind: 'link-work'; itemId?: string; sessionId?: string }
  /** The project on screen lost its last open session: offer to close it. */
  | { kind: 'close-project'; projectId: string }
  | { kind: 'settings'; tab?: SettingsTab; agent?: HarnessId }
  | { kind: 'palette' }
  /** Move a session to another stage, starting from `preset` (a stage clicked in the stage bar). */
  | { kind: 'move'; sessionId: string; preset?: MovePreset }
  /** Open PR with instructions for AgentK. */
  | { kind: 'open-pr'; sessionId: string }
  | { kind: 'rename'; sessionId: string }
  /** What's new from one version to another, and what opened it (for Back: `whatsNewBack`). */
  | { kind: 'whats-new'; from: string; to: string; opener: WhatsNewOpener };

const isDrawerEntry = () => !!(history.state as { akDrawer?: boolean } | null)?.akDrawer;

// Notifications go through the service worker when it's running: its notifications stay clickable
// after this page reloads (as it does after an update) or closes, and a click switches to the tab
// already showing the session (see sw.js). Without one, the page shows them itself (`new Notification`).
const canNotify = typeof Notification !== 'undefined';

/**
 * Shows a notification. `tag` replaces an earlier one with the same tag; `data` is what the
 * service worker gets when it's clicked (see sw.js), and `onClick` runs for a page notification.
 */
async function showNotification(
  n: { title: string; body: string },
  tag: string,
  data: Record<string, string>,
  onClick: () => void,
): Promise<void> {
  const options = { body: n.body, icon: appUrl('/logo.png'), tag };
  const reg = await navigator.serviceWorker?.getRegistration().catch(() => undefined);
  if (reg?.active) {
    try {
      await reg.showNotification(n.title, { ...options, data });
      return;
    } catch {
      // Fall back to the page's own notification.
    }
  }
  const note = new Notification(n.title, options);
  note.onclick = () => {
    onClick();
    note.close();
  };
}

if (typeof navigator !== 'undefined') {
  navigator.serviceWorker?.addEventListener('message', (ev: MessageEvent<{ type?: string; projectId?: string; sessionId?: string }>) => {
    if (ev.data?.type === 'ak-open' && ev.data.projectId && ev.data.sessionId) {
      navigate({ view: 'project', projectId: ev.data.projectId, sessionId: ev.data.sessionId });
    }
  });
}

function sortedProjects(projects: ProjectWithSessions[]): ProjectWithSessions[] {
  return [...projects].sort((a, b) => a.position - b.position || a.createdAt - b.createdAt);
}

// Only the AgentK page used last shows notifications, so clicking one lands in the window in use.
const notifier = notifyLeader(typeof navigator !== 'undefined' ? navigator.locks : undefined);
if (typeof window !== 'undefined') {
  window.addEventListener('focus', () => notifier.claim());
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) notifier.claim();
  });
  if (document.hasFocus()) notifier.claim();
}

/** `mekoAuth`: what Meko, which signs people in to AgentK, says about this browser (null without Meko). */
export function App({ mekoAuth }: { mekoAuth: MekoAuth | null }) {
  const route = useRoute();
  const [theme, cycleTheme] = useTheme();
  const [modal, setModal] = useState<Modal>({ kind: 'none' });
  const [toast, setToast] = useState<string | null>(null);
  // The work item the queue scrolls to and opens (picked in the command palette).
  const [queueFocus, setQueueFocus] = useState<string | null>(null);
  const mobile = useIsMobile();
  const [drawer, setDrawer] = useState(false);
  const [railWidth, setRailWidth] = useState(railWidthSaved);
  useEffect(() => saveRailWidth(railWidth), [railWidth]);
  const railSessions = useRailSessions();
  // The wide layout's projects pane, hidden or shown by its buttons; the phone's drawer is `drawer`.
  const [railHidden, setRailHidden] = useState(railHiddenSaved);
  const toggleRail = (hidden: boolean) => {
    setRailHidden(hidden);
    saveRailHidden(hidden);
  };
  // A project just opened, as Open project returned it: shown (withOpened) until a state push brings
  // it, since the push can come after the reply, or late on a slow or reconnecting events socket.
  const [opened, setOpened] = useState<Project | null>(null);
  // A session just created, as New session returned it: shown (withCreated) until a state push
  // brings it, since the reply comes first. `seen` is the last state looked at for it; two newer
  // ones without it mean it's gone (a failed start, say).
  const [created, setCreated] = useState<{ session: Session; seen: AppState | null; misses: number } | null>(null);
  const focusRef = useRef<{ projectId: string | null; sessionId: string | null }>({ projectId: null, sessionId: null });

  const onNotify = useCallback((n: NotifyEvent) => {
    if (!canNotify || Notification.permission !== 'granted' || !notifier.leader) return;
    const focused = focusRef.current;
    if (!document.hidden && focused.sessionId === n.sessionId) return;
    const project = stateRef.current?.projects.find((p) => p.id === n.projectId);
    if (project && !project.settings.notify) return;
    void showNotification(n, `ak-${n.sessionId}`, { projectId: n.projectId, sessionId: n.sessionId }, () => {
      window.focus();
      navigate({ view: 'project', projectId: n.projectId, sessionId: n.sessionId });
    }).catch(() => undefined);
  }, []);

  /** Opens the Open project dialog for a share, found by its datapack in the latest state. */
  const openInvite = useCallback((datapackId: string) => {
    const invite = stateRef.current?.invites.find((i) => i.datapackId === datapackId);
    setModal({ kind: 'new-project', invite });
  }, []);

  const onInvite = useCallback(
    (n: InviteEvent) => {
      if (!canNotify || Notification.permission !== 'granted' || !notifier.leader) return;
      void showNotification(n, `ak-invite-${n.datapackId}`, { datapackId: n.datapackId }, () => {
        window.focus();
        openInvite(n.datapackId);
      }).catch(() => undefined);
    },
    [openInvite],
  );

  const { state, conn } = useEvents(onNotify, onInvite);
  const stale = useReload(state?.version);
  const stateRef = useRef<AppState | null>(null);
  stateRef.current = state;

  // A share's notification clicked through the service worker: open it here.
  useEffect(() => {
    const sw = navigator.serviceWorker;
    if (!sw) return;
    const onMessage = (ev: MessageEvent<{ type?: string; datapackId?: string }>) => {
      if (ev.data?.type === 'ak-invite' && ev.data.datapackId) openInvite(ev.data.datapackId);
    };
    sw.addEventListener('message', onMessage);
    return () => sw.removeEventListener('message', onMessage);
  }, [openInvite]);

  const projects = useMemo(
    () => sortedProjects(withCreated(withOpened(state?.projects ?? [], opened), created?.session ?? null)),
    [state, opened, created],
  );
  // Logos beside session names in the tabs and the rail, while open sessions run on more than one
  // harness and the setting (Settings › Harness) is on.
  const twoAgents = useMemo(() => harnessLogos(projects), [projects]);
  const logos = harnessLogosToggle.use() && twoAgents;
  useEffect(() => {
    if (!created || !state || state === created.seen) return;
    const inState = state.projects.some((p) => p.sessions.some((s) => s.id === created.session.id));
    setCreated(inState || created.misses >= 1 ? null : { ...created, seen: state, misses: created.misses + 1 });
  }, [state, created]);
  // Once a push has it, the state's copy takes over. A push without it may be an older snapshot or
  // mean it was closed meanwhile: the server knows, and a 404 drops it.
  const openedId = opened?.id ?? null;
  const openedInState = !!openedId && !!state?.projects.some((p) => p.id === openedId);
  useEffect(() => {
    if (!openedId || !state) return;
    if (openedInState) {
      setOpened(null);
      return;
    }
    let live = true;
    api.project(openedId).catch((e: unknown) => {
      if (live && e instanceof ApiError && e.status === 404) setOpened((o) => (o?.id === openedId ? null : o));
    });
    return () => {
      live = false;
    };
  }, [state, openedId, openedInState]);

  // Ask for notification permission on the first click anywhere, except one that just opened the
  // browser's install dialog (the next click asks), so a tap never brings up two prompts.
  useEffect(() => {
    if (!canNotify || Notification.permission !== 'default') return;
    const ask = () => {
      if (installPrompting()) return;
      Notification.requestPermission().catch(() => undefined);
      window.removeEventListener('click', ask);
    };
    window.addEventListener('click', ask);
    return () => window.removeEventListener('click', ask);
  }, []);

  // Dispose terminals of sessions that are gone or archived.
  useEffect(() => {
    if (!state) return;
    const ids = new Set<string>();
    for (const p of state.projects) for (const s of liveWithReviews(p)) ids.add(s.id);
    pruneTerminals(ids);
    const sessions = state.projects.flatMap((p) => p.sessions);
    settleApprovals(sessions);
    settleRevisions(sessions);
  }, [state]);

  const currentProject =
    route.view === 'root' || route.view === 'usage' || route.view === 'queue' ? null : (projects.find((p) => p.id === route.projectId) ?? null);
  const live = useMemo(
    () => (currentProject ? openSessions(currentProject) : []),
    [currentProject],
  );
  // A task's code-review session has no tab of its own, but opens like any other (its part of the task's tab).
  const currentSession =
    route.view === 'project' && currentProject
      ? (liveWithReviews(currentProject).find((s) => s.id === route.sessionId) ?? null)
      : null;

  focusRef.current = { projectId: currentProject?.id ?? null, sessionId: currentSession?.id ?? null };

  useEffect(() => {
    if (currentProject && currentSession) rememberOpenSession(currentProject.id, currentSession.id);
  }, [currentProject, currentSession]);

  // When the project on screen loses its last open session, offer to close it. Only on the
  // project's view, with no other dialog open, and in a visible page (a background tab stays quiet).
  // Not when that session failed to start: its banner offers Try again instead.
  const liveRef = useRef<LiveCount>({ projectId: null, live: 0 });
  const liveIdsRef = useRef<string[]>([]);
  const currentId = currentProject?.id ?? null;
  const liveCount = live.length;
  const liveIds = live.map((s) => s.id).join('\n');
  const failedIds = (state?.failedStarts ?? []).map((f) => f.id).join('\n');
  const modalKind = modal.kind;
  useEffect(() => {
    const prev = liveRef.current;
    const prevIds = liveIdsRef.current;
    const next = { projectId: currentId, live: liveCount };
    liveRef.current = next;
    liveIdsRef.current = liveIds ? liveIds.split('\n') : [];
    const failed = new Set(failedIds.split('\n'));
    const failedStart = prevIds.some((id) => failed.has(id));
    if (currentId && lastSessionClosed(prev, next) && route.view === 'project' && modalKind === 'none' && !failedStart && !document.hidden) {
      setModal({ kind: 'close-project', projectId: currentId });
    }
  }, [currentId, liveCount, liveIds, failedIds, route.view, modalKind]);

  // Normalize the route once state is known: pick a project/session when none is chosen
  // or the chosen one no longer exists.
  useEffect(() => {
    if (!state || route.view === 'usage' || route.view === 'queue') return;
    if (route.view === 'root' || !currentProject) {
      const first = projects[0];
      if (first) {
        const s = sessionToOpen(liveSessions(first), lastOpenSession(first.id));
        navigate({ view: 'project', projectId: first.id, sessionId: s?.id ?? null }, { replace: true });
      } else if (route.view !== 'root') {
        navigate({ view: 'root' }, { replace: true });
      }
      return;
    }
    if (route.view === 'project' && !currentSession && live.length > 0) {
      const s = sessionToOpen(live, lastOpenSession(currentProject.id));
      if (s) navigate({ view: 'project', projectId: currentProject.id, sessionId: s.id }, { replace: true });
    }
  }, [state, route, currentProject, currentSession, live, projects]);

  const goProject = useCallback((p: ProjectWithSessions) => {
    // Reopen the session last open there, else the one with the most recent activity.
    const s = sessionToOpen(liveSessions(p), lastOpenSession(p.id));
    // From the drawer, replace its history entry so Back doesn't reopen it.
    navigate({ view: 'project', projectId: p.id, sessionId: s?.id ?? null }, { replace: isDrawerEntry() });
  }, []);

  // A task's magnifier (its tab, its rail row) asks for the task on screen with its local review's
  // findings open, and the rail's task part, while they're open, for them closed: a request
  // ProjectView takes once. `findingsOpenFor` is the task whose findings ProjectView shows.
  const [findingsRequest, setFindingsRequest] = useState<{ id: string; open: boolean } | null>(null);
  const [findingsOpenFor, setFindingsOpenFor] = useState<string | null>(null);
  const openFindings = (s: Session) => {
    navigate({ view: 'project', projectId: s.projectId, sessionId: s.id }, { replace: isDrawerEntry() });
    setFindingsRequest({ id: s.id, open: true });
  };
  const findingsTaken = useCallback(() => setFindingsRequest(null), []);

  // The phone layout's projects drawer gets its own history entry, so the Back gesture closes it.
  const openDrawer = () => {
    if (!isDrawerEntry()) history.pushState({ akDrawer: true }, '', location.href);
    setDrawer(true);
  };
  const closeDrawer = useCallback(() => {
    setDrawer(false);
    if (isDrawerEntry()) history.back();
  }, []);
  useEffect(() => {
    const pop = () => setDrawer(isDrawerEntry());
    window.addEventListener('popstate', pop);
    return () => window.removeEventListener('popstate', pop);
  }, []);
  useEffect(() => {
    if (!mobile && drawer) closeDrawer();
  }, [mobile, drawer, closeDrawer]);
  useEffect(() => {
    if (!drawer) return;
    const onKey = (e: KeyboardEvent) => e.key === 'Escape' && closeDrawer();
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [drawer, closeDrawer]);
  /** Runs a drawer action with the drawer closed. */
  const fromDrawer = (f: () => void) => () => {
    closeDrawer();
    f();
  };

  // Global shortcuts; none while a dialog is open, except ⌘K to close the palette.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (modalKind !== 'none' && modalKind !== 'palette') return;
      if (modKey(e) && (e.key === 'k' || e.key === 'K')) {
        e.preventDefault();
        setModal((m) => (m.kind === 'palette' ? { kind: 'none' } : { kind: 'palette' }));
        return;
      }
      if (modKey(e) && /^[1-9]$/.test(e.key) && !e.altKey && !e.shiftKey) {
        const p = projects[Number(e.key) - 1];
        if (p) {
          e.preventDefault();
          goProject(p);
        }
        return;
      }
      if (e.ctrlKey && e.key === 'Tab' && currentProject && live.length > 0) {
        e.preventDefault();
        // From a task's code-review session, its task's place.
        const onScreen = currentSession?.parentId ?? currentSession?.id;
        const idx = Math.max(0, live.findIndex((s) => s.id === onScreen));
        const next = live[(idx + (e.shiftKey ? -1 : 1) + live.length) % live.length];
        navigate({ view: 'project', projectId: currentProject.id, sessionId: next.id });
      }
    };
    window.addEventListener('keydown', onKey, true);
    return () => window.removeEventListener('keydown', onKey, true);
  }, [projects, goProject, currentProject, currentSession, live, modalKind]);

  // A toast goes after 5 s, but not while the pointer is on it (say, to select its text).
  const toastTimer = useRef<number | undefined>(undefined);
  const hideToastLater = useCallback(() => {
    window.clearTimeout(toastTimer.current);
    toastTimer.current = window.setTimeout(() => setToast(null), 5000);
  }, []);
  const showToast = useCallback((msg: string) => {
    setToast(msg);
    hideToastLater();
  }, [hideToastLater]);

  // One PATCH: the server moves the project to that index and renumbers the rest.
  const moveProject = async (id: string, index: number) => {
    try {
      await api.updateProject(id, { position: index });
    } catch (e) {
      showToast(errorMessage(e));
    }
  };

  const sessionToTop = async (id: string) => {
    try {
      await api.updateSession(id, { toTop: true });
    } catch (e) {
      showToast(errorMessage(e));
    }
  };

  // A New session dialog whose project was closed meanwhile closes. One for a project just opened
  // has it in `projects` at once (withOpened), so it never waits on the state push.
  const newSessionProject = modal.kind === 'new-session' ? projects.find((p) => p.id === modal.projectId) : undefined;
  const workQueue = useMemo(() => state?.workQueue ?? [], [state]);
  const queue = useMemo(() => queueSummary(workQueue, projects), [workQueue, projects]);
  /** Start session for a work item: New session filled in from it, in the project it fits best, which can be changed there. */
  const startForItem = (item: WorkItem) => {
    const projectId = itemProject(item, projects, currentProject?.id ?? null);
    if (!projectId) {
      showToast('Open a project first: a session for a work item starts in one.');
      return;
    }
    setModal({ kind: 'new-session', projectId, initial: itemStartRequest(item), pickProject: true });
  };
  const linkWorkItem = modal.kind === 'link-work' && modal.itemId ? workQueue.find((i) => i.id === modal.itemId) : undefined;
  const linkWorkSession = modal.kind === 'link-work' && modal.sessionId ? projects.flatMap((p) => p.sessions).find((s) => s.id === modal.sessionId) : undefined;
  // Gone once the session is archived (or its project deleted) under the dialog.
  const moveSession =
    modal.kind === 'move' ? projects.flatMap((p) => p.sessions).find((s) => s.id === modal.sessionId && s.desired === 'running') : undefined;
  useEffect(() => {
    if (state && modalKind === 'move' && !moveSession) setModal({ kind: 'none' });
  }, [state, modalKind, moveSession]);
  // Gone once the session leaves Code (the PR went, or it moved) or is archived.
  const openPrSession =
    modal.kind === 'open-pr' ? projects.flatMap((p) => p.sessions).find((s) => s.id === modal.sessionId && s.desired === 'running' && s.stage === 'code') : undefined;
  useEffect(() => {
    if (state && modalKind === 'open-pr' && !openPrSession) setModal({ kind: 'none' });
  }, [state, modalKind, openPrSession]);
  const renameSession =
    modal.kind === 'rename' ? projects.flatMap((p) => p.sessions).find((s) => s.id === modal.sessionId && s.desired === 'running') : undefined;
  useEffect(() => {
    if (state && modalKind === 'rename' && !renameSession) setModal({ kind: 'none' });
  }, [state, modalKind, renameSession]);
  const whatsNewTab = modal.kind === 'whats-new' ? whatsNewBack(modal.opener) : undefined;
  useEffect(() => {
    if (state && modalKind === 'new-session' && !newSessionProject) setModal({ kind: 'none' });
  }, [state, modalKind, newSessionProject]);
  // The close offer closes once its project is gone or has an open session again (say, from another tab).
  const closeProject = modal.kind === 'close-project' ? projects.find((p) => p.id === modal.projectId) : undefined;
  const closeProjectStale = !closeProject || liveSessions(closeProject).length > 0;
  useEffect(() => {
    if (state && modalKind === 'close-project' && closeProjectStale) setModal({ kind: 'none' });
  }, [state, modalKind, closeProjectStale]);

  // A click closes it, unless it selected some of its text (to copy it).
  const toastBox = toast && (
    <div
      className="toast"
      role="status"
      onMouseEnter={() => window.clearTimeout(toastTimer.current)}
      onMouseLeave={hideToastLater}
      onClick={() => {
        if (!window.getSelection()?.toString()) setToast(null);
      }}
    >
      {toast}
    </div>
  );

  if (!state) {
    return <div className="boot">
        <Busy label={conn === 'closed' ? 'Reconnecting to the server' : 'Loading your projects'} />
      </div>;
  }

  // A design doc opened in its own tab: just the doc, full page.
  if (route.view === 'design') {
    const s = currentProject?.sessions.find((x) => x.id === route.sessionId);
    if (!s) return <div className="boot">That session no longer exists.</div>;
    return (
      <>
        <DesignPanel
          key={s.id}
          page
          session={s}
          datapacks={currentProject?.datapacks}
          onApprove={() => approveDesign(s.id).catch((e) => showToast(errorMessage(e)))}
          onAutoRun={(on) => api.updateSession(s.id, { autoRun: on }).then(() => undefined, (e: unknown) => showToast(errorMessage(e)))}
        />
        {toastBox}
      </>
    );
  }

  return (
    <div
      className={cx('app', !mobile && railHidden && 'rail-hidden')}
      style={railWidth ? ({ '--rail-w': `${railWidth}px` } as CSSProperties) : undefined}
    >
      {mobile && drawer && <div className="rail-scrim" onClick={closeDrawer} aria-hidden="true" />}
      {!mobile && railHidden ? (
        <nav className="rail-strip" aria-label="Projects">
          <button className="icon-btn" onClick={() => toggleRail(false)} title="Show projects" aria-label="Show projects" aria-expanded={false}>
            <PaneIcon />
          </button>
        </nav>
      ) : (
      <Rail
        projects={projects}
        currentId={currentProject?.id ?? null}
        currentSessionId={currentSession?.id ?? null}
        sessionsShown={railSessions}
        harnessLogos={logos}
        usageOpen={route.view === 'usage'}
        onUsage={() => {
          navigate({ view: 'usage' }, { replace: isDrawerEntry() });
          closeDrawer();
        }}
        queue={queue}
        queueOpen={route.view === 'queue'}
        onQueue={() => {
          setQueueFocus(null);
          navigate({ view: 'queue' }, { replace: isDrawerEntry() });
          closeDrawer();
        }}
        username={state.username}
        serverName={state.serverName ?? ''}
        serverHost={state.serverHost ?? ''}
        version={state.version}
        limits={state.limits ?? null}
        kiroPlan={state.kiroPlan ?? null}
        theme={theme}
        onCycleTheme={cycleTheme}
        onSelect={(p) => {
          goProject(p);
          closeDrawer();
        }}
        onSelectSession={(s) => {
          navigate({ view: 'project', projectId: s.projectId, sessionId: s.id }, { replace: isDrawerEntry() });
          closeDrawer();
        }}
        onOpenReview={(s) => {
          openFindings(s);
          closeDrawer();
        }}
        findingsOpenFor={findingsOpenFor}
        onCloseFindings={(s) => {
          setFindingsRequest({ id: s.id, open: false });
          closeDrawer();
        }}
        onNewSession={(projectId) => {
          closeDrawer();
          setModal({ kind: 'new-session', projectId });
        }}
        onRenameSession={(sessionId) => setModal({ kind: 'rename', sessionId })}
        onSessionToTop={(sessionId) => void sessionToTop(sessionId)}
        onNewProject={fromDrawer(() => setModal({ kind: 'new-project' }))}
        onSettings={fromDrawer(() => setModal({ kind: 'settings' }))}
        onPalette={fromDrawer(() => setModal({ kind: 'palette' }))}
        onReorder={moveProject}
        onResize={mobile ? undefined : setRailWidth}
        onHide={mobile ? undefined : () => toggleRail(true)}
        open={drawer}
      />
      )}
      <div className="main">
        <Banners
          state={state}
          conn={conn}
          stale={stale}
          onError={showToast}
          onOpenInvite={(invite) => setModal({ kind: 'new-project', invite })}
          onRetryStart={(retry) => setModal({ kind: 'new-session', projectId: retry.projectId, retry })}
          onMekoSettings={() => setModal({ kind: 'settings', tab: 'harness', agent: 'claude' })}
          onSettings={() => setModal({ kind: 'settings', tab: 'harness' })}
          onWhatsNew={(from, to) => setModal({ kind: 'whats-new', from, to, opener: 'banner' })}
        />
        {route.view === 'usage' ? (
          <UsageView onMenu={openDrawer} mekoConfigured={state.meko.configured} />
        ) : route.view === 'queue' ? (
          <WorkQueueView
            items={workQueue}
            sync={state.workSync ?? { running: false, at: null, github: null, slack: null, slackAuto: false, sources: { github: true, slack: true } }}
            projects={projects}
            focusId={queueFocus}
            onMenu={openDrawer}
            onAdd={() => setModal({ kind: 'work-item' })}
            onEdit={(item) => setModal({ kind: 'work-item', item })}
            onLinkSession={(item) => setModal({ kind: 'link-work', itemId: item.id })}
            onStartSession={startForItem}
            onError={showToast}
            onSettings={() => setModal({ kind: 'settings', tab: 'work-queue' })}
          />
        ) : currentProject && route.view === 'meko' ? (
          <MekoCallsView
            key={currentProject.id}
            project={currentProject}
            sessionId={route.sessionId}
            onMenu={openDrawer}
            onError={showToast}
          />
        ) : currentProject && route.view === 'meko-gaps' ? (
          <MekoGapsView key={currentProject.id} project={currentProject} mekoConfigured={state.meko.configured} onMenu={openDrawer} onToast={showToast} />
        ) : currentProject && route.view === 'home' ? (
          <ProjectHomeView
            key={currentProject.id}
            project={currentProject}
            order={{ index: projects.indexOf(currentProject), count: projects.length }}
            onMove={(index) => moveProject(currentProject.id, index)}
            meko={state.meko}
            mekoConfigured={state.meko.configured}
            mekoLocal={state.meko.local}
            ghLogin={state.ghLogin ?? null}
            onMenu={openDrawer}
            onBack={() => goProject(currentProject)}
            onLearn={() => setModal({ kind: 'new-session', projectId: currentProject.id, learn: true })}
            onError={showToast}
          />
        ) : currentProject ? (
          <ProjectView
            project={currentProject}
            sessions={live}
            session={currentSession}
            meko={state.meko}
            onNewSession={() => setModal({ kind: 'new-session', projectId: currentProject.id })}
            onMove={(preset) => currentSession && setModal({ kind: 'move', sessionId: currentSession.id, preset })}
            onOpenPr={() => currentSession && setModal({ kind: 'open-pr', sessionId: currentSession.id })}
            onRename={(sessionId) => setModal({ kind: 'rename', sessionId })}
            onLinkWorkItem={(sessionId) => setModal({ kind: 'link-work', sessionId })}
            onToTop={(sessionId) => void sessionToTop(sessionId)}
            onMenu={openDrawer}
            showTabs={sessionTabsShown(railSessions, railHidden, mobile)}
            harnessLogos={logos}
            findingsRequest={findingsRequest}
            onFindingsTaken={findingsTaken}
            onFindingsShown={setFindingsOpenFor}
            onOpenFindings={openFindings}
            onError={showToast}
          />
        ) : (
          <>
            {mobile && <TopBar onMenu={openDrawer} title="AgentK" />}
            <div className="empty-main">
              <img src={appUrl('/logo.png')} alt="" width={96} height={96} />
              <h2>No projects yet</h2>
              <p className="muted">
                A project is an effort to build or improve a feature, product or service, worked on in one of your git repos. Each task you start in it gets its own worktree and Claude session.
              </p>
              <button className="btn primary" onClick={() => setModal({ kind: 'new-project' })}>
                Open project
              </button>
            </div>
          </>
        )}
      </div>

      {modal.kind === 'new-project' && (
        <NewProjectDialog
          mekoConfigured={state.meko.configured}
          projects={projects}
          initialInvite={modal.invite ?? null}
          onClose={() => setModal({ kind: 'none' })}
          onCreated={(p) => {
            // Straight on to the project's first session.
            setOpened(p);
            setModal({ kind: 'new-session', projectId: p.id, fresh: true, learn: p.datapackEmpty });
            navigate({ view: 'project', projectId: p.id, sessionId: null });
          }}
        />
      )}
      {newSessionProject && (
        <NewSessionDialog
          key={modal.kind === 'new-session' && modal.pickProject ? newSessionProject.id : undefined}
          project={newSessionProject}
          projectChoice={modal.kind === 'new-session' && modal.pickProject ? {
            projects,
            onPick: (projectId, draft) => setModal({ kind: 'new-session', projectId, initial: draft, pickProject: true }),
          } : undefined}
          ghLogin={state.ghLogin ?? null}
          harnesses={state.harnesses ?? []}
          fresh={modal.kind === 'new-session' && !!modal.fresh}
          mekoOn={state.meko.configured}
          learnFirst={modal.kind === 'new-session' && !!modal.learn}
          initial={modal.kind === 'new-session' ? (modal.retry?.request ?? modal.initial) : undefined}
          onClose={() => setModal({ kind: 'none' })}
          onCreated={(s) => {
            // The failed start it retried is done with. One under the same id was cleared when the
            // session was written, and dismissing it now could hide this start's own failure.
            if (modal.kind === 'new-session' && modal.retry && modal.retry.id !== s.id) api.dismissFailedStart(modal.retry.id).catch(() => {});
            setCreated({ session: s, seen: stateRef.current, misses: 0 });
            setModal({ kind: 'none' });
            navigate({ view: 'project', projectId: s.projectId, sessionId: s.id });
          }}
        />
      )}
      {closeProject && !closeProjectStale && (
        <CloseProjectDialog
          project={closeProject}
          onClose={() => setModal({ kind: 'none' })}
          onNewSession={() => setModal({ kind: 'new-session', projectId: closeProject.id })}
          onClosed={() => {
            setModal({ kind: 'none' });
            navigate({ view: 'root' });
          }}
        />
      )}
      {moveSession && (
        <MoveSessionDialog
          key={moveSession.id}
          session={moveSession}
          preset={modal.kind === 'move' ? modal.preset : undefined}
          onClose={() => setModal({ kind: 'none' })}
        />
      )}
      {openPrSession && (
        <OpenPrDialog
          key={openPrSession.id}
          session={openPrSession}
          projectReview={projects.find((p) => p.id === openPrSession.projectId)?.settings.codeReview ?? {}}
          onClose={() => setModal({ kind: 'none' })}
        />
      )}
      {renameSession && <RenameSessionDialog key={renameSession.id} session={renameSession} onClose={() => setModal({ kind: 'none' })} />}
      {modal.kind === 'settings' && (
        <SettingsDialog mekoAuth={mekoAuth} update={state.update} meko={state.meko} harnesses={state.harnesses ?? []} twoAgents={twoAgents} serverName={state.serverName} initialTab={modal.tab} initialAgent={modal.agent} onClose={() => setModal({ kind: 'none' })} onWhatsNew={(from, to) => setModal({ kind: 'whats-new', from, to, opener: 'settings' })} />
      )}
      {modal.kind === 'whats-new' && (
        <WhatsNewDialog
          from={modal.from}
          to={modal.to}
          onClose={() => setModal({ kind: 'none' })}
          onBack={whatsNewTab && (() => setModal({ kind: 'settings', tab: whatsNewTab }))}
        />
      )}
      {modal.kind === 'work-item' && <WorkItemDialog item={modal.item} onClose={() => setModal({ kind: 'none' })} />}
      {(linkWorkItem || linkWorkSession) && (
        <LinkWorkItemDialog item={linkWorkItem} session={linkWorkSession} items={workQueue} projects={projects} onClose={() => setModal({ kind: 'none' })} />
      )}
      {modal.kind === 'palette' && (
        <CommandPalette
          projects={projects}
          currentProjectId={currentProject?.id ?? null}
          currentSession={currentSession}
          onClose={() => setModal({ kind: 'none' })}
          onNewProject={() => setModal({ kind: 'new-project' })}
          onNewSession={(projectId) => setModal({ kind: 'new-session', projectId })}
          onMoveSession={() => currentSession && setModal({ kind: 'move', sessionId: currentSession.id })}
          onOpenPr={() => currentSession && setModal({ kind: 'open-pr', sessionId: currentSession.id })}
          onRenameSession={() => currentSession && setModal({ kind: 'rename', sessionId: currentSession.id })}
          onSessionToTop={currentSession && live[0]?.id !== currentSession.id ? () => void sessionToTop(currentSession.id) : undefined}
          onSettings={() => setModal({ kind: 'settings' })}
          workItems={workQueue}
          onAddWorkItem={() => setModal({ kind: 'work-item' })}
          onShowWorkItem={(id) => {
            setQueueFocus(id);
            navigate({ view: 'queue' });
          }}
          onStartForItem={startForItem}
          onLinkWorkItem={() => currentSession && setModal({ kind: 'link-work', sessionId: currentSession.id })}
          onError={showToast}
          askAgentk={!!state.askAgentk}
          onEditSession={(projectId, initial) => setModal({ kind: 'new-session', projectId, initial })}
          mekoWebUrl={state.meko.webUrl}
          onOpenInvite={(invite) => setModal({ kind: 'new-project', invite })}
          onStarted={({ project, session }) => {
            // As after Open project and New session: both shown until a state push brings them.
            setOpened(project);
            setCreated({ session, seen: stateRef.current, misses: 0 });
            navigate({ view: 'project', projectId: session.projectId, sessionId: session.id });
          }}
        />
      )}
      {toastBox}
    </div>
  );
}
