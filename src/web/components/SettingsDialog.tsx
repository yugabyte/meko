import { useEffect, useState, type FormEvent } from 'react';
import type { AuthStatus, Settings, TotpEnrollResponse, UpdateStatus } from '../../shared/types';
import { api, errorMessage } from '../api';
import { cx, relativeTime } from '../util';
import { Dialog, ErrorText, Field } from './common';

type Tab = 'general' | 'account' | 'updates';

export function SettingsDialog({
  auth,
  update,
  onClose,
}: {
  auth: AuthStatus;
  update: UpdateStatus;
  onClose: () => void;
}) {
  const [tab, setTab] = useState<Tab>('general');
  return (
    <Dialog
      title="Settings"
      wide
      onClose={onClose}
      aside={
        <div className="seg" role="tablist">
          {(['general', 'account', 'updates'] as Tab[]).map((t) => (
            <button key={t} role="tab" aria-selected={tab === t} className={cx(tab === t && 'on')} onClick={() => setTab(t)}>
              {t === 'general' ? 'General' : t === 'account' ? 'Account' : 'Updates'}
            </button>
          ))}
        </div>
      }
    >
      {tab === 'general' && <GeneralTab />}
      {tab === 'account' && <AccountTab auth={auth} />}
      {tab === 'updates' && <UpdatesTab initial={update} />}
    </Dialog>
  );
}

function GeneralTab() {
  const [s, setS] = useState<Settings | null>(null);
  const [roots, setRoots] = useState<string[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api
      .settings()
      .then((x) => {
        setS(x);
        setRoots(x.reposRoots);
      })
      .catch((e) => setError(errorMessage(e)));
  }, []);

  if (!s) return error ? <ErrorText error={error} /> : <p className="muted">Loading…</p>;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const next = await api.saveSettings({
        reposRoots: roots.map((r) => r.trim()).filter(Boolean),
        reposDepth: s.reposDepth,
        cloneInto: s.cloneInto.trim(),
        updateChannel: s.updateChannel,
      });
      setS(next);
      setRoots(next.reposRoots);
      setSaved(true);
      setTimeout(() => setSaved(false), 2500);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="stack" onSubmit={save}>
      <div className="field">
        <span className="label">Repo folders</span>
        <span className="hint">The New project dialog lists the git repos in these folders.</span>
        {roots.map((r, i) => (
          <div className="filter-row" key={i}>
            <input
              id={`set-root-${i}`}
              className="input mono"
              value={r}
              onChange={(e) => setRoots(roots.map((x, j) => (j === i ? e.target.value : x)))}
            />
            <button
              type="button"
              className="btn ghost"
              disabled={roots.length <= 1}
              onClick={() => setRoots(roots.filter((_, j) => j !== i))}
            >
              Remove
            </button>
          </div>
        ))}
        <button type="button" className="link-btn" onClick={() => setRoots([...roots, ''])}>
          + Add folder
        </button>
      </div>
      <div className="fields">
        <Field label="Scan depth" hint="1 lists only the folders directly inside each repo folder.">
          <input
            id="set-depth"
            className="input"
            type="number"
            min={1}
            max={4}
            value={s.reposDepth}
            onChange={(e) => setS({ ...s, reposDepth: Math.max(1, Math.min(4, Number(e.target.value) || 1)) })}
          />
        </Field>
        <Field label="Clone into" hint="Where “Clone from URL” puts new repos.">
          <input
            id="set-clone"
            className="input mono"
            value={s.cloneInto}
            onChange={(e) => setS({ ...s, cloneInto: e.target.value })}
          />
        </Field>
      </div>
      <Field label="Update channel">
        <select
          id="set-channel"
          className="input"
          value={s.updateChannel}
          onChange={(e) => setS({ ...s, updateChannel: e.target.value as Settings['updateChannel'] })}
        >
          <option value="stable">Stable releases</option>
          <option value="prerelease">Include pre-releases</option>
          <option value="off">Off (don’t check)</option>
        </select>
      </Field>
      <div className="fields">
        <Field label="Listen address">
          <input id="set-bind" className="input mono" value={s.bind} readOnly disabled />
        </Field>
        <Field label="Port">
          <input id="set-port" className="input mono" value={s.port} readOnly disabled />
        </Field>
      </div>
      <p className="hint">Change the address and port in config.toml, then restart the server.</p>
      <ErrorText error={error} />
      <div className="settings-actions">
        <button className="btn primary" disabled={busy}>
          {busy ? 'Saving…' : 'Save'}
        </button>
        {saved && <span className="muted small">Saved</span>}
      </div>
    </form>
  );
}

