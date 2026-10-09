import { useEffect, useState, type ClipboardEvent, type FormEvent } from 'react';
import type { DatapackPeople, DatapackRef, ShareGrant } from '../../shared/types';
import { SHARE_GRANTS } from '../../shared/types';
import { isProjectDatapack } from '../../shared/datapacks';
import { MAX_SHARE_EMAILS, flattenPastedEmails, parseEmails } from '../../shared/emails';
import { api, errorMessage } from '../api';
import { cx } from '../util';
import { Busy, Dialog, ErrorText } from './common';
import { ShareReviewers } from './ShareReviewers';

const GRANT_TEXT: Record<ShareGrant, { title: string; text: string }> = {
  viewer: { title: 'Viewer', text: 'Read only. They can attach it to a project but can’t open it as one.' },
  contributor: { title: 'Contributor', text: 'Runs sessions and adds their own memories.' },
  maintainer: { title: 'Maintainer', text: 'Can also promote memories to shared knowledge and invite others.' },
};

/**
 * Invite people to a project's dedicated datapack through Meko's share API, the reviewers of the
 * project's open PRs first. The Emails field takes a list; addresses that already have access or
 * an invite are left out.
 */
export function ShareDialog({
  projectId,
  projectName,
  datapack,
  focusSessionId = null,
  onClose,
}: {
  projectId: string;
  projectName: string;
  datapack: DatapackRef;
  /** The session the dialog was opened from: its PR's reviewers come first. */
  focusSessionId?: string | null;
  onClose: () => void;
}) {
  const [people, setPeople] = useState<DatapackPeople | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [email, setEmail] = useState('');
  const [grant, setGrant] = useState<ShareGrant>('contributor');
  const [error, setError] = useState<string | null>(null);
  const [sent, setSent] = useState<string | null>(null);
  /** Addresses in the list that already had access or an invite, so weren't sent. */
  const [skipped, setSkipped] = useState<string | null>(null);
  /** The change went through, but the lists couldn't be read again: the ones shown may be out of date. */
  const [stale, setStale] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  useEffect(() => {
    api
      .datapackPeople(datapack.id)
      .then(setPeople)
      .catch((e) => setLoadError(errorMessage(e)));
  }, [datapack.id]);

  /** Runs one change and shows the lists it returns; null when it failed (the error is shown). */
  const run = async <T extends DatapackPeople>(key: string, fn: () => Promise<T>, done?: string): Promise<T | null> => {
    setBusy(key);
    setError(null);
    setSent(null);
    setSkipped(null);
    try {
      const next = await fn();
      if (next.refreshError) setStale(next.refreshError);
      else {
        setPeople(next);
        setStale(null);
      }
      if (done) setSent(done);
      return next;
    } catch (e) {
      setError(errorMessage(e));
      return null;
    } finally {
      setBusy(null);
    }
  };

  const listed = parseEmails(email).emails.length;
  const withAccess = new Set(
    [...(people?.members ?? []).map((m) => m.email), ...(people?.invites ?? []).map((i) => i.email)].map((a) => a.toLowerCase()),
  );

  const invite = (e: FormEvent) => {
    e.preventDefault();
    const typed = email;
    const { emails, invalid } = parseEmails(typed);
    setSent(null);
    setSkipped(null);
    if (invalid.length) {
      setError(`Not an email: ${invalid.join(', ')}. Fix or remove ${invalid.length === 1 ? 'it' : 'them'}, then press Invite.`);
      return;
    }
    const have = emails.filter((a) => withAccess.has(a.toLowerCase()));
    const to = emails.filter((a) => !withAccess.has(a.toLowerCase()));
    if (to.length > MAX_SHARE_EMAILS) {
      setError(`Invite at most ${MAX_SHARE_EMAILS} people at a time. This list has ${to.length}.`);
      return;
    }
    const skippedText = have.length ? `Already has access or an invite: ${have.join(', ')}.` : null;
    // Only the addresses still to invite stay in the field (unless it was changed meanwhile).
    const keep = (rest: string[]) => setEmail((v) => (v === typed ? rest.join(', ') : v));
    if (!to.length) {
      setError(null);
      setSkipped(skippedText);
      keep([]);
      return;
    }
    void run('invite', () => api.shareDatapack(datapack.id, { emails: to, grant })).then((r) => {
      setSkipped(skippedText);
      if (!r) return;
      setSent(`Invited ${r.invited.join(', ')}. Meko emails them the invite.`);
      if (r.failed.length) setError(r.failed.map((f) => `Couldn't invite ${f.email}: ${f.error}`).join(' '));
      keep(r.failed.map((f) => f.email));
    });
  };

  /** A column or row pasted from a spreadsheet: its new lines and tabs become separators. */
  const pasteEmails = (e: ClipboardEvent<HTMLInputElement>) => {
    const text = e.clipboardData.getData('text');
    if (!/[\r\n\t]/.test(text)) return;
    e.preventDefault();
    const el = e.currentTarget;
    const flat = flattenPastedEmails(text);
    const start = el.selectionStart ?? el.value.length;
    const end = el.selectionEnd ?? start;
    setEmail(el.value.slice(0, start) + flat + el.value.slice(end));
    requestAnimationFrame(() => el.setSelectionRange(start + flat.length, start + flat.length));
  };

  const shareReviewer = (login: string, to: string, g: ShareGrant) =>
    run(`reviewer-${login}`, () => api.shareDatapack(datapack.id, { email: to, grant: g, login }), `Invited ${to}. Meko emails them the invite.`).then(
      (r) => r !== null,
    );

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
          <span className="label">Emails</span>
          <input
            id="share-email"
            className="input"
            type="text"
            inputMode="email"
            autoComplete="off"
            spellCheck={false}
            placeholder="name@example.com, other@example.com"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            onPaste={pasteEmails}
          />
          <span className="hint">Several at once: separate them with commas, semicolons or spaces.</span>
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
            {busy === 'invite' ? <Busy label="Inviting" /> : listed > 1 ? `Invite ${listed}` : 'Invite'}
          </button>
        </div>
        {skipped && <p className="hint">{skipped}</p>}
        <ErrorText error={error} />
      </form>

      <div className="field">
        <span className="label">People</span>
        {loadError && <p className="error-text">{loadError}</p>}
        {!people && !loadError && <p className="muted"><Busy label="Loading" /></p>}
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
                    {busy === `resend-${i.inviteId}` ? <Busy label="Sending" /> : 'Resend'}
                  </button>
                  <button
                    className="btn small danger-outline"
                    disabled={busy !== null}
                    onClick={() => void run(`revoke-${i.inviteId}`, () => api.revokeDatapackInvite(datapack.id, i.inviteId))}
                  >
                    {busy === `revoke-${i.inviteId}` ? <Busy label="Revoking" /> : 'Revoke'}
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
