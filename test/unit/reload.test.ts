import { describe, expect, it } from 'vitest';
import { isBusy, pageVersion, planReload, type DocLike, type FieldLike } from '../../src/web/reload.js';

const field = (tagName: string, o: Partial<FieldLike> & { classes?: string[]; safe?: boolean } = {}): FieldLike => ({
  tagName,
  type: o.type,
  value: o.value ?? '',
  isContentEditable: o.isContentEditable ?? false,
  dataset: o.safe ? { reloadSafe: '' } : {},
  classList: { contains: (c) => (o.classes ?? []).includes(c) },
});

/** A page with these textareas, this focused element, and optionally an open dialog or a version meta tag. */
function page(o: { active?: FieldLike; textareas?: FieldLike[]; dialog?: boolean; version?: string } = {}): DocLike {
  return {
    activeElement: o.active ?? field('BODY'),
    querySelector: (sel) => {
      if (sel === '[role="dialog"]') return o.dialog ? {} : null;
      if (sel === 'meta[name="ak-version"]') return o.version === undefined ? null : { getAttribute: () => o.version! };
      throw new Error(`unexpected selector ${sel}`);
    },
    getElementsByTagName: () => o.textareas ?? [],
  };
}

describe('the version that served the page', () => {
  it.each([
    { meta: 'the meta tag the server stamps', version: '0.20260928.1', is: '0.20260928.1' },
    { meta: 'no meta tag', version: undefined, is: null },
    { meta: 'an empty meta tag', version: '', is: null },
  ])('is $is with $meta', ({ version, is }) => {
    expect(pageVersion(page({ version }))).toBe(is);
  });
});

describe('whether the user is busy', () => {
  it('is not on a page with nothing open or typed', () => {
    expect(isBusy(page())).toBe(false);
    expect(isBusy(page({ textareas: [field('TEXTAREA'), field('TEXTAREA', { value: '  \n' })] }))).toBe(false);
  });

  it('is while a dialog, sheet or popover is open', () => {
    expect(isBusy(page({ dialog: true }))).toBe(true);
  });

  it.each([
    { focused: 'a text input', el: field('INPUT'), busy: true },
    { focused: 'a search input', el: field('INPUT', { type: 'search' }), busy: true },
    { focused: 'a textarea', el: field('textarea'), busy: true },
    { focused: 'a select', el: field('SELECT'), busy: true },
    { focused: 'a contenteditable', el: field('DIV', { isContentEditable: true }), busy: true },
    { focused: 'a button', el: field('BUTTON'), busy: false },
    { focused: 'a checkbox', el: field('INPUT', { type: 'checkbox' }), busy: false },
    { focused: 'a link', el: field('A'), busy: false },
  ])('is $busy with $focused focused', ({ el, busy }) => {
    expect(isBusy(page({ active: el }))).toBe(busy);
  });

  it('is while a textarea holds unsent text, focused or not', () => {
    expect(isBusy(page({ textareas: [field('TEXTAREA'), field('TEXTAREA', { value: 'Rename this' })] }))).toBe(true);
  });

  it('ignores the terminal and the compose box, which a reload does not lose', () => {
    const xterm = field('TEXTAREA', { classes: ['xterm-helper-textarea'], value: 'x' });
    const compose = field('TEXTAREA', { safe: true, value: 'half a message' });
    expect(isBusy(page({ active: xterm, textareas: [xterm, compose] }))).toBe(false);
    expect(isBusy(page({ active: compose, textareas: [xterm, compose] }))).toBe(false);
  });
});

describe('what to do about the server version', () => {
  const plan = (o: Partial<Parameters<typeof planReload>[0]> = {}) =>
    planReload({ loaded: '0.20260927.1', server: '0.20260928.1', busy: false, reloadedFor: null, ...o });

  it.each([
    { when: 'the server runs the page’s version', o: { server: '0.20260927.1' }, is: 'current' },
    { when: 'the server runs the page’s version, busy or not', o: { server: '0.20260927.1', busy: true, reloadedFor: '0.20260927.1' }, is: 'current' },
    { when: 'the server runs a newer version', o: {}, is: 'reload' },
    { when: 'the server runs an older version (a rollback)', o: { server: '0.20260926.3' }, is: 'reload' },
    { when: 'the user is busy', o: { busy: true }, is: 'wait' },
    // Only once for a version, so a mismatch a reload cannot fix does not loop.
    { when: 'it already reloaded for this version', o: { reloadedFor: '0.20260928.1' }, is: 'wait' },
    { when: 'it reloaded for another version', o: { reloadedFor: '0.20260926.3' }, is: 'reload' },
  ])('is $is when $when', ({ o, is }) => {
    expect(plan(o)).toBe(is);
  });
});
