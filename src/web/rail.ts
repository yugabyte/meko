// The projects pane in the wide layout: whether it's hidden and how wide it is, kept in this browser.

const HIDDEN_KEY = 'ak-rail-hidden';
const WIDTH_KEY = 'ak-rail-width';

export const RAIL_DEFAULT = 228;
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
