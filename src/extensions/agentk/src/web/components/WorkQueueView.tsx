import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import type { ProjectWithSessions, WorkItem, WorkLink, WorkMove, WorkSync } from '../../shared/types';
import { moveItem } from '../../shared/workorder';
import { api, errorMessage } from '../api';
import { useIsMobile, useTouch } from '../hooks/useMedia';
import { navigate } from '../router';
import { cx, dotState, relativeTime, stageWords } from '../util';
import { itemSession, movedOrder, workSyncText } from '../workqueue';
import { Busy, ConfirmButton, StatusDot } from './common';
import { TopBar } from './TopBar';

const KIND_TEXT: Record<WorkLink['kind'], string> = { github: 'GitHub', jira: 'Jira', slack: 'Slack', other: 'Link' };

/** A link as a chip: what it is and its short name, opening it in a new tab when it has a URL. */
export function WorkLinkChip({ link }: { link: WorkLink }) {
  const text = (
    <>
      <b>{KIND_TEXT[link.kind]}</b> {link.label}
    </>
  );
  return link.url ? (
    <a className={cx('wq-chip', `wq-${link.kind}`)} href={link.url} target="_blank" rel="noopener noreferrer" title={link.url}>
      {text}
    </a>
  ) : (
    <span className={cx('wq-chip', `wq-${link.kind}`)}>{text}</span>
  );
}

/**
 * The work queue (`/queue`): the open items in your order, each with its links and its session's
 * status (or Start session), new synced items marked until moved to their place, Sync, Dismiss on
 * each item and Dismiss all, and the dismissed items below. Reordered by dragging (not on touch) or
 * from each item's ⋯ actions.
 */
