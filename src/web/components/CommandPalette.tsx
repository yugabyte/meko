import { useEffect, useMemo, useRef, useState } from 'react';
import { parseGithubRef } from '../../shared/github-refs';
import {
  STAGE_LABELS, type AskAgentkAction, type AskAgentkResult, type CreateSessionRequest, type LinkPlan, type ProjectWithSessions, type Session,
  type StartFromLinkResult,
} from '../../shared/types';
import { moveBlockedIn } from '../../shared/stages';
import { api, errorMessage } from '../api';
import { useMedia, useTouch } from '../hooks/useMedia';
import { PALETTE_HINTS, PALETTE_HINT_STILL, askEntries, filterItems, linkEntries, type LabelPart } from '../palette';
import { navigate } from '../router';
import { STATUS_TEXT, cx, dotState, isMac, liveSessions, type DotState } from '../util';
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
  /** Its label wraps rather than being cut (AgentK's actions, read before they run, and a link's items, whose project ends the label). */
  wrap?: boolean;
  dot?: DotState;
  /** Where in the blink signal the dot is: its session's id. */
  phase?: string;
}

/** How long typing pauses before a pasted link is looked up. */
const LINK_LOOKUP_MS = 250;
/** How long each of the placeholder's hints shows. */
const HINT_MS = 2000;

