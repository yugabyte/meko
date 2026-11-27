// The screens shown in place of the app: signing in through Meko Cloud (remote access with [web]
// signin = "cloud"), and coming back from it; when local access didn't take a request as local; and
// when Meko didn't answer.

import { useEffect, useState, type ReactNode } from "react";
import { Button, Code, ErrorNote, Spinner } from "../components/ui";
import { api, ApiError } from "../lib/api";
import { SIGNIN_CALLBACK_PATH, SIGNIN_PATH, safeNext } from "../lib/auth";

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

// Local access lets in only requests made on this computer straight to Meko; one that came through
// a proxy, or by another name, is refused.
export function NotLocalPage() {
  return (
    <AuthFrame
      title="This request wasn't recognized as local"
      sub="Meko is set to local access, which takes only requests made on the computer it runs on, straight to its address. This one came another way, for example through a proxy."
    >
      <p className="text-sm text-muted-foreground">
        On that computer, open Meko with <Code>meko ui</Code>. To reach it from other devices, set{" "}
        <Code>[web] access = "remote"</Code> in <Code>config.toml</Code> in Meko's data folder and restart Meko. To
        have other computers sign in with the Meko Cloud account Meko is linked to, set{" "}
        <Code>[web] signin = "cloud"</Code> too.
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

// Remote access with Meko Cloud sign-in, for a browser that isn't signed in: one button, which goes
// to Meko Cloud and comes back to next.
export function SignInPage({ next }: { next: string }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<Error>();
  const start = () => {
    setBusy(true);
    setError(undefined);
    api.startSignIn(next).then(
      ({ url }) => window.location.assign(url),
      (err: Error) => {
        setError(err);
        setBusy(false);
      },
    );
  };
  return (
    <AuthFrame
      title="Sign in to Meko"
      sub="This Meko asks other computers to sign in with the Meko Cloud account it's linked to. Meko learns only whether that's you."
    >
      {error && <ErrorNote error={error} />}
      <Button variant="primary" onClick={start} disabled={busy}>
        {busy && <Spinner />}
        Sign in with Meko Cloud
      </Button>
    </AuthFrame>
  );
}

// What Meko Cloud sent the browser back with: a code and state, or a refusal.
type Back = { code: string; state: string } | { cancelled: boolean; error: string };

function readBack(): Back {
  const q = new URLSearchParams(window.location.search);
  const code = q.get("code");
  const state = q.get("state");
  const error = q.get("error");
  if (!error && code && state) return { code, state };
  return { cancelled: error === "access_denied", error: error || "it sent no sign-in code back" };
}

// The finish under way, by state, so the page's effect running twice (React's Strict Mode) posts it
// once: the code works only once.
let finishing: { state: string; done: Promise<{ next: string }> } | undefined;

// Back from Meko Cloud at /signin/callback: finishes signing in, then loads the page asked for.
export function SignInCallbackPage() {
  const [back] = useState(readBack);
  const [error, setError] = useState<{ title: string; message: string }>();
  useEffect(() => {
    // The code is used up either way: take it out of the address and the history.
    history.replaceState(null, "", SIGNIN_CALLBACK_PATH);
    if (!("code" in back)) {
      setError(
        back.cancelled
          ? { title: "Sign-in cancelled", message: "You cancelled signing in at Meko Cloud." }
          : { title: "Meko Cloud didn't sign you in", message: `Meko Cloud answered: ${back.error}.` },
      );
      return;
    }
    if (finishing?.state !== back.state) {
      finishing = { state: back.state, done: api.finishSignIn(back.code, back.state) };
    }
    finishing.done.then(
      ({ next }) => window.location.replace(safeNext(next)),
      (err: Error) =>
        setError({
          title: err instanceof ApiError && err.code === "NOT_THE_OWNER" ? "Signed in as someone else" : "Couldn't sign in",
          message: err.message,
        }),
    );
  }, [back]);
  if (!error) {
    return (
      <AuthFrame title="Signing in…" sub="Finishing signing in with Meko Cloud.">
        <Spinner />
      </AuthFrame>
    );
  }
  return (
    <AuthFrame title={error.title} sub={error.message}>
      <Button onClick={() => window.location.assign(SIGNIN_PATH)}>Sign in again</Button>
    </AuthFrame>
  );
}
