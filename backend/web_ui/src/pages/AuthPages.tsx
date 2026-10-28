// The screens before the app: sign in (password, then an authenticator code when the login has
// one), set up or reset the login from a setup link, what to do when remote access has no login
// yet, and what to do when local access didn't take a request as local.

import { useState, type FormEvent, type ReactNode } from "react";
import { ApiError, api } from "../lib/api";
import { SIGNIN_PATH, safeNext } from "../lib/auth";
import { CopyBlock } from "../components/CopyBlock";
import { Button, Code, ErrorNote, Field, Input, Spinner } from "../components/ui";

export const MIN_PASSWORD = 12;

function AuthFrame({ title, sub, children }: { title: string; sub: ReactNode; children?: ReactNode }) {
  return (
    <div className="flex min-h-screen items-center justify-center px-4 py-8">
      <div className="w-full max-w-sm space-y-5 rounded-lg border bg-card p-6 shadow-sm">
        <img src="/meko-logo.svg" alt="Meko" className="h-7 dark:hidden" />
        <img src="/meko-logo-white.svg" alt="Meko" className="hidden h-7 dark:block" />
        <div className="space-y-1.5">
          <h1 className="text-lg font-semibold">{title}</h1>
          <div className="text-sm text-muted-foreground">{sub}</div>
        </div>
        {children}
      </div>
    </div>
  );
}

function Submit({ busy, label, busyLabel }: { busy: boolean; label: string; busyLabel: string }) {
  return (
    <Button type="submit" variant="primary" className="w-full" disabled={busy}>
      {busy ? (
        <>
          <Spinner className="text-primary-foreground" /> {busyLabel}
        </>
      ) : (
        label
      )}
    </Button>
  );
}

// There's no email to reset a password with: the login guards this install's data, so a new one
// comes from someone who can run commands on the computer Meko runs on.
function ForgotPassword() {
  return (
    <div className="space-y-2 rounded-lg border bg-muted/50 p-3 text-sm">
      <p className="font-medium">Reset your login from the computer Meko runs on</p>
      <p className="text-muted-foreground">Sign in there as the user Meko runs as, and run:</p>
      <CopyBlock text="meko setup_link" />
      <p className="text-muted-foreground">
        Open the link it prints (it works once, for 7 days) and choose a new username and password. That signs out every
        browser and turns two-step sign-in off. Your datapacks stay as they are.
      </p>
    </div>
  );
}

export function SignInPage() {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [totp, setTotp] = useState("");
  const [needTotp, setNeedTotp] = useState(false);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [forgot, setForgot] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(undefined);
    setBusy(true);
    try {
      await api.login(username.trim(), password, needTotp ? totp : undefined);
      window.location.assign(safeNext(new URLSearchParams(window.location.search).get("next")));
      return;
    } catch (err) {
      if (err instanceof ApiError && err.code === "TOTP_REQUIRED") {
        // The first time, the code just wasn't asked for yet; after that the message says what was wrong.
        if (needTotp) setError(err.message);
        setNeedTotp(true);
        setTotp("");
      } else {
        // RATE_LIMITED's message says how long to wait.
        setError((err as Error).message);
      }
    }
    setBusy(false);
  };

  return (
    <AuthFrame title="Sign in" sub="Sign in to reach your datapacks.">
      <form className="space-y-4" onSubmit={submit}>
        <Field label="Username">
          <Input
            id="signin-username"
            autoComplete="username"
            autoCapitalize="none"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoFocus
            required
          />
        </Field>
        <Field label="Password">
          <Input
            id="signin-password"
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
          />
        </Field>
        {needTotp && (
          <Field label="Authenticator code" hint="The 6-digit code your authenticator app shows for Meko.">
            <Input
              id="signin-totp"
              className="font-mono tracking-widest"
              inputMode="numeric"
              autoComplete="one-time-code"
              pattern="[0-9]{6}"
              maxLength={6}
              value={totp}
              onChange={(e) => setTotp(e.target.value.replace(/\D/g, ""))}
              autoFocus
              required
            />
          </Field>
        )}
        {error && <ErrorNote error={new Error(error)} />}
        <Submit busy={busy} label="Sign in" busyLabel="Signing in" />
      </form>
      {forgot ? (
        <ForgotPassword />
      ) : (
        <button type="button" className="text-sm text-primary hover:underline" onClick={() => setForgot(true)}>
          Forgot your password?
        </button>
      )}
    </AuthFrame>
  );
}

