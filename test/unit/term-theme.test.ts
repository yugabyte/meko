import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
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
    const names = ['foreground', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'brightBlack', 'brightRed', 'brightGreen', 'brightYellow', 'brightBlue', 'brightMagenta', 'brightCyan'] as const;
    for (const mode of ['dark', 'light'] as TermTheme[]) {
      const { theme } = termOptions(mode);
      for (const n of names) expect(contrast(theme[n]!, theme.background!), `${mode} ${n}`).toBeGreaterThanOrEqual(3);
      // White on light is a gray, still visible.
      expect(contrast(theme.white!, theme.background!), `${mode} white`).toBeGreaterThanOrEqual(mode === 'light' ? 3 : 7);
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

  it('applies the stored choice before first paint with the same rule', () => {
    const html = readFileSync(new URL('../../src/web/index.html', import.meta.url), 'utf8');
    expect(html).toContain("localStorage.getItem('ak-term-theme')");
    expect(html).toContain("tt === 'light' || (tt === 'app' && !dark)");
  });
});
