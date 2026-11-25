import { useEffect, useRef, useState, type ReactNode } from 'react';
import { decisionNumbers } from '../../shared/decisions';
import type { DatapackRef, DesignDoc, DesignQuestion, Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import {
  approvalSent, changeDraft, decidedOption, draftFor, draftsBlocker, draftsPersist, loadDrafts, pruneDrafts, revisionSent, saveDrafts, shownDraft, toRequest, useDesignDrafts,
  useRevision,
  type Draft,
} from '../design-answers';
import { cx, designApprovalBlocker, designReviseBlocker, heldForTurnEnd, isMac, isSubmitKey, SESSION_BUSY } from '../util';
import { AskForm } from './AskSheet';
import { Busy, RunToMergeCheck } from './common';
import { MekoBadge, MekoRefs } from './MekoRefs';

function StateChip({ q, d, answerable }: { q: DesignQuestion; d: Draft; answerable: boolean }) {
  if (q.kind === 'decision') return <span className="dq-chip custom">✎ Changing</span>;
  if (q.status === 'resolved') return <span className="dq-chip done">✓ Resolved</span>;
  if (d.custom && d.text.trim() && q.options.length) return <span className="dq-chip custom">✎ Custom</span>;
  if (d.choice !== null || (d.custom && d.text.trim())) return <span className="dq-chip answered">✓ Answered</span>;
  return <span className="dq-chip open">{answerable ? '● Open' : '● Unanswered'}</span>;
}

/**
 * Who settled a key design decision: the user, Kode on its own, or Kode on a question left to it. A
 * file from before `decidedBy` reads from its decision's words.
 */
function DecidedChip({ q }: { q: DesignQuestion }) {
  if (q.decidedBy === 'user') return <span className="dq-chip user">You decided</span>;
  if (q.kind === 'decision') return <span className="dq-chip agent">Kode decided</span>;
  if (q.decidedBy === 'kode' || q.decision?.startsWith('Left to Kode') || q.decision?.startsWith('Left to AgentK')) return <span className="dq-chip">Left to Kode</span>;
  return <span className="dq-chip done">✓ Resolved</span>;
}

/** The options weighed against the one the decision stands on now. */
function alsoWeighed(q: DesignQuestion): string[] {
  const i = decidedOption(q);
  return q.options.filter((_, j) => j !== i).map((o) => o.label);
}

function QuestionCard({
  q,
  n,
  d,
  answerable,
  editable,
  onChange,
  datapacks,
  lead,
  foot,
}: {
  q: DesignQuestion;
  /** Its number as the user sees it: Q1, or D1 for one of Kode's own decisions. */
  n: string;
  d: Draft;
  /** False past Design: nothing is picked for the user. */
  answerable: boolean;
  editable: boolean;
  onChange: (d: Draft) => void;
  datapacks?: DatapackRef[];
  /** Under the context: for a decision being changed, what Kode decided. */
  lead?: ReactNode;
  foot?: ReactNode;
}) {
  const name = `dq-${q.id}`;
  const text = useRef<HTMLTextAreaElement>(null);
  const answer = d.custom && q.options.length > 0;
  return (
    <li className="dq-card">
      <div className="dq-top">
        <span className="dq-n">{n}</span>
        <span className="dq-q">{q.question}</span>
        {q.meko.length > 0 && <MekoBadge />}
        <StateChip q={q} d={d} answerable={answerable} />
      </div>
      {q.context && <p className="dq-ctx">{q.context}</p>}
      {lead}
      <MekoRefs refs={q.meko} datapacks={datapacks} />
      {q.options.length > 0 && (
        <div className="dq-opts" role="radiogroup" aria-label={`Answer to ${n}`}>
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
                  {q.recommended === i && <span className="dq-rec">{q.kind === 'decision' ? 'Kode’s pick' : 'Recommended'}</span>}
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
              <span className="dq-detail">Tell Kode what you want instead.</span>
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
        placeholder={answer ? 'Your answer' : q.options.length ? 'Add a note (optional)' : q.kind === 'decision' ? 'What Kode should do instead' : 'Your answer (or leave it to Kode)'}
        aria-label={answer || !q.options.length ? `Your answer to ${n}` : `Note on ${n}`}
        data-reload-safe={draftsPersist() ? '' : undefined}
        onChange={(e) => onChange({ ...d, text: e.target.value })}
      />
      {foot}
    </li>
  );
}

/**
 * A key design decision, read-only: an answered question, or one Kode made itself, with who decided
 * it, why, what else Kode weighed and the Meko items it rests on. `onChange` (in Design) offers
 * Change this on Kode's own decisions.
 */
function DecidedCard({ q, n, datapacks, onChange }: { q: DesignQuestion; n: string; datapacks?: DatapackRef[]; onChange?: () => void }) {
  const weighed = q.kind === 'decision' ? alsoWeighed(q) : [];
  return (
    <li className="dq-card">
      <div className="dq-top">
        <span className="dq-n">{n}</span>
        <span className="dq-q">{q.question}</span>
        {q.meko.length > 0 && <MekoBadge />}
        <DecidedChip q={q} />
      </div>
      {q.decision && (
        <p className="dq-decision">
          <span className="dq-decision-label">Decision</span> {q.decision}
        </p>
      )}
      {q.kind === 'decision' && q.context && <p className="dq-ctx">{q.context}</p>}
      {weighed.length > 0 && (
        <p className="dq-ctx">
          {q.decidedBy === 'user' ? 'Other options' : 'Also weighed'}: {weighed.join(' · ')}
        </p>
      )}
      <MekoRefs refs={q.meko} datapacks={datapacks} />
      {onChange && (
        <div className="dq-actions">
          <button type="button" className="btn small" onClick={onChange}>
            Change this
          </button>
        </div>
      )}
    </li>
  );
}

/**
 * The button Ctrl+Enter (⌘+Enter) presses in the questions: Answer while typing in Kode is asking
 * (it goes on its own), else Approve. None while the approval waits for the turn's end, when Cancel
 * approval stands in its place.
 */
export function questionsSubmitButton(focused: Element, approve: HTMLButtonElement | null): HTMLButtonElement | null {
  const ask = focused.closest('.ask-form');
  return ask ? ask.querySelector<HTMLButtonElement>('button[data-submit]') : approve;
}

/**
 * The design's questions for the user: open ones to answer (an option, with an optional note, or
 * a custom answer), the key design decisions (answered questions and Kode's own, which the user can
 * change), and other feedback. The answers stay in this browser until they go to Kode: with the
 * approval, or to revise the design. Questions Claude is asking right now (AskUserQuestion,
 * through Kode's mod) come first, answered on their own. In Design, the key design decisions
 * start collapsed: they open for a change to one of Kode's decisions, and each time
 * `showDecisions` is bumped (the Design tab's decisions bar, or Approve with a change to finish).
 * Past Design they're all there is to read, so they start open. With `onAutoRun`, the footer
 * holds Run to merge beside its Approve (or Cancel approval).
 */
export function DesignQuestions({
  session,
  doc,
  datapacks,
  showDecisions = 0,
  onAutoRun,
}: {
  session: Session;
  doc: DesignDoc;
  datapacks?: DatapackRef[];
  /**
   * Turns Run to merge on or off from its box in the footer. Given only while the panel's header
   * leaves out its own box (`headerApproves()`), so the box shows once, wherever the approval goes.
   */
  onAutoRun?: (on: boolean) => Promise<void>;
  showDecisions?: number;
}) {
  const [drafts, update] = useDesignDrafts(session.id);
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  const [sending, setSending] = useState<'revise' | 'approve' | 'cancel' | null>(null);
  const approve = useRef<HTMLButtonElement>(null);
  // The line saying a revision was sent goes once that revision's turn is over (the design panel's
  // header shows Kode at work meanwhile).
  const revising = useRevision(session.id) !== null;
  useEffect(() => {
    if (!revising) setNote(null);
  }, [revising]);

  const editable = session.stage === 'design' && session.desired === 'running';
  // While an approval waits for the turn's end, the answers it took stay as they were.
  const canEdit = editable && !session.approvalQueued;
  const open = doc.questions.filter((q) => q.kind === 'question' && q.status === 'open');
  const resolved = doc.questions.filter((q) => q.kind === 'decision' || q.status === 'resolved');
  const number = decisionNumbers(doc.questions);
  const request = toRequest(doc.questions, drafts);
  // Kode's decisions the user is changing (Change this), until the decision itself changes.
  const changing = (q: DesignQuestion) => q.kind === 'decision' && drafts.answers[q.id]?.against === q.decision;
  // A change in progress opens the key design decisions; Approve opens them again to finish it.
  const changingAny = resolved.some(changing);
  const [decidedOpen, setDecidedOpen] = useState(showDecisions > 0 || changingAny || !editable);
  const decided = useRef<HTMLDetailsElement>(null);
  useEffect(() => {
    if (changingAny) setDecidedOpen(true);
  }, [changingAny]);
  useEffect(() => {
    if (!showDecisions) return;
    setDecidedOpen(true);
    decided.current?.scrollIntoView({ block: 'nearest' });
  }, [showDecisions]);
  const setDraft = (id: string, d: Draft | null) =>
    update((x) => {
      const answers = { ...x.answers };
      if (d) answers[id] = d;
      else delete answers[id];
      return { ...x, answers };
    });
  const notDesign = !editable ? 'Answers go to Kode only while the session is in Design.' : null;
  const blocker = notDesign ?? draftsBlocker(doc.questions, drafts) ?? designApprovalBlocker(session);
  const reviseBlocker = notDesign ?? draftsBlocker(doc.questions, drafts) ?? designReviseBlocker(session)
    ?? (!request.answers.length && !request.feedback.trim() ? 'Answer a question or write feedback first.' : null);
  // Kode is mid-turn with its mod running: answers go now and wait in Claude, an approval waits here.
  const held = heldForTurnEnd(session);
  // Answers to questions that are no longer open (resolved as they were sent) are done with.
  useEffect(() => {
    const pruned = pruneDrafts(doc.questions, loadDrafts(session.id));
    if (pruned) saveDrafts(session.id, pruned);
  }, [doc.questions, session.id]);
  const queued = session.approvalQueued
    ? 'Approved: it goes to Kode when this turn ends. If the turn changes the design, you’re asked again.'
    : null;
  const waiting = held && !reviseBlocker ? 'Kode is working: your answers go now, and it takes them when this turn ends.' : null;
  // Shown in the footer too, as tooltips aren't seen on a phone.
  const busy = session.busy ? SESSION_BUSY : null;

  const go = async (what: 'revise' | 'approve' | 'cancel') => {
    setSending(what);
    setNote(null);
    try {
      if (what === 'cancel') {
        await api.sessionAction(session.id, 'cancel-approval');
      } else if (what === 'revise') {
        const at = Date.now();
        await api.reviseDesign(session.id, request);
        revisionSent(session.id, at);
        setNote(held ? 'Sent. Kode takes them when this turn ends, then revises the design.' : 'Sent to Kode. The design is updated when its turn ends.');
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
      <div
        className="dq-scroll"
        onKeyDown={(e) => {
          // Ctrl+Enter in an answer or the feedback presses Approve, as a click would (nothing while
          // it's disabled), as in a dialog.
          if (!isSubmitKey(e.nativeEvent) || !(e.target instanceof Element)) return;
          const button = questionsSubmitButton(e.target, approve.current);
          if (!button) return;
          e.preventDefault();
          button.click();
        }}
      >
        {session.asking && (
          <section className="dq-section" aria-label="Kode is asking">
            <h4>Kode is asking</h4>
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
                n={number.get(q.id) ?? ''}
                d={shownDraft(q, drafts.answers[q.id], editable)}
                answerable={editable}
                editable={canEdit}
                datapacks={datapacks}
                onChange={(d) => setDraft(q.id, d)}
              />
            ))}
          </ol>
        ) : (
          <p className="muted">
            {resolved.length
              ? 'Every question is resolved.'
              : doc.questionsSource === null
                ? 'Kode listed no open questions for this design.'
                : 'No open questions.'}{' '}
            {editable && 'You can still send Kode feedback on the design below.'}
          </p>
        )}
        {doc.questionsSource === 'markdown' && open.length > 0 && (
          <p className="muted small">These are the items under the design’s “Open questions” heading.</p>
        )}

        {resolved.length > 0 && (
          <details ref={decided} className="dq-section dq-decided" open={decidedOpen} onToggle={(e) => setDecidedOpen(e.currentTarget.open)}>
            <summary>Key Design Decisions ({resolved.length})</summary>
            <ol className="dq-list">
              {resolved.map((q) =>
                changing(q) ? (
                  <QuestionCard
                    key={q.id}
                    q={q}
                    n={number.get(q.id) ?? ''}
                    d={draftFor(q, drafts.answers[q.id])}
                    answerable
                    editable={canEdit}
                    datapacks={datapacks}
                    lead={
                      <p className="dq-decision">
                        <span className="dq-decision-label">{q.decidedBy === 'user' ? 'You decided' : 'Kode decided'}</span> {q.decision}
                      </p>
                    }
                    foot={
                      <div className="dq-actions">
                        <button type="button" className="btn small" disabled={!canEdit} onClick={() => setDraft(q.id, null)}>
                          Keep Kode’s decision
                        </button>
                      </div>
                    }
                    onChange={(d) => setDraft(q.id, d)}
                  />
                ) : (
                  <DecidedCard
                    key={q.id}
                    q={q}
                    n={number.get(q.id) ?? ''}
                    datapacks={datapacks}
                    onChange={canEdit && q.kind === 'decision' ? () => setDraft(q.id, changeDraft(q)) : undefined}
                  />
                ),
              )}
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
              placeholder="Anything else Kode should change"
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
          {/* Why Approve is closed comes before what Send for revision needs: a phone has no tooltips. */}
          <span className="dq-foot-note">
            {error ? <span className="error-text">{error}</span> : (note ?? busy ?? queued ?? blocker ?? waiting ?? reviseBlocker ?? '')}
          </span>
          <button
            className="btn small"
            disabled={!!reviseBlocker || !!sending || !!busy}
            title={busy ?? reviseBlocker ?? 'Kode revises the design to match your answers'}
            onClick={() => void go('revise')}
          >
            {sending === 'revise' ? <Busy label="Sending" /> : 'Send for revision'}
          </button>
          {onAutoRun && <RunToMergeCheck session={session} disabled={session.busy} onChange={onAutoRun} />}
          {session.approvalQueued ? (
            <button className="btn small" disabled={!!sending} onClick={() => void go('cancel')}>
              {sending === 'cancel' ? <Busy label="Cancelling" /> : 'Cancel approval'}
            </button>
          ) : (
            <button
              ref={approve}
              className="btn small primary"
              disabled={!!blocker || !!sending || !!busy}
              aria-keyshortcuts={isMac() ? 'Meta+Enter' : 'Control+Enter'}
              title={busy ?? blocker ?? `${held
                ? 'Kode is mid-turn: the approval goes when the turn ends, if the design stays the same'
                : 'Approve the design with your answers, and move to Code'} (${isMac() ? '⌘' : 'Ctrl+'}Enter)`}
              onClick={() => void go('approve')}
            >
              {sending === 'approve' ? <Busy label="Approving" /> : open.length || request.answers.length ? 'Approve with answers' : 'Approve design'}
            </button>
          )}
        </div>
      )}
    </>
  );
}
