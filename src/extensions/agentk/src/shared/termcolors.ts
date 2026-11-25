// The terminals' two color themes, as the server and the session host need them: the web UI's
// palettes (src/web/termtheme.ts) start from these, and a program that asks the terminal for its
// colors (OSC 10/11) is answered with them.

export type TermTheme = 'dark' | 'light';

export interface TermColors {
  /** `#rrggbb` */
  foreground: string;
  /** `#rrggbb` */
  background: string;
}

export const TERM_COLORS: Record<TermTheme, TermColors> = {
  dark: { foreground: '#D5DDD8', background: '#101412' },
  light: { foreground: '#1F2622', background: '#FAFBFA' },
};

/** A terminal theme from anything (a query string, a stored value), or null for anything else. */
export function parseTermTheme(v: unknown): TermTheme | null {
  return v === 'dark' || v === 'light' ? v : null;
}

/**
 * A color as a terminal reports it in an OSC 10/11 answer (`rgb:rrrr/gggg/bbbb`, as xterm.js
 * does), or null for anything but `#rrggbb`.
 */
export function oscColor(hex: string): string | null {
  const m = /^#([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec(hex);
  return m ? `rgb:${m.slice(1).map((c) => (c + c).toLowerCase()).join('/')}` : null;
}
