import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { AppState, AuthStatus, DatapackInvite, FailedStart, Project, ProjectWithSessions, Session } from '../../shared/types';
import { api, ApiError, errorMessage } from '../api';
import { approveDesign, settleApprovals } from '../design-answers';
import { useEvents, type InviteEvent, type NotifyEvent } from '../hooks/useEvents';
import { useIsMobile } from '../hooks/useMedia';
import { useReload } from '../hooks/useReload';
import { navigate, useRoute } from '../router';
import { notifyLeader } from '../notifyLeader';
import { pruneTerminals } from '../terminals';
import { useTheme } from '../theme';
import {
  lastOpenSession,
  lastSessionClosed,
  liveSessions,
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
import { RenameSessionDialog } from './RenameSessionDialog';
import { NewSessionDialog } from './NewSessionDialog';
import { DesignPanel } from './DesignPanel';
import { ProjectHomeView } from './ProjectHome';
import { ProjectView } from './ProjectView';
import { Rail } from './Rail';
import { SettingsDialog, type SettingsTab } from './SettingsDialog';
import { TopBar } from './TopBar';
import { UsageView } from './UsageView';
import { MekoCallsView } from './MekoCallsView';

type Modal =
  | { kind: 'none' }
  /** `invite`: a share to open as the project (chosen as its datapack). */
  | { kind: 'new-project'; invite?: DatapackInvite }
  /**
   * `fresh`: the project was just opened, so a Review a PR is offered first when it has PRs to review.
   * `retry`: a failed start to try again, filling the dialog in.
   */
  | { kind: 'new-session'; projectId: string; fresh?: boolean; retry?: FailedStart }
  /** The project on screen lost its last open session: offer to close it. */
  | { kind: 'close-project'; projectId: string }
  | { kind: 'settings'; tab?: SettingsTab }
  | { kind: 'palette' }
  /** Move a session to another stage, starting from `preset` (a stage clicked in the stage bar). */
  | { kind: 'move'; sessionId: string; preset?: MovePreset }
  | { kind: 'rename'; sessionId: string };

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
  const options = { body: n.body, icon: '/logo.png', tag };
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

export function App({ auth, onLoggedOut }: { auth: AuthStatus; onLoggedOut: () => void }) {
  const route = useRoute();
  const [theme, cycleTheme] = useTheme();
  const [modal, setModal] = useState<Modal>({ kind: 'none' });
  const [toast, setToast] = useState<string | null>(null);
  const mobile = useIsMobile();
  const [drawer, setDrawer] = useState(false);
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

  // Ask for notification permission on the first click anywhere.
  useEffect(() => {
    if (!canNotify || Notification.permission !== 'default') return;
    const ask = () => {
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
    for (const p of state.projects) for (const s of liveSessions(p)) ids.add(s.id);
    pruneTerminals(ids);
    settleApprovals(state.projects.flatMap((p) => p.sessions));
  }, [state]);

  const currentProject =
    route.view === 'root' || route.view === 'usage' ? null : (projects.find((p) => p.id === route.projectId) ?? null);
  const live = useMemo(
    () => (currentProject ? liveSessions(currentProject).sort((a, b) => a.createdAt - b.createdAt) : []),
    [currentProject],
  );
  const currentSession =
    route.view === 'project' && currentProject
      ? (live.find((s) => s.id === route.sessionId) ?? null)
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
    if (!state || route.view === 'usage') return;
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
        const idx = Math.max(0, live.findIndex((s) => s.id === currentSession?.id));
        const next = live[(idx + (e.shiftKey ? -1 : 1) + live.length) % live.length];
        navigate({ view: 'project', projectId: currentProject.id, sessionId: next.id });
      }
    };
    window.addEventListener('keydown', onKey, true);
    return () => window.removeEventListener('keydown', onKey, true);
  }, [projects, goProject, currentProject, currentSession, live, modalKind]);

  const showToast = useCallback((msg: string) => {
    setToast(msg);
    window.setTimeout(() => setToast((t) => (t === msg ? null : t)), 5000);
  }, []);

  const logout = async () => {
    try {
      await api.logout();
    } catch {
      /* ignore */
    }
    onLoggedOut();
  };

  // One PATCH: the server moves the project to that index and renumbers the rest.
  const moveProject = async (id: string, index: number) => {
    try {
      await api.updateProject(id, { position: index });
    } catch (e) {
      showToast(errorMessage(e));
    }
  };

  // A New session dialog whose project was closed meanwhile closes. One for a project just opened
  // has it in `projects` at once (withOpened), so it never waits on the state push.
  const newSessionProject = modal.kind === 'new-session' ? projects.find((p) => p.id === modal.projectId) : undefined;
  // Gone once the session is archived (or its project deleted) under the dialog.
  const moveSession =
    modal.kind === 'move' ? projects.flatMap((p) => p.sessions).find((s) => s.id === modal.sessionId && s.desired === 'running') : undefined;
  useEffect(() => {
    if (state && modalKind === 'move' && !moveSession) setModal({ kind: 'none' });
  }, [state, modalKind, moveSession]);
  const renameSession =
    modal.kind === 'rename' ? projects.flatMap((p) => p.sessions).find((s) => s.id === modal.sessionId && s.desired === 'running') : undefined;
  useEffect(() => {
    if (state && modalKind === 'rename' && !renameSession) setModal({ kind: 'none' });
  }, [state, modalKind, renameSession]);
  useEffect(() => {
    if (state && modalKind === 'new-session' && !newSessionProject) setModal({ kind: 'none' });
  }, [state, modalKind, newSessionProject]);
  // The close offer closes once its project is gone or has an open session again (say, from another tab).
  const closeProject = modal.kind === 'close-project' ? projects.find((p) => p.id === modal.projectId) : undefined;
  const closeProjectStale = !closeProject || liveSessions(closeProject).length > 0;
  useEffect(() => {
    if (state && modalKind === 'close-project' && closeProjectStale) setModal({ kind: 'none' });
  }, [state, modalKind, closeProjectStale]);

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
          page
          session={s}
          onApprove={() => approveDesign(s.id).catch((e) => showToast(errorMessage(e)))}
        />
        {toast && (
          <div className="toast" role="status" onClick={() => setToast(null)}>
            {toast}
          </div>
        )}
      </>
    );
  }

  return (
    <div className="app">
      {mobile && drawer && <div className="rail-scrim" onClick={closeDrawer} aria-hidden="true" />}
      <Rail
        projects={projects}
        currentId={currentProject?.id ?? null}
        usageOpen={route.view === 'usage'}
        onUsage={() => {
          navigate({ view: 'usage' }, { replace: isDrawerEntry() });
          closeDrawer();
        }}
        username={state.username || auth.username || ''}
        serverName={state.serverName ?? ''}
        serverHost={state.serverHost ?? ''}
        version={state.version}
        limits={state.limits ?? null}
        theme={theme}
        onCycleTheme={cycleTheme}
        onSelect={(p) => {
          goProject(p);
          closeDrawer();
        }}
        onNewProject={fromDrawer(() => setModal({ kind: 'new-project' }))}
        onSettings={fromDrawer(() => setModal({ kind: 'settings' }))}
        onPalette={fromDrawer(() => setModal({ kind: 'palette' }))}
        onLogout={auth.localAccess ? undefined : fromDrawer(logout)}
        onReorder={moveProject}
        open={drawer}
      />
      <div className="main">
        <Banners
          state={state}
          conn={conn}
          stale={stale}
          onError={showToast}
          onOpenInvite={(invite) => setModal({ kind: 'new-project', invite })}
          onRetryStart={(retry) => setModal({ kind: 'new-session', projectId: retry.projectId, retry })}
          onMekoSettings={() => setModal({ kind: 'settings', tab: 'meko' })}
        />
        {route.view === 'usage' ? (
          <UsageView onMenu={openDrawer} mekoConfigured={state.meko.configured} />
        ) : currentProject && route.view === 'meko' ? (
          <MekoCallsView
            key={currentProject.id}
            project={currentProject}
            sessionId={route.sessionId}
            onMenu={openDrawer}
            onError={showToast}
          />
        ) : currentProject && route.view === 'home' ? (
          <ProjectHomeView
            key={currentProject.id}
            project={currentProject}
            order={{ index: projects.indexOf(currentProject), count: projects.length }}
            onMove={(index) => moveProject(currentProject.id, index)}
            mekoWebUrl={state.meko.webUrl}
            mekoConfigured={state.meko.configured}
            ghLogin={state.ghLogin ?? null}
            onMenu={openDrawer}
            onBack={() => goProject(currentProject)}
            onError={showToast}
          />
        ) : currentProject ? (
          <ProjectView
            project={currentProject}
            sessions={live}
            session={currentSession}
            mekoWebUrl={state.meko.webUrl}
            meko={state.meko}
            onNewSession={() => setModal({ kind: 'new-session', projectId: currentProject.id })}
            onMove={(preset) => currentSession && setModal({ kind: 'move', sessionId: currentSession.id, preset })}
            onRename={(sessionId) => setModal({ kind: 'rename', sessionId })}
            onMenu={openDrawer}
            onError={showToast}
          />
        ) : (
          <>
            {mobile && <TopBar onMenu={openDrawer} title="AgentK Local" />}
            <div className="empty-main">
              <img src="/logo.png" alt="" width={96} height={96} />
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
            setModal({ kind: 'new-session', projectId: p.id, fresh: true });
            navigate({ view: 'project', projectId: p.id, sessionId: null });
          }}
        />
      )}
      {newSessionProject && (
        <NewSessionDialog
          project={newSessionProject}
          ghLogin={state.ghLogin ?? null}
          fresh={modal.kind === 'new-session' && !!modal.fresh}
          initial={modal.kind === 'new-session' ? modal.retry?.request : undefined}
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
      {renameSession && <RenameSessionDialog key={renameSession.id} session={renameSession} onClose={() => setModal({ kind: 'none' })} />}
      {modal.kind === 'settings' && (
        <SettingsDialog auth={auth} update={state.update} meko={state.meko} initialTab={modal.tab} onClose={() => setModal({ kind: 'none' })} />
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
          onRenameSession={() => currentSession && setModal({ kind: 'rename', sessionId: currentSession.id })}
          onSettings={() => setModal({ kind: 'settings' })}
        />
      )}
      {toast && (
        <div className="toast" role="status" onClick={() => setToast(null)}>
          {toast}
        </div>
      )}
    </div>
  );
}
