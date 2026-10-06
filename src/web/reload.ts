// Reloading the page after the server comes back on another version (an update, a reinstall or a
// rollback), so the browser never keeps running the old UI. Pure helpers; useReload.ts wires them up.

/** The meta tag the server stamps into index.html with its version (server.ts). */
export const VERSION_META = 'ak-version';

/** sessionStorage key: the server version this tab last reloaded for, so a reload can't loop. */
export const RELOADED_FOR_KEY = 'ak-reloaded-for';

/** Just what isBusy reads of an element, so tests can pass plain objects. */
export interface FieldLike {
  tagName: string;
  type?: string;
  value?: string;
  isContentEditable?: boolean;
  dataset?: { reloadSafe?: string };
  classList?: { contains(c: string): boolean };
}

/** Just what isBusy and pageVersion read of the document. */
export interface DocLike {
  activeElement: FieldLike | null;
  querySelector(sel: string): { getAttribute?(name: string): string | null } | null;
  getElementsByTagName(tag: 'textarea'): ArrayLike<FieldLike>;
}

/** The version that served this page, or null when it isn't stamped (the Vite dev server, /index.html). */
export function pageVersion(doc: DocLike): string | null {
  return doc.querySelector(`meta[name="${VERSION_META}"]`)?.getAttribute?.('content') || null;
}

/**
 * Text a reload doesn't lose: xterm's hidden input, and fields marked `data-reload-safe` (the
 * terminal compose box keeps its draft in sessionStorage; a design answer is marked only while
 * its text is saved on the server).
 */
function reloadSafe(el: FieldLike): boolean {
  return el.dataset?.reloadSafe !== undefined || !!el.classList?.contains('xterm-helper-textarea');
}

const TEXT_INPUTS = new Set(['', 'text', 'search', 'email', 'url', 'password', 'tel', 'number']);

function editable(el: FieldLike): boolean {
  const tag = el.tagName.toUpperCase();
  if (tag === 'TEXTAREA' || tag === 'SELECT') return true;
  if (tag === 'INPUT') return TEXT_INPUTS.has((el.type ?? '').toLowerCase());
  return !!el.isContentEditable;
}

/**
 * Whether a reload now could lose something: a dialog, sheet or popover is open, something unsent
 * that lives only in the page is showing (`data-unsent`, like images held for Claude), the user is in
 * a text field, or a textarea holds unsent text (say, a review comment being written).
 */
export function isBusy(doc: DocLike): boolean {
  if (doc.querySelector('[role="dialog"]')) return true;
  if (doc.querySelector('[data-unsent]')) return true;
  const a = doc.activeElement;
  if (a && editable(a) && !reloadSafe(a)) return true;
  const areas = doc.getElementsByTagName('textarea');
  for (let i = 0; i < areas.length; i++) {
    const t = areas[i];
    if (t.value?.trim() && !reloadSafe(t)) return true;
  }
  return false;
}

/**
 * What to do about the server's version: `current` when it's the page's, else `reload`, or `wait`
 * (show the Reload banner) while the user is busy or once this tab already reloaded for that
 * version and is still stale, which another reload wouldn't fix.
 */
export function planReload(o: { loaded: string; server: string; busy: boolean; reloadedFor: string | null }): 'current' | 'reload' | 'wait' {
  if (o.server === o.loaded) return 'current';
  if (o.busy || o.reloadedFor === o.server) return 'wait';
  return 'reload';
}
