import { useEffect, useState, type FormEvent } from 'react';
import type { DatapackPeople, DatapackRef, ShareGrant } from '../../shared/types';
import { SHARE_GRANTS } from '../../shared/types';
import { isProjectDatapack } from '../../shared/datapacks';
import { api, errorMessage } from '../api';
import { cx } from '../util';
import { Dialog, ErrorText } from './common';
import { ShareReviewers } from './ShareReviewers';

const GRANT_TEXT: Record<ShareGrant, { title: string; text: string }> = {
  contributor: { title: 'Contributor', text: 'Runs sessions and adds their own memories.' },
  maintainer: { title: 'Maintainer', text: 'Can also promote memories to shared knowledge and invite others.' },
  viewer: { title: 'Viewer', text: 'Read only. They can attach it to a project but can’t open it as one.' },
};

/**
 * Invite people to a project's dedicated datapack through Meko's share API (API mode), the
 * reviewers of the project's open PRs first. In link mode (`shareUrl` set) only the reviewers are
 * listed, with their emails to copy, and sharing happens on Meko's Share page.
 */
export function ShareDialog({
  projectId,
  projectName,
  datapack,
  focusSessionId = null,
  shareUrl = null,
  onClose,
}: {
  projectId: string;
  projectName: string;
  datapack: DatapackRef;
  /** The session the dialog was opened from: its PR's reviewers come first. */
  focusSessionId?: string | null;
  /** Link mode: Meko's Share page for the datapack. */
  shareUrl?: string | null;
  onClose: () => void;
}) {
  const [people, setPeople] = useState<DatapackPeople | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [email, setEmail] = useState('');
  const [grant, setGrant] = useState<ShareGrant>('contributor');
  const [error, setError] = useState<string | null>(null);
  const [sent, setSent] = useState<string | null>(null);
  /** The change went through, but the lists couldn't be read again: the ones shown may be out of date. */
  const [stale, setStale] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  useEffect(() => {
    if (shareUrl) return; // link mode: Meko's share API isn't ours to call
    api
      .datapackPeople(datapack.id)
      .then(setPeople)
      .catch((e) => setLoadError(errorMessage(e)));
  }, [datapack.id, shareUrl]);

  /** Runs one change and shows the lists it returns; false when it failed (the error is shown). */
  const run = async (key: string, fn: () => Promise<DatapackPeople>, done?: string): Promise<boolean> => {
    setBusy(key);
    setError(null);
    setSent(null);
    try {
      const next = await fn();
      if (next.refreshError) setStale(next.refreshError);
      else {
        setPeople(next);
        setStale(null);
      }
      if (done) setSent(done);
      return true;
    } catch (e) {
      setError(errorMessage(e));
      return false;
    } finally {
      setBusy(null);
    }
  };

  const invite = (e: FormEvent) => {
    e.preventDefault();
    const to = email.trim();
    void run('invite', () => api.shareDatapack(datapack.id, { email: to, grant }), `Invited ${to}. Meko emails them the invite.`).then((ok) => {
      if (ok) setEmail((v) => (v.trim() === to ? '' : v));
    });
  };

  const shareReviewer = (login: string, to: string, g: ShareGrant) =>
    run(`reviewer-${login}`, () => api.shareDatapack(datapack.id, { email: to, grant: g, login }), `Invited ${to}. Meko emails them the invite.`);

  if (shareUrl) {
    return (
      <Dialog title={`Share “${projectName}”`} onClose={onClose}>
        <div className="stack">
          <span className="mono small muted share-dp">{datapack.name}</span>
          <ShareReviewers projectId={projectId} focusSessionId={focusSessionId} people={null} busy={false} />
          <p className="hint">This Meko shares datapacks on its own Share page. Invite the people above there.</p>
          <div className="share-actions">
            <a className="btn primary" href={shareUrl} target="_blank" rel="noreferrer">
              Share in Meko ↗
            </a>
          </div>
        </div>
      </Dialog>
    );
  }

  return (
    <Dialog title={`Share “${projectName}”`} onClose={onClose}>
      <form className="stack" onSubmit={invite}>
        <span className="mono small muted share-dp">{datapack.name}</span>
        <ShareReviewers projectId={projectId} focusSessionId={focusSessionId} people={people} onShare={shareReviewer} busy={busy !== null} />
        {!isProjectDatapack(datapack.name) && (
          <p className="hint">
            This datapack’s name doesn’t start with adlc/proj/, so AgentK won’t offer it to them as a project. They can still
            attach it to one.
          </p>
        )}
        <label className="field">
          <span className="label">Email</span>
          <input
            id="share-email"
            className="input"
            type="email"
            autoComplete="off"
            placeholder="name@example.com"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
        </label>
        <div className="field" role="radiogroup" aria-label="Access">
          <span className="label">Access</span>
          <div className="dp-list share-grants">
            {SHARE_GRANTS.map((g) => (
              <button
                type="button"
                key={g}
                role="radio"
                aria-checked={grant === g}
                className={cx('dp-row', grant === g && 'sel')}
                onClick={() => setGrant(g)}
              >
                <span className="radio" />
                <span className="share-grant-text">
                  <b>{GRANT_TEXT[g].title}.</b> {GRANT_TEXT[g].text}
                </span>
              </button>
            ))}
          </div>
        </div>
        <p className="hint">Their memories stay their own. Promoted knowledge and knowledge-base files are shared.</p>
        <div className="share-actions">
          {sent && <span className="ok-text">{sent}</span>}
          <button className="btn primary" type="submit" disabled={!email.trim() || busy !== null}>
            {busy === 'invite' ? 'Inviting…' : 'Invite'}
          </button>
        </div>
        <ErrorText error={error} />
      </form>

      <div className="field">
        <span className="label">People</span>
        {loadError && <p className="error-text">{loadError}</p>}
        {!people && !loadError && <p className="muted">Loading…</p>}
        {stale && <p className="hint">{stale} The list below may be out of date.</p>}
        {people && (
          <div className="share-people">
            {people.members.map((m) => (
              <div className="share-person" key={`m-${m.email}`}>
                <span className="share-who">
                  {m.name ? `${m.name} · ` : ''}
                  <span className="muted">{m.email}</span>
                </span>
                <span className={cx('tag', m.grant !== 'owner' && 'shared')}>{m.grant}</span>
              </div>
            ))}
            {people.invites.map((i) => (
              <div className="share-person" key={`i-${i.inviteId}`}>
                <span className="share-who">{i.email}</span>
                <span className="tag shared">invited · {i.grant}</span>
                <span className="share-person-btns">
                  <button
                    className="btn small"
                    disabled={busy !== null}
                    onClick={() => void run(`resend-${i.inviteId}`, () => api.resendDatapackInvite(datapack.id, i.inviteId), `Sent the invite to ${i.email} again.`)}
                  >
                    {busy === `resend-${i.inviteId}` ? 'Sending…' : 'Resend'}
                  </button>
                  <button
                    className="btn small danger-outline"
                    disabled={busy !== null}
                    onClick={() => void run(`revoke-${i.inviteId}`, () => api.revokeDatapackInvite(datapack.id, i.inviteId))}
                  >
                    {busy === `revoke-${i.inviteId}` ? 'Revoking…' : 'Revoke'}
                  </button>
                </span>
              </div>
            ))}
            {people.members.length === 0 && people.invites.length === 0 && <p className="muted">No one yet.</p>}
          </div>
        )}
        <span className="hint">Removing someone who has accepted is done in Meko.</span>
      </div>
    </Dialog>
  );
}
