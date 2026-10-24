import { useEffect, useState, type FormEvent } from 'react';
import type { MekoSettings } from '../../shared/types';
import { api, errorMessage } from '../api';
import { mekoKeyLinkBase } from '../util';
import { Busy, Dialog, ErrorText, Field } from './common';
import { MekoGetKey } from './MekoGetKey';

/**
 * Turns Meko on with an API key (for an install with no key and no Meko running on this computer).
 * The same as the Meko tab in Settings, cut down to what's needed: the server checks the key with
 * Meko before saving it.
 */
export function MekoKeyDialog({ reason, mekoWebUrl, onClose }: { reason?: string; mekoWebUrl: string | null; onClose: () => void }) {
  const [key, setKey] = useState('');
  const [url, setUrl] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // For the sign-up links when another MCP URL is typed; without it they wait for an empty field.
  const [meko, setMeko] = useState<MekoSettings | null>(null);

  useEffect(() => {
    api
      .settings()
      .then((s) => setMeko(s.meko))
      .catch(() => setMeko(null));
  }, []);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.saveSettings({ meko: { apiKey: key.trim(), ...(url.trim() ? { url: url.trim() } : {}) } });
      onClose();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog title="Add a Meko API key" onClose={onClose}>
      <form className="stack" onSubmit={save}>
        <p className="muted">
          {reason ?? 'Meko gives projects a datapack: shared memory for their sessions, and what you share a project with.'}
        </p>
        {meko && meko.local !== 'running' && (
          <p className="muted small">
            {meko.local === 'stopped' ? 'Meko is installed on this computer but not running.' : 'No Meko was found on this computer.'} AgentK
            uses the Meko on this computer without a key while it runs (meko start).
          </p>
        )}
        <MekoGetKey
          title="No account or key yet?"
          // An empty MCP URL keeps the current server; the web app URL isn't changed here.
          webUrl={!url.trim() ? mekoWebUrl : meko && mekoKeyLinkBase({ url, webUrl: meko.webUrl }, meko)}
          fallback="Sign up and make a key in your Meko's web app, then paste the key below."
        />
        <Field label="API key">
          <input
            id="meko-key-input"
            className="input mono"
            type="password"
            autoComplete="off"
            placeholder="mko_tkn_…"
            value={key}
            onChange={(e) => setKey(e.target.value)}
          />
        </Field>
        <Field label="MCP server URL" hint="Leave empty for Meko Cloud. More settings are on the Meko tab in Settings.">
          <input id="meko-url-input" className="input mono" placeholder="https://mcp.mekodata.ai/mcp" value={url} onChange={(e) => setUrl(e.target.value)} />
        </Field>
        <ErrorText error={error} />
        <div className="settings-actions">
          <button className="btn primary" disabled={busy || !key.trim()}>
            {busy ? <Busy label="Checking" /> : 'Turn on Meko'}
          </button>
        </div>
      </form>
    </Dialog>
  );
}
