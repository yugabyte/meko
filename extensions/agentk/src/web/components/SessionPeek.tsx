import { useEffect, useState, type KeyboardEvent } from 'react';
import { QUICK_PROMPT_MAX, type Session, type SessionPeek as Peek } from '../../shared/types';
import { api, errorMessage } from '../api';
import { useTouch } from '../hooks/useMedia';
import { STATUS_TEXT, agentName, dotState, isMac, relativeTime } from '../util';
import { Busy, ErrorText } from './common';
import { MarkdownView } from './MarkdownView';

/**
 * A quick look at a work item's session without opening it: its status, the tool it's running (or ran
 * last) and its last reply, read again as the session moves, and a box to send it a prompt.
 */
export function SessionPeek({ session }: { session: Session }) {
  const touch = useTouch();
  const [peek, setPeek] = useState<Peek | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [text, setText] = useState('');
  const [sending, setSending] = useState(false);
  const [sent, setSent] = useState(false);
  const agent = agentName(session);

  // Read again whenever the session reports something (a hook event, a status change).
  useEffect(() => {
    let live = true;
    api.peekSession(session.id).then(
      (p) => live && (setPeek(p), setError(null)),
      (e: unknown) => live && setError(errorMessage(e)),
    );
    return () => {
      live = false;
    };
  }, [session.id, session.lastEventAt, session.status]);

  const canSend = session.status !== 'exited' && session.status !== 'starting';
  const send = async () => {
    if (!text.trim() || sending || !canSend) return;
    setSending(true);
    setError(null);
    try {
      await api.promptSession(session.id, text);
      setText('');
      setSent(true);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSending(false);
    }
  };
  const onKey = (e: KeyboardEvent<HTMLTextAreaElement>) => {
    if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      void send();
    }
  };

  const tool = peek?.tool && (!peek.reply?.at || !peek.tool.at || peek.tool.at >= peek.reply.at) ? peek.tool : null;
  return (
    <div className="wq-peek">
      <div className="wq-peek-state small">
        <b>{STATUS_TEXT[dotState(session)]}</b>
        {session.statusDetail && <span className="muted"> · {session.statusDetail}</span>}
        {tool && (
          <div className="muted">
            {session.status === 'working' ? 'Now: ' : 'Last: '}
            <code>{tool.name}</code>
            {tool.summary && ` ${tool.summary}`}
          </div>
        )}
      </div>
      {!peek && !error && <Busy label="Loading" />}
      {peek && !peek.readable && <p className="muted small">AgentK can't read {agent}'s replies here; open the session to see them.</p>}
      {peek?.readable && !peek.reply && <p className="muted small">{agent} hasn't replied yet.</p>}
      {peek?.reply && (
        <div className="wq-peek-reply">
          <div className="muted small">Last reply{peek.reply.at ? ` · ${relativeTime(peek.reply.at)}` : ''}</div>
          <div className="wq-peek-text">
            <MarkdownView markdown={peek.reply.text} />
          </div>
        </div>
      )}
      <form
        className="wq-peek-send"
        onSubmit={(e) => {
          e.preventDefault();
          void send();
        }}
      >
        <textarea
          className="input"
          rows={2}
          value={text}
          maxLength={QUICK_PROMPT_MAX}
          placeholder={canSend ? `Message ${agent}…` : session.status === 'exited' ? `${agent} isn't running: open the session to resume it` : `${agent} is starting`}
          disabled={!canSend}
          aria-label={`Message ${agent} in ${session.name}`}
          onChange={(e) => {
            setText(e.target.value);
            setSent(false);
          }}
          onKeyDown={onKey}
        />
        <button className="btn primary small" type="submit" disabled={!text.trim() || sending || !canSend} title={touch ? undefined : `${isMac() ? '⌘' : 'Ctrl'}+Enter`}>
          {sending ? <Busy label="Sending" /> : 'Send'}
        </button>
      </form>
      {sent && <p className="muted small">Sent. {session.status === 'working' ? `${agent} takes it when this turn ends.` : ''}</p>}
      <ErrorText error={error} />
    </div>
  );
}
