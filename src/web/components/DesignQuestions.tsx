import { useCallback, useEffect, useRef, useState } from 'react';
import {
  DESIGN_FEEDBACK_ID,
  type DesignAnswer,
  type DesignAnswerUpdate,
  type DesignDoc,
  type DesignQuestion,
  type Session,
} from '../../shared/types';
import { api, errorMessage } from '../api';
import { answerSummary, answersByQuestion, plural, sendLabel } from '../design-answers';
import { cx } from '../util';
import { Busy } from './common';

/**
 * A textarea whose text is saved as you type (after a pause, on blur, and when it goes away).
 * Text from the server replaces it only while it isn't being edited. It's reload-safe only while
 * its text is what the server has, so the reload after an update waits for unsaved text (one
 * still being typed, or a save that failed).
 */
function AutosaveText({
  id,
  value,
  disabled,
  placeholder,
  label,
  rows = 2,
  onSave,
}: {
  id: string;
  value: string;
  disabled: boolean;
  placeholder: string;
  label: string;
  rows?: number;
  onSave: (text: string) => void;
}) {
  const [text, setText] = useState(value);
  const editing = useRef(false);
  const pending = useRef<string | null>(null);
  const timer = useRef<number | undefined>(undefined);
  const save = useRef(onSave);
  useEffect(() => {
    save.current = onSave;
  });

  useEffect(() => {
    if (!editing.current && pending.current === null) setText(value);
  }, [value]);

  const flush = useCallback(() => {
    window.clearTimeout(timer.current);
    if (pending.current !== null) {
      const t = pending.current;
      pending.current = null;
      save.current(t);
    }
  }, []);
  // Saves what's left when the card goes away (a tab switch, a revised design).
  useEffect(() => flush, [flush]);

  return (
    <textarea
      id={id}
      className="input dq-text"
      rows={rows}
      value={text}
      disabled={disabled}
      placeholder={placeholder}
      aria-label={label}
      data-reload-safe={text === value ? '' : undefined}
      onFocus={() => {
        editing.current = true;
      }}
      onBlur={() => {
        editing.current = false;
        flush();
      }}
      onChange={(e) => {
        const t = e.target.value;
        setText(t);
        pending.current = t;
        window.clearTimeout(timer.current);
        timer.current = window.setTimeout(flush, 800);
      }}
    />
  );
}

function StateChip({ q, unsent, sent }: { q: DesignQuestion; unsent?: DesignAnswer; sent?: DesignAnswer }) {
  if (q.status === 'resolved') return <span className="dq-chip done">✓ Resolved</span>;
  if (unsent?.state === 'queued') return <span className="dq-chip queued">◔ Waiting to send</span>;
  if (unsent) return <span className="dq-chip answered">✓ Answered</span>;
  if (sent) return <span className="dq-chip sent">↗ Sent</span>;
  return <span className="dq-chip open">● Open</span>;
}

