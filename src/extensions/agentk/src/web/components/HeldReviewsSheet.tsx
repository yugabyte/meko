import { useEffect, useState } from 'react';
import type { HeldReview, HeldReviewsRequest, Session } from '../../shared/types';
import { api, errorMessage } from '../api';
import { relativeTime } from '../util';
import { Busy, Dialog } from './common';
import { MarkdownView } from './MarkdownView';

const VERBS: Record<string, string> = { CHANGES_REQUESTED: 'requested changes', COMMENTED: 'commented' };
const KINDS: Record<HeldReview['kind'], string> = { person: '', bot: 'bot', outsider: 'not a collaborator' };

/**
 * The reviews on the session's PR held for the user (`Session.heldReviews`): each one's text and
 * inline comments, checked to start, and **Send to AgentK** (Review & PR only) or **Dismiss** for
 * the checked ones. Refetched whenever the count changes.
 */
export function HeldReviewsSheet({ session, onClose }: { session: Session; onClose: () => void }) {
  const [reviews, setReviews] = useState<HeldReview[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Unchecked rather than checked, so a review that arrives while the sheet is open starts checked.
  const [unchecked, setUnchecked] = useState<Set<string>>(new Set());
  const [busy, setBusy] = useState<HeldReviewsRequest['action'] | null>(null);
  const count = session.heldReviews.count;
  useEffect(() => {
    let live = true;
    api
      .heldReviews(session.id)
      .then((r) => live && setReviews(r))
      .catch((e: unknown) => live && setError(errorMessage(e)));
    return () => {
      live = false;
    };
  }, [session.id, count]);

  const chosen = (reviews ?? []).filter((r) => !unchecked.has(r.id));
  const canSend = session.stage === 'review' && session.desired === 'running';
  const act = async (action: HeldReviewsRequest['action']) => {
    setBusy(action);
    setError(null);
    try {
      await api.decideHeldReviews(session.id, { action, ids: chosen.map((r) => r.id) });
      if (chosen.length === reviews?.length) onClose();
      else setUnchecked(new Set());
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };
  const toggle = (id: string) =>
    setUnchecked((u) => {
      const next = new Set(u);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  const n = chosen.length;

  return (
    <Dialog
      title={session.prNumber ? `Held reviews · PR #${session.prNumber}` : 'Held reviews'}
      className="held-dialog"
      wide
      onClose={onClose}
      footer={
        <>
          {error && <span className="error-text small">{error}</span>}
          <button className="btn" disabled={!n || !!busy} onClick={() => void act('dismiss')}>
            {busy === 'dismiss' ? <Busy label="Dismissing" /> : n ? `Dismiss ${n}` : 'Dismiss'}
          </button>
          <button
            className="btn primary"
            disabled={!n || !!busy || !canSend}
            title={canSend ? 'AgentK addresses the checked reviews' : 'Held reviews go to AgentK in Review & PR'}
            onClick={() => void act('send')}
          >
            {busy === 'send' ? <Busy label="Sending" /> : n ? `Send ${n} to AgentK` : 'Send to AgentK'}
          </button>
        </>
      }
    >
      {!canSend && <p className="hint">Held reviews go to AgentK in Review &amp; PR. Move the session back there to send them, or dismiss them.</p>}
      {!reviews && !error && <Busy label="Reading the held reviews" />}
      {reviews && !reviews.length && <p className="muted">No reviews are held now.</p>}
      {reviews && reviews.length > 0 && (
        <ul className="held-list">
          {reviews.map((r) => (
            <li key={r.id} className="held-review">
              <div className="held-top">
                <label className="check">
                  <input type="checkbox" checked={!unchecked.has(r.id)} onChange={() => toggle(r.id)} aria-label={`Include the review from @${r.author}`} />
                  <span>
                    <b>@{r.author}</b>
                    {KINDS[r.kind] && <span className="held-kind">{KINDS[r.kind]}</span>} {VERBS[r.state] ?? r.state.toLowerCase()}
                    <span className="muted"> · {relativeTime(Date.parse(r.submittedAt))}</span>
                  </span>
                </label>
                <a className="btn small" href={r.url} target="_blank" rel="noreferrer">On GitHub</a>
              </div>
              {r.kind === 'outsider' && (
                <p className="held-warn">@{r.author} has no role on this repo. Read it before you send it: AgentK will act on what it says.</p>
              )}
              {r.body.trim() ? <MarkdownView markdown={r.body} /> : <p className="hint">No summary: only inline comments.</p>}
              {r.comments.length > 0 && (
                <ul className="held-comments">
                  {r.comments.map((c, i) => (
                    <li key={i}>
                      <a className="mono small" href={c.url} target="_blank" rel="noreferrer">{c.line ? `${c.path}:${c.line}` : c.path}</a>
                      <MarkdownView markdown={c.body} />
                    </li>
                  ))}
                </ul>
              )}
              {r.moreComments > 0 && <p className="hint">And {r.moreComments} more inline {r.moreComments === 1 ? 'comment' : 'comments'} on GitHub.</p>}
            </li>
          ))}
        </ul>
      )}
    </Dialog>
  );
}
