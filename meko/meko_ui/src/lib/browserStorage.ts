/** Get an item from `sessionStorage`. Returns `null` when missing or
 *  when storage is unavailable. */
export function getSessionItem(key: string): string | null {
  try {
    return sessionStorage.getItem(key);
  } catch {
    return null;
  }
}

/** Write an item to `sessionStorage`. No-op when storage is unavailable. */
export function setSessionItem(key: string, value: string): void {
  try {
    sessionStorage.setItem(key, value);
  } catch {
    // private mode / denied — silently drop. Callers should treat
    // storage as best-effort, not authoritative.
  }
}

/** Remove an item from `sessionStorage`. No-op when storage is unavailable. */
export function removeSessionItem(key: string): void {
  try {
    sessionStorage.removeItem(key);
  } catch {
    // ignore — nothing to recover
  }
}

/** Get an item from `localStorage`. Returns `null` when missing or
 *  when storage is unavailable. */
export function getLocalItem(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

/** Write an item to `localStorage`. No-op when storage is unavailable. */
export function setLocalItem(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    // private mode / denied / quota exceeded — silently drop.
  }
}

/** Remove an item from `localStorage`. No-op when storage is unavailable. */
export function removeLocalItem(key: string): void {
  try {
    localStorage.removeItem(key);
  } catch {
    // ignore
  }
}

/** Read with the hybrid strategy: prefer this tab's `sessionStorage`,
 *  fall back to shared `localStorage`. When the value is found in
 *  `localStorage` it's mirrored back to `sessionStorage` so the tab
 *  pins on that value even if another tab updates `localStorage` later
 *  in the session. Returns `null` when both miss. */
export function getPersistedItem(key: string): string | null {
  const sessionHit = getSessionItem(key);
  if (sessionHit !== null) return sessionHit;
  const localHit = getLocalItem(key);
  if (localHit !== null) {
    // Mirror into session so subsequent reads in this tab are stable
    // even if another tab races on a localStorage write.
    setSessionItem(key, localHit);
    return localHit;
  }
  return null;
}

/** Write with the hybrid strategy: this tab's `sessionStorage` AND
 *  shared `localStorage`. `sessionStorage` shadows for the rest of
 *  this tab's lifetime; `localStorage` becomes the "last known good"
 *  that any future fresh tab will inherit on its first read. */
export function setPersistedItem(key: string, value: string): void {
  setSessionItem(key, value);
  setLocalItem(key, value);
}

/** Clear with the hybrid strategy: removes from BOTH `sessionStorage`
 *  and `localStorage` so a future tab doesn't resurrect a value the
 *  user explicitly cleared. */
export function removePersistedItem(key: string): void {
  removeSessionItem(key);
  removeLocalItem(key);
}
