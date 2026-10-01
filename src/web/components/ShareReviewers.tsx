import { useEffect, useState } from 'react';
import type { DatapackPeople, PrReviewer, PrReviewers, ShareGrant } from '../../shared/types';
import { SHARE_GRANTS } from '../../shared/types';
import { api, errorMessage } from '../api';
import { copyText } from '../clipboard';

const GRANT_NAMES: Record<ShareGrant, string> = { contributor: 'Contributor', maintainer: 'Maintainer', viewer: 'Viewer' };

/**
 * The Share dialog's Reviewers section: who reviews each open PR of the project's running sessions
 * (each in its own repo), the PR the dialog was opened from first, with a Share per person. In link
 * mode (`onShare` absent) it lists their emails to copy instead. Hidden while the project has no
 * open PRs.
 */
export function ShareReviewers({
  projectId,
  focusSessionId,
  people,
  onShare,
  busy,
}: {
  projectId: string;
  focusSessionId: string | null;
  /** Who already has access or a pending invite, to mark them; null while loading (or in link mode). */
  people: DatapackPeople | null;
  /** Invites one reviewer; resolves true when it went through. */
  onShare?: (login: string, email: string, grant: ShareGrant) => Promise<boolean>;
  busy: boolean;
}) {
  const [prs, setPrs] = useState<PrReviewers[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    api
      .prReviewers(projectId)
      .then((r) => live && setPrs(r))
      .catch((e) => live && setError(errorMessage(e)));
    return () => {
      live = false;
    };
  }, [projectId]);

  if (error) return <p className="hint">Couldn't list the reviewers of the project's PRs: {error}</p>;
  if (!prs) return <p className="muted small">Looking up the reviewers of the project's PRs…</p>;
  if (!prs.length) return null;
  const ordered = [...prs.filter((p) => p.sessionId === focusSessionId), ...prs.filter((p) => p.sessionId !== focusSessionId)];
  const withAccess = new Set(
    [...(people?.members ?? []).map((m) => m.email), ...(people?.invites ?? []).map((i) => i.email)].map((e) => e.toLowerCase()),
  );
  const listed = new Map<string, number>();

  return (
    <div className="field">
      <span className="label">Reviewers on the project's open PRs</span>
      <div className="share-people share-reviewers">
        {ordered.map((pr) => (
          <div key={`${pr.repo}#${pr.number}`} className="share-pr">
            <div className="share-pr-head">
              <span className="mono small muted">{pr.repo.split('/')[1] ?? pr.repo}</span> <b>#{pr.number}</b> {pr.title}
            </div>
            {pr.error && <p className="hint">{pr.error}</p>}
            {pr.reviewers.map((r) => {
              const first = listed.get(r.login.toLowerCase());
              if (first === undefined) listed.set(r.login.toLowerCase(), pr.number);
              return first !== undefined ? (
                <div className="share-person" key={r.login}>
                  <span className="share-who">
                    <b>@{r.login}</b>
                  </span>
                  <span className="hint">Listed under #{first}</span>
                </div>
              ) : (
                <ReviewerRow key={r.login} reviewer={r} hasAccess={!!r.email && withAccess.has(r.email.toLowerCase())} onShare={onShare} busy={busy} />
              );
            })}
            {pr.teams.map((t) => (
              <div className="share-person" key={t}>
                <span className="share-who">
                  <b>@{t}</b> <span className="tag">team</span>
                </span>
                <span className="hint">{onShare ? 'Invite its members by email below.' : 'Invite its members in Meko.'}</span>
              </div>
            ))}
            {!pr.error && !pr.reviewers.length && !pr.teams.length && <p className="hint">No reviewers asked for yet.</p>}
          </div>
        ))}
      </div>
      <span className="hint">
        People asked to review, and people who have reviewed. An email you share with is remembered for that GitHub login.
      </span>
    </div>
  );
}

function ReviewerRow({
  reviewer,
  hasAccess,
  onShare,
  busy,
}: {
  reviewer: PrReviewer;
  hasAccess: boolean;
  onShare?: (login: string, email: string, grant: ShareGrant) => Promise<boolean>;
  busy: boolean;
}) {
  const [email, setEmail] = useState(reviewer.email ?? '');
  const [grant, setGrant] = useState<ShareGrant>('contributor');
  const [done, setDone] = useState(false);
  const [copied, setCopied] = useState(false);
  const valid = /^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.trim());
  const id = `share-rv-${reviewer.login}`;

  return (
    <div className="share-person share-reviewer">
      <span className="share-who">
        <b>@{reviewer.login}</b>
        {reviewer.name && <span className="muted"> {reviewer.name}</span>}
        {!reviewer.email && !hasAccess && <span className="muted small"> · no public email on GitHub</span>}
      </span>
      {hasAccess || done ? (
        <span className="tag shared">{done ? '✓ invited' : '✓ has access'}</span>
      ) : (
        <span className="share-person-btns">
          <input
            id={`${id}-email`}
            className="input small"
            type="email"
            aria-label={`Email for @${reviewer.login}`}
            placeholder="name@example.com"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
          {onShare ? (
            <>
              <select
                id={`${id}-grant`}
                className="input small"
                aria-label={`Access for @${reviewer.login}`}
                value={grant}
                onChange={(e) => setGrant(e.target.value as ShareGrant)}
              >
                {SHARE_GRANTS.map((g) => (
                  <option key={g} value={g}>
                    {GRANT_NAMES[g]}
                  </option>
                ))}
              </select>
              <button
                type="button"
                className="btn small primary"
                disabled={!valid || busy}
                onClick={() => void onShare(reviewer.login, email.trim(), grant).then((ok) => ok && setDone(true))}
              >
                Share
              </button>
            </>
          ) : (
            <button
              type="button"
              className="btn small"
              disabled={!valid}
              onClick={() => void copyText(email.trim()).then((ok) => setCopied(ok))}
            >
              {copied ? 'Copied' : 'Copy email'}
            </button>
          )}
        </span>
      )}
    </div>
  );
}
