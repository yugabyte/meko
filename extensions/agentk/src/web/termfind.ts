// Find in a session's terminal (⌘F, Ctrl+Shift+F elsewhere): the shortcut, the search options and
// the bar's count. Pure, so it's unit-tested; terminals.ts runs the search with xterm's search addon.

import type { ISearchOptions } from '@xterm/addon-search';
import type { TermTheme } from './termtheme';

/** Highlighted matches at most; the addon stops counting there. */
export const FIND_LIMIT = 1000;

/** The bar's toggles. */
export type FindToggles = { caseSensitive: boolean; wholeWord: boolean; regex: boolean };

/** What the search addon last reported: the current match (-1: none, or past the limit) and the count. */
export type FindResult = { index: number; count: number };

/**
 * ⌘F on a Mac; Ctrl+Shift+F elsewhere, so Ctrl+F (forward a character in Claude Code's input)
 * still reaches the agent.
 */
export function isFindKey(e: Pick<KeyboardEvent, 'key' | 'metaKey' | 'ctrlKey' | 'shiftKey' | 'altKey'>, mac: boolean): boolean {
  if (e.key !== 'f' && e.key !== 'F') return false;
  if (e.altKey) return false;
  return mac ? e.metaKey && !e.ctrlKey && !e.shiftKey : e.ctrlKey && e.shiftKey && !e.metaKey;
}

/** Whether the addon can search for the query: an empty one finds nothing, and a bad regex throws. */
export function findPattern(query: string, regex: boolean): 'empty' | 'invalid' | 'ok' {
  if (!query) return 'empty';
  if (!regex) return 'ok';
  try {
    new RegExp(query);
    return 'ok';
  } catch {
    return 'invalid';
  }
}

/** The bar's count: "3 of 12", "No results", "More than 1,000", or why it can't search. */
export function findLabel(query: string, regex: boolean, r: FindResult): string {
  const p = findPattern(query, regex);
  if (p === 'empty') return '';
  if (p === 'invalid') return 'Invalid pattern';
  if (r.count === 0) return 'No results';
  if (r.count >= FIND_LIMIT) return `More than ${FIND_LIMIT.toLocaleString('en-US')}`;
  return r.index < 0 ? `${r.count} found` : `${r.index + 1} of ${r.count}`;
}

/** Match colors for each terminal theme (backgrounds must be #RRGGBB for the addon). */
const DECORATIONS: Record<TermTheme, NonNullable<ISearchOptions['decorations']>> = {
  dark: {
    matchBackground: '#4D3F14',
    matchOverviewRuler: '#F2B35B',
    activeMatchBackground: '#8A5D00',
    activeMatchBorder: '#FFC97A',
    activeMatchColorOverviewRuler: '#FFC97A',
  },
  light: {
    matchBackground: '#FBE7B5',
    matchOverviewRuler: '#A86C00',
    activeMatchBackground: '#F2B35B',
    activeMatchBorder: '#8F5C00',
    activeMatchColorOverviewRuler: '#8F5C00',
  },
};

/** The search addon's options for the bar's toggles, highlighting every match in the theme's colors. */
export function findOptions(t: FindToggles, theme: TermTheme): ISearchOptions {
  return { caseSensitive: t.caseSensitive, wholeWord: t.wholeWord, regex: t.regex, decorations: DECORATIONS[theme] };
}

/** The text to start a search with: the terminal's selection, when it's one short line. */
export function seedQuery(selection: string): string | null {
  const s = selection.trim();
  return s && s.length <= 200 && !/[\r\n]/.test(s) ? s : null;
}
