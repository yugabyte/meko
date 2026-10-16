// The projects pane: whether it lists sessions, how wide it is, and which projects' sessions are
// folded, kept in this browser.
import { useEffect, useState } from 'react';

const WIDTH_KEY = 'ak-rail-width';
const COLLAPSED_KEY = 'ak-rail-collapsed';
const SESSIONS_KEY = 'ak-rail-sessions';
const SESSIONS_CHANGED = 'ak-rail-sessions-changed';

export const RAIL_DEFAULT = 248;
export const RAIL_MIN = 180;
export const RAIL_MAX = 480;

export function clampRailWidth(w: number): number {
  return Math.round(Math.min(RAIL_MAX, Math.max(RAIL_MIN, w)));
}

/** The saved width, or null for the default. */
export function railWidthSaved(): number | null {
  try {
    const v = Number(localStorage.getItem(WIDTH_KEY));
    return v ? clampRailWidth(v) : null;
  } catch {
    return null;
  }
}

export function saveRailWidth(w: number | null): void {
  try {
    if (w === null) localStorage.removeItem(WIDTH_KEY);
    else localStorage.setItem(WIDTH_KEY, String(w));
  } catch {
    /* storage unavailable: it holds for this page */
  }
}

/** The projects whose session lists are folded away. */
export function railCollapsedSaved(): Set<string> {
  try {
    const v: unknown = JSON.parse(localStorage.getItem(COLLAPSED_KEY) ?? '[]');
    return new Set(Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []);
  } catch {
    return new Set();
  }
}

export function saveRailCollapsed(ids: Set<string>): void {
  try {
    if (ids.size) localStorage.setItem(COLLAPSED_KEY, JSON.stringify([...ids]));
    else localStorage.removeItem(COLLAPSED_KEY);
  } catch {
    /* storage unavailable: it holds for this page */
  }
}

// Set by a save in this page, so the setting holds here when storage can't keep it; another tab's
// change (a storage event) clears it.
let sessionsHere: boolean | null = null;

/** Whether the pane lists each project's sessions under it, in place of the tabs above the terminal. Off by default. */
export function railSessionsSaved(): boolean {
  if (sessionsHere !== null) return sessionsHere;
  try {
    return localStorage.getItem(SESSIONS_KEY) === '1';
  } catch {
    return false;
  }
}

export function saveRailSessions(on: boolean): void {
  sessionsHere = on;
  try {
    if (on) localStorage.setItem(SESSIONS_KEY, '1');
    else localStorage.removeItem(SESSIONS_KEY);
  } catch {
    /* storage unavailable: it holds for this page */
  }
  window.dispatchEvent(new Event(SESSIONS_CHANGED));
}

/** The setting, kept current as it changes here (Settings) or in another tab. */
export function useRailSessions(): boolean {
  const [on, setOn] = useState(railSessionsSaved);
  useEffect(() => {
    const update = () => setOn(railSessionsSaved());
    const onStorage = (e: StorageEvent) => {
      if (e.key !== SESSIONS_KEY && e.key !== null) return;
      sessionsHere = null;
      update();
    };
    window.addEventListener(SESSIONS_CHANGED, update);
    window.addEventListener('storage', onStorage);
    return () => {
      window.removeEventListener(SESSIONS_CHANGED, update);
      window.removeEventListener('storage', onStorage);
    };
  }, []);
  return on;
}
