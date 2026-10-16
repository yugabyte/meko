// The projects pane in the wide layout: whether it's hidden and how wide it is, kept in this browser.

const HIDDEN_KEY = 'ak-rail-hidden';
const WIDTH_KEY = 'ak-rail-width';
const COLLAPSED_KEY = 'ak-rail-collapsed';

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
