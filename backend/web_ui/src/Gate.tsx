// What a page load shows first, from /api/v1/auth/status: the setup screens while remote access has
// no login (or a setup link waits), the sign-in screen while this browser isn't signed in, else the
// app. A request Meko later refuses (401 UNAUTHORIZED) puts a remote browser back on the sign-in
// screen, and tells a local one its request didn't look local.

import { useEffect, useState } from "react";
import App from "./App";
import { api, onUnauthorized } from "./lib/api";
import { SETUP_PATH, SIGNIN_PATH, safeNext, signinHref } from "./lib/auth";
import { useLoad } from "./lib/useLoad";
import { Loading } from "./components/ui";
import { NotLocalPage, SetupNeededPage, SetupPage, SignInPage, UnreachablePage } from "./pages/AuthPages";

export function Gate() {
  const status = useLoad(() => api.authStatus(), []);
  const [refused, setRefused] = useState(false);
  const auth = status.data;
  const path = window.location.pathname;

  useEffect(() => {
    onUnauthorized(() => {
      if (auth?.access === "remote") history.replaceState(null, "", signinHref());
      setRefused(true);
    });
  }, [auth?.access]);

  const token = new URLSearchParams(window.location.search).get("token");
  // A setup link's screen comes before sign-in, so its address (and the token in it) stays put.
  const onSetupLink = !!auth?.setup_required && path === SETUP_PATH && !!token;
  const needsSignIn = auth?.access === "remote" && !auth.signed_in && !auth.local_access;
  const inApp = auth && !refused && !auth.setup_required && !needsSignIn;
  // The sign-in screens' addresses mean nothing inside the app: go where the sign-in was headed.
  useEffect(() => {
    if (inApp && (path === SIGNIN_PATH || path === SETUP_PATH)) {
      history.replaceState(null, "", safeNext(new URLSearchParams(window.location.search).get("next")));
      // The app read its route from the old address; popstate has it read the new one.
      window.dispatchEvent(new PopStateEvent("popstate"));
    }
  }, [inApp, path]);
  // Any other page asks for the sign-in screen first, coming back to it after.
  useEffect(() => {
    if (needsSignIn && !onSetupLink && path !== SIGNIN_PATH) history.replaceState(null, "", signinHref());
  }, [needsSignIn, onSetupLink, path]);

  if (!auth) return status.error ? <UnreachablePage error={status.error} onRetry={status.reload} /> : <Loading />;
  // Shown in place, not loaded again, so a server that keeps refusing can't loop the page.
  if (refused) return auth.access === "remote" ? <SignInPage /> : <NotLocalPage />;
  if (auth.setup_required) {
    if (onSetupLink) return <SetupPage reset={auth.reset} />;
    // A login that a waiting setup link would replace still signs in until the link is used.
    if (auth.reset && path === SIGNIN_PATH) return <SignInPage />;
    return <SetupNeededPage reset={auth.reset} />;
  }
  if (needsSignIn) return <SignInPage />;
  return <App auth={auth} onAuthChanged={status.reload} />;
}
