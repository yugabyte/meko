// What a page load shows first, from /api/v1/auth/status: the app, since there's no login. The
// old sign-in screens' addresses (/signin, /setup) go on to where they were headed, and the old
// account page's (/account) to Settings; a page outside the app (an extension's, /ext/<name>/) is
// loaded itself. A request Meko later refuses (401 UNAUTHORIZED, which only local access sends)
// says it didn't look local.

import { useEffect, useState } from "react";
import App from "./App";
import { api, onUnauthorized } from "./lib/api";
import { oldAddressTarget } from "./lib/auth";
import { isAppPath } from "./lib/route";
import { useLoad } from "./lib/useLoad";
import { Loading } from "./components/ui";
import { NotLocalPage, UnreachablePage } from "./pages/AuthPages";

export function Gate() {
  const status = useLoad(() => api.authStatus(), []);
  const [refused, setRefused] = useState(false);
  const auth = status.data;

  useEffect(() => onUnauthorized(() => setRefused(true)), []);
  // The app reads its route from the address, so an old one is replaced before it renders.
  useEffect(() => {
    const to = oldAddressTarget(window.location.pathname, window.location.search);
    if (to === null) return;
    if (!isAppPath(new URL(to, window.location.origin).pathname)) {
      window.location.replace(to);
      return;
    }
    history.replaceState(null, "", to);
    window.dispatchEvent(new PopStateEvent("popstate"));
  }, []);

  if (!auth) return status.error ? <UnreachablePage error={status.error} onRetry={status.reload} /> : <Loading />;
  // Shown in place, not loaded again, so a server that keeps refusing can't loop the page.
  if (refused) return <NotLocalPage />;
  return <App auth={auth} />;
}
