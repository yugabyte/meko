import { useEffect, useRef, useState } from 'react';
import { DISMISSED_BY_YOU, MAX_FINDING_NOTE, reviewWaitsOnYou, strategyName, type CodeReviewDetail, type CodeReviewFinding } from '../../shared/codereview';
import type { FindingChoiceRequest, Session, SessionAction } from '../../shared/types';
import { api, ApiError, errorMessage } from '../api';
import { useTouch } from '../hooks/useMedia';
import { canRunReview, findingGroups, findingsCountsText, findingsReviewing, onDemandControls } from '../util';
import { Busy } from './common';
import { SidePanel } from './SidePanel';

/**
 * A code review's findings (GET /api/sessions/:id/code-review), beside its task's terminal:
 * every candidate by round and sweep, kept, pending or dropped (under Dismissed) and why, the scorer's verdict, the Meko
 * gotchas it credits and what the task did with it; the passes' notes and the datapacks the
 * MEKO.md files listed, by name with the gotchas one tagged (what each holds is for the review's
 * prompt, not the panel). Fetched again whenever the review moves on. While the review runs it reads
 * Reviewing, under the findings so far, with a bar along its header and no counts yet. While a
 * round's findings wait on the user (`triage`), each of that round's kept findings takes a note or
 * a dismissal (saved as it's made), and the header sends them to AgentK (`review-send`).
 */