function AccountTab({ auth }: { auth: AuthStatus }) {
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const [pwMsg, setPwMsg] = useState<string | null>(null);
  const [pwErr, setPwErr] = useState<string | null>(null);

  const [totpOn, setTotpOn] = useState(auth.totpEnabled);
  const [enroll, setEnroll] = useState<TotpEnrollResponse | null>(null);
  const [code, setCode] = useState('');
  const [totpErr, setTotpErr] = useState<string | null>(null);
  const [totpMsg, setTotpMsg] = useState<string | null>(null);

  const changePw = async (e: FormEvent) => {
    e.preventDefault();
    setPwErr(null);
    setPwMsg(null);
    if (next.length < 12) return setPwErr('Use at least 12 characters.');
    if (next !== confirm) return setPwErr('The two new passwords don’t match.');
    try {
      await api.changePassword(current, next);
      setCurrent('');
      setNext('');
      setConfirm('');
      setPwMsg('Password changed.');
    } catch (err) {
      setPwErr(errorMessage(err));
    }
  };

  const startEnroll = async () => {
    setTotpErr(null);
    try {
      setEnroll(await api.totpEnroll());
    } catch (err) {
      setTotpErr(errorMessage(err));
    }
  };

  const confirmTotp = async (e: FormEvent) => {
    e.preventDefault();
    setTotpErr(null);
    try {
      if (totpOn) {
        await api.totpDisable(code);
        setTotpOn(false);
        setTotpMsg('Two-step sign-in is off.');
      } else {
        await api.totpConfirm(code);
        setTotpOn(true);
        setEnroll(null);
        setTotpMsg('Two-step sign-in is on.');
      }
      setCode('');
    } catch (err) {
      setTotpErr(errorMessage(err));
    }
  };

  return (
    <div className="stack">
      <form className="stack" onSubmit={changePw}>
        <h3>Change password</h3>
        <Field label="Current password">
          <input
            id="acc-current"
            className="input"
            type="password"
            autoComplete="current-password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
          />
        </Field>
        <div className="fields">
          <Field label="New password">
            <input
              id="acc-next"
              className="input"
              type="password"
              autoComplete="new-password"
              value={next}
              onChange={(e) => setNext(e.target.value)}
            />
          </Field>
          <Field label="Confirm new password">
            <input
              id="acc-confirm"
              className="input"
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
            />
          </Field>
        </div>
        <ErrorText error={pwErr} />
        {pwMsg && <p className="ok-text">{pwMsg}</p>}
        <div>
          <button className="btn primary" disabled={!current || !next}>
            Change password
          </button>
        </div>
      </form>

      <div className="stack">
        <h3>Two-step sign-in</h3>
        <p className="muted">
          {totpOn
            ? 'On. Sign-in asks for a code from your authenticator app.'
            : 'Off. Add a code from an authenticator app to every sign-in.'}
        </p>
        {!totpOn && !enroll && (
          <div>
            <button className="btn" onClick={startEnroll}>
              Set up authenticator
            </button>
          </div>
        )}
        {enroll && !totpOn && (
          <div className="totp-enroll">
            <div className="qr" dangerouslySetInnerHTML={{ __html: enroll.qrSvg }} />
            <div className="stack">
              <p>Scan the code with your authenticator app, or enter this key:</p>
              <code className="secret">{enroll.secret}</code>
            </div>
          </div>
        )}
        {(enroll || totpOn) && (
          <form className="filter-row" onSubmit={confirmTotp}>
            <input
              id="acc-totp"
              className="input mono"
              inputMode="numeric"
              autoComplete="one-time-code"
              placeholder="6-digit code"
              maxLength={6}
              value={code}
              onChange={(e) => setCode(e.target.value.replace(/\D/g, ''))}
            />
            <button className={cx('btn', totpOn ? 'danger' : 'primary')} disabled={code.length !== 6}>
              {totpOn ? 'Turn off' : 'Confirm'}
            </button>
          </form>
        )}
        <ErrorText error={totpErr} />
        {totpMsg && <p className="ok-text">{totpMsg}</p>}
      </div>
    </div>
  );
}

function UpdatesTab({ initial }: { initial: UpdateStatus }) {
  const [u, setU] = useState(initial);
  const [busy, setBusy] = useState<'check' | 'apply' | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => setU(initial), [initial]);

  const run = async (kind: 'check' | 'apply') => {
    setBusy(kind);
    setError(null);
    try {
      setU(kind === 'check' ? await api.checkUpdate() : await api.applyUpdate());
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="stack">
      <dl className="kv">
        <dt>Installed</dt>
        <dd className="mono">v{u.current}</dd>
        <dt>Latest</dt>
        <dd className="mono">{u.latest ? `v${u.latest}` : '—'}</dd>
        <dt>Channel</dt>
        <dd>{u.channel}</dd>
        <dt>Last checked</dt>
        <dd>{relativeTime(u.lastCheckedAt)}</dd>
      </dl>
      {u.lastError && <p className="error-text">Last check failed: {u.lastError}</p>}
      <p className="muted">
        Updates restart only the web server. Your Claude sessions keep running, and the page reconnects on its own.
      </p>
      <ErrorText error={error} />
      <div className="settings-actions">
        <button className="btn" disabled={!!busy || u.channel === 'off'} onClick={() => run('check')}>
          {busy === 'check' ? 'Checking…' : 'Check now'}
        </button>
        {u.available && (
          <button className="btn primary" disabled={!!busy || u.applying} onClick={() => run('apply')}>
            {busy === 'apply' || u.applying ? 'Updating…' : `Update to v${u.latest}`}
          </button>
        )}
        {u.releaseUrl && (
          <a href={u.releaseUrl} target="_blank" rel="noreferrer" className="link-btn">
            Release notes
          </a>
        )}
      </div>
    </div>
  );
}
