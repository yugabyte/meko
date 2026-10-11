import { useEffect, useMemo, useRef, useState } from 'react';
import { parseGithubRef } from '../../shared/github-refs';
import { STAGE_LABELS, type LinkPlan, type ProjectWithSessions, type Session, type StartFromLinkResult } from '../../shared/types';
import { moveBlockedIn } from '../../shared/stages';
import { api, errorMessage } from '../api';
import { filterItems, linkEntries, type LabelPart } from '../palette';
import { navigate } from '../router';
import { STATUS_TEXT, cx, dotState, liveSessions, type DotState } from '../util';
import { Busy, ErrorText, StatusDot, trapTab } from './common';

interface Item {
  id: string;
  /** Session and project names are parts of their own, so they don't read as commands. */
  label: LabelPart[];
  hint: string;
  keywords: string;
  /** Left out for an item that only says something (a lookup under way, or why a link can't start). */
  run?: () => void;
  /** It closes the palette itself, once it's done (a pasted link's start). */
  keepOpen?: boolean;
  /** Shown instead of the label while it runs. */
  busy?: string;
  dot?: DotState;
}

/** How long typing pauses before a pasted link is looked up. */
const LINK_LOOKUP_MS = 250;

export function CommandPalette({
  projects,
  currentProjectId,
  currentSession,
  onClose,
  onNewProject,
  onNewSession,
  onMoveSession,
  onOpenPr,
  onRenameSession,
  onSettings,
  onStarted,
}: {
  projects: ProjectWithSessions[];
  currentProjectId: string | null;
  /** The session on screen, if any. */
  currentSession: Session | null;
  onClose: () => void;
  onNewProject: () => void;
  onNewSession: (projectId: string) => void;
  /** Opens the Move to another stage dialog for the session on screen. */
  onMoveSession: () => void;
  /** Opens the Open PR with instructions dialog for the session on screen. */
  onOpenPr: () => void;
  /** Opens the Rename session dialog for the session on screen. */
  onRenameSession: () => void;
  onSettings: () => void;
  /** A session started from a pasted link (in a project it may just have opened). */
  onStarted: (r: StartFromLinkResult) => void;
}) {
  const [q, setQ] = useState('');
  const [idx, setIdx] = useState(0);
  const listRef = useRef<HTMLDivElement>(null);
  const paletteRef = useRef<HTMLDivElement>(null);

  // A pasted GitHub PR or issue: looked up once typing pauses, and kept while the palette is open.
  const ref = useMemo(() => parseGithubRef(q), [q]);
  const refKey = ref ? `${ref.repo}#${ref.number}` : null;
  const lookups = useRef(new Map<string, { plan: LinkPlan } | { error: string }>());
  const [, setLooked] = useState(0);
  const [starting, setStarting] = useState<string | null>(null);
  const [startError, setStartError] = useState<string | null>(null);
  // Set in the effect, not only at first render: StrictMode mounts, cleans up and mounts again.
  const mounted = useRef(false);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  // A plan whose project was closed meanwhile is looked up again, rather than started in a project that's gone.
  const cached = refKey ? lookups.current.get(refKey) : undefined;
  const planProject = cached && 'plan' in cached ? cached.plan.project?.id : undefined;
  const stale = !!planProject && !projects.some((p) => p.id === planProject);
  useEffect(() => {
    setStartError(null);
    if (refKey && stale) lookups.current.delete(refKey);
    if (!refKey || lookups.current.has(refKey)) return;
    let live = true;
    const t = window.setTimeout(() => {
      api
        .githubLink(refKey)
        .then((plan) => lookups.current.set(refKey, { plan }), (e: unknown) => lookups.current.set(refKey, { error: errorMessage(e) }))
        .then(() => live && setLooked((n) => n + 1));
    }, LINK_LOOKUP_MS);
    return () => {
      live = false;
      window.clearTimeout(t);
    };
  }, [refKey, stale]);
  const looked = stale ? undefined : cached;
  const plan = looked && 'plan' in looked ? looked.plan : null;

  const linkItems = useMemo<Item[]>(() => {
    if (!refKey) return [];
    if (!looked) return [{ id: 'link-lookup', label: [], busy: `Looking up ${refKey}`, hint: '', keywords: '' }];
    if (!('plan' in looked)) return [{ id: 'link-error', label: [looked.error], hint: '', keywords: '' }];
    const p = looked.plan;
    const startFrom = async (id: string, start: 'design' | 'code' | null) => {
      setStarting(id);
      setStartError(null);
      try {
        const r = await api.startFromLink({ ref: refKey, projectId: p.project?.id, start: start ?? undefined });
        // Closed meanwhile: the session shows up in the state anyway, and another dialog may be open now.
        if (!mounted.current) return;
        onClose();
        onStarted(r);
      } catch (e) {
        if (mounted.current) setStartError(errorMessage(e));
      } finally {
        if (mounted.current) setStarting(null);
      }
    };
    return linkEntries(p).map((e): Item => {
      const a = e.action;
      return {
        id: e.id,
        label: e.label,
        hint: e.hint,
        keywords: '',
        busy: starting === e.id ? (e.clones ? `Cloning ${p.repo}` : 'Starting') : undefined,
        ...(a && 'go' in a
          ? { run: () => navigate({ view: 'project', projectId: a.go.projectId, sessionId: a.go.sessionId }) }
          : a
            ? { keepOpen: true, run: () => void (!starting && startFrom(e.id, a.start)) }
            : {}),
      };
    });
  }, [refKey, looked, starting, onClose, onStarted]);

  const items = useMemo<Item[]>(() => {
    const out: Item[] = [];
    const current = projects.find((p) => p.id === currentProjectId);
    if (current) {
      out.push({
        id: 'new-session',
        label: ['New session in ', { project: current.name }],
        hint: 'Action',
        keywords: 'new session task start',
        run: () => onNewSession(current.id),
      });
    }
    if (currentSession && !moveBlockedIn(currentSession.stage)) {
      out.push({
        id: 'move-session',
        label: ['Move ', { session: currentSession.name }, ' to another stage'],
        hint: 'Action',
        keywords: 'change type stage move exploration explore plan quick code design pr review',
        run: onMoveSession,
      });
    }
    if (currentSession?.stage === 'code') {
      out.push({
        id: 'open-pr',
        label: currentSession.queued === 'open-pr'
          ? ['Instructions for the queued Open PR of ', { session: currentSession.name }]
          : ['Open PR for ', { session: currentSession.name }, ' with instructions'],
        hint: 'Action',
        keywords: 'open pr pull request instructions notes draft',
        run: onOpenPr,
      });
    }
    if (currentSession) {
      out.push({
        id: 'rename-session',
        label: ['Rename ', { session: currentSession.name }],
        hint: 'Action',
        keywords: 'rename name title session',
        run: onRenameSession,
      });
    }
    if (currentSession && current?.settings.mekoLog) {
      out.push({
        id: 'meko-calls',
        label: ['Meko calls of ', { session: currentSession.name }],
        hint: 'View',
        keywords: 'meko calls log debug memory search',
        run: () => navigate({ view: 'meko', projectId: current.id, sessionId: currentSession.id }),
      });
    }
    for (const p of projects) {
      for (const s of liveSessions(p)) {
        out.push({
          id: `s-${s.id}`,
          label: [{ session: s.name }],
          hint: `${p.name} · ${STAGE_LABELS[s.stage]} · ${STATUS_TEXT[dotState(s)]}`,
          keywords: `${p.name} ${s.name} ${s.branch}`,
          dot: dotState(s),
          run: () => navigate({ view: 'project', projectId: p.id, sessionId: s.id }),
        });
      }
    }
    for (const p of projects) {
      out.push({
        id: `home-${p.id}`,
        label: [{ project: p.name }, ': project home'],
        hint: 'Datapacks, search, activity, settings',
        keywords: `${p.name} home datapack search settings`,
        run: () => navigate({ view: 'home', projectId: p.id }),
      });
      if (p.id !== currentProjectId) {
        out.push({
          id: `ns-${p.id}`,
          label: ['New session in ', { project: p.name }],
          hint: 'Action',
          keywords: `new session ${p.name}`,
          run: () => onNewSession(p.id),
        });
      }
    }
    out.push({ id: 'new-project', label: ['Open project'], hint: 'Action', keywords: 'open new create project repo add', run: onNewProject });
    out.push({ id: 'usage', label: ['Usage'], hint: 'View', keywords: 'usage tokens prs commits pushes projects', run: () => navigate({ view: 'usage' }) });
    out.push({ id: 'settings', label: ['Settings'], hint: 'Action', keywords: 'settings password update totp', run: onSettings });
    return out;
  }, [projects, currentProjectId, currentSession, onNewProject, onNewSession, onMoveSession, onOpenPr, onRenameSession, onSettings]);

  const shown = useMemo(() => [...linkItems, ...filterItems(items, q)], [linkItems, items, q]);

  useEffect(() => setIdx(0), [q, plan]);
  useEffect(() => {
    listRef.current?.querySelector('.pal-item.sel')?.scrollIntoView({ block: 'nearest' });
  }, [idx]);

  const choose = (it: Item | undefined) => {
    if (!it?.run) return;
    if (!it.keepOpen) onClose();
    it.run();
  };

  return (
    <div className="overlay top" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div
        className="palette"
        role="dialog"
        aria-label="Command palette"
        ref={paletteRef}
        onKeyDown={(e) => trapTab(e.nativeEvent, paletteRef.current)}
      >
        <input
          id="palette-input"
          className="pal-input"
          placeholder="Jump to a session, type a command, or paste a GitHub link…"
          value={q}
          autoFocus
          onChange={(e) => setQ(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'ArrowDown') {
              e.preventDefault();
              setIdx((i) => Math.min(shown.length - 1, i + 1));
            } else if (e.key === 'ArrowUp') {
              e.preventDefault();
              setIdx((i) => Math.max(0, i - 1));
            } else if (e.key === 'Enter') {
              e.preventDefault();
              choose(shown[idx]);
            } else if (e.key === 'Escape') {
              e.preventDefault();
              onClose();
            }
          }}
        />
        <div className="pal-list" ref={listRef} role="listbox">
          {shown.length === 0 && <p className="muted pad">Nothing matches.</p>}
          {shown.map((it, i) => (
            <button
              key={it.id}
              role="option"
              aria-selected={i === idx}
              aria-disabled={!it.run || undefined}
              className={cx('pal-item', i === idx && 'sel', !it.run && 'inert')}
              onMouseEnter={() => setIdx(i)}
              onClick={() => choose(it)}
            >
              {it.dot ? <StatusDot status={it.dot} /> : <span className="pal-icon">›</span>}
              <span className="pal-label">
                {it.busy ? <Busy label={it.busy} /> : it.label.map((part, j) =>
                  typeof part === 'string' ? (
                    part
                  ) : 'session' in part ? (
                    <span key={j} className="pal-session">{part.session}</span>
                  ) : (
                    <span key={j} className="pal-project">{part.project}</span>
                  ),
                )}
              </span>
              <span className="pal-hint">{it.hint}</span>
            </button>
          ))}
        </div>
        {startError && (
          <div className="pal-error">
            <ErrorText error={startError} />
          </div>
        )}
      </div>
    </div>
  );
}