export function ReviewFindings({ session, onClose, onTerminal, onAct }: {
  session: Session;
  onClose: () => void;
  /** Shows the review session's terminal: beside the task, while the review has one. */
  onTerminal?: () => void;
  /**
   * The task's review actions, on the task's panel (not the review session's): Review again
   * (`review-run`, canRunReview), and Cancel or Retry for a review the user ran (onDemandControls),
   * which the Code gate doesn't show. Absent while an action is under way.
   */
  onAct?: (a: SessionAction) => void;
}) {
  const [detail, setDetail] = useState<CodeReviewDetail | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const r = session.codeReview;
  const reviewing = findingsReviewing(r);
  const counts = findingsCountsText(r, detail);
  const run = onAct ? canRunReview(session) : null;
  const controls = onAct ? onDemandControls(session) : { cancel: false, retry: false };
  const triage = r && reviewWaitsOnYou(r) && detail?.review.round === r.round ? r.round : null;
  const toSend = triage === null || !detail ? 0 : detail.findings.filter((f) => f.round === triage && f.kept && !f.you?.dismissed).length;
  const [saveError, setSaveError] = useState<string | null>(null);
  // Choices still being saved: Send waits for them (a note's save starts as its field loses focus to Send).
  const saving = useRef(new Set<Promise<boolean>>());
  // The note being written, kept here rather than in its row: a dismissal moves the row to the
  // Dismissed list, which mounts it again, and the reason box it opened must stay open.
  const [editing, setEditing] = useState<{ key: string; draft: string } | null>(null);
  // A choice is saved as it's made; the finding it returns replaces the one shown. False when it failed.
  const choose = (key: string, choice: FindingChoiceRequest): Promise<boolean> => {
    setSaveError(null);
    const p = api.setFinding(session.id, key, choice).then(
      (f) => {
        setDetail((d) => (d ? { ...d, findings: d.findings.map((x) => (x.key === f.key ? f : x)) } : d));
        return true;
      },
      (e: unknown) => {
        setSaveError(`Your note or dismissal wasn't saved: ${errorMessage(e)}`);
        return false;
      },
    );
    saving.current.add(p);
    void p.finally(() => saving.current.delete(p));
    return p;
  };
  // Not past a choice that didn't save: the findings would go without it.
  const send = async () => {
    const saved = await Promise.all([...saving.current]);
    if (saveError || saved.includes(false)) return;
    onAct?.('review-send');
  };
  const row = (f: CodeReviewFinding) => (
    <FindingRow
      key={f.key}
      f={f}
      onChoose={triage === f.round && f.kept ? (c) => choose(f.key, c) : undefined}
      draft={editing?.key === f.key ? editing.draft : null}
      onDraft={(draft) => setEditing(draft === null ? null : { key: f.key, draft })}
    />
  );
  // Anything the summary says moving on is a reason to read the findings again.
  const key = r ? `${r.id}:${r.state}:${r.round}:${r.sweep}:${r.passesDone}:${r.scoresDone}:${r.candidates}:${r.addressed}` : '';
  useEffect(() => {
    let live = true;
    setLoading(true);
    api.codeReview(session.id).then(
      (d) => {
        if (!live) return;
        setDetail(d);
        setError(null);
      },
      (e: unknown) => {
        if (!live) return;
        setDetail(null);
        setError(e instanceof ApiError && e.status === 404 ? null : errorMessage(e));
      },
    ).finally(() => live && setLoading(false));
    return () => {
      live = false;
    };
  }, [session.id, key]);

  return (
    <SidePanel name="review" label="Review findings" className="design-panel review-panel">
      <div className="design-hd">
        <b>Findings</b>
        {detail && (
          <span className="muted small">
            {detail.review.firstLook ? 'First look · ' : ''}
            {strategyName(detail.review.strategy)}
            {counts ? ` · ${counts}` : ''}
          </span>
        )}
        <span className="sp" />
        {/* Fetched again at every step while it runs: Reviewing says enough. */}
        {loading && !reviewing && <Busy label="Loading" />}
        {onTerminal && (
          <button className="btn small" onClick={onTerminal} title="Show the review's terminal">
            Terminal
          </button>
        )}
        {controls.cancel && (
          <button className="btn small" onClick={() => onAct?.('review-cancel')} title="Stop the code review you ran">
            Cancel
          </button>
        )}
        {controls.retry && (
          <button className="btn small" onClick={() => onAct?.('review-retry')} title="Review again, reusing the steps already reported">
            Retry
          </button>
        )}
        {run?.offered && (
          <button className="btn small" disabled={!run.ready} onClick={() => onAct?.('review-run')} title="Run the code review again on the change as it is now">
            Review again
          </button>
        )}
        {run?.wait && <span className="muted small">{run.wait}</span>}
        <button className="icon-btn panel-close" onClick={onClose} aria-label="Close findings">
          <span className="desktop-only">×</span>
          <span className="mobile-only">← Back</span>
        </button>
        {reviewing && <span className="design-edge" aria-hidden="true" />}
      </div>
      {triage !== null && (
        <div className="rv-triage">
          <span className="gate-pill warn">Waits on you</span>
          <span className="muted small rv-triage-text">Add notes or dismiss findings, then send the rest to AgentK.</span>
          {onAct && (
            <button className="btn small primary" disabled={!!saveError} onClick={() => void send()} title={saveError ? 'Save your note or dismissal again first' : 'AgentK gets the findings left, with your notes'}>
              {toSend ? `Send ${toSend} to AgentK` : 'Finish review'}
            </button>
          )}
        </div>
      )}
      <div className="review-body">
        {error && <p className="error">{error}</p>}
        {saveError && <p className="error">{saveError}</p>}
        {!detail && !error && reviewing && <p className="muted"><Busy label="Reviewing" /></p>}
        {!detail && !loading && !error && !reviewing && <p className="muted">No findings yet. They show here as the passes report them.</p>}
        {detail && (
          <>
            {detail.datapacks.length > 0 && (
              <section className="rv-section">
                <h3>Meko datapacks</h3>
                <ul className="rv-datapacks">
                  {detail.datapacks.map((d) => (
                    <li key={d.id}>
                      <span className="mono">{d.name}</span>
                      {d.gotchas && <span className="rv-tag">gotchas</span>}
                    </li>
                  ))}
                </ul>
              </section>
            )}
            {detail.notes.filter((n) => n.text.trim()).map((n) => (
              <section key={`notes-${n.round}`} className="rv-section">
                <h3>Notes{detail.review.rounds > 1 ? `, round ${n.round}` : ''}</h3>
                <p className="rv-notes">{n.text}</p>
              </section>
            ))}
            {findingGroups(detail.findings).map((g) => (
              <section key={g.key} className="rv-section">
                <h3>{g.label}</h3>
                {g.findings.length > 0 && (
                  <ul className="rv-findings">
                    {g.findings.map(row)}
                  </ul>
                )}
                {g.dismissed.length > 0 && (
                  <>
                    <h4>Dismissed</h4>
                    <ul className="rv-findings">
                      {g.dismissed.map(row)}
                    </ul>
                  </>
                )}
              </section>
            ))}
            {reviewing ? (
              <p className="muted"><Busy label="Reviewing" /></p>
            ) : (
              detail.findings.length === 0 && <p className="muted">No findings.</p>
            )}
          </>
        )}
      </div>
    </SidePanel>
  );
}

