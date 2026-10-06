import { useRef, useState } from 'react';
import type { DesignDoc, DesignQuestion, Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import { clearDrafts, draftFor, draftsBlocker, draftsPersist, toRequest, useDesignDrafts, type Draft } from '../design-answers';
import { cx, designApprovalBlocker } from '../util';
import { Busy } from './common';

function StateChip({ q, d }: { q: DesignQuestion; d: Draft }) {
  if (q.status === 'resolved') return <span className="dq-chip done">✓ Resolved</span>;
  if (d.custom && d.text.trim() && q.options.length) return <span className="dq-chip custom">✎ Custom</span>;
  if (d.choice !== null || (d.custom && d.text.trim())) return <span className="dq-chip answered">✓ Answered</span>;
  return <span className="dq-chip open">● Open</span>;
}

function QuestionCard({
  q,
  n,
  d,
  editable,
  onChange,
}: {
  q: DesignQuestion;
  n: number;
  d: Draft;
  editable: boolean;
  onChange: (d: Draft) => void;
}) {
  const name = `dq-${q.id}`;
  const text = useRef<HTMLTextAreaElement>(null);
  const answer = d.custom && q.options.length > 0;
  return (
    <li className="dq-card">
      <div className="dq-top">
        <span className="dq-n">Q{n}</span>
        <span className="dq-q">{q.question}</span>
        <StateChip q={q} d={d} />
      </div>
      {q.context && <p className="dq-ctx">{q.context}</p>}
      {q.options.length > 0 && (
        <div className="dq-opts" role="radiogroup" aria-label={`Answer to Q${n}`}>
          {q.options.map((o, i) => (
            <label key={i} className={cx('dq-opt', !d.custom && d.choice === i && 'on')}>
              <input
                type="radio"
                name={name}
                checked={!d.custom && d.choice === i}
                disabled={!editable}
                onChange={() => onChange({ ...d, choice: i, label: o.label, custom: false })}
              />
              <span className="dq-opt-text">
                <span className="dq-label">
                  {o.label}
                  {q.recommended === i && <span className="dq-rec">Recommended</span>}
                </span>
                {o.detail && <span className="dq-detail">{o.detail}</span>}
              </span>
            </label>
          ))}
          <label className={cx('dq-opt', d.custom && 'on')}>
            <input
              type="radio"
              name={name}
              checked={d.custom}
              disabled={!editable}
              onChange={() => {
                onChange({ ...d, choice: null, label: null, custom: true });
                // Picking Custom answer is to write one.
                text.current?.focus();
              }}
            />
            <span className="dq-opt-text">
              <span className="dq-label">Custom answer</span>
              <span className="dq-detail">Tell AgentK what you want instead.</span>
            </span>
          </label>
        </div>
      )}
      <textarea
        ref={text}
        id={`dq-text-${q.id}`}
        className="input dq-text"
        rows={2}
        value={d.text}
        disabled={!editable}
        placeholder={answer ? 'Your answer' : q.options.length ? 'Add a note (optional)' : 'Your answer (or leave it to AgentK)'}
        aria-label={answer || !q.options.length ? `Your answer to Q${n}` : `Note on Q${n}`}
        data-reload-safe={draftsPersist() ? '' : undefined}
        onChange={(e) => onChange({ ...d, text: e.target.value })}
      />
    </li>
  );
}

/**
 * The design's questions for the user: open ones to answer (an option, with an optional note, or
 * a custom answer), what's decided, and other feedback. The answers stay in this browser until
 * they go to AgentK: with the approval, or to revise the design.
 */
export function DesignQuestions({ session, doc }: { session: Session; doc: DesignDoc }) {
  const [drafts, update] = useDesignDrafts(session.id);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [sending, setSending] = useState<'revise' | 'approve' | null>(null);

  const editable = session.stage === 'design' && session.desired === 'running';
  const open = doc.questions.filter((q) => q.status === 'open');
  const resolved = doc.questions.filter((q) => q.status === 'resolved');
  const number = new Map(doc.questions.map((q, i) => [q.id, i + 1]));
  const request = toRequest(doc.questions, drafts);
  const blocker = !editable
    ? 'Answers go to AgentK only while the session is in Design.'
    : (draftsBlocker(doc.questions, drafts) ?? designApprovalBlocker(session));
  const reviseBlocker = blocker ?? (!request.answers.length && !request.feedback.trim() ? 'Answer a question or write feedback first.' : null);
  // Shown in the footer too, as tooltips aren't seen on a phone.
  const busy = session.busy ? 'AgentK is moving the session on. Wait a moment.' : null;

  const go = async (what: 'revise' | 'approve') => {
    setSending(what);
    setNote(null);
    try {
      if (what === 'revise') await api.reviseDesign(session.id, request);
      else await api.approveDesign(session.id, request);
      clearDrafts(session.id);
      setError(null);
      if (what === 'revise') setNote('Sent to AgentK. The design is updated when its turn ends.');
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSending(null);
    }
  };

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
                d={draftFor(q, drafts.answers[q.id])}
                editable={editable}
                onChange={(d) => update((x) => ({ ...x, answers: { ...x.answers, [q.id]: d } }))}
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

        {editable && (
          <section className="dq-section">
            <label className="dq-fb-label" htmlFor="dq-text-feedback">
              Other feedback on the design
            </label>
            <textarea
              id="dq-text-feedback"
              className="input dq-text"
              rows={3}
              value={drafts.feedback}
              placeholder="Anything else AgentK should change"
              data-reload-safe={draftsPersist() ? '' : undefined}
              onChange={(e) => {
                const feedback = e.target.value;
                update((x) => ({ ...x, feedback }));
              }}
            />
          </section>
        )}
      </div>
      {editable && (
        <div className="dq-foot">
          <span className="dq-foot-note">
            {error ? <span className="error-text">{error}</span> : (note ?? busy ?? reviseBlocker ?? '')}
          </span>
          <button
            className="btn small"
            disabled={!!reviseBlocker || !!sending || !!busy}
            title={busy ?? reviseBlocker ?? 'AgentK revises the design to match your answers'}
            onClick={() => void go('revise')}
          >
            {sending === 'revise' ? <Busy label="Sending" /> : 'Send for revision'}
          </button>
          <button
            className="btn small primary"
            disabled={!!blocker || !!sending || !!busy}
            title={busy ?? blocker ?? 'Approve the design with your answers, and move to Code'}
            onClick={() => void go('approve')}
          >
            {sending === 'approve' ? <Busy label="Approving" /> : open.length ? 'Approve with answers' : 'Approve design'}
          </button>
        </div>
      )}
    </>
  );
}
