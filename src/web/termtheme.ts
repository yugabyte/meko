// The terminals' colors: dark (the default), light, or the app's theme. Remembered in this browser.
// `data-term-theme="light"` on <html> switches the CSS around the terminal (tabs, key bar) too.

import type { ITheme } from '@xterm/xterm';

export type TermThemePref = 'dark' | 'light' | 'app';
export type TermTheme = 'dark' | 'light';

const KEY = 'ak-term-theme';

export const TERM_THEME_LABEL: Record<TermThemePref, string> = {
  dark: 'Dark',
  light: 'Light',
  app: 'Same as the app',
};

const DARK: ITheme = {
  background: '#101412',
  foreground: '#D5DDD8',
  cursor: '#D5DDD8',
  cursorAccent: '#101412',
  selectionBackground: '#2E3B55',
  black: '#1A1F1C',
  red: '#F07366',
  green: '#6FD19F',
  yellow: '#F2B35B',
  blue: '#8FB0FF',
  magenta: '#C9A2FF',
  cyan: '#6CCFD6',
  white: '#D5DDD8',
  brightBlack: '#7C8A83',
  brightRed: '#FF8F84',
  brightGreen: '#8BE3B5',
  brightYellow: '#FFC97A',
  brightBlue: '#AFC6FF',
  brightMagenta: '#DCC0FF',
  brightCyan: '#8FE3E8',
  brightWhite: '#F2F5F3',
};

// "White" is a mid gray here, as in other light terminal themes: text drawn white must still show.
const LIGHT: ITheme = {
  background: '#FAFBFA',
  foreground: '#1F2622',
  cursor: '#1F2622',
  cursorAccent: '#FAFBFA',
  selectionBackground: '#C9D7F7',
  black: '#2B332E',
  red: '#C4312A',
  green: '#1F7A4A',
  yellow: '#8F5C00',
  blue: '#2F5BD3',
  magenta: '#8240C9',
  cyan: '#0F7580',
  white: '#6E7873',
  brightBlack: '#56615B',
  brightRed: '#D9473F',
  brightGreen: '#278F5A',
  brightYellow: '#A86C00',
  brightBlue: '#4570E0',
  brightMagenta: '#9657D8',
  brightCyan: '#168895',
  brightWhite: '#8A948F',
};

/** The xterm.js options for a terminal theme. */
export function termOptions(theme: TermTheme): { theme: ITheme; minimumContrastRatio: number } {
  // Claude Code picks some colors itself (its own theme); on light, xterm darkens any that would be
  // hard to read against the background.
  return theme === 'light' ? { theme: LIGHT, minimumContrastRatio: 4.5 } : { theme: DARK, minimumContrastRatio: 1 };
}

/** A stored preference, read leniently: anything unknown is the default, dark. */
export function parseTermThemePref(v: string | null): TermThemePref {
  return v === 'light' || v === 'app' ? v : 'dark';
}

/** The terminal theme a preference gives, with the app showing dark or not. */
export function resolveTermTheme(pref: TermThemePref, appDark: boolean): TermTheme {
  if (pref === 'app') return appDark ? 'dark' : 'light';
  return pref;
}

/** The choice made in this page; it outlasts a write that blocked storage refused. */
let chosen: TermThemePref | null = null;

export function termThemePref(): TermThemePref {
  if (chosen) return chosen;
  try {
    return parseTermThemePref(localStorage.getItem(KEY));
  } catch {
    return 'dark';
  }
}

const DARK_QUERY = '(prefers-color-scheme: dark)';

/** Whether the app shows dark: the theme picked (`data-theme`), else the system's. */
function appDark(): boolean {
  const t = document.documentElement.getAttribute('data-theme');
  return t ? t === 'dark' : matchMedia(DARK_QUERY).matches;
}

export function currentTermTheme(): TermTheme {
  return resolveTermTheme(termThemePref(), appDark());
}

const listeners = new Set<(theme: TermTheme) => void>();
let watching = false;

function apply() {
  const theme = currentTermTheme();
  const root = document.documentElement;
  if (theme === 'light') root.setAttribute('data-term-theme', 'light');
  else root.removeAttribute('data-term-theme');
  for (const l of listeners) l(theme);
}

/** Keeps `data-term-theme` and every listener current, following the app's theme for "app". */
export function watchTermTheme(): void {
  if (watching) return;
  watching = true;
  apply();
  new MutationObserver(apply).observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
  matchMedia(DARK_QUERY).addEventListener('change', apply);
}

/** Calls `l` with the terminal theme whenever it changes; returns the unsubscribe. */
export function onTermTheme(l: (theme: TermTheme) => void): () => void {
  listeners.add(l);
  return () => listeners.delete(l);
}

/** Sets the terminals' colors, remembered in this browser. */
export function setTermThemePref(pref: TermThemePref): void {
  chosen = pref;
  try {
    if (pref === 'dark') localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, pref);
  } catch {
    /* storage blocked: the choice applies to this page only */
  }
  if (watching) apply();
}
