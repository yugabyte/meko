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