/**
 * One finding. `onChoose`: it belongs to the round that waits on the user, who can give it a note
 * (saved when the note loses focus) or dismiss it (and restore it), with the note as the reason.
 * `draft`: the note being written, or null when its box is closed; held by the panel, since a
 * dismissal moves the row to another list (`onDraft` opens, updates and closes it).
 */
function FindingRow({ f, onChoose, draft, onDraft }: {
  f: CodeReviewFinding;
  onChoose?: (c: FindingChoiceRequest) => Promise<boolean>;
  draft: string | null;
  onDraft: (draft: string | null) => void;
}) {
  const touch = useTouch();
  const note = f.you?.note ?? '';
  const dismissed = !!f.you?.dismissed || f.dropped === DISMISSED_BY_YOU;
  const editing = draft !== null && !!onChoose;
  const [saving, setSaving] = useState(false);
  const save = async (c: FindingChoiceRequest) => {
    if (!onChoose) return;
    setSaving(true);
    try {
      await onChoose(c);
    } finally {
      setSaving(false);
    }
  };
  const status = f.kept && !f.you?.dismissed ? 'kept' : dismissed ? 'dismissed by you' : f.pending ? 'pending' : `dropped${f.dropped ? `: ${f.dropped}` : ''}`;
  return (
    <li className={`rv-finding${(f.kept || f.pending) && !dismissed ? '' : ' dropped'}`}>
      <div className="rv-head">
        <span className={`rv-sev ${f.severity}`}>{f.severity}</span>
        <span className="mono rv-where">{f.file}:{f.line}</span>
        <span className="rv-kept">{status}</span>
      </div>
      <p className="rv-comment">{f.comment}</p>
      <div className="rv-meta muted">
        <span>Raised by {f.passes.join(', ')}</span>
        {f.verdict && (
          <span>
            {' '}· {f.verdict}
            {f.confidence !== null ? ` (${f.confidence})` : ''}
          </span>
        )}
        {f.gotchas.map((g) => (
          <span key={g} className="rv-tag" title="A Meko gotcha, mined from the repo's history, led the review to this">Caught with Meko · {g}</span>
        ))}
      </div>
      {(f.credits ?? []).filter((c) => c.quote).map((c) => (
        <blockquote key={c.id} className="rv-quote">
          <span className="rv-quote-id">{c.id}</span> {c.quote}
        </blockquote>
      ))}
      {f.reason && <p className="rv-reason muted">{f.reason}</p>}
      {editing ? (
        <textarea
          className="input rv-note-input"
          aria-label={dismissed ? 'Why you dismissed it' : 'Your note for AgentK'}
          placeholder={dismissed ? 'Why you dismissed it (optional)' : 'Your note for AgentK'}
          autoFocus={!touch}
          rows={2}
          maxLength={MAX_FINDING_NOTE}
          value={draft ?? ''}
          onChange={(e) => onDraft(e.target.value)}
          onBlur={() => {
            onDraft(null);
            // Only the note: the dismissal stays as it is now, even while its own save is out.
            if (draft !== null && draft.trim() !== note) void save({ note: draft });
          }}
        />
      ) : (
        note && <p className="rv-you"><b>Your note:</b> {note}</p>
      )}
      {onChoose && !editing && (
        <div className="rv-actions">
          <button className="btn small" disabled={saving} onClick={() => onDraft(note)}>
            {note ? 'Edit note' : dismissed ? 'Add a reason' : 'Add a note'}
          </button>
          <button
            className="btn small"
            disabled={saving}
            onClick={() => {
              void save({ dismissed: !f.you?.dismissed });
              // A dismissal asks for its reason: the box stays open as the row moves under Dismissed.
              if (!f.you?.dismissed) onDraft(note);
            }}
          >
            {f.you?.dismissed ? 'Restore' : 'Dismiss'}
          </button>
        </div>
      )}
      {f.outcome && (
        <p className={`rv-outcome ${f.outcome.status}`}>
          {f.outcome.status === 'fixed' ? 'Fixed' : 'Declined'}
          {f.outcome.note ? `: ${f.outcome.note}` : ''}
        </p>
      )}
    </li>
  );
}
