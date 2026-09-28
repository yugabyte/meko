import { useEffect, useRef, useState } from 'react';
import { isBusy, pageVersion, planReload, RELOADED_FOR_KEY } from '../reload';

/** How often a stale page that's waiting on the user checks again (a dialog has no close event). */
const RECHECK_MS = 5000;

function readReloadedFor(): string | null {
  try {
    return sessionStorage.getItem(RELOADED_FOR_KEY);
  } catch {
    return null;
  }
}

function writeReloadedFor(v: string | null): void {
  try {
    if (v) sessionStorage.setItem(RELOADED_FOR_KEY, v);
    else sessionStorage.removeItem(RELOADED_FOR_KEY);
  } catch {
    /* storage blocked: the page just may reload once more */
  }
}

/**
 * Reloads the page once the server runs another version than the one that served it, unless the
 * user is busy (see isBusy); then it checks again until they aren't. Returns the server's version
 * while the page is stale and waiting, for the Reload banner, else null.
 */
export function useReload(serverVersion: string | undefined): string | null {
  // The version the page came from; without the meta tag, the first one the server reports.
  const loaded = useRef<string | null>(pageVersion(document));
  const [stale, setStale] = useState<string | null>(null);

  useEffect(() => {
    if (!serverVersion) return;
    loaded.current ??= serverVersion;
    const check = () => {
      const plan = planReload({ loaded: loaded.current!, server: serverVersion, busy: isBusy(document), reloadedFor: readReloadedFor() });
      if (plan === 'current') {
        writeReloadedFor(null);
        setStale(null);
        return true;
      }
      if (plan === 'reload') {
        writeReloadedFor(serverVersion);
        location.reload();
        return true;
      }
      setStale(serverVersion);
      return false;
    };
    if (check()) return;
    // Waiting: try again when focus leaves a field, the tab is shown or hidden, and every few seconds.
    const later = () => setTimeout(check, 0);
    document.addEventListener('focusout', later);
    document.addEventListener('visibilitychange', later);
    const timer = setInterval(check, RECHECK_MS);
    return () => {
      document.removeEventListener('focusout', later);
      document.removeEventListener('visibilitychange', later);
      clearInterval(timer);
    };
  }, [serverVersion]);

  return stale;
}
