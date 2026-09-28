import { useEffect, useReducer, useRef, useState } from 'react';
import type { Session } from '../../shared/types';
import { useTouch } from '../hooks/useMedia';
import { getTerminal } from '../terminals';
import { StartupPanel } from './StartupPanel';
import { TermKeys } from './TermKeys';

export function TerminalPane({ session }: { session: Session }) {
  const { id: sessionId, status } = session;
  const hostRef = useRef<HTMLDivElement>(null);
  const [, rerender] = useReducer((x: number) => x + 1, 0);
  const term = getTerminal(sessionId);
  const prevStatus = useRef(status);
  const touch = useTouch();
  // The startup the user chose to look past (Show terminal); a later start shows the panel again.
  const startKey = `${sessionId}:${session.startup?.startedAt ?? ''}`;
  const [peeked, setPeeked] = useState<string | null>(null);
  const showStartup = status === 'starting' && peeked !== startKey;

  useEffect(() => {
    const host = hostRef.current;
    if (!host) return;
    term.attach(host);
    const unsub = term.subscribe(rerender);
    return () => {
      unsub();
      term.detach();
    };
  }, [term]);

  // After Resume the pane is live again: reconnect straight away instead of waiting for backoff.
  useEffect(() => {
    if (prevStatus.current === 'exited' && status !== 'exited') term.reconnect();
    prevStatus.current = status;
  }, [status, term]);

  let overlay: string | null = null;
  if (showStartup) overlay = null; // the startup panel covers this
  else if (term.conn === 'connecting' && status !== 'exited') overlay = 'Connecting to the session…';
  else if (term.conn === 'closed') overlay = 'Connection lost. Reconnecting…';
  else if (term.conn === 'ended' && status !== 'exited') overlay = term.message ?? 'The terminal closed.';

  return (
    <div className="term-col">
      {/* On touch screens a tap only focuses the terminal in typing mode (the ⌨ key). */}
      <div className="term-wrap" onClick={() => (!touch || term.typing) && term.focus()}>
        <div className="term-host" ref={hostRef} />
        {showStartup && <StartupPanel session={session} onShowTerminal={() => setPeeked(startKey)} />}
        {overlay && (
          <div className="term-overlay">
            {overlay}
            {term.failed && term.conn === 'ended' && (
              <button className="btn small" onClick={() => term.reconnect()}>
                Try again
              </button>
            )}
          </div>
        )}
        {term.notice && (
          <div className="term-toast" role="status">
            {term.notice}
          </div>
        )}
        {status === 'exited' && (
          <div className="term-overlay">Claude exited. Use Resume to continue this conversation in the same worktree.</div>
        )}
      </div>
      {touch && <TermKeys term={term} sessionId={sessionId} disabled={status === 'exited'} />}
    </div>
  );
}
