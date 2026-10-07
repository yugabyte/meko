import { useRef, useState } from 'react';
import type { AskAnswerInput, AskQuestion, AskRequest, Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import { useIsMobile } from '../hooks/useMedia';
import { cx } from '../util';
import { Busy, Dialog } from './common';

/** One question's answer as the user builds it: the options picked, and "Other" with its text. */
interface Pick {
  labels: string[];
  other: boolean;
  text: string;
}

const NONE: Pick = { labels: [], other: false, text: '' };

/** The answer to send for a question, or null when it has none (left unanswered). */
function toAnswer(q: AskQuestion, i: number, p: Pick): AskAnswerInput | null {
  const text = p.other ? p.text.trim() : '';
  const labels = q.multiSelect ? p.labels : p.labels.slice(0, 1);
  return labels.length || text ? { question: i, labels, text } : null;
}

/**
 * An option's preview, kept as preformatted text so tables and code line up, with a **Wrap lines**
 * toggle for lines too long for the screen. Wrapped from the start on a phone, where most are.
 */
function AskPreview({ text }: { text: string }) {
  const mobile = useIsMobile();
  const [wrap, setWrap] = useState<boolean | null>(null);
  const on = wrap ?? mobile;
  return (
    <>
      <span className="ask-preview-hd">
        <button
          type="button"
          className={cx('btn small', on && 'on')}
          aria-pressed={on}
          // The preview is inside the option's label: toggling must not pick the option.
          onClick={(e) => {
            e.preventDefault();
            setWrap(!on);
          }}
        >
          Wrap lines
        </button>
      </span>
      <pre className={cx('ask-preview', on && 'wrap')}>{text}</pre>
    </>
  );
}

function AskCard({ q, n, p, disabled, onChange }: { q: AskQuestion; n: number; p: Pick; disabled: boolean; onChange: (p: Pick) => void }) {
  const text = useRef<HTMLTextAreaElement>(null);
  const name = `ask-${n}`;
  const pick = (label: string) => {
    if (!q.multiSelect) onChange({ ...p, labels: [label], other: false });
    else onChange({ ...p, labels: p.labels.includes(label) ? p.labels.filter((l) => l !== label) : [...p.labels, label] });
  };
  const other = () => {
    onChange(q.multiSelect ? { ...p, other: !p.other } : { ...p, labels: [], other: true });
    // Picking Other is to write an answer.
    if (!p.other) setTimeout(() => text.current?.focus(), 0);
  };
  return (
    <li className="dq-card">
      <div className="dq-top">
        {q.header && <span className="dq-n">{q.header}</span>}
        <span className="dq-q">{q.question}</span>
        {q.multiSelect && <span className="dq-chip">Pick any</span>}
      </div>
      <div className="dq-opts" role={q.multiSelect ? 'group' : 'radiogroup'} aria-label={q.question}>
        {q.options.map((o) => {
          const on = p.labels.includes(o.label) && (q.multiSelect || !p.other);
          return (
            <label key={o.label} className={cx('dq-opt', on && 'on')}>
              <input type={q.multiSelect ? 'checkbox' : 'radio'} name={name} checked={on} disabled={disabled} onChange={() => pick(o.label)} />
              <span className="dq-opt-text">
                <span className="dq-label">{o.label}</span>
                {o.description && <span className="dq-detail">{o.description}</span>}
                {o.preview && <AskPreview text={o.preview} />}
              </span>
            </label>
          );
        })}
        <label className={cx('dq-opt', p.other && 'on')}>
          <input type={q.multiSelect ? 'checkbox' : 'radio'} name={name} checked={p.other} disabled={disabled} onChange={other} />
          <span className="dq-opt-text">
            <span className="dq-label">Other</span>
            <span className="dq-detail">Answer in your own words.</span>
          </span>
        </label>
      </div>
      {p.other && (
        <textarea
          ref={text}
          className="input dq-text"
          rows={2}
          value={p.text}
          disabled={disabled}
          placeholder="Your answer"
          aria-label={`Your answer to: ${q.question}`}
          onChange={(e) => onChange({ ...p, text: e.target.value })}
        />
      )}
    </li>
  );
}

/**
 * The questions Claude is asking with AskUserQuestion (`Session.asking`), to answer here instead of
 * in the terminal's dialog, which stays open meanwhile: whichever answers first wins. Key it by the
 * ask's id, so a new ask starts blank.
 */
export function AskForm({ session, ask, onDone }: { session: Session; ask: AskRequest; onDone?: () => void }) {
  const [picks, setPicks] = useState<Record<number, Pick>>({});
  const [sending, setSending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const answers = ask.questions.map((q, i) => toAnswer(q, i, picks[i] ?? NONE)).filter((a): a is AskAnswerInput => !!a);
  const blank = ask.questions.findIndex((_, i) => picks[i]?.other && !picks[i].text.trim() && !picks[i].labels.length);
  const blocker = blank >= 0
    ? `Write your answer to “${ask.questions[blank].question}”, or pick an option.`
    : !answers.length ? 'Pick an answer.' : null;

  const send = async () => {
    setSending(true);
    setError(null);
    try {
      await api.answerAsk(session.id, ask.id, { answers });
      onDone?.();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSending(false);
    }
  };

  return (
    <div className="ask-form">
      <ol className="dq-list">
        {ask.questions.map((q, i) => (
          <AskCard key={i} q={q} n={i} p={picks[i] ?? NONE} disabled={sending} onChange={(p) => setPicks((x) => ({ ...x, [i]: p }))} />
        ))}
      </ol>
      <div className="ask-foot">
        <span className="dq-foot-note">
          {error ? <span className="error-text">{error}</span> : (blocker ?? 'The terminal shows the same question: whichever you answer first counts.')}
        </span>
        <button className="btn small primary" disabled={!!blocker || sending} onClick={() => void send()}>
          {sending ? <Busy label="Answering" /> : 'Answer'}
        </button>
      </div>
    </div>
  );
}

/** "AgentK asks: …" above the terminal while Claude asks something, opening the questions to answer. */
export function AskBar({ ask, onOpen }: { ask: AskRequest; onOpen: () => void }) {
  const more = ask.questions.length - 1;
  return (
    <div className="ask-bar" role="status">
      <span className="ask-bar-text">
        <b>AgentK asks:</b> {ask.questions[0]?.question}
        {more > 0 && <span className="muted"> (+{more} more)</span>}
      </span>
      <button className="btn small primary" onClick={onOpen}>
        Answer
      </button>
    </div>
  );
}

/**
 * The questions Claude is asking, over the terminal: it fills the window (a full-height sheet on a
 * phone), so previews have room; the questions scroll while the footer keeps **Answer** in view.
 */
export function AskSheet({ session, onClose }: { session: Session; onClose: () => void }) {
  const ask = session.asking;
  return (
    <Dialog title="AgentK is asking" className="ask-dialog" onClose={onClose}>
      {ask ? (
        <AskForm key={ask.id} session={session} ask={ask} onDone={onClose} />
      ) : (
        <p className="muted">AgentK has its answer: it was given in the terminal, or AgentK moved on.</p>
      )}
    </Dialog>
  );
}
