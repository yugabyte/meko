import { useEffect, useState } from 'react';
import { strategyName, type CodeReviewDetail, type CodeReviewFinding } from '../../shared/codereview';
import type { Session, SessionAction } from '../../shared/types';
import { api, ApiError, errorMessage } from '../api';
import { canRunReview, findingGroups, findingsCountsText, findingsReviewing, onDemandControls } from '../util';
import { Busy } from './common';
import { SidePanel } from './SidePanel';

/**
 * A code review's findings (GET /api/sessions/:id/code-review), beside its task's terminal:
 * every candidate by round and sweep, kept, pending or dropped (under Dismissed) and why, the scorer's verdict, the Meko
 * gotchas it credits and what the task did with it; the passes' notes and the datapacks the
 * MEKO.md files listed, by name with the gotchas one tagged (what each holds is for the review's
 * prompt, not the panel). Fetched again whenever the review moves on. While the review runs it reads
 * Reviewing, under the findings so far, with a bar along its header and no counts yet.
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
      <div className="review-body">
        {error && <p className="error">{error}</p>}
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
                    {g.findings.map((f) => <FindingRow key={f.key} f={f} />)}
                  </ul>
                )}
                {g.dismissed.length > 0 && (
                  <>
                    <h4>Dismissed</h4>
                    <ul className="rv-findings">
                      {g.dismissed.map((f) => <FindingRow key={f.key} f={f} />)}
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

function FindingRow({ f }: { f: CodeReviewFinding }) {
  return (
    <li className={`rv-finding${f.kept || f.pending ? '' : ' dropped'}`}>
      <div className="rv-head">
        <span className={`rv-sev ${f.severity}`}>{f.severity}</span>
        <span className="mono rv-where">{f.file}:{f.line}</span>
        <span className="rv-kept">{f.kept ? 'kept' : f.pending ? 'pending' : `dropped${f.dropped ? `: ${f.dropped}` : ''}`}</span>
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
          <span key={g} className="rv-tag">Meko gotcha {g}</span>
        ))}
      </div>
      {f.reason && <p className="rv-reason muted">{f.reason}</p>}
      {f.outcome && (
        <p className={`rv-outcome ${f.outcome.status}`}>
          {f.outcome.status === 'fixed' ? 'Fixed' : 'Declined'}
          {f.outcome.note ? `: ${f.outcome.note}` : ''}
        </p>
      )}
    </li>
  );
}
