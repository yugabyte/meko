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
  it('comes from the meta tag the server stamps', () => {
    expect(pageVersion(page({ version: '0.20260928.1' }))).toBe('0.20260928.1');
  });

  it('is unknown without one, or with an empty one', () => {
    expect(pageVersion(page())).toBeNull();
    expect(pageVersion(page({ version: '' }))).toBeNull();
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

  it('is while a text field has focus', () => {
    expect(isBusy(page({ active: field('INPUT') }))).toBe(true);
    expect(isBusy(page({ active: field('INPUT', { type: 'search' }) }))).toBe(true);
    expect(isBusy(page({ active: field('textarea') }))).toBe(true);
    expect(isBusy(page({ active: field('SELECT') }))).toBe(true);
    expect(isBusy(page({ active: field('DIV', { isContentEditable: true }) }))).toBe(true);
  });

  it('is not for a focused button, checkbox or link', () => {
    expect(isBusy(page({ active: field('BUTTON') }))).toBe(false);
    expect(isBusy(page({ active: field('INPUT', { type: 'checkbox' }) }))).toBe(false);
    expect(isBusy(page({ active: field('A') }))).toBe(false);
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

  it('does nothing while the server runs the page’s version', () => {
    expect(plan({ server: '0.20260927.1' })).toBe('current');
    expect(plan({ server: '0.20260927.1', busy: true, reloadedFor: '0.20260927.1' })).toBe('current');
  });

  it('reloads for any other version, older ones (a rollback) included', () => {
    expect(plan()).toBe('reload');
    expect(plan({ server: '0.20260926.3' })).toBe('reload');
  });

  it('waits while the user is busy', () => {
    expect(plan({ busy: true })).toBe('wait');
  });

  it('reloads only once for a version, so a mismatch a reload cannot fix does not loop', () => {
    expect(plan({ reloadedFor: '0.20260928.1' })).toBe('wait');
    expect(plan({ reloadedFor: '0.20260926.3' })).toBe('reload');
  });
});