function QuestionCard({
  q,
  n,
  unsent,
  sent,
  editable,
  onSave,
  onClear,
}: {
  q: DesignQuestion;
  n: number;
  unsent?: DesignAnswer;
  sent?: DesignAnswer;
  editable: boolean;
  onSave: (b: DesignAnswerUpdate) => void;
  onClear: () => void;
}) {
  const locked = !editable || unsent?.state === 'queued';
  const name = `dq-${q.id}`;
  return (
    <li className="dq-card">
      <div className="dq-top">
        <span className="dq-n">Q{n}</span>
        <span className="dq-q">{q.question}</span>
        <StateChip q={q} unsent={unsent} sent={sent} />
      </div>
      {q.context && <p className="dq-ctx">{q.context}</p>}
      {sent && !unsent && <p className="dq-sent">You sent: {answerSummary(sent)}</p>}
      {unsent && unsent.choice === null && unsent.choiceLabel !== null && (
        <p className="dq-sent">
          You picked “{unsent.choiceLabel}”, which is no longer one of AgentK’s options. Pick again, or send it as it is.
        </p>
      )}
      <div className="dq-opts" role="radiogroup" aria-label={`Answer to Q${n}`}>
        {q.options.map((o, i) => (
          <label key={i} className={cx('dq-opt', unsent?.choice === i && 'on')}>
            <input type="radio" name={name} checked={unsent?.choice === i} disabled={locked} onChange={() => onSave({ choice: i })} />
            <span className="dq-opt-text">
              <span className="dq-label">
                {o.label}
                {q.recommended === i && <span className="dq-rec">Recommended</span>}
              </span>
              {o.detail && <span className="dq-detail">{o.detail}</span>}
            </span>
          </label>
        ))}
        <label className={cx('dq-opt', unsent?.yourCall && 'on')}>
          <input type="radio" name={name} checked={!!unsent?.yourCall} disabled={locked} onChange={() => onSave({ yourCall: true })} />
          <span className="dq-opt-text">
            <span className="dq-label">Your call</span>
            <span className="dq-detail">
              {q.recommended !== null ? 'AgentK goes with its recommendation.' : 'AgentK decides, and says what it picked.'}
            </span>
          </span>
        </label>
      </div>
      <AutosaveText
        id={`dq-text-${q.id}`}
        value={unsent?.text ?? ''}
        disabled={locked}
        placeholder={q.options.length ? 'Add a note (optional)' : 'Your answer'}
        label={`Note on Q${n}`}
        onSave={(text) => onSave({ text })}
      />
      {unsent?.state === 'draft' && editable && (
        <button className="link-btn dq-clear" onClick={onClear}>
          Clear answer
        </button>
      )}
    </li>
  );
}

/**
 * The design's questions for the user, with their answers: open ones to answer (an option, Your
 * call, or text), what's decided, answers to questions no longer open, other feedback, and Send.
 */