export function WorkQueueView({
  items,
  sync,
  projects,
  focusId,
  onMenu,
  onAdd,
  onEdit,
  onLinkSession,
  onStartSession,
  onError,
  onSettings,
}: {
  items: WorkItem[];
  sync: WorkSync;
  projects: ProjectWithSessions[];
  /** An item to scroll to and open (from the command palette). */
  focusId: string | null;
  onMenu: () => void;
  onAdd: () => void;
  onEdit: (item: WorkItem) => void;
  onLinkSession: (item: WorkItem) => void;
  onStartSession: (item: WorkItem) => void;
  onError: (msg: string) => void;
  /** Opens Settings › Work queue. */
  onSettings: () => void;
}) {
  const mobile = useIsMobile();
  const touch = useTouch();
  const [open, setOpen] = useState<string | null>(focusId);
  const [syncing, setSyncing] = useState(false);
  // The order a drop or a move asked for, shown until the state carries it (or the request fails).
  const [order, setOrder] = useState<string[] | null>(null);
  const [dragId, setDragId] = useState<string | null>(null);
  const [closed, setClosed] = useState<WorkItem[] | null>(null);
  const [showClosed, setShowClosed] = useState(false);
  const rootRef = useRef<HTMLDivElement>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  // Where the list was scrolled when a move was asked for, put back once it's redrawn, so the page
  // stays where the user was instead of following the item.
  const keepScroll = useRef<number | null>(null);

  // A drop's order shows until the queue itself changes, not on every state push.
  const queueKey = items.map((i) => `${i.id}:${i.status}`).join();
  useEffect(() => setOrder(null), [queueKey]);
  const loadClosed = useCallback(() => void api.closedWorkItems().then(setClosed, (e: unknown) => onError(errorMessage(e))), [onError]);
  // An item picked in the palette: opened and scrolled to, in the Dismissed section when it's dismissed.
  const focusOpen = !!focusId && items.some((i) => i.id === focusId);
  useEffect(() => {
    if (!focusId) return;
    setOpen(focusId);
    if (!focusOpen) {
      setShowClosed(true);
      loadClosed();
    }
  }, [focusId, focusOpen, loadClosed]);
  useEffect(() => {
    if (focusId) rootRef.current?.querySelector(`[data-item="${CSS.escape(focusId)}"]`)?.scrollIntoView({ block: 'center' });
  }, [focusId, closed]);

  const syncOff = !sync.sources.github && !sync.sources.slack;
  const byId = new Map(items.map((i) => [i.id, i]));
  const shown = order ? order.map((id) => byId.get(id)).filter((i): i is WorkItem => !!i) : items;
  const shownKey = shown.map((i) => i.id).join();
  useLayoutEffect(() => {
    if (keepScroll.current == null || !scrollRef.current) return;
    scrollRef.current.scrollTop = keepScroll.current;
    keepScroll.current = null;
  }, [shownKey]);

  const act = async (fn: () => Promise<unknown>) => {
    try {
      await fn();
    } catch (e) {
      onError(errorMessage(e));
    }
  };
  const runSync = async () => {
    setSyncing(true);
    await act(() => api.syncWork());
    setSyncing(false);
  };
  const move = (item: WorkItem, to: WorkMove) => {
    keepScroll.current = scrollRef.current?.scrollTop ?? null;
    setOrder(moveItem(shown.map((i) => i.id), item.id, to));
    // Its actions would go with it, out of sight.
    if (to === 'top' || to === 'bottom') setOpen(null);
    void api.updateWorkItem(item.id, { move: to }).catch((e: unknown) => {
      setOrder(null);
      onError(errorMessage(e));
    });
  };
  const dismiss = (item: WorkItem) => act(async () => {
    await api.updateWorkItem(item.id, { status: 'closed' });
    if (showClosed) loadClosed();
  });
  // The items shown when Dismiss all started asking: one a sync adds while it asks isn't dismissed unseen.
  const [dismissIds, setDismissIds] = useState<string[]>([]);
  const dismissAll = () => act(async () => {
    await api.closeWorkItems({ ids: dismissIds });
    if (showClosed) loadClosed();
  });
  const drop = (to: number) => {
    const id = dragId;
    setDragId(null);
    if (!id) return;
    const ids = movedOrder(shown.map((i) => i.id), id, to);
    setOrder(ids);
    void api.reorderWorkItems({ ids, moved: id }).catch((e: unknown) => {
      setOrder(null);
      onError(errorMessage(e));
    });
  };

  return (
    <div className="home-view wq" ref={rootRef}>
      {mobile && <TopBar onMenu={onMenu} title="Work queue" />}
      <header className="home-hd wq-hd">
        {!mobile && <h1>Work queue</h1>}
        <div className="wq-bar">
          <button className="btn primary" onClick={onAdd}>
            + Add
          </button>
          <button
            className="btn"
            disabled={syncing || sync.running || syncOff}
            onClick={() => void runSync()}
            title="Pull in GitHub review requests and your Slack list"
          >
            {syncing || sync.running ? <Busy label="Syncing" /> : 'Sync'}
          </button>
          <span className="muted small wq-sync">{workSyncText(sync)}</span>
          <button className="btn small" onClick={onSettings}>
            Sync settings
          </button>
          {shown.length > 0 && (
            <ConfirmButton
              className="btn small wq-dismiss-all"
              label="Dismiss all"
              prompt={`Dismiss all ${dismissIds.length}?`}
              confirmLabel="Dismiss all"
              danger
              onAsk={() => setDismissIds(shown.map((i) => i.id))}
              onConfirm={dismissAll}
            />
          )}
        </div>
      </header>
      <div className="home-scroll wq-scroll" ref={scrollRef}>
      {!shown.length && (
        <p className="muted wq-empty">
          Nothing on the queue. Add what you're working on, or {syncOff ? 'turn on Sync in Sync settings' : 'press Sync'} to pull in the PRs waiting
          for your review and the messages on your Slack list.
        </p>
      )}
      <ol className="wq-list">
        {shown.map((item, i) => {
          const s = itemSession(item, projects);
          const isOpen = open === item.id;
          return (
            <li
              key={item.id}
              data-item={item.id}
              className={cx('wq-item', item.unplaced && 'unplaced', dragId === item.id && 'dragging')}
              draggable={!touch}
              onDragStart={(e) => {
                setDragId(item.id);
                e.dataTransfer.effectAllowed = 'move';
              }}
              onDragEnd={() => setDragId(null)}
              onDragOver={(e) => dragId && e.preventDefault()}
              onDrop={(e) => {
                e.preventDefault();
                drop(i);
              }}
            >
              <div className="wq-row">
                {!touch && <span className="wq-handle" aria-hidden="true">⠿</span>}
                {s ? <StatusDot status={dotState(s)} phase={s.id} /> : <i className="dot none" title="No session" aria-label="No session" />}
                <button className="wq-title" onClick={() => onEdit(item)} title="Edit">
                  {item.title}
                  {item.notes && <span className="wq-notes">{item.notes}</span>}
                </button>
                <button className="btn small wq-dismiss" onClick={() => void dismiss(item)} title="Take it off the queue (it stays under Dismissed)">
                  Dismiss
                </button>
                <button
                  className="icon-btn wq-more"
                  aria-label={`Actions for ${item.title}`}
                  aria-expanded={isOpen}
                  onClick={() => setOpen(isOpen ? null : item.id)}
                >
                  ⋯
                </button>
              </div>
              <div className="wq-meta">
                {item.unplaced && (
                  <span className="wq-chip wq-new" title="Synced: move it to its place in the queue">
                    New · move it to its place
                    <button className="wq-keep" onClick={() => void act(() => api.updateWorkItem(item.id, { placed: true }))}>
                      Keep here
                    </button>
                  </span>
                )}
                {item.source === 'github' && <span className="wq-chip wq-src">Review request</span>}
                {item.source === 'slack' && <span className="wq-chip wq-src">Slack list</span>}
                {item.githubDone && <span className="wq-chip wq-done" title="Merged, closed, or your review isn't requested any more">Done on GitHub</span>}
                {item.links.map((l) => (
                  <WorkLinkChip key={l.key} link={l} />
                ))}
              </div>
              <div className="wq-session">
                {s ? (
                  <button className="wq-go" onClick={() => navigate({ view: 'project', projectId: s.projectId, sessionId: s.id })}>
                    {s.name} <span className="muted">· {stageWords(s)}</span>
                  </button>
                ) : (
                  <>
                    {item.sessionId && <span className="muted small">Its session is closed.</span>}
                    <button className="btn small" onClick={() => onStartSession(item)}>
                      ▶ Start session
                    </button>
                  </>
                )}
              </div>
              {isOpen && (
                <div className="wq-acts">
                  <button className="btn small" onClick={() => onEdit(item)}>Edit</button>
                  <button className="btn small" onClick={() => onLinkSession(item)}>Link a session…</button>
                  {item.sessionId && (
                    <button className="btn small" onClick={() => void act(() => api.updateWorkItem(item.id, { sessionId: null }))}>Unlink session</button>
                  )}
                  {i > 0 && <button className="btn small" onClick={() => move(item, 'top')}>Move to top</button>}
                  {i > 0 && <button className="btn small" onClick={() => move(item, 'up')}>Move up</button>}
                  {i < shown.length - 1 && <button className="btn small" onClick={() => move(item, 'down')}>Move down</button>}
                  {i < shown.length - 1 && <button className="btn small" onClick={() => move(item, 'bottom')}>Move to bottom</button>}
                </div>
              )}
            </li>
          );
        })}
        {dragId && (
          <li className="wq-drop-end" onDragOver={(e) => e.preventDefault()} onDrop={(e) => {
            e.preventDefault();
            drop(shown.length);
          }} aria-hidden="true" />
        )}
      </ol>
      <section className="wq-closed">
        <button
          className="btn small"
          aria-expanded={showClosed}
          onClick={() => {
            if (!showClosed) loadClosed();
            setShowClosed(!showClosed);
          }}
        >
          {showClosed ? '▾' : '▸'} Dismissed{closed ? ` (${closed.length})` : ''}
        </button>
        {showClosed && !closed && <Busy label="Loading" />}
        {showClosed && closed && !closed.length && <p className="muted small">No dismissed items.</p>}
        {showClosed && closed && (
          <ul className="wq-list closed">
            {closed.map((item) => (
              <li key={item.id} data-item={item.id} className={cx('wq-item', focusId === item.id && 'focused')}>
                <div className="wq-row">
                  <span className="wq-title closed">{item.title}</span>
                  <span className="muted small">Dismissed {relativeTime(item.closedAt)}</span>
                  <button className="btn small" onClick={() => void act(async () => {
                    await api.updateWorkItem(item.id, { status: 'open' });
                    loadClosed();
                  })}>
                    Reopen
                  </button>
                </div>
                {!!item.links.length && (
                  <div className="wq-meta">
                    {item.links.map((l) => (
                      <WorkLinkChip key={l.key} link={l} />
                    ))}
                  </div>
                )}
              </li>
            ))}
          </ul>
        )}
      </section>
      </div>
    </div>
  );
}