// Opened from the link meko setup_link prints (/setup?token=...): creates the login, or replaces
// it when one exists (reset).
export function SetupPage({ reset }: { reset: boolean }) {
  // Read once: the address can change under the page (Gate's replaceState) without a remount.
  const [token] = useState(() => new URLSearchParams(window.location.search).get("token") ?? "");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(undefined);
    if (password.length < MIN_PASSWORD) return setError(`Use at least ${MIN_PASSWORD} characters for the password.`);
    if (password !== confirm) return setError("The two passwords don't match. Type them again.");
    setBusy(true);
    try {
      await api.setup(token, username.trim(), password);
      window.location.assign("/");
      return;
    } catch (err) {
      // FORBIDDEN (a used or expired link) and RATE_LIMITED say what to do in their message.
      setError((err as Error).message);
    }
    setBusy(false);
  };

  return (
    <AuthFrame
      title={reset ? "Reset your login" : "Create your login"}
      sub={
        reset
          ? "Choose a new username and password. They replace the old ones, sign out every browser and turn two-step sign-in off. Your datapacks stay as they are."
          : "Meko is open to other devices, so it takes a login. Choose the username and password you'll sign in with."
      }
    >
      <form className="space-y-4" onSubmit={submit}>
        <Field label="Username" hint="Up to 64 letters, digits, dots, @, dashes or underscores.">
          <Input
            id="setup-username"
            autoComplete="username"
            autoCapitalize="none"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            autoFocus
            required
          />
        </Field>
        <Field label="Password" hint={`At least ${MIN_PASSWORD} characters.`}>
          <Input
            id="setup-password"
            type="password"
            autoComplete="new-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            required
          />
        </Field>
        <Field label="Confirm password">
          <Input
            id="setup-confirm"
            type="password"
            autoComplete="new-password"
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
            required
          />
        </Field>
        {error && <ErrorNote error={new Error(error)} />}
        <Submit
          busy={busy}
          label={reset ? "Reset login" : "Create login"}
          busyLabel={reset ? "Resetting login" : "Creating login"}
        />
      </form>
    </AuthFrame>
  );
}

// Remote access without a login, or with a setup link not used yet: the link comes from the host.
export function SetupNeededPage({ reset }: { reset: boolean }) {
  return (
    <AuthFrame
      title={reset ? "A setup link is waiting" : "Set up a login first"}
      sub={
        reset
          ? "Someone made a setup link to reset this Meko's login. Open that link to choose a new username and password, or make a new one: on the computer Meko runs on, run:"
          : "Meko is open to other devices, so it needs a login before anyone can use it. On the computer Meko runs on, run:"
      }
    >
      <CopyBlock text="meko setup_link" />
      <p className="text-sm text-muted-foreground">
        Open the link it prints (it works once, for 7 days) and choose your username and password.
      </p>
      {reset && (
        <p className="text-sm text-muted-foreground">
          Still know your password?{" "}
          <a className="text-primary hover:underline" href={SIGNIN_PATH}>
            Sign in
          </a>
          .
        </p>
      )}
    </AuthFrame>
  );
}

// Local access lets in only requests made on this computer straight to Meko; one that came through
// a proxy, or by another name, is refused.
export function NotLocalPage() {
  return (
    <AuthFrame
      title="This request wasn't recognized as local"
      sub="Meko is set to local access, which needs no login but takes only requests made on the computer it runs on, straight to its address. This one came another way, for example through a proxy."
    >
      <p className="text-sm text-muted-foreground">
        On that computer, open Meko with <Code>meko ui</Code>. To reach it from other devices with a password instead,
        set <Code>[web] access = "remote"</Code> in <Code>config.toml</Code> in Meko's data folder, restart Meko and
        run <Code>meko setup_link</Code> to create the login.
      </p>
    </AuthFrame>
  );
}

// The status couldn't be read: the server is down or isn't Meko.
export function UnreachablePage({ error, onRetry }: { error: Error; onRetry: () => void }) {
  return (
    <AuthFrame title="Meko didn't answer" sub="Check that Meko is running (meko status), then try again.">
      <ErrorNote error={error} />
      <Button onClick={onRetry}>Try again</Button>
    </AuthFrame>
  );
}
