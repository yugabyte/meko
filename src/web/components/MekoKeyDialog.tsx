import { useState, type FormEvent } from 'react';
import { api, errorMessage } from '../api';
import { Dialog, ErrorText, Field } from './common';

/**
 * Turns Meko on with an API key (for an install without one). The same as the Meko tab in
 * Settings, cut down to what's needed: the server checks the key with Meko before saving it.
 */
export function MekoKeyDialog({ reason, mekoWebUrl, onClose }: { reason?: string; mekoWebUrl: string | null; onClose: () => void }) {
  const [key, setKey] = useState('');
  const [url, setUrl] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.saveSettings({ meko: { apiKey: key.trim(), enabled: true, ...(url.trim() ? { url: url.trim() } : {}) } });
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
          {mekoWebUrl && (
            <>
              {' '}
              No account or key yet? Create them in{' '}
              <a href={mekoWebUrl} target="_blank" rel="noreferrer">
                Meko ↗
              </a>
              .
            </>
          )}
        </p>
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
            {busy ? 'Checking…' : 'Turn on Meko'}
          </button>
        </div>
      </form>
    </Dialog>
  );
}
