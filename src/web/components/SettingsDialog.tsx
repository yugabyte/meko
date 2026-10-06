import { useEffect, useState, type FormEvent } from 'react';
import type { AuthStatus, MekoKeySource, MekoSettingsUpdate, MekoStatus, Settings, TotpEnrollResponse, UpdateStatus } from '../../shared/types';
import { api, errorMessage } from '../api';
import { cx, httpHost, mekoKeyLinkBase, relativeTime } from '../util';
import { Busy, ConfirmButton, Dialog, ErrorText, Field } from './common';
import { MekoGetKey } from './MekoGetKey';

type Tab = 'general' | 'meko' | 'account' | 'updates';

const TAB_LABEL: Record<Tab, string> = { general: 'General', meko: 'Meko', account: 'Account', updates: 'Updates' };

export function SettingsDialog({
  auth,
  update,
  meko,
  onClose,
}: {
  auth: AuthStatus;
  update: UpdateStatus;
  meko: MekoStatus;
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
          {(['general', 'meko', 'account', 'updates'] as Tab[]).map((t) => (
            <button key={t} role="tab" aria-selected={tab === t} className={cx(tab === t && 'on')} onClick={() => setTab(t)}>
              {TAB_LABEL[t]}
            </button>
          ))}
        </div>
      }
    >
      {tab === 'general' && <GeneralTab />}
      {tab === 'meko' && <MekoTab status={meko} />}
      {tab === 'account' && (
        <div className="stack">
          {auth.localAccess ? <LocalAccessNote /> : <AccountTab auth={auth} />}
          <UsageStatsCheck />
        </div>
      )}
      {tab === 'updates' && <UpdatesTab initial={update} />}
    </Dialog>
  );
}

function GeneralTab() {
  const [s, setS] = useState<Settings | null>(null);
  const [roots, setRoots] = useState<string[]>([]);
  // The depth as typed; clamped to 1–4 on blur and on save.
  const [depth, setDepth] = useState('');
  // Idle worktrees per repo and days to keep them, as typed; clamped on blur and on save.
  const [idle, setIdle] = useState('');
  const [idleDays, setIdleDays] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api
      .settings()
      .then((x) => {
        setS(x);
        setRoots(x.reposRoots);
        setDepth(String(x.reposDepth));
        setIdle(String(x.idleWorktreesPerRepo));
        setIdleDays(String(x.idleWorktreeDays));
      })
      .catch((e) => setError(errorMessage(e)));
  }, []);

  if (!s) return error ? <ErrorText error={error} /> : <p className="muted"><Busy label="Loading" /></p>;

  const clampDepth = () => Math.max(1, Math.min(4, Math.round(Number(depth)) || 1));
  // A blank or unreadable entry keeps the saved value: a stray 0 would remove every idle worktree.
  const clampOr = (typed: string, max: number, saved: number) => {
    const n = Math.round(Number(typed));
    return typed.trim() && Number.isFinite(n) ? Math.max(0, Math.min(max, n)) : saved;
  };
  const clampIdle = () => clampOr(idle, 20, s.idleWorktreesPerRepo);
  const clampIdleDays = () => clampOr(idleDays, 365, s.idleWorktreeDays);

  const save = async (e: FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const next = await api.saveSettings({
        reposRoots: roots.map((r) => r.trim()).filter(Boolean),
        reposDepth: clampDepth(),
        cloneInto: s.cloneInto.trim(),
        serverName: s.serverName.trim(),
        idleWorktreesPerRepo: clampIdle(),
        idleWorktreeDays: clampIdleDays(),
        resolveConflicts: s.resolveConflicts,
      });
      setS(next);
      setRoots(next.reposRoots);
      setDepth(String(next.reposDepth));
      setIdle(String(next.idleWorktreesPerRepo));
      setIdleDays(String(next.idleWorktreeDays));
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
        <span className="hint">The Open project dialog lists the git repos in these folders.</span>
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
            value={depth}
            onChange={(e) => setDepth(e.target.value)}
            onBlur={() => setDepth(String(clampDepth()))}
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
      <div className="field">
        <span className="hint">
          When a session ends, its worktree waits for the next session on the same repo, from any project, with its build
          outputs intact. Each idle worktree keeps those outputs on disk.
        </span>
        <div className="fields">
          <Field label="Idle worktrees per repo" hint="0 removes worktrees when sessions end.">
            <input
              id="set-idle-worktrees"
              className="input"
              type="number"
              min={0}
              max={20}
              value={idle}
              onChange={(e) => setIdle(e.target.value)}
              onBlur={() => setIdle(String(clampIdle()))}
            />
          </Field>
          <Field label="Remove unused after (days)" hint="An idle worktree no session has used for this long is removed. 0 keeps it.">
            <input
              id="set-idle-worktree-days"
              className="input"
              type="number"
              min={0}
              max={365}
              value={idleDays}
              onChange={(e) => setIdleDays(e.target.value)}
              onBlur={() => setIdleDays(String(clampIdleDays()))}
            />
          </Field>
        </div>
      </div>
      <div className="field">
        <label className="check">
          <input
            id="set-resolve-conflicts"
            type="checkbox"
            checked={s.resolveConflicts}
            onChange={(e) => setS({ ...s, resolveConflicts: e.target.checked })}
          />
          <span>Resolve merge conflicts on PRs</span>
        </label>
        <span className="hint">
          When a PR in PR &amp; Review conflicts with its base, AgentK merges the base into the branch, resolves the
          conflicts and pushes. Off leaves them to you.
        </span>
      </div>
      <Field label="Server name" hint={`Shown in the rail next to your user name. Leave it empty to use this machine’s name (${s.hostName}).`}>
        <input
          id="set-server-name"
          className="input"
          value={s.serverName}
          placeholder={s.hostName}
          maxLength={64}
          onChange={(e) => setS({ ...s, serverName: e.target.value })}
        />
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
          {busy ? <Busy label="Saving" /> : 'Save'}
        </button>
        {saved && <span className="muted small">Saved</span>}
      </div>
    </form>
  );
}

