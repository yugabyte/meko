import { useEffect, useState } from 'react';
import { countsText, strategyName, type CodeReviewDetail, type CodeReviewFinding } from '../../shared/codereview';
import type { Session } from '../../shared/types';
import { api, ApiError, errorMessage } from '../api';
import { findingGroups } from '../util';
import { Busy } from './common';
import { SidePanel } from './SidePanel';

/**
 * A local review's findings (GET /api/sessions/:id/code-review), beside its task's terminal:
 * every candidate by round and sweep, kept or dropped and why, the scorer's verdict, the Meko
 * gotchas it credits and what the task did with it; the passes' notes and the datapacks the
 * MEKO.md files listed. Fetched again whenever the review moves on.
 */
export function ReviewFindings({ session, onClose, onTerminal }: {
  session: Session;
  onClose: () => void;
  /** Shows the review session's terminal: beside the task, while the review has one. */
  onTerminal?: () => void;
}) {
  const [detail, setDetail] = useState<CodeReviewDetail | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const r = session.codeReview;
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
            {strategyName(detail.review.strategy)} · {countsText(detail.review.counts)}
          </span>
        )}
        <span className="sp" />
        {loading && <Busy label="Loading" />}
        {onTerminal && (
          <button className="btn small" onClick={onTerminal} title="Show the review's terminal">
            Terminal
          </button>
        )}
        <button className="icon-btn panel-close" onClick={onClose} aria-label="Close findings">
          <span className="desktop-only">×</span>
          <span className="mobile-only">← Back</span>
        </button>
      </div>
      <div className="review-body">
        {error && <p className="error">{error}</p>}
        {!detail && !loading && !error && <p className="muted">No findings yet. They show here as the passes report them.</p>}
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
                      {d.about && <span className="muted"> · {d.about}</span>}
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
                <ul className="rv-findings">
                  {g.findings.map((f) => <FindingRow key={f.key} f={f} />)}
                </ul>
              </section>
            ))}
            {detail.findings.length === 0 && <p className="muted">No findings.</p>}
          </>
        )}
      </div>
    </SidePanel>
  );
}

function FindingRow({ f }: { f: CodeReviewFinding }) {
  return (
    <li className={`rv-finding${f.kept ? '' : ' dropped'}`}>
      <div className="rv-head">
        <span className={`rv-sev ${f.severity}`}>{f.severity}</span>
        <span className="mono rv-where">{f.file}:{f.line}</span>
        <span className="rv-kept">{f.kept ? 'kept' : `dropped${f.dropped ? `: ${f.dropped}` : ''}`}</span>
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
