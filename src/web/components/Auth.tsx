import { useState, type FormEvent, type ReactNode } from 'react';
import { ApiError, api, errorMessage } from '../api';
import { Busy, ErrorText, Field } from './common';

function AuthFrame({ title, sub, children }: { title: string; sub: string; children: ReactNode }) {
  return (
    <div className="auth-page">
      <div className="auth-card">
        <div className="auth-brand">
          <img src="/logo.png" alt="" width={56} height={56} />
          <div>
            <div className="brand-name">AgentK Local</div>
            <div className="muted small">Agentic coder</div>
          </div>
        </div>
        <h1>{title}</h1>
        <p className="muted">{sub}</p>
        {children}
      </div>
    </div>
  );
}

export function SetupScreen({ reset, onDone }: { reset: boolean; onDone: () => void }) {
  const [token, setToken] = useState(() => new URLSearchParams(location.search).get('token') ?? '');
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    if (password.length < 12) return setError('Use at least 12 characters.');
    if (password !== confirm) return setError('The two passwords don’t match.');
    setBusy(true);
    try {
      await api.setup({ token: token.trim(), username: username.trim(), password });
      history.replaceState(null, '', '/');
      onDone();
    } catch (err) {
      setError(errorMessage(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthFrame
      title={reset ? 'Reset your login' : 'Set up your account'}
      sub={
        reset
          ? 'Choose a new username and password. This signs out every browser and turns off two-step sign-in, if you had it on. Your projects and sessions stay as they are.'
          : 'This install has one owner. The terminal runs as your Unix user, so choose a password you’d trust with your SSH key.'
      }
    >
      <form className="stack" onSubmit={submit}>
        <Field
          label="Setup token"
          hint={reset ? 'Printed by `agent-k-local setup-link` on the host. Run it again for a new one.' : 'Printed by the installer. Run `agent-k-local setup-link` for a new one.'}
        >
          <input id="setup-token" className="input mono" value={token} onChange={(e) => setToken(e.target.value)} required />
        </Field>
        <Field label="Username" hint="Letters, digits, dots, @, dashes or underscores. An email address works.">
          <input
            id="setup-username"
            className="input"
            autoComplete="username"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            required
          />
        </Field>
        <Field label="Password">
          <input
            id="setup-password"
            className="input"
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
          />
        </Field>
        <Field label="Confirm password">
          <input
            id="setup-confirm"
            className="input"
            type="password"
            autoComplete="new-password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            required
          />
        </Field>
        <ErrorText error={error} />
        <button className="btn primary block" disabled={busy}>
          {busy ? <Busy label={reset ? 'Resetting login' : 'Creating account'} /> : reset ? 'Reset login' : 'Create account'}
        </button>
      </form>
    </AuthFrame>
  );
}

export function LoginScreen({ onDone }: { onDone: () => void }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const [totp, setTotp] = useState('');
  const [needTotp, setNeedTotp] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [forgot, setForgot] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(null);
    setBusy(true);
    try {
      await api.login({ username: username.trim(), password, totp: needTotp ? totp.trim() : undefined });
      onDone();
    } catch (err) {
      if (err instanceof ApiError && err.status === 401 && err.body.totpRequired) {
        if (needTotp) setError(err.message || 'That code didn’t work. Try the current one.');
        setNeedTotp(true);
      } else if (err instanceof ApiError && err.status === 429) {
        setError('Too many failed sign-ins. Sign-in is locked for 15 minutes. Try again then, or reset your login now.');
        setForgot(true);
      } else {
        setError(errorMessage(err));
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <AuthFrame title="Sign in" sub="Sign in to reach your projects and sessions.">
      <form className="stack" onSubmit={submit}>
        <Field label="Username">
          <input
            id="login-username"
            className="input"
            autoComplete="username"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoFocus
            required
          />
        </Field>
        <Field label="Password">
          <input
            id="login-password"
            className="input"
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
          />
        </Field>
        {needTotp && (
          <Field label="Authenticator code">
            <input
              id="login-totp"
              className="input mono"
              inputMode="numeric"
              autoComplete="one-time-code"
              pattern="[0-9]{6}"
              maxLength={6}
              value={totp}
              onChange={(e) => setTotp(e.target.value.replace(/\D/g, ''))}
              autoFocus
              required
            />
          </Field>
        )}
        <ErrorText error={error} />
        <button className="btn primary block" disabled={busy}>
          {busy ? <Busy label="Signing in" /> : 'Sign in'}
        </button>
      </form>
      {forgot ? <ForgotPassword /> : (
        <button type="button" className="link-btn" onClick={() => setForgot(true)}>
          Forgot password?
        </button>
      )}
    </AuthFrame>
  );
}

/**
 * How to get back in. There's no email or reset form: this install's one login guards a shell
 * on the host, so a reset has to come from someone who can already run commands there.
 */
function ForgotPassword() {
  return (
    <div className="auth-forgot" id="login-forgot">
      <strong>Reset your login from the host</strong>
      <p className="muted small">
        On the computer AgentK Local runs on, sign in as the user it runs as and run:
      </p>
      <code className="auth-cmd">agent-k-local setup-link</code>
      <p className="muted small">
        Open the one-time link it prints (it expires in 7 days) and choose a new username and password. This also signs
        out every browser and turns off two-step sign-in, if you had it on. Your projects and sessions stay as they are.
      </p>
    </div>
  );
}