const KEY_SOURCE: Record<MekoKeySource, string> = {
  config: 'from config.toml',
  env: 'from the MEKO_API_KEY environment variable',
  claude: 'from Claude Code’s settings',
};

/** The Meko server and key AgentK uses, its links, and whether Claude Code's sessions use the same. */
function MekoTab({ status }: { status: MekoStatus }) {
  const [s, setS] = useState<Settings | null>(null);
  const [url, setUrl] = useState('');
  const [key, setKey] = useState('');
  const [webUrl, setWebUrl] = useState('');
  const [apiUrl, setApiUrl] = useState('');
  const [confirmHost, setConfirmHost] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState<'save' | 'claude' | null>(null);
  const [claudeDone, setClaudeDone] = useState(false);

  const show = (x: Settings) => {
    setS(x);
    setUrl(x.meko.url);
    setWebUrl(x.meko.webUrl);
    setApiUrl(x.meko.apiUrl);
    setKey('');
  };

  useEffect(() => {
    api
      .settings()
      .then(show)
      .catch((e) => setError(errorMessage(e)));
  }, []);

  if (!s) return error ? <ErrorText error={error} /> : <p className="muted"><Busy label="Loading" /></p>;
  const m = s.meko;

  const save = async (e: FormEvent) => {
    e.preventDefault();
    const next: MekoSettingsUpdate = {};
    if (url.trim() !== m.url) next.url = url.trim();
    if (key.trim()) next.apiKey = key.trim();
    if (webUrl.trim() !== m.webUrl) next.webUrl = webUrl.trim();
    if (apiUrl.trim() !== m.apiUrl) next.apiUrl = apiUrl.trim();
    if (!Object.keys(next).length) return;
    // Datapacks live on one Meko: moving to another server leaves existing projects' behind.
    const newHost = httpHost(url.trim() || m.defaultUrl);
    if (next.url !== undefined && newHost && newHost !== httpHost(m.effectiveUrl) && confirmHost !== newHost) {
      setConfirmHost(newHost);
      return;
    }
    setBusy('save');
    setError(null);
    try {
      show(await api.saveSettings({ meko: next }));
      setConfirmHost(null);
      setSaved(true);
      setTimeout(() => setSaved(false), 2500);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const setUpClaude = async () => {
    setBusy('claude');
    setError(null);
    setClaudeDone(false);
    try {
      show(await api.mekoClaudeSetup());
      setClaudeDone(true);
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  const claudeMatches = m.claudeKeyMatches && (m.claudeUrl ?? m.defaultUrl) === m.effectiveUrl;

  /** Turns Meko off (the key is kept) or back on (checked with Meko first). */
  const setEnabled = async (enabled: boolean) => {
    setBusy('save');
    setError(null);
    try {
      show(await api.saveSettings({ meko: { enabled } }));
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(null);
    }
  };

  return (
    <form className="stack" onSubmit={save}>
      <div className="meko-onoff">
        {m.enabled ? (
          <>
            <span className="muted">Meko gives projects datapacks: shared memory, and what a project is shared through.</span>
            <ConfirmButton
              className="btn small"
              label="Turn off Meko"
              confirmLabel="Turn off"
              danger
              prompt="AgentK stops using Meko: new sessions get no datapack, and designs and summaries aren't saved there. Projects keep their datapacks for when you turn it back on. Claude Code's own Meko setup isn't changed."
              onConfirm={() => setEnabled(false)}
            />
          </>
        ) : (
          <>
            <span>
              <b>Meko is off.</b> <span className="muted">Projects work without datapacks.</span>
            </span>
            <button type="button" className="btn small primary" disabled={busy !== null || m.keyHint === null} onClick={() => void setEnabled(true)}>
              Turn on Meko
            </button>
          </>
        )}
      </div>
      <Field label="MCP server URL" hint={`Empty means Meko Cloud (${m.defaultUrl}), unless the environment or Claude Code’s settings name one. Now using ${m.effectiveUrl}.`}>
        <input
          id="set-meko-url"
          className="input mono"
          placeholder={m.defaultUrl}
          value={url}
          onChange={(e) => {
            setUrl(e.target.value);
            setConfirmHost(null);
          }}
        />
      </Field>
      {m.keyHint === null && (
        <MekoGetKey
          title="No Meko API key yet."
          webUrl={mekoKeyLinkBase({ url, webUrl }, m)}
          fallback={`Fill in the web app URL below to get links to sign up and make a key on ${httpHost(url.trim() || m.fallbackUrl) ?? 'this Meko'}.`}
        />
      )}
      <Field
        label="API key"
        hint={
          m.keyHint === null
            ? 'Paste the key you generated in Meko.'
            : `A key is set${m.keyHint ? `, ending in ${m.keyHint}` : ''}${m.keySource ? `, ${KEY_SOURCE[m.keySource]}` : ''}. Type a new key to replace it. The key is never shown in full.`
        }
      >
        <input
          id="set-meko-key"
          className="input mono"
          type="password"
          autoComplete="off"
          placeholder={m.keyHint ? `••••••••${m.keyHint}` : 'mko_tkn_…'}
          value={key}
          onChange={(e) => setKey(e.target.value)}
        />
      </Field>
      <div className="fields">
        <Field label="Web app URL" hint="For links to datapacks, and sharing: the API URL defaults to it plus /api/v1.">
          <input
            id="set-meko-web"
            className="input mono"
            placeholder={m.effectiveWebUrl ?? 'https://meko.example.com'}
            value={webUrl}
            onChange={(e) => setWebUrl(e.target.value)}
          />
        </Field>
        <Field label="API URL" hint="Meko’s REST API, for sharing.">
          <input
            id="set-meko-api"
            className="input mono"
            placeholder={m.effectiveApiUrl ?? 'https://meko.example.com/api/v1'}
            value={apiUrl}
            onChange={(e) => setApiUrl(e.target.value)}
          />
        </Field>
      </div>
      <p className="hint">
        Leave the web app and API URLs empty for Meko Cloud: they’re worked out from the MCP URL. For your own Meko, fill in
        the web app URL; the API URL defaults to it plus /api/v1.
      </p>
      {status.configured && !m.effectiveWebUrl && (
        <p className="confirm-warn" role="status">
          AgentK can’t work out the web app for {httpHost(m.effectiveUrl) ?? 'this Meko'}, so datapack links
          {m.effectiveApiUrl ? ' are' : ' and sharing are'} off. Set the web app URL to turn them on.
        </p>
      )}

      <div className="meko-status">
        <div className="meko-status-row">
          <span>Meko</span>
          {status.configured ? <span className="tag">on</span> : <span className="tag shared">off</span>}
          {status.configured && <span className="hint">A new URL or key is checked with Meko when you save it.</span>}
        </div>
        <div className="meko-status-row">
          <span>Sharing</span>
          <span className={cx('tag', !status.sharing && 'shared')}>{status.sharing ? 'on' : 'off'}</span>
          {status.configured && !status.sharing && <span className="hint">Needs Meko’s API URL: set the web app or API URL above.</span>}
        </div>
        <div className="meko-status-row">
          <span>Claude Code</span>
          <span className={cx('tag', !claudeMatches && 'shared')}>{claudeMatches ? 'matches' : 'differs'}</span>
          <span className="hint">
            {claudeMatches
              ? `Sessions use ${m.effectiveUrl} with the same key.`
              : `Sessions use ${m.claudeUrl ?? 'the Meko server Claude Code has registered'}${m.claudeKeyMatches ? ' with the same key' : ' with a different key (or none)'}. New sessions pick up a change; running ones keep theirs.`}
          </span>
          {!claudeMatches && (
            <button type="button" className="btn small" disabled={busy !== null} onClick={() => void setUpClaude()}>
              {busy === 'claude' ? <Busy label="Setting up" /> : 'Set up Claude Code with this Meko'}
            </button>
          )}
          {claudeDone && <span className="ok-text">Claude Code now uses this Meko.</span>}
        </div>
      </div>

      {confirmHost && (
        <p className="confirm-warn" role="alert">
          Existing projects’ datapacks live on {httpHost(m.effectiveUrl) ?? 'the current Meko'}, not on {confirmHost}, so they
          won’t work after the switch. Save again to switch anyway.
        </p>
      )}
      <ErrorText error={error} />
      <div className="settings-actions">
        <button className="btn primary" disabled={busy !== null}>
          {busy === 'save' ? <Busy label="Checking" /> : confirmHost ? 'Switch Meko' : 'Save'}
        </button>
        {saved && <span className="muted small">Saved</span>}
      </div>
    </form>
  );
}

/** `[call_home] enabled`, saved as soon as it's changed. */
function UsageStatsCheck() {
  const [on, setOn] = useState<boolean | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    api
      .settings()
      .then((x) => setOn(x.callHomeEnabled))
      .catch((e) => setError(errorMessage(e)));
  }, []);

  const change = async (next: boolean) => {
    setBusy(true);
    setError(null);
    try {
      setOn((await api.saveSettings({ callHomeEnabled: next })).callHomeEnabled);
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="stack">
      {on !== null && (
        <label className="check">
          <input id="acc-usage-stats" type="checkbox" checked={on} disabled={busy} onChange={(e) => void change(e.target.checked)} />
          <span>Send anonymous usage stats to YugabyteDB</span>
        </label>
      )}
      <ErrorText error={error} />
    </div>
  );
}

function LocalAccessNote() {
  return (
    <div className="stack">
      <h3>No sign-in needed</h3>
      <p className="muted">
        AgentK Local only listens on this computer, so it opens without a username and password. To reach it from other
        devices, rerun <code>agent-k-local install</code> and choose remote access: that turns on HTTPS and a login.
      </p>
    </div>
  );
}

function AccountTab({ auth }: { auth: AuthStatus }) {
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const [pwMsg, setPwMsg] = useState<string | null>(null);
  const [pwErr, setPwErr] = useState<string | null>(null);
  const [pwBusy, setPwBusy] = useState(false);

  // The login-time auth status goes stale once two-step sign-in is turned on or off, so ask again.
  const [totpOn, setTotpOn] = useState<boolean | null>(null);
  const [enroll, setEnroll] = useState<TotpEnrollResponse | null>(null);
  const [code, setCode] = useState('');
  const [totpErr, setTotpErr] = useState<string | null>(null);
  const [totpMsg, setTotpMsg] = useState<string | null>(null);
  const [totpBusy, setTotpBusy] = useState(false);

  useEffect(() => {
    let live = true;
    api
      .authStatus()
      .then((a) => live && setTotpOn(a.totpEnabled))
      .catch(() => live && setTotpOn(auth.totpEnabled));
    return () => {
      live = false;
    };
  }, [auth.totpEnabled]);

  const changePw = async (e: FormEvent) => {
    e.preventDefault();
    if (pwBusy) return;
    setPwErr(null);
    setPwMsg(null);
    if (next.length < 12) return setPwErr('Use at least 12 characters.');
    if (next !== confirm) return setPwErr('The two new passwords don’t match.');
    setPwBusy(true);
    try {
      await api.changePassword(current, next);
      setCurrent('');
      setNext('');
      setConfirm('');
      setPwMsg('Password changed.');
    } catch (err) {
      setPwErr(errorMessage(err));
    } finally {
      setPwBusy(false);
    }
  };

  const startEnroll = async () => {
    if (totpBusy) return;
    setTotpErr(null);
    setTotpBusy(true);
    try {
      setEnroll(await api.totpEnroll());
    } catch (err) {
      setTotpErr(errorMessage(err));
    } finally {
      setTotpBusy(false);
    }
  };

  const confirmTotp = async (e: FormEvent) => {
    e.preventDefault();
    if (totpBusy) return;
    setTotpErr(null);
    setTotpBusy(true);
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
    } finally {
      setTotpBusy(false);
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
          <button className="btn primary" disabled={!current || !next || pwBusy}>
            {pwBusy ? <Busy label="Changing" /> : 'Change password'}
          </button>
        </div>
      </form>

      <div className="stack">
        <h3>Two-step sign-in</h3>
        <p className="muted">
          {totpOn === null
            ? <Busy label="Checking" />
            : totpOn
              ? 'On. Sign-in asks for a code from your authenticator app.'
              : 'Off. Add a code from an authenticator app to every sign-in.'}
        </p>
        {totpOn === false && !enroll && (
          <div>
            <button className="btn" disabled={totpBusy} onClick={startEnroll}>
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
            <button className={cx('btn', totpOn ? 'danger' : 'primary')} disabled={code.length !== 6 || totpBusy}>
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
  const [busy, setBusy] = useState<'check' | 'apply' | 'channel' | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => setU(initial), [initial]);

  const setChannel = async (channel: Settings['updateChannel']) => {
    setBusy('channel');
    setError(null);
    try {
      const next = await api.saveSettings({ updateChannel: channel });
      setU((cur) => ({ ...cur, channel: next.updateChannel }));
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(null);
    }
  };

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
      <Field label="Update channel" hint="Saved as soon as you pick one.">
        <select
          id="set-channel"
          className="input"
          value={u.channel}
          disabled={busy === 'channel'}
          onChange={(e) => void setChannel(e.target.value as Settings['updateChannel'])}
        >
          <option value="stable">Stable releases</option>
          <option value="prerelease">Include pre-releases</option>
          <option value="off">Off (don’t check)</option>
        </select>
      </Field>
      <dl className="kv">
        <dt>Installed</dt>
        <dd className="mono">v{u.current}</dd>
        <dt>Latest</dt>
        <dd className="mono">{u.latest ? `v${u.latest}` : '—'}</dd>
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
          {busy === 'check' ? <Busy label="Checking" /> : 'Check now'}
        </button>
        {u.available && (
          <button className="btn primary" disabled={!!busy || u.applying} onClick={() => run('apply')}>
            {busy === 'apply' || u.applying ? <Busy label="Updating" /> : `Update to v${u.latest}`}
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
