// The projects pane: whether it lists sessions, is hidden (the wide layout), how wide it is, and which
// projects' sessions are folded, kept in this browser.
import { browserToggle } from './toggle';

const HIDDEN_KEY = 'ak-rail-hidden';
const WIDTH_KEY = 'ak-rail-width';
const COLLAPSED_KEY = 'ak-rail-collapsed';
const SESSIONS_KEY = 'ak-rail-sessions';

export const RAIL_DEFAULT = 248;
export const RAIL_MIN = 180;
export const RAIL_MAX = 480;

export function clampRailWidth(w: number): number {
  return Math.round(Math.min(RAIL_MAX, Math.max(RAIL_MIN, w)));
}

export function railHiddenSaved(): boolean {
  try {
    return localStorage.getItem(HIDDEN_KEY) === '1';
  } catch {
    return false;
  }
}

export function saveRailHidden(hidden: boolean): void {
  try {
    if (hidden) localStorage.setItem(HIDDEN_KEY, '1');
    else localStorage.removeItem(HIDDEN_KEY);
  } catch {
    /* storage unavailable: it holds for this page */
  }
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

const sessions = browserToggle(SESSIONS_KEY, false);

/** Whether the pane lists each project's sessions under it, in place of the tabs above the terminal. Off by default. */
export const railSessionsSaved = sessions.saved;
export const saveRailSessions = sessions.save;
export const useRailSessions = sessions.use;

/**
 * Whether the session screen shows the session tabs (above the terminal, or in a phone's app bar): not
 * while the pane lists the sessions and is shown. A phone's drawer always shows the pane, whatever the
 * wide layout's hidden setting says.
 */
export function sessionTabsShown(railSessions: boolean, railHidden: boolean, mobile: boolean): boolean {
  return !railSessions || (railHidden && !mobile);
}
