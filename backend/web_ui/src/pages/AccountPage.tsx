// The login's settings at /account: change the password, turn two-step sign-in (an authenticator
// app's codes) on or off, and sign out. With local access and no login there's nothing to set, so
// it says how remote access gets one.

import { useEffect, useState, type FormEvent, type ReactNode } from "react";
import { LogOut } from "lucide-react";
import { api } from "../lib/api";
import { SIGNIN_PATH } from "../lib/auth";
import type { AuthStatus, TotpEnroll } from "../lib/types";
import { QrCode } from "../components/QrCode";
import { Badge, Button, Card, Code, ErrorNote, Field, Input, Spinner } from "../components/ui";
import { MIN_PASSWORD } from "./AuthPages";

// Whether there's a login to manage: remote access always has one (or will), and local access
// keeps the one remote access made.
export function hasAccount(auth: AuthStatus): boolean {
  return auth.access === "remote" || auth.username !== null;
}

export function LocalAccessNote() {
  return (
    <div className="space-y-1.5 text-sm">
      <p className="font-medium">No sign-in needed on this computer</p>
      <p className="text-muted-foreground">
        Meko takes only requests from this computer, so it opens without a username and password. To reach it from
        other devices, set <Code>[web] access = "remote"</Code> in <Code>config.toml</Code> in Meko's data folder and
        restart Meko: that turns on HTTPS and a login, which <Code>meko setup_link</Code> creates.
      </p>
    </div>
  );
}

function NoLoginYet() {
  return (
    <div className="space-y-1.5 text-sm">
      <p className="font-medium">No login yet</p>
      <p className="text-muted-foreground">
        Meko is open to other devices, and this computer needs no login to reach it. To create the login other devices
        sign in with, run <Code>meko setup_link</Code> here and open the link it prints.
      </p>
    </div>
  );
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Card className="space-y-4 p-4 sm:p-5">
      <h2 className="font-semibold">{title}</h2>
      {children}
    </Card>
  );
}

function ChangePassword() {
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState<string>();
  const [done, setDone] = useState(false);
  const [busy, setBusy] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(undefined);
    setDone(false);
    if (next.length < MIN_PASSWORD) return setError(`Use at least ${MIN_PASSWORD} characters for the new password.`);
    if (next !== confirm) return setError("The two new passwords don't match. Type them again.");
    setBusy(true);
    try {
      await api.changePassword(current, next);
      setCurrent("");
      setNext("");
      setConfirm("");
      setDone(true);
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Section title="Change password">
      <form className="space-y-4" onSubmit={submit}>
        <Field label="Current password">
          <Input
            id="account-current"
            type="password"
            autoComplete="current-password"
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
            required
          />
        </Field>
        <div className="grid gap-4 sm:grid-cols-2">
          <Field label="New password" hint={`At least ${MIN_PASSWORD} characters.`}>
            <Input
              id="account-next"
              type="password"
              autoComplete="new-password"
              value={next}
              onChange={(e) => setNext(e.target.value)}
              required
            />
          </Field>
          <Field label="Confirm new password">
            <Input
              id="account-confirm"
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              required
            />
          </Field>
        </div>
        {error && <ErrorNote error={new Error(error)} />}
        {done && <p className="text-sm text-success">Password changed.</p>}
        <Button type="submit" variant="primary" disabled={busy || !current || !next}>
          {busy && <Spinner className="text-primary-foreground" />} Change password
        </Button>
      </form>
    </Section>
  );
}

