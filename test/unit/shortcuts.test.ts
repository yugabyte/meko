import { afterEach, describe, expect, it, vi } from 'vitest';
import { railCollapsedSaved, railWidthSaved } from '../../src/web/rail.js';
import { actionFor, assign, bindingProblem, changedBindings, defaultBindings, eventBinding, formatBinding, mergeBindings } from '../../src/web/shortcuts.js';

const press = (key: string, code: string, mods: { ctrl?: boolean; alt?: boolean; shift?: boolean; meta?: boolean } = {}) => ({
  key, code, ctrlKey: !!mods.ctrl, altKey: !!mods.alt, shiftKey: !!mods.shift, metaKey: !!mods.meta,
});

describe('keyboard shortcuts', () => {
  it.each([
    [press('b', 'KeyB', { meta: true }), 'Meta+B'],
    // Dvorak: the key labelled K is at QWERTY's V. The letter typed names it.
    [press('k', 'KeyV', { meta: true }), 'Meta+K'],
    // A non-Latin layout types no Latin letter, so the physical key names it.
    [press('л', 'KeyK', { ctrl: true, shift: true }), 'Ctrl+Shift+K'],
    [press('B', 'KeyB', { ctrl: true, shift: true }), 'Ctrl+Shift+B'],
    // Option changes the character a Mac types; the physical key still names it.
    [press('∫', 'KeyB', { alt: true, meta: true }), 'Alt+Meta+B'],
    [press('!', 'Digit1', { ctrl: true, shift: true }), 'Ctrl+Shift+1'],
    [press('Tab', 'Tab', { ctrl: true, shift: true }), 'Ctrl+Shift+Tab'],
    [press(' ', 'Space', { ctrl: true }), 'Ctrl+Space'],
    [press('Meta', 'MetaLeft', { meta: true }), null],
    [press('Shift', 'ShiftLeft', { shift: true }), null],
  ])('reads %o as %s', (e, binding) => {
    expect(eventBinding(e)).toBe(binding);
  });

  it('finds the action a binding runs, with the defaults for each platform', () => {
    const mac = defaultBindings(true);
    const other = defaultBindings(false);
    expect(actionFor(mac, 'Meta+B')).toBe('toggle-projects');
    expect(actionFor(other, 'Ctrl+Shift+B')).toBe('toggle-projects');
    // Off a Mac, Ctrl+B stays Claude Code's.
    expect(actionFor(other, 'Ctrl+B')).toBeNull();
    expect(actionFor(other, 'Ctrl+K')).toBe('palette');
    expect(actionFor(mac, 'Ctrl+Shift+Tab')).toBe('prev-session');
    expect(actionFor(mac, null)).toBeNull();
  });

  it.each([
    ['Meta+B', true, '⌘B'],
    ['Ctrl+Shift+Tab', true, '⌃⇧Tab'],
    ['Ctrl+Shift+B', false, 'Ctrl+Shift+B'],
    ['Alt+ArrowUp', false, 'Alt+↑'],
    ['Meta+K', false, 'Win+K'],
  ])('shows %s as %s', (binding, mac, shown) => {
    expect(formatBinding(binding, mac as boolean)).toBe(shown);
  });

  it.each([
    ['Meta+B', true, null],
    ['Ctrl+Alt+P', false, null],
    ['F2', false, null],
    ['Ctrl+Shift+R', false, null],
    ['Ctrl+Meta+D', true, null],
    ['B', true, /typing still reaches the terminal/],
    ['Shift+B', false, /typing still reaches the terminal/],
    // Ctrl+letter is the terminal's, on a Mac too (Ctrl+C interrupts Claude).
    ['Ctrl+D', true, /goes to the terminal/],
    ['Ctrl+R', false, /goes to the terminal/],
    // Option types characters on a Mac (⌥L is @ on a German one); Alt+key is the terminal's elsewhere.
    ['Alt+L', true, /types a character/],
    ['Alt+Shift+2', false, /goes to the terminal/],
    ['Meta+3', true, /switches projects/],
    ['Ctrl+V', false, /editing text/],
    ['Ctrl+Shift+C', false, /editing text/],
    ['Meta+Enter', true, /sends dialogs/],
    // Ctrl is not the Mac's command key, so this one is free there.
    ['Ctrl+3', true, null],
  ])('%s (Mac: %s) can be a shortcut unless %s', (binding, mac, problem) => {
    const p = bindingProblem(binding, mac as boolean);
    if (problem) expect(p).toMatch(problem);
    else expect(p).toBeNull();
  });

  it('saves only the changes, reads them back, and moves a binding from the action that had it', () => {
    const d = defaultBindings(true);
    const mine = assign(assign(d, 'new-session', 'Meta+B'), 'palette', null);
    expect(mine['toggle-projects']).toBeNull();
    const saved = JSON.parse(JSON.stringify(changedBindings(d, mine))) as unknown;
    expect(saved).toEqual({ 'new-session': 'Meta+B', 'toggle-projects': null, palette: null });
    expect(mergeBindings(d, saved)).toEqual(mine);
    // Anything unreadable leaves its action at the default.
    expect(mergeBindings(d, { palette: 7, bogus: 'Meta+X' })).toEqual(d);
    expect(mergeBindings(d, 'junk')).toEqual(d);
  });
});

describe('projects pane', () => {
  afterEach(() => vi.unstubAllGlobals());

  it.each([
    [null, null],
    ['', null],
    ['junk', null],
    ['0', null],
    ['300', 300],
    ['300.6', 301],
    ['90', 180],
    ['2000', 480],
  ])('reads a stored %s as %s', (stored, width) => {
    vi.stubGlobal('localStorage', { getItem: () => stored });
    expect(railWidthSaved()).toBe(width);
  });

  it.each([
    [null, []],
    ['junk', []],
    ['{"a":1}', []],
    ['["p1", 7, "p2"]', ['p1', 'p2']],
  ])('reads stored folded projects %s as %o', (stored, ids) => {
    vi.stubGlobal('localStorage', { getItem: () => stored });
    expect([...railCollapsedSaved()]).toEqual(ids);
  });
});