export function DesignQuestions({ session, doc }: { session: Session; doc: DesignDoc }) {
  const [answers, setAnswers] = useState<DesignAnswer[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  // Responses can come back out of order; only the newest request's answers are kept.
  const seq = useRef(0);

  const editable = session.stage === 'design' && session.desired === 'running';

  const load = useCallback(() => {
    const n = ++seq.current;
    api
      .designAnswers(session.id)
      .then((a) => n === seq.current && setAnswers(a))
      .catch((e) => n === seq.current && setError(errorMessage(e)));
  }, [session.id]);
  // Again when the design is revised, or any answer changes (here, in another tab, a send, an approval).
  useEffect(load, [load, session.designUpdatedAt, session.designQuestions.changedAt, session.stage]);

  const save = async (qid: string, b: DesignAnswerUpdate) => {
    const n = ++seq.current;
    try {
      const list = await api.putDesignAnswer(session.id, qid, b);
      if (n === seq.current) setAnswers(list);
      setError(null);
    } catch (e) {
      setError(errorMessage(e));
      load();
    }
  };
  const clear = async (qid: string) => {
    try {
      await api.deleteDesignAnswer(session.id, qid);
      setError(null);
    } catch (e) {
      setError(errorMessage(e));
    }
    load();
  };
  const send = async () => {
    setSending(true);
    setNote(null);
    try {
      const r = await api.sendDesignAnswers(session.id);
      setNote(r.sent ? 'Sent to AgentK. The design is updated when its turn ends.' : 'Queued: they go to AgentK when its turn ends.');
      setError(null);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSending(false);
      load();
    }
  };

  if (!answers) {
    return (
      <div className="dq-scroll">
        {error ? <p className="error-text">{error}</p> : <p className="muted"><Busy label="Loading" /></p>}
      </div>
    );
  }

  const { unsent, sent } = answersByQuestion(answers);
  const open = doc.questions.filter((q) => q.status === 'open');
  const resolved = doc.questions.filter((q) => q.status === 'resolved');
  const number = new Map(doc.questions.map((q, i) => [q.id, i + 1]));
  const openIds = new Set(open.map((q) => q.id));
  const orphans = [...unsent.values()].filter((a) => a.questionId !== DESIGN_FEEDBACK_ID && !openIds.has(a.questionId));
  const drafts = [...unsent.values()].filter((a) => a.state === 'draft');
  const queued = [...unsent.values()].filter((a) => a.state === 'queued').length;
  const feedback = unsent.get(DESIGN_FEEDBACK_ID);
  const blocker = !editable
    ? 'Answers go to AgentK only while the session is in Design.'
    : session.status === 'exited'
      ? 'Claude has exited. Resume the session first.'
      : !drafts.length
        ? 'Answer a question or write feedback first.'
        : null;

  return (
    <>
      <div className="dq-scroll">
        {!editable && <p className="muted small">The session is past Design, so these can no longer be answered here.</p>}
        {open.length ? (
          <ol className="dq-list">
            {open.map((q) => (
              <QuestionCard
                key={q.id}
                q={q}
                n={number.get(q.id) ?? 0}
                unsent={unsent.get(q.id)}
                sent={sent.get(q.id)}
                editable={editable}
                onSave={(b) => void save(q.id, b)}
                onClear={() => void clear(q.id)}
              />
            ))}
          </ol>
        ) : (
          <p className="muted">
            {resolved.length
              ? 'Every question is resolved.'
              : doc.questionsSource === null
                ? 'AgentK listed no open questions for this design.'
                : 'No open questions.'}{' '}
            {editable && 'You can still send AgentK feedback on the design below.'}
          </p>
        )}
        {doc.questionsSource === 'markdown' && open.length > 0 && (
          <p className="muted small">These are the items under the design’s “Open questions” heading.</p>
        )}

        {orphans.length > 0 && (
          <section className="dq-section">
            <h4>Answers to questions no longer open</h4>
            <p className="muted small">AgentK changed or resolved these questions after you answered. They still go with the others when you send.</p>
            <ul className="dq-orphans">
              {orphans.map((a) => (
                <li key={a.id}>
                  <span className="dq-q">{a.question}</span>
                  <span className="dq-sent">{answerSummary(a)}</span>
                  {a.state === 'draft' && editable && (
                    <button className="link-btn" onClick={() => void clear(a.questionId)}>
                      Delete
                    </button>
                  )}
                </li>
              ))}
            </ul>
          </section>
        )}

        {resolved.length > 0 && (
          <details className="dq-section dq-decided">
            <summary>Decided ({resolved.length})</summary>
            <ul>
              {resolved.map((q) => (
                <li key={q.id}>
                  <span className="dq-n">Q{number.get(q.id)}</span> <span className="dq-q">{q.question}</span>
                  {q.decision && <p className="dq-ctx">{q.decision}</p>}
                </li>
              ))}
            </ul>
          </details>
        )}

        {(editable || feedback) && (
          <section className="dq-section">
            <label className="dq-fb-label" htmlFor="dq-text-feedback">
              Other feedback on the design
            </label>
            <AutosaveText
              id="dq-text-feedback"
              value={feedback?.text ?? ''}
              disabled={!editable || feedback?.state === 'queued'}
              placeholder="Anything else AgentK should change"
              label="Other feedback on the design"
              rows={3}
              onSave={(text) => void save(DESIGN_FEEDBACK_ID, { text })}
            />
          </section>
        )}
      </div>
      <div className="dq-foot">
        <span className="dq-foot-note">
          {error ? (
            <span className="error-text">{error}</span>
          ) : note ? (
            note
          ) : queued ? (
            `${plural(queued, 'answer')} waiting for AgentK’s turn to end.`
          ) : drafts.length && session.status === 'working' ? (
            'AgentK is working. Send now, and your answers go when its turn ends.'
          ) : (
            blocker && drafts.length ? blocker : ''
          )}
        </span>
        <button className="btn small primary" disabled={!!blocker || sending} title={blocker ?? undefined} onClick={() => void send()}>
          {sending ? <Busy label="Sending" /> : sendLabel(drafts)}
        </button>
      </div>
    </>
  );
}
