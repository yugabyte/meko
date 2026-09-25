import { useState } from 'react';
import type { AppState } from '../../shared/types';
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

export function Banners({ state, conn, onError }: { state: AppState; conn: Conn; onError: (m: string) => void }) {
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
    </div>
  );
}
