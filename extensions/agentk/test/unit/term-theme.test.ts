import { readFileSync } from 'node:fs';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { findOptions } from '../../src/web/termfind.js';
import { parseTermThemePref, resolveTermTheme, termOptions, type TermTheme } from '../../src/web/termtheme.js';

/** WCAG contrast ratio between two #rrggbb colors. */
function contrast(a: string, b: string): number {
  const lum = (hex: string) => {
    const [r, g, b] = [1, 3, 5].map((i) => {
      const c = parseInt(hex.slice(i, i + 2), 16) / 255;
      return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
    });
    return 0.2126 * r! + 0.7152 * g! + 0.0722 * b!;
  };
  const [hi, lo] = [lum(a), lum(b)].sort((x, y) => y - x);
  return (hi! + 0.05) / (lo! + 0.05);
}

describe('terminal colors', () => {
  it('reads a stored preference, defaulting to dark', () => {
    expect(parseTermThemePref('light')).toBe('light');
    expect(parseTermThemePref('app')).toBe('app');
    expect(parseTermThemePref('dark')).toBe('dark');
    expect(parseTermThemePref(null)).toBe('dark');
    expect(parseTermThemePref('LIGHT')).toBe('dark');
  });

  it('follows the app only when asked to', () => {
    expect(resolveTermTheme('dark', false)).toBe('dark');
    expect(resolveTermTheme('light', true)).toBe('light');
    expect(resolveTermTheme('app', true)).toBe('dark');
    expect(resolveTermTheme('app', false)).toBe('light');
  });

  it('keeps every ANSI color readable on its background', () => {
    const names = ['foreground', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'brightBlack', 'brightRed', 'brightGreen', 'brightYellow', 'brightBlue', 'brightMagenta', 'brightCyan', 'brightWhite'] as const;
    for (const mode of ['dark', 'light'] as TermTheme[]) {
      const { theme } = termOptions(mode);
      for (const n of names) expect(contrast(theme[n]!, theme.background!), `${mode} ${n}`).toBeGreaterThanOrEqual(3);
      // Black on dark is a shade of the background, as in other dark themes; on light it's text.
      if (mode === 'light') expect(contrast(theme.black!, theme.background!), 'light black').toBeGreaterThanOrEqual(3);
      // White on light is a gray, still visible.
      expect(contrast(theme.white!, theme.background!), `${mode} white`).toBeGreaterThanOrEqual(mode === 'light' ? 3 : 7);
    }
  });

  it('keeps text readable on a find match, current or not', () => {
    const toggles = { caseSensitive: false, wholeWord: false, regex: false };
    for (const mode of ['dark', 'light'] as TermTheme[]) {
      const fg = termOptions(mode).theme.foreground!;
      const d = findOptions(toggles, mode).decorations!;
      for (const bg of [d.matchBackground!, d.activeMatchBackground!]) expect(contrast(fg, bg), `${mode} ${bg}`).toBeGreaterThanOrEqual(4);
    }
  });

  it('raises contrast only on light, where Claude Code may draw colors meant for dark', () => {
    expect(termOptions('dark').minimumContrastRatio).toBe(1);
    expect(termOptions('light').minimumContrastRatio).toBeGreaterThan(1);
  });

  it('paints the area around the terminal with the terminal background', () => {
    const css = readFileSync(new URL('../../src/web/styles.css', import.meta.url), 'utf8');
    const block = (selector: string) => {
      const i = css.indexOf(`${selector} {`);
      expect(i, selector).toBeGreaterThanOrEqual(0);
      return css.slice(i, css.indexOf('}', i));
    };
    const term = (b: string) => /--term:\s*(#[0-9a-f]{6});/i.exec(b)?.[1]?.toLowerCase();
    expect(term(block(':root'))).toBe(termOptions('dark').theme.background!.toLowerCase());
    expect(term(block(":root[data-term-theme='light']"))).toBe(termOptions('light').theme.background!.toLowerCase());
    // The app's dark theme doesn't change it: only the terminal setting does.
    expect(term(block(":root[data-theme='dark']"))).toBeUndefined();
  });

  it("keeps the selected tab's learning dot readable on either terminal", () => {
    const css = readFileSync(new URL('../../src/web/styles.css', import.meta.url), 'utf8');
    const accent = (root: string) => {
      const i = css.indexOf(`${root} .tab.sel .dot.learning {`);
      expect(i, root).toBeGreaterThanOrEqual(0);
      return /--accent:\s*(#[0-9a-f]{6});/i.exec(css.slice(i, css.indexOf('}', i)))![1]!;
    };
    // The selected tab is the terminal's background, so its dots follow the terminal, not the app.
    expect(css).not.toMatch(/^\.tab\.sel \.dot/m);
    const on = { dark: ":root:not([data-term-theme='light'])", light: ":root[data-term-theme='light']" } as const;
    for (const mode of ['dark', 'light'] as TermTheme[]) {
      expect(contrast(accent(on[mode]), termOptions(mode).theme.background!), mode).toBeGreaterThanOrEqual(3);
    }
  });

  it('applies the stored choice before first paint with the same rule', () => {
    const html = readFileSync(new URL('../../src/web/index.html', import.meta.url), 'utf8');
    expect(html).toContain("localStorage.getItem('ak-term-theme')");
    expect(html).toContain("tt === 'light' || (tt === 'app' && !dark)");
  });
});

describe('watching the terminal colors', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
    vi.resetModules();
  });

  /** A page with the app theme in `data-theme` and the system dark; returns hooks to drive it. */
  function page(stored: string | null) {
    const attrs = new Map<string, string>();
    let mutated = () => {};
    let storage: (e: { key: string | null; newValue: string | null }) => void = () => {};
    vi.stubGlobal('document', {
      documentElement: {
        getAttribute: (k: string) => attrs.get(k) ?? null,
        setAttribute: (k: string, v: string) => {
          attrs.set(k, v);
          if (k === 'data-theme') mutated();
        },
        removeAttribute: (k: string) => {
          attrs.delete(k);
          if (k === 'data-theme') mutated();
        },
      },
    });
    vi.stubGlobal('MutationObserver', class {
      constructor(cb: () => void) {
        mutated = cb;
      }
      observe() {}
    });
    vi.stubGlobal('matchMedia', () => ({ matches: true, addEventListener() {} }));
    vi.stubGlobal('localStorage', { getItem: () => stored, setItem() {}, removeItem() {} });
    vi.stubGlobal('window', { addEventListener: (_t: string, cb: typeof storage) => (storage = cb) });
    return { attrs, storage: (key: string | null, newValue: string | null) => storage({ key, newValue }) };
  }

  it('calls listeners only when the terminal theme changes', async () => {
    const p = page('app');
    const tt = await import('../../src/web/termtheme.js');
    const seen: TermTheme[] = [];
    tt.onTermTheme((t) => seen.push(t));
    tt.watchTermTheme();
    document.documentElement.setAttribute('data-theme', 'dark');
    document.documentElement.setAttribute('data-theme', 'light');
    document.documentElement.setAttribute('data-theme', 'light');
    expect(seen).toEqual(['dark', 'light']);
    expect(p.attrs.get('data-term-theme')).toBe('light');
  });

  it('follows a choice made in another tab', async () => {
    const p = page(null);
    const tt = await import('../../src/web/termtheme.js');
    const seen: TermTheme[] = [];
    tt.onTermTheme((t) => seen.push(t));
    tt.watchTermTheme();
    p.storage('ak-other', 'light');
    expect(seen).toEqual(['dark']);
    p.storage('ak-term-theme', 'light');
    expect(tt.termThemePref()).toBe('light');
    expect(p.attrs.get('data-term-theme')).toBe('light');
    // Storage cleared: back to the default.
    p.storage(null, null);
    expect(tt.termThemePref()).toBe('dark');
    expect(p.attrs.has('data-term-theme')).toBe(false);
    expect(seen).toEqual(['dark', 'light', 'dark']);
  });
});
