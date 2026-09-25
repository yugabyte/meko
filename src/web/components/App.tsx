import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { AppState, AuthStatus, ProjectWithSessions } from '../../shared/types';
import { api, errorMessage } from '../api';
import { useEvents, type NotifyEvent } from '../hooks/useEvents';
import { navigate, useRoute } from '../router';
import { pruneTerminals } from '../terminals';
import { useTheme } from '../theme';
import { liveSessions, modKey } from '../util';
import { Banners } from './Banners';
import { CommandPalette } from './CommandPalette';
import { NewProjectDialog } from './NewProjectDialog';
import { NewSessionDialog } from './NewSessionDialog';
import { ProjectHomeView } from './ProjectHome';
import { ProjectView } from './ProjectView';
import { Rail } from './Rail';
import { SettingsDialog } from './SettingsDialog';

type Modal =
  | { kind: 'none' }
  | { kind: 'new-project' }
  | { kind: 'new-session'; projectId: string }
  | { kind: 'settings' }
  | { kind: 'palette' };

function sortedProjects(state: AppState | null): ProjectWithSessions[] {
  if (!state) return [];
  return [...state.projects].sort((a, b) => a.position - b.position || a.createdAt - b.createdAt);
}

export function App({ auth, onLoggedOut }: { auth: AuthStatus; onLoggedOut: () => void }) {
  const route = useRoute();
  const [theme, cycleTheme] = useTheme();
  const [modal, setModal] = useState<Modal>({ kind: 'none' });
  const [toast, setToast] = useState<string | null>(null);
  const focusRef = useRef<{ projectId: string | null; sessionId: string | null }>({ projectId: null, sessionId: null });

  const onNotify = useCallback((n: NotifyEvent) => {
    if (typeof Notification === 'undefined' || Notification.permission !== 'granted') return;
    const focused = focusRef.current;
    if (!document.hidden && focused.sessionId === n.sessionId) return;
    const project = stateRef.current?.projects.find((p) => p.id === n.projectId);
    if (project && !project.settings.notify) return;
    const note = new Notification(n.title, { body: n.body, icon: '/logo.png', tag: `ak-${n.sessionId}` });
    note.onclick = () => {
      window.focus();
      navigate({ view: 'project', projectId: n.projectId, sessionId: n.sessionId });
      note.close();
    };
  }, []);

  const { state, conn } = useEvents(onNotify);
  const stateRef = useRef<AppState | null>(null);
  stateRef.current = state;

  const projects = useMemo(() => sortedProjects(state), [state]);

  // Ask for notification permission on the first click anywhere.
  useEffect(() => {
    if (typeof Notification === 'undefined' || Notification.permission !== 'default') return;
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
  }, [state]);

  const currentProject =
    route.view === 'root' ? null : (projects.find((p) => p.id === route.projectId) ?? null);
  const live = currentProject ? liveSessions(currentProject).sort((a, b) => a.createdAt - b.createdAt) : [];
  const currentSession =
    route.view === 'project' && currentProject
      ? (live.find((s) => s.id === route.sessionId) ?? null)
      : null;

  focusRef.current = { projectId: currentProject?.id ?? null, sessionId: currentSession?.id ?? null };

  // Normalize the route once state is known: pick a project/session when none is chosen
  // or the chosen one no longer exists.
  useEffect(() => {
    if (!state) return;
    if (route.view === 'root' || !currentProject) {
      const first = projects[0];
      if (first) {
        const s = liveSessions(first).sort((a, b) => a.createdAt - b.createdAt)[0];
        navigate({ view: 'project', projectId: first.id, sessionId: s?.id ?? null }, { replace: true });
      } else if (route.view !== 'root') {
        navigate({ view: 'root' }, { replace: true });
      }
      return;
    }
    if (route.view === 'project' && !currentSession && live.length > 0) {
      navigate({ view: 'project', projectId: currentProject.id, sessionId: live[0].id }, { replace: true });
    }
  }, [state, route, currentProject, currentSession, live, projects]);

  const goProject = useCallback((p: ProjectWithSessions) => {
    // Open the session with the most recent activity.
    const s = liveSessions(p).sort(
      (a, b) => (b.lastEventAt ?? b.createdAt) - (a.lastEventAt ?? a.createdAt),
    )[0];
    navigate({ view: 'project', projectId: p.id, sessionId: s?.id ?? null });
  }, []);

  // Global shortcuts.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
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
  }, [projects, goProject, currentProject, currentSession, live]);

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

  const reorder = async (ids: string[]) => {
    try {
      await Promise.all(
        ids.map((id, i) => {
          const p = projects.find((x) => x.id === id);
          return p && p.position !== i ? api.updateProject(id, { position: i }) : null;
        }),
      );
    } catch (e) {
      showToast(errorMessage(e));
    }
  };

  if (!state) {
    return <div className="boot">{conn === 'closed' ? 'Reconnecting to the server…' : 'Loading your projects…'}</div>;
  }

  return (
    <div className="app">
      <Rail
        projects={projects}
        currentId={currentProject?.id ?? null}
        username={state.username || auth.username || ''}
        version={state.version}
        theme={theme}
        onCycleTheme={cycleTheme}
        onSelect={goProject}
        onNewProject={() => setModal({ kind: 'new-project' })}
        onSettings={() => setModal({ kind: 'settings' })}
        onLogout={logout}
        onReorder={reorder}
      />
      <div className="main">
        <Banners state={state} conn={conn} onError={showToast} />
        {currentProject && route.view === 'home' ? (
          <ProjectHomeView
            project={currentProject}
            mekoWebUrl={state.meko.webUrl}
            onBack={() => goProject(currentProject)}
            onError={showToast}
          />
        ) : currentProject ? (
          <ProjectView
            project={currentProject}
            sessions={live}
            session={currentSession}
            mekoWebUrl={state.meko.webUrl}
            onNewSession={() => setModal({ kind: 'new-session', projectId: currentProject.id })}
            onError={showToast}
          />
        ) : (
          <div className="empty-main">
            <img src="/logo.png" alt="" width={96} height={96} />
            <h2>No projects yet</h2>
            <p className="muted">
              A project is one of your git repos. Each task you start in it gets its own worktree and Claude session.
            </p>
            <button className="btn primary" onClick={() => setModal({ kind: 'new-project' })}>
              New project
            </button>
          </div>
        )}
      </div>

      {modal.kind === 'new-project' && (
        <NewProjectDialog
          mekoOk={state.meko.ok}
          onClose={() => setModal({ kind: 'none' })}
          onCreated={(p) => {
            setModal({ kind: 'none' });
            navigate({ view: 'project', projectId: p.id, sessionId: null });
          }}
        />
      )}
      {modal.kind === 'new-session' && (
        <NewSessionDialog
          project={projects.find((p) => p.id === modal.projectId)!}
          onClose={() => setModal({ kind: 'none' })}
          onCreated={(s) => {
            setModal({ kind: 'none' });
            navigate({ view: 'project', projectId: s.projectId, sessionId: s.id });
          }}
        />
      )}
      {modal.kind === 'settings' && (
        <SettingsDialog auth={auth} update={state.update} onClose={() => setModal({ kind: 'none' })} />
      )}
      {modal.kind === 'palette' && (
        <CommandPalette
          projects={projects}
          currentProjectId={currentProject?.id ?? null}
          onClose={() => setModal({ kind: 'none' })}
          onNewProject={() => setModal({ kind: 'new-project' })}
          onNewSession={(projectId) => setModal({ kind: 'new-session', projectId })}
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
