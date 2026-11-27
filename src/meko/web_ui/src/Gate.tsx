// What a page load shows first, from /api/v1/auth/status: the app, or, with remote access and Meko
// Cloud sign-in, the sign-in page for a browser that isn't signed in (coming back to the page it
// was on). /signin/callback, where Meko Cloud sends it back, finishes signing in before anything
// else. Once in, /signin goes on to its next, the old setup page's address (/setup) to the start,
// the old account page's (/account) to Settings, and a datapack's old Connect tab
// (/datapacks/<id>/connect) to Agents; a page outside the app (an extension's, /ext/<name>/) is
// loaded itself. A request Meko later refuses says so: 401 SIGNIN_REQUIRED (the session ended)
// shows the sign-in page; 401 UNAUTHORIZED, which only local access sends, that it didn't look
// local.

import { useEffect, useState } from "react";
import App from "./App";
import { api, onSignInRequired, onUnauthorized } from "./lib/api";
import { oldAddressTarget, signInNext, SIGNIN_CALLBACK_PATH } from "./lib/auth";
import { isAppPath } from "./lib/route";
import { useLoad } from "./lib/useLoad";
import { Loading } from "./components/ui";
import { NotLocalPage, SignInCallbackPage, SignInPage, UnreachablePage } from "./pages/AuthPages";

export function Gate() {
  if (window.location.pathname === SIGNIN_CALLBACK_PATH) return <SignInCallbackPage />;
  return <Signed />;
}

function Signed() {
  const status = useLoad(() => api.authStatus(), []);
  const [refused, setRefused] = useState(false);
  const [signedOut, setSignedOut] = useState(false);
  const auth = status.data;
  const signedIn = !!auth?.signed_in && !signedOut;

  useEffect(() => onUnauthorized(() => setRefused(true)), []);
  useEffect(() => onSignInRequired(() => setSignedOut(true)), []);
  // The app reads its route from the address, so an old one is replaced before it renders. Only
  // once in: a browser that must sign in stays on its address, to come back to after.
  useEffect(() => {
    if (!signedIn) return;
    const to = oldAddressTarget(window.location.pathname, window.location.search);
    if (to === null) return;
    if (!isAppPath(new URL(to, window.location.origin).pathname)) {
      window.location.replace(to);
      return;
    }
    history.replaceState(null, "", to);
    window.dispatchEvent(new PopStateEvent("popstate"));
  }, [signedIn]);

  if (!auth) return status.error ? <UnreachablePage error={status.error} onRetry={status.reload} /> : <Loading />;
  if (!signedIn && (auth.signin === "cloud" || signedOut)) {
    const { pathname, search, hash } = window.location;
    return <SignInPage next={signInNext(pathname, search, hash)} />;
  }
  // Shown in place, not loaded again, so a server that keeps refusing can't loop the page.
  if (refused) return <NotLocalPage />;
  return <App auth={auth} />;
}
