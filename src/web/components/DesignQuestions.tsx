import { useEffect, useRef, useState } from 'react';
import type { DesignDoc, DesignQuestion, Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import {
  approvalSent, draftsBlocker, draftsPersist, loadDrafts, pruneDrafts, revisionSent, saveDrafts, shownDraft, toRequest, useDesignDrafts, type Draft,
} from '../design-answers';
import { cx, designApprovalBlocker, designReviseBlocker, heldForTurnEnd } from '../util';
import { AskForm } from './AskSheet';
import { Busy } from './common';

function StateChip({ q, d, answerable }: { q: DesignQuestion; d: Draft; answerable: boolean }) {
  if (q.status === 'resolved') return <span className="dq-chip done">✓ Resolved</span>;
  if (d.custom && d.text.trim() && q.options.length) return <span className="dq-chip custom">✎ Custom</span>;
  if (d.choice !== null || (d.custom && d.text.trim())) return <span className="dq-chip answered">✓ Answered</span>;
  return <span className="dq-chip open">{answerable ? '● Open' : '● Unanswered'}</span>;
}

function QuestionCard({
  q,
  n,
  d,
  answerable,
  editable,
  onChange,
}: {
  q: DesignQuestion;
  n: number;
  d: Draft;
  /** False past Design: nothing is picked for the user. */
  answerable: boolean;
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
        <StateChip q={q} d={d} answerable={answerable} />
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
 * they go to AgentK: with the approval, or to revise the design. Questions Claude is asking right
 * now (AskUserQuestion, through AgentK's mod) come first, answered on their own.
 */
export function DesignQuestions({ session, doc }: { session: Session; doc: DesignDoc }) {
  const [drafts, update] = useDesignDrafts(session.id);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [sending, setSending] = useState<'revise' | 'approve' | 'cancel' | null>(null);

  const editable = session.stage === 'design' && session.desired === 'running';
  // While an approval waits for the turn's end, the answers it took stay as they were.
  const canEdit = editable && !session.approvalQueued;
  const open = doc.questions.filter((q) => q.status === 'open');
  const resolved = doc.questions.filter((q) => q.status === 'resolved');
  const number = new Map(doc.questions.map((q, i) => [q.id, i + 1]));
  const request = toRequest(doc.questions, drafts);
  const notDesign = !editable ? 'Answers go to AgentK only while the session is in Design.' : null;
  const blocker = notDesign ?? draftsBlocker(doc.questions, drafts) ?? designApprovalBlocker(session);
  const reviseBlocker = notDesign ?? draftsBlocker(doc.questions, drafts) ?? designReviseBlocker(session)
    ?? (!request.answers.length && !request.feedback.trim() ? 'Answer a question or write feedback first.' : null);
  // AgentK is mid-turn with its mod running: answers go now and wait in Claude, an approval waits here.
  const held = heldForTurnEnd(session);
  // Answers to questions that are no longer open (resolved as they were sent) are done with.
  useEffect(() => {
    const pruned = pruneDrafts(doc.questions, loadDrafts(session.id));
    if (pruned) saveDrafts(session.id, pruned);
  }, [doc.questions, session.id]);
  const queued = session.approvalQueued
    ? 'Approved: it goes to AgentK when this turn ends. If the turn changes the design, you’re asked again.'
    : null;
  const waiting = held && !reviseBlocker ? 'AgentK is working: your answers go now, and it takes them when this turn ends.' : null;
  // Shown in the footer too, as tooltips aren't seen on a phone.
  const busy = session.busy ? 'AgentK is moving the session on. Wait a moment.' : null;

  const go = async (what: 'revise' | 'approve' | 'cancel') => {
    setSending(what);
    setNote(null);
    try {
      if (what === 'cancel') {
        await api.sessionAction(session.id, 'cancel-approval');
      } else if (what === 'revise') {
        await api.reviseDesign(session.id, request);
        revisionSent(session.id);
        setNote(held ? 'Sent. AgentK takes them when this turn ends, then revises the design.' : 'Sent to AgentK. The design is updated when its turn ends.');
      } else {
        // An approval queued for the turn's end keeps the answers until it's sent, in case it's dropped.
        approvalSent(await api.approveDesign(session.id, request));
      }
      setError(null);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setSending(null);
    }
  };

  return (
    <>
      <div className="dq-scroll">
        {session.asking && (
          <section className="dq-section" aria-label="AgentK is asking">
            <h4>AgentK is asking</h4>
            <AskForm key={session.asking.id} session={session} ask={session.asking} />
          </section>
        )}
        {!editable && <p className="muted small">The session is past Design, so these can no longer be answered here.</p>}
        {open.length ? (
          <ol className="dq-list">
            {open.map((q) => (
              <QuestionCard
                key={q.id}
                q={q}
                n={number.get(q.id) ?? 0}
                d={shownDraft(q, drafts.answers[q.id], editable)}
                answerable={editable}
                editable={canEdit}
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
          // Open when nothing else is, so what was answered shows at once.
          <details className="dq-section dq-decided" open={!open.length}>
            <summary>Decided ({resolved.length})</summary>
            <ol className="dq-list">
              {resolved.map((q) => (
                <li key={q.id} className="dq-card">
                  <div className="dq-top">
                    <span className="dq-n">Q{number.get(q.id)}</span>
                    <span className="dq-q">{q.question}</span>
                    <span className="dq-chip done">✓ Resolved</span>
                  </div>
                  {q.decision && (
                    <p className="dq-decision">
                      <span className="dq-decision-label">Decision</span> {q.decision}
                    </p>
                  )}
                </li>
              ))}
            </ol>
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
              disabled={!canEdit}
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
            {error ? <span className="error-text">{error}</span> : (note ?? busy ?? queued ?? waiting ?? reviseBlocker ?? '')}
          </span>
          <button
            className="btn small"
            disabled={!!reviseBlocker || !!sending || !!busy}
            title={busy ?? reviseBlocker ?? 'AgentK revises the design to match your answers'}
            onClick={() => void go('revise')}
          >
            {sending === 'revise' ? <Busy label="Sending" /> : 'Send for revision'}
          </button>
          {session.approvalQueued ? (
            <button className="btn small" disabled={!!sending} onClick={() => void go('cancel')}>
              {sending === 'cancel' ? <Busy label="Cancelling" /> : 'Cancel approval'}
            </button>
          ) : (
            <button
              className="btn small primary"
              disabled={!!blocker || !!sending || !!busy}
              title={busy ?? blocker ?? (held
                ? 'AgentK is mid-turn: the approval goes when the turn ends, if the design stays the same'
                : 'Approve the design with your answers, and move to Code')}
              onClick={() => void go('approve')}
            >
              {sending === 'approve' ? <Busy label="Approving" /> : open.length ? 'Approve with answers' : 'Approve design'}
            </button>
          )}
        </div>
      )}
    </>
  );
}