function TwoStep({ enabled, onChanged }: { enabled: boolean; onChanged: () => void }) {
  const [on, setOn] = useState(enabled);
  const [enroll, setEnroll] = useState<TotpEnroll>();
  const [code, setCode] = useState("");
  const [error, setError] = useState<string>();
  const [note, setNote] = useState<string>();
  const [busy, setBusy] = useState(false);

  useEffect(() => setOn(enabled), [enabled]);

  const start = async () => {
    setError(undefined);
    setNote(undefined);
    setBusy(true);
    try {
      setEnroll(await api.totpEnroll());
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(undefined);
    setBusy(true);
    try {
      if (on) {
        await api.totpDisable(code);
        setOn(false);
        setNote("Two-step sign-in is off.");
      } else {
        await api.totpConfirm(code);
        setOn(true);
        setEnroll(undefined);
        setNote("Two-step sign-in is on. Sign-in now asks for a code from your authenticator app.");
      }
      setCode("");
      onChanged();
    } catch (err) {
      setError((err as Error).message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <Section title="Two-step sign-in">
      <div className="flex flex-wrap items-center gap-2 text-sm">
        <Badge tone={on ? "success" : "neutral"}>{on ? "on" : "off"}</Badge>
        <span className="text-muted-foreground">
          {on
            ? "Sign-in asks for a code from your authenticator app after the password."
            : "Add a code from an authenticator app to every sign-in."}
        </span>
      </div>
      {!on && !enroll && (
        <Button onClick={start} disabled={busy}>
          {busy && <Spinner />} Set up an authenticator
        </Button>
      )}
      {enroll && !on && (
        <div className="flex flex-col gap-4 sm:flex-row sm:items-start">
          <QrCode text={enroll.otpauth_url} label="QR code to add Meko to your authenticator app" />
          <div className="min-w-0 space-y-2 text-sm">
            <p>Scan the code with your authenticator app, or enter this key in it:</p>
            <code className="block break-all rounded bg-muted px-2 py-1.5 font-mono text-[12px]">{enroll.secret}</code>
            <p className="text-muted-foreground">Then type the 6-digit code it shows to turn two-step sign-in on.</p>
          </div>
        </div>
      )}
      {(enroll || on) && (
        <form className="flex flex-wrap gap-2" onSubmit={submit}>
          <Input
            id="account-totp"
            className="w-36 font-mono tracking-widest"
            inputMode="numeric"
            autoComplete="one-time-code"
            placeholder="6-digit code"
            aria-label="Authenticator code"
            maxLength={6}
            value={code}
            onChange={(e) => setCode(e.target.value.replace(/\D/g, ""))}
          />
          <Button type="submit" variant={on ? "outline" : "primary"} disabled={busy || code.length !== 6}>
            {busy && <Spinner />} {on ? "Turn off" : "Turn on"}
          </Button>
        </form>
      )}
      {error && <ErrorNote error={new Error(error)} />}
      {note && <p className="text-sm text-success">{note}</p>}
    </Section>
  );
}

function SignOut() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();
  const signOut = async () => {
    setBusy(true);
    setError(undefined);
    try {
      await api.logout();
      window.location.assign(SIGNIN_PATH);
    } catch (err) {
      setError(err as Error);
      setBusy(false);
    }
  };
  return (
    <Section title="Sign out">
      <p className="text-sm text-muted-foreground">Signs this browser out. Other browsers stay signed in.</p>
      <Button onClick={signOut} disabled={busy}>
        {busy ? <Spinner /> : <LogOut className="size-4" />} Sign out
      </Button>
      {error && <ErrorNote error={error} />}
    </Section>
  );
}

export function AccountPage({ auth, onChanged }: { auth: AuthStatus; onChanged: () => void }) {
  return (
    <div className="max-w-2xl space-y-4">
      <div className="space-y-1">
        <h1 className="text-xl font-semibold">Account</h1>
        {auth.username && (
          <p className="text-sm text-muted-foreground">
            {auth.signed_in ? "Signed in as" : "The login is"} <span className="font-medium text-foreground">{auth.username}</span>
          </p>
        )}
      </div>
      {!hasAccount(auth) ? (
        <Card className="p-4 sm:p-5">
          <LocalAccessNote />
        </Card>
      ) : !auth.username ? (
        <Card className="p-4 sm:p-5">
          <NoLoginYet />
        </Card>
      ) : (
        <>
          <ChangePassword />
          <TwoStep enabled={auth.totp_enabled} onChanged={onChanged} />
          {auth.signed_in && <SignOut />}
        </>
      )}
    </div>
  );
}
