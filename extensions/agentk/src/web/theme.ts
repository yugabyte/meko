import { useCallback, useEffect, useState } from 'react';

export type ThemePref = 'system' | 'light' | 'dark';
const KEY = 'ak-theme';

function read(): ThemePref {
  try {
    const v = localStorage.getItem(KEY);
    return v === 'light' || v === 'dark' ? v : 'system';
  } catch {
    return 'system';
  }
}

/** The page background per theme, which an installed app's status bar takes (see index.html). */
const BAR_COLOR = { light: '#f5f6f4', dark: '#111513' };

function apply(pref: ThemePref) {
  const root = document.documentElement;
  if (pref === 'system') root.removeAttribute('data-theme');
  else root.setAttribute('data-theme', pref);
  // Each theme-color tag covers one system scheme; a chosen theme sets both to its own color.
  for (const m of document.querySelectorAll<HTMLMetaElement>('meta[name="theme-color"]')) {
    const scheme = m.media.includes('dark') ? 'dark' : 'light';
    m.content = BAR_COLOR[pref === 'system' ? scheme : pref];
  }
}

export function useTheme(): [ThemePref, () => void] {
  const [pref, setPref] = useState<ThemePref>(read);
  useEffect(() => apply(pref), [pref]);
  const cycle = useCallback(() => {
    setPref((p) => {
      const next: ThemePref = p === 'system' ? 'light' : p === 'light' ? 'dark' : 'system';
      try {
        if (next === 'system') localStorage.removeItem(KEY);
        else localStorage.setItem(KEY, next);
      } catch {
        /* storage unavailable; theme still applies for this page */
      }
      return next;
    });
  }, []);
  return [pref, cycle];
}

/** The app's explicit theme (`data-theme` on <html>), or null when it follows the system. */
export function useDataTheme(): string | null {
  const read = () => document.documentElement.getAttribute('data-theme');
  const [theme, setTheme] = useState(read);
  useEffect(() => {
    const obs = new MutationObserver(() => setTheme(read()));
    obs.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] });
    return () => obs.disconnect();
  }, []);
  return theme;
}

const DARK_QUERY = '(prefers-color-scheme: dark)';

/** Whether the app shows dark: the theme picked, else the system's. */
export function useDarkTheme(): boolean {
  const theme = useDataTheme();
  const [systemDark, setSystemDark] = useState(() => matchMedia(DARK_QUERY).matches);
  useEffect(() => {
    const mq = matchMedia(DARK_QUERY);
    const on = () => setSystemDark(mq.matches);
    mq.addEventListener('change', on);
    return () => mq.removeEventListener('change', on);
  }, []);
  return theme ? theme === 'dark' : systemDark;
}
