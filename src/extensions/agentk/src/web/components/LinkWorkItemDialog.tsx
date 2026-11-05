import { useEffect, useState } from 'react';
import type { ProjectWithSessions, Session, WorkItem } from '../../shared/types';
import { api, errorMessage } from '../api';
import { cx, dotState, liveSessions, stageWords } from '../util';
import { itemSession } from '../workqueue';
import { Busy, Dialog, ErrorText, StatusDot } from './common';

/**
 * Links a session and a work item, one to one: picks a session for `item`, or an item for `session`.
 * A pick that takes a session or item from its other link says so before it's made.
 */
export function LinkWorkItemDialog({
  item,
  session,
  items,
  projects,
  onClose,
}: {
  item?: WorkItem;
  session?: Session;
  items: WorkItem[];
  projects: ProjectWithSessions[];
  onClose: () => void;
}) {
  const [picked, setPicked] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // A session can be on a closed item too, which a link here would take it from.
  const [closed, setClosed] = useState<WorkItem[]>([]);
  useEffect(() => {
    if (item) void api.closedWorkItems().then(setClosed, () => undefined);
  }, [item]);
  const sessions = projects.flatMap((p) => liveSessions(p).map((s) => ({ s, project: p.name })));
  const itemOf = (sessionId: string) => [...items, ...closed].find((i) => i.sessionId === sessionId);
  const current = session ? items.find((i) => i.sessionId === session.id) : undefined;
  const choices = item
    ? sessions.map(({ s, project }) => {
      const other = itemOf(s.id);
      return { id: s.id, name: s.name, hint: `${project} · ${stageWords(s)}`, dot: dotState(s), taken: other && other.id !== item.id ? `On "${other.title}"` : null };
    })
    : items.filter((i) => i.id !== current?.id).map((i) => {
      const s = itemSession(i, projects);
      const taken = !i.sessionId || i.sessionId === session?.id ? null : s ? `Has "${s.name}"` : 'Has a closed session';
      return { id: i.id, name: i.title, hint: i.links.map((l) => l.label).join(' · '), dot: s ? dotState(s) : null, taken };
    });
  const choice = choices.find((c) => c.id === picked);

  const save = async () => {
    if (!picked || busy) return;
    setError(null);
    setBusy(true);
    try {
      if (item) await api.updateWorkItem(item.id, { sessionId: picked });
      else if (session) await api.updateWorkItem(picked, { sessionId: session.id });
      onClose();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  const unlink = async (itemId: string) => {
    setBusy(true);
    try {
      await api.updateWorkItem(itemId, { sessionId: null });
      onClose();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={item ? `Link a session to ${item.title}` : `Link ${session?.name ?? 'the session'} to a work item`}
      onClose={onClose}
      footer={
        <>
          <button className="btn" onClick={onClose}>
            Cancel
          </button>
          {session?.workItemId && (
            <button className="btn" disabled={busy} onClick={() => void unlink(session.workItemId!)}>
              Unlink
            </button>
          )}
          <button className="btn primary" disabled={!picked || busy} onClick={() => void save()}>
            {busy ? <Busy label="Linking" /> : choice?.taken || current ? 'Move the link here' : 'Link'}
          </button>
        </>
      }
    >
      <div className="stack">
        {!choices.length && <p className="muted">{item ? 'No sessions are open.' : 'The work queue is empty. Add an item first.'}</p>}
        <div className="wq-pick" role="listbox" aria-label={item ? 'Sessions' : 'Work items'}>
          {choices.map((c) => (
            <button
              key={c.id}
              type="button"
              role="option"
              aria-selected={picked === c.id}
              className={cx('wq-pick-row', picked === c.id && 'sel')}
              onClick={() => setPicked(c.id)}
            >
              {c.dot ? <StatusDot status={c.dot} phase={c.id} /> : <i className="dot none" aria-hidden="true" />}
              <span className="wq-pick-name">{c.name}</span>
              <span className="muted small">{[c.hint, c.taken].filter(Boolean).join(' · ')}</span>
            </button>
          ))}
        </div>
        {current && <p className="muted small">It's on "{current.title}" now: linking it to another item takes it off that one.</p>}
        {choice?.taken && (
          <p className="muted small">
            {item ? 'The session moves off its other work item: a session is on one item at most.' : "That item's session is unlinked: an item has one session at most."}
          </p>
        )}
        <ErrorText error={error} />
      </div>
    </Dialog>
  );
}
