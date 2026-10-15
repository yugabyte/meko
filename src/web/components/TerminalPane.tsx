import { useEffect, useReducer, useRef, useState, type ReactNode } from 'react';
import type { Session } from '../../shared/types';
import { useTouch } from '../hooks/useMedia';
import { getTerminal, screenLabel } from '../terminals';
import { AskBar } from './AskSheet';
import { Busy } from './common';
import { HeldImages } from './HeldImages';
import { KnowledgeUploadDialog } from './KnowledgeUploadDialog';
import { StartupPanel } from './StartupPanel';
import { TermKeys } from './TermKeys';

/**
 * Where files for the knowledge base go: the project's dedicated datapack, or why they can't go
 * anywhere (Meko is off, or the project has no datapack).
 */
export type KnowledgeTarget = { datapack: string } | { off: string };

/**
 * `onAsk` opens the questions Claude is asking (Session.asking), offered in a bar above the terminal.
 * `knowledge` says where files dropped for the knowledge base go.
 */
export function TerminalPane({ session, onAsk, knowledge }: { session: Session; onAsk?: () => void; knowledge: KnowledgeTarget }) {
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

  // Files for the knowledge base with nowhere to go: say why, and drop them.
  const kbOff = 'off' in knowledge ? knowledge.off : null;
  useEffect(() => {
    if (!term.knowledge || !kbOff) return;
    term.dropKnowledge();
    term.showNotice(kbOff, 6000);
  }, [term, term.knowledge, kbOff]);

  // While waiting for the session, count down to the move each second.
  const waiting = term.conn === 'open' && term.screen === 'waiting';
  useEffect(() => {
    if (!waiting) return;
    const t = setInterval(rerender, 1000);
    return () => clearInterval(t);
  }, [waiting]);

  let overlay: ReactNode = null;
  /** The session is live on another screen: this one shows its last picture, dimmed. */
  const away = term.conn === 'open' && term.screen !== 'here' && status !== 'exited';
  if (showStartup) overlay = null; // the startup panel covers this
  else if (term.conn === 'connecting' && status !== 'exited') overlay = <Busy label="Connecting to the session" />;
  else if (term.conn === 'closed') overlay = <Busy label="Connection lost. Reconnecting" />;
  else if (term.conn === 'ended' && status !== 'exited')
    overlay = term.retrying && term.message ? <Busy label={term.message} /> : (term.message ?? 'The terminal closed.');
  else if (away && term.screen === 'waiting') {
    const secs = Math.max(1, Math.ceil((term.moveAt - Date.now()) / 1000));
    overlay = `This session is in use on ${screenLabel(term.other)}. It moves here in ${secs} s unless that screen is used.`;
  } else if (away) overlay = term.other ? `This session is open on ${screenLabel(term.other)}.` : 'This session was open on another screen.';

  return (
    <div className="term-col">
      {session.asking && onAsk && <AskBar ask={session.asking} onOpen={onAsk} />}
      {/* On touch screens a tap only focuses the terminal in typing mode (the ⌨ key); on another screen's session it still asks for it. */}
      <div
        className={away ? 'term-wrap away' : 'term-wrap'}
        onClick={() => {
          if (!touch || term.typing) term.focus();
          else if (away) term.show();
        }}
      >
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
            {away && (
              <button
                className="btn small"
                onClick={(e) => {
                  e.stopPropagation();
                  term.moveHere();
                }}
              >
                Move to this screen
              </button>
            )}
          </div>
        )}
        {term.notice && (
          <div className="term-toast" role="status">
            {term.noticeBusy ? <Busy label={term.notice} /> : term.notice}
          </div>
        )}
        {status === 'exited' && (
          <div className="term-overlay">Claude exited. Use Resume to continue this conversation in the same worktree.</div>
        )}
      </div>
      {term.knowledge && 'datapack' in knowledge && (
        <KnowledgeUploadDialog
          datapackName={knowledge.datapack}
          batch={term.knowledge}
          onUpload={() => void term.uploadKnowledge()}
          onClose={() => term.closeKnowledge()}
        />
      )}
      {/* Touch screens show held images above the compose box instead. */}
      {!touch && <HeldImages held={term.held} hint="sent with your next Enter" />}
      {touch && <TermKeys term={term} sessionId={sessionId} disabled={status === 'exited'} />}
    </div>
  );
}
