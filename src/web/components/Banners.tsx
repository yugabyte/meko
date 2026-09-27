import { useState } from 'react';
import type { AppState, DatapackInvite } from '../../shared/types';
import { api, errorMessage } from '../api';
import type { Conn } from '../hooks/useEvents';

const DISMISS_KEY = 'ak-dismissed-updated-from';

function readDismissed(): string | null {
  try {
    return localStorage.getItem(DISMISS_KEY);
  } catch {
    return null;
  }
}

/** Past this many, invite banners collapse into one line with Show. */
const MAX_INVITE_BANNERS = 3;

/** Project datapacks shared with us: open as a project, decline, or put off. */
function InviteBanners({
  invites,
  onOpen,
  onError,
}: {
  invites: DatapackInvite[];
  onOpen: (i: DatapackInvite) => void;
  onError: (m: string) => void;
}) {
  const [expanded, setExpanded] = useState(false);
  const [busy, setBusy] = useState<string | null>(null);
  if (!invites.length) return null;
  if (invites.length > MAX_INVITE_BANNERS && !expanded) {
    return (
      <div className="banner info">
        <span className="banner-text">{invites.length} project datapacks were shared with you.</span>
        <button className="btn small" onClick={() => setExpanded(true)}>
          Show
        </button>
      </div>
    );
  }
  const act = async (i: DatapackInvite, fn: (id: string) => Promise<void>) => {
    setBusy(i.datapackId);
    try {
      await fn(i.datapackId);
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };
  return (
    <>
      {invites.map((i) => {
        const who = i.ownerName ?? i.ownerEmail ?? 'Someone';
        const as = i.grant ? ` as ${i.grant}` : '';
        const viewer = i.grant === 'viewer';
        const invited = !!i.inviteId;
        const disabled = busy === i.datapackId;
        return (
          <div className="banner info invite-banner" key={i.datapackId}>
            <span className="banner-text">
              <b>{who}</b> {invited ? 'invited you to ' : 'shared '}
              <span className="mono">{i.name}</span>
              {invited ? as : ` with you${as}`}.
              {viewer &&
                (invited
                  ? ' It’s read only, so it can’t be a project’s own datapack.'
                  : ' It’s read only, so attach it from a project’s home.')}
            </span>
            <span className="banner-btns">
              {!viewer && (
                <button className="btn primary small" disabled={disabled} onClick={() => onOpen(i)}>
                  Open as project
                </button>
              )}
              {viewer && invited && (
                <button className="btn primary small" disabled={disabled} onClick={() => void act(i, api.acceptInvite)}>
                  Accept
                </button>
              )}
              {invited && (
                <button className="btn small" disabled={disabled} onClick={() => void act(i, api.declineInvite)}>
                  Decline
                </button>
              )}
              <button className="btn small" disabled={disabled} onClick={() => void act(i, api.laterInvite)}>
                {viewer && !invited ? 'OK' : 'Later'}
              </button>
            </span>
          </div>
        );
      })}
    </>
  );
}

export function Banners({
  state,
  conn,
  onError,
  onOpenInvite,
}: {
  state: AppState;
  conn: Conn;
  onError: (m: string) => void;
  onOpenInvite: (i: DatapackInvite) => void;
}) {
  const [dismissed, setDismissed] = useState<string | null>(readDismissed);
  const [applying, setApplying] = useState(false);
  const u = state.update;
  const updatedKey = u.justUpdatedFrom ? `${u.justUpdatedFrom}->${u.current}` : null;

  return (
    <div className="banners">
      {conn === 'closed' && (
        <div className="banner warn">Lost the connection to the server. Reconnecting… Your sessions keep running.</div>
      )}
      {!state.meko.ok && (
        <div className="banner warn">
          <b>Meko unreachable.</b> Running sessions keep going and will catch up once it’s back. New projects can’t be
          created until then.
          {state.meko.error && <span className="muted"> ({state.meko.error})</span>}
        </div>
      )}
      {updatedKey && dismissed !== updatedKey && (
        <div className="banner info">
          Updated to v{u.current} (from v{u.justUpdatedFrom}).
          {u.releaseUrl && (
            <>
              {' '}
              <a href={u.releaseUrl} target="_blank" rel="noreferrer">
                What’s new
              </a>
            </>
          )}
          <button
            className="link-btn"
            onClick={() => {
              try {
                localStorage.setItem(DISMISS_KEY, updatedKey);
              } catch {
                /* ignore */
              }
              setDismissed(updatedKey);
            }}
          >
            Dismiss
          </button>
        </div>
      )}
      {u.available && u.latest && !u.applying && (
        <div className="banner info">
          Version {u.latest} is available. Updating restarts the web server only; your sessions keep running.
          <button
            className="btn primary small"
            disabled={applying}
            onClick={async () => {
              setApplying(true);
              try {
                await api.applyUpdate();
              } catch (e) {
                onError(errorMessage(e));
              } finally {
                setApplying(false);
              }
            }}
          >
            {applying ? 'Updating…' : `Update to v${u.latest}`}
          </button>
        </div>
      )}
      {u.applying && <div className="banner info">Installing the update… The page reconnects on its own.</div>}
      <InviteBanners invites={state.invites ?? []} onOpen={onOpenInvite} onError={onError} />
    </div>
  );
}