/** One exchange with AgentK while the palette is open: what was asked, and its answer. */
interface Exchange extends AskAgentkResult {
  text: string;
}

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
  askAgentk,
  onEditSession,
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
  /** A session started from a pasted link (in a project it may just have opened), or one AgentK proposed. */
  onStarted: (r: StartFromLinkResult) => void;
  /** `[ask_agentk] enabled`: offer Ask AgentK. */
  askAgentk: boolean;
  /** Opens New session filled in with a task AgentK proposed. */
  onEditSession: (projectId: string, initial: CreateSessionRequest) => void;
}) {
  const [q, setQ] = useState('');
  const [idx, setIdx] = useState(0);
  const listRef = useRef<HTMLDivElement>(null);
  const paletteRef = useRef<HTMLDivElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);

  // A pasted GitHub PR or issue: looked up once typing pauses, and kept while the palette is open
  // (per project on screen, which the plan prefers).
  const ref = useMemo(() => parseGithubRef(q), [q]);
  const refKey = ref ? `${ref.repo}#${ref.number}` : null;
  const lookupKey = refKey ? `${refKey}@${currentProjectId ?? ''}` : null;
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
  // A plan naming a project that was closed meanwhile is looked up again, rather than started in a project that's gone.
  const cached = lookupKey ? lookups.current.get(lookupKey) : undefined;
  const planProjects = cached && 'plan' in cached ? [cached.plan.project, ...cached.plan.others] : [];
  const stale = planProjects.some((pp) => pp && !projects.some((p) => p.id === pp.id));
  useEffect(() => {
    setStartError(null);
    if (lookupKey && stale) lookups.current.delete(lookupKey);
    if (!refKey || !lookupKey || lookups.current.has(lookupKey)) return;
    let live = true;
    const t = window.setTimeout(() => {
      api
        .githubLink(refKey, currentProjectId)
        .then((plan) => lookups.current.set(lookupKey, { plan }), (e: unknown) => lookups.current.set(lookupKey, { error: errorMessage(e) }))
        .then(() => live && setLooked((n) => n + 1));
    }, LINK_LOOKUP_MS);
    return () => {
      live = false;
      window.clearTimeout(t);
    };
  }, [refKey, lookupKey, currentProjectId, stale]);
  const looked = stale ? undefined : cached;
  const plan = looked && 'plan' in looked ? looked.plan : null;

  // Ask AgentK: the exchange so far (forgotten when the palette closes), the ask Claude is answering, and one that failed.
  const [chat, setChat] = useState<Exchange[]>([]);
  const [asking, setAsking] = useState<string | null>(null);
  const [askError, setAskError] = useState<{ text: string; message: string } | null>(null);
  const askAbort = useRef<AbortController | null>(null);
  useEffect(() => () => askAbort.current?.abort(), []);
  const chatRef = useRef<HTMLDivElement>(null);
  useEffect(() => {
    chatRef.current?.scrollTo({ top: chatRef.current.scrollHeight });
  }, [chat, asking, askError]);
  // An ask started with a click took focus to a button that's gone by the answer; Enter picks from
  // the input, so focus goes back there. Not on a touch screen, where it would raise the keyboard.
  const touch = useTouch();
  useEffect(() => {
    if (asking || touch) return;
    if (!paletteRef.current?.contains(document.activeElement)) inputRef.current?.focus();
  }, [asking, touch]);

  // The placeholder rotates through what the palette takes, on every screen, unless motion is reduced.
  const still = useMedia('(prefers-reduced-motion: reduce)');
  const [hint, setHint] = useState(0);
  useEffect(() => {
    if (still) return;
    const t = window.setInterval(() => setHint((h) => (h + 1) % PALETTE_HINTS.length), HINT_MS);
    return () => window.clearInterval(t);
  }, [still]);
  const placeholder = chat.length ? 'Reply to AgentK, or type a command…' : still ? PALETTE_HINT_STILL : PALETTE_HINTS[hint];

  const go = (a: Extract<AskAgentkAction, { type: 'go' }>) => {
    if (a.view === 'session') navigate({ view: 'project', projectId: a.projectId, sessionId: a.sessionId });
    else if (a.view === 'home') navigate({ view: 'home', projectId: a.projectId });
    else if (a.view === 'usage') navigate({ view: 'usage' });
    else onSettings();
  };

  const ask = async (raw: string) => {
    const text = raw.trim();
    if (!text || askAbort.current) return;
    const ac = new AbortController();
    askAbort.current = ac;
    setAsking(text);
    setAskError(null);
    setStartError(null);
    setQ('');
    try {
      const r = await api.askAgentk(
        { text, history: chat.map((x) => ({ text: x.text, reply: x.reply })), projectId: currentProjectId, sessionId: currentSession?.id ?? null },
        ac.signal,
      );
      if (ac.signal.aborted || !mounted.current) return;
      // Every action waits to be picked, a lone go too: the reply is the answer, and it stays on screen.
      setChat((c) => [...c, { text, ...r }]);
    } catch (e) {
      if (!ac.signal.aborted && mounted.current) setAskError({ text, message: errorMessage(e) });
    } finally {
      if (askAbort.current === ac) {
        askAbort.current = null;
        if (mounted.current) setAsking(null);
      }
    }
  };
  /** Esc while AgentK thinks: the ask stops, and its text comes back to change. */
  const cancelAsk = () => {
    askAbort.current?.abort();
    askAbort.current = null;
    if (asking) setQ(asking);
    setAsking(null);
  };

  const last = chat.at(-1);
  const answerItems = useMemo<Item[]>(() => {
    if (!last) return [];
    const run = async (id: string, work: () => Promise<void>) => {
      setStarting(id);
      setStartError(null);
      try {
        await work();
      } catch (e) {
        if (mounted.current) setStartError(errorMessage(e));
      } finally {
        if (mounted.current) setStarting(null);
      }
    };
    const busyLabel: Record<AskAgentkAction['type'], string> = { start: 'Starting', link: '', go: '', move: 'Moving', openPr: 'Opening the PR' };
    return askEntries(last.actions, projects).map((e): Item => {
      const a = e.action;
      const item: Item = { id: e.id, label: e.label, hint: e.hint, keywords: '', busy: starting === e.id ? busyLabel[a.type] : undefined, wrap: true };
      if (e.edit && a.type === 'start') return { ...item, run: () => onEditSession(a.projectId, { name: a.name, prompt: a.prompt, start: a.start }) };
      switch (a.type) {
        case 'go':
          return { ...item, run: () => go(a) };
        // The link's own items follow, as for a pasted link.
        case 'link':
          return { ...item, keepOpen: true, run: () => setQ(a.ref) };
        case 'start':
          return {
            ...item, keepOpen: true,
            run: () => void (!starting && run(e.id, async () => {
              const session = await api.createSession(a.projectId, { name: a.name, prompt: a.prompt, start: a.start });
              const project = projects.find((p) => p.id === a.projectId);
              if (!mounted.current || !project) return;
              onClose();
              onStarted({ project, session });
            })),
          };
        case 'move':
        case 'openPr':
          return {
            ...item, keepOpen: true,
            run: () => void (!starting && run(e.id, async () => {
              const instructions = a.instructions || undefined;
              const s = a.type === 'move' ? await api.moveSession(a.sessionId, { stage: a.stage, instructions }) : await api.openPr(a.sessionId, { instructions });
              if (!mounted.current) return;
              onClose();
              navigate({ view: 'project', projectId: s.projectId, sessionId: s.id });
            })),
          };
      }
    });
    // go() only reads onSettings, which the deps have.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [last, projects, starting, onClose, onStarted, onEditSession, onSettings]);

  const linkItems = useMemo<Item[]>(() => {
    if (!refKey) return [];
    if (!looked) return [{ id: 'link-lookup', label: [], busy: `Looking up ${refKey}`, hint: '', keywords: '' }];
    if (!('plan' in looked)) return [{ id: 'link-error', label: [looked.error], hint: '', keywords: '' }];
    const p = looked.plan;
    const startFrom = async (id: string, start: 'design' | 'code' | null, projectId: string | undefined) => {
      setStarting(id);
      setStartError(null);
      try {
        const r = await api.startFromLink({ ref: refKey, projectId, start: start ?? undefined });
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
        wrap: true,
        busy: starting === e.id ? (e.clones ? `Cloning ${p.repo}` : 'Starting') : undefined,
        ...(a && 'go' in a
          ? { run: () => navigate({ view: 'project', projectId: a.go.projectId, sessionId: a.go.sessionId }) }
          : a
            ? { keepOpen: true, run: () => void (!starting && startFrom(e.id, a.start, a.projectId)) }
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
          phase: s.id,
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

  const typed = q.trim();
  const shown = useMemo<Item[]>(() => {
    if (asking) return [];
    // Before typing again, the last answer's actions.
    if (!typed && last) return answerItems;
    // Last: alone, and so picked by Enter, when nothing matches (a sentence never does). Not for a pasted link.
    const askItem: Item[] = askAgentk && typed && !refKey
      ? [{ id: 'ask', label: [`Ask AgentK: “${typed}”`], hint: `${isMac() ? '⌘' : 'Ctrl'}+Enter`, keywords: '', run: () => void ask(typed), keepOpen: true }]
      : [];
    return [...linkItems, ...filterItems(items, q), ...askItem];
    // ask() reads the chat and screen, which change no more often than the deps here.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [linkItems, items, q, typed, refKey, asking, last, answerItems, askAgentk]);

  useEffect(() => setIdx(0), [q, plan, last]);
  useEffect(() => {
    listRef.current?.querySelector('.pal-item.sel')?.scrollIntoView({ block: 'nearest' });
  }, [idx]);

  const choose = (it: Item | undefined) => {
    if (!it?.run) return;
    if (!it.keepOpen) onClose();
    it.run();
  };

  return (
    // On desktop it opens over the rail's search bar (`pal-overlay`); on a phone, across the top.
    <div className="overlay top pal-overlay" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div
        className="palette"
        role="dialog"
        aria-label="Ask AgentK"
        ref={paletteRef}
        onKeyDown={(e) => trapTab(e.nativeEvent, paletteRef.current)}
      >
        <div className="pal-row">
          <img src="/logo.png" alt="" width={18} height={18} />
          <input
            id="palette-input"
            ref={inputRef}
            className="pal-input"
            aria-label="Ask AgentK, run a command, or paste a GitHub link"
            placeholder={placeholder}
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
              } else if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
                e.preventDefault();
                if (askAgentk) void ask(q);
              } else if (e.key === 'Enter') {
                e.preventDefault();
                choose(shown[idx]);
              } else if (e.key === 'Escape') {
                e.preventDefault();
                if (asking) cancelAsk();
                else onClose();
              }
            }}
          />
        </div>
        {(chat.length > 0 || asking || askError) && (
          <div className="pal-chat" ref={chatRef} aria-live="polite">
            {chat.map((x, i) => (
              <div key={i} className="pal-turn">
                <p className="pal-you">You: {x.text}</p>
                <p className="pal-reply">{x.reply}</p>
                {x.notes.map((n, j) => (
                  <p key={j} className="pal-note">{n}</p>
                ))}
              </div>
            ))}
            {asking && (
              <div className="pal-turn">
                <p className="pal-you">You: {asking}</p>
                {/* A tap's way to Esc: the ask stops, its text comes back, and the exchange stays. */}
                <div className="pal-reply muted">
                  <Busy label="AgentK is thinking" />
                  <button className="btn small" onClick={cancelAsk}>Cancel</button>
                </div>
              </div>
            )}
            {askError && (
              <div className="pal-turn">
                <p className="pal-you">You: {askError.text}</p>
                <div className="pal-reply">
                  <ErrorText error={askError.message} />
                  <button className="btn small" onClick={() => void ask(askError.text)}>Try again</button>
                </div>
              </div>
            )}
          </div>
        )}
        <div className="pal-list" ref={listRef} role="listbox" hidden={!!asking}>
          {shown.length === 0 && !asking && !last && <p className="muted pad">Nothing matches.</p>}
          {shown.map((it, i) => (
            <button
              key={it.id}
              role="option"
              aria-selected={i === idx}
              aria-disabled={!it.run || undefined}
              className={cx('pal-item', i === idx && 'sel', !it.run && 'inert', it.wrap && 'wrap')}
              onMouseEnter={() => setIdx(i)}
              onClick={() => choose(it)}
            >
              {it.dot ? <StatusDot status={it.dot} phase={it.phase} /> : <span className="pal-icon">›</span>}
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
