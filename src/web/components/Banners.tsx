import { useState } from 'react';
import type { AppState, DatapackInvite, FailedStart } from '../../shared/types';
import { api, errorMessage } from '../api';
import type { Conn } from '../hooks/useEvents';
import { Busy } from './common';

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

/** New sessions whose worktree couldn't be made after the dialog closed: try again (the dialog filled in) or dismiss. */
function FailedStartBanners({
  state,
  onRetry,
  onError,
}: {
  state: AppState;
  onRetry: (f: FailedStart) => void;
  onError: (m: string) => void;
}) {
  const projects = new Map(state.projects.map((p) => [p.id, p.name]));
  const failed = (state.failedStarts ?? []).filter((f) => projects.has(f.projectId));
  return (
    <>
      {failed.map((f) => (
        <div className="banner warn" key={f.id} role="alert">
          <span className="banner-text">
            <b>{f.name}</b> in {projects.get(f.projectId)} couldn’t start: {f.error}
          </span>
          <span className="banner-btns">
            <button className="btn primary small" onClick={() => onRetry(f)}>
              Try again
            </button>
            <button className="btn small" onClick={() => api.dismissFailedStart(f.id).catch((e) => onError(errorMessage(e)))}>
              Dismiss
            </button>
          </span>
        </div>
      ))}
    </>
  );
}

export function Banners({
  state,
  conn,
  stale,
  onError,
  onOpenInvite,
  onRetryStart,
}: {
  state: AppState;
  conn: Conn;
  /** The server's version while this page still runs an older one (see useReload), else null. */
  stale: string | null;
  onError: (m: string) => void;
  onOpenInvite: (i: DatapackInvite) => void;
  onRetryStart: (f: FailedStart) => void;
}) {
  const [dismissed, setDismissed] = useState<string | null>(readDismissed);
  const [applying, setApplying] = useState(false);
  const u = state.update;
  const updatedKey = u.justUpdatedFrom ? `${u.justUpdatedFrom}->${u.current}` : null;

  return (
    <div className="banners">
      {conn === 'closed' && (
        <div className="banner warn">
          <span>
            Lost the connection to the server. <Busy label="Reconnecting" /> Your sessions keep running.
          </span>
        </div>
      )}
      {stale && (
        <div className="banner info">
          <span className="banner-text">AgentK Local was updated to v{stale}. Reload to use it.</span>
          <button className="btn primary small" onClick={() => location.reload()}>
            Reload
          </button>
        </div>
      )}
      {/* After an update this shows once the page has reloaded, not beside the Reload banner. */}
      {!stale && updatedKey && dismissed !== updatedKey && (
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
            {applying ? <Busy label="Updating" /> : `Update to v${u.latest}`}
          </button>
        </div>
      )}
      {u.applying && (
        <div className="banner info">
          <span>
            <Busy label="Installing the update" /> The page reloads on its own when it’s done.
          </span>
        </div>
      )}
      <FailedStartBanners state={state} onRetry={onRetryStart} onError={onError} />
      <InviteBanners invites={state.invites ?? []} onOpen={onOpenInvite} onError={onError} />
    </div>
  );
}
