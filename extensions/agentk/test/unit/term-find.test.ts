import { describe, expect, it } from 'vitest';
import { FIND_LIMIT, findLabel, findOptions, isFindKey, seedQuery } from '../../src/web/termfind.js';

const key = (k: string, mods: Partial<Record<'metaKey' | 'ctrlKey' | 'shiftKey' | 'altKey', boolean>> = {}) => ({
  key: k, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, ...mods,
});

describe('find in the terminal', () => {
  it.each([
    ['⌘F on a Mac opens it', key('f', { metaKey: true }), true, true],
    ['Ctrl+Shift+F elsewhere opens it', key('F', { ctrlKey: true, shiftKey: true }), false, true],
    // Ctrl+F is the agent's (forward a character), on a Mac and elsewhere.
    ['Ctrl+F elsewhere', key('f', { ctrlKey: true }), false, false],
    ['Ctrl+F on a Mac', key('f', { ctrlKey: true }), true, false],
    ['Ctrl+Shift+F on a Mac', key('F', { ctrlKey: true, shiftKey: true }), true, false],
    ['⇧⌘F on a Mac', key('F', { metaKey: true, shiftKey: true }), true, false],
    ['⌥⌘F on a Mac', key('ƒ', { metaKey: true, altKey: true }), true, false],
    ['Ctrl+Alt+Shift+F elsewhere', key('F', { ctrlKey: true, shiftKey: true, altKey: true }), false, false],
    ['⌘G on a Mac', key('g', { metaKey: true }), true, false],
  ])('%s', (_name, e, mac, opens) => {
    expect(isFindKey(e, mac)).toBe(opens);
  });

  it.each([
    ['', false, { index: -1, count: 0 }, ''],
    ['migration', false, { index: 2, count: 12 }, '3 of 12'],
    ['migration', false, { index: -1, count: 0 }, 'No results'],
    // The match picked isn't among those counted (the output moved under it).
    ['migration', false, { index: -1, count: 4 }, '4 found'],
    ['e', false, { index: -1, count: FIND_LIMIT }, 'More than 1,000'],
    ['fix(', true, { index: -1, count: 0 }, 'Invalid pattern'],
    ['fix\\(', true, { index: 0, count: 1 }, '1 of 1'],
    // Not a regex: the same text is searched as it is.
    ['fix(', false, { index: 0, count: 2 }, '1 of 2'],
  ])('labels %j (regex %s) as %j', (query, regex, result, label) => {
    expect(findLabel(query, regex, result)).toBe(label);
  });

  it('passes the toggles to the search, highlighting in the theme’s colors', () => {
    const dark = findOptions({ caseSensitive: true, wholeWord: false, regex: true }, 'dark');
    expect(dark).toMatchObject({ caseSensitive: true, wholeWord: false, regex: true });
    expect(dark.decorations?.matchBackground).not.toBe(findOptions({ caseSensitive: true, wholeWord: false, regex: true }, 'light').decorations?.matchBackground);
  });

  it.each([
    ['  migration  ', 'migration'],
    ['', null],
    ['two\nlines', null],
    ['x'.repeat(201), null],
  ])('starts from the selection %j: %j', (selection, query) => {
    expect(seedQuery(selection)).toBe(query);
  });
});
