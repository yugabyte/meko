import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { keepFocusInView } from '../../src/web/hooks/useMedia.js';

const STYLES = readFileSync(join(import.meta.dirname, '../../src/web/styles.css'), 'utf8');

/** The bodies of every `.auth-page { … }` rule in styles.css, in order (the desktop one, then the phone one). */
const authPageRules = () => [...STYLES.matchAll(/^\s*\.auth-page \{([^}]*)\}/gm)].map((m) => m[1]);

/** A stand-in for a DOM element: what keepFocusInView reads of the focused field and the page. */
function fakeEl(tag: string, inside: boolean, field?: { scrollIntoView: () => void }) {
  return {
    matches: (sel: string) => sel.split(',').map((s) => s.trim()).includes(tag),
    closest: (sel: string) => (sel === '.field' ? (field ?? null) : null),
    scrollIntoView: vi.fn(),
    inside,
  };
}

describe('keepFocusInView', () => {
  let resized: (() => void) | null = null;
  const disconnect = vi.fn();

  const watch = (active: unknown) => {
    vi.stubGlobal(
      'ResizeObserver',
      class {
        constructor(cb: () => void) {
          resized = cb;
        }
        observe() {}
        disconnect = disconnect;
      },
    );
    vi.stubGlobal('document', { activeElement: active });
    const box = { contains: (el: { inside?: boolean }) => !!el.inside } as unknown as HTMLElement;
    return keepFocusInView(box);
  };

  afterEach(() => {
    vi.unstubAllGlobals();
    resized = null;
    disconnect.mockReset();
  });

  it('scrolls the focused field, label and all, back into view when the page shrinks for the keyboard', () => {
    const field = { scrollIntoView: vi.fn() };
    const input = fakeEl('input', true, field);
    watch(input);
    resized!();
    expect(field.scrollIntoView).toHaveBeenCalledWith({ block: 'nearest' });
    expect(input.scrollIntoView).not.toHaveBeenCalled();
  });

  it('scrolls a field that has no label wrapper by itself', () => {
    const input = fakeEl('input', true);
    watch(input);
    resized!();
    expect(input.scrollIntoView).toHaveBeenCalledWith({ block: 'nearest' });
  });

  it('leaves the page alone when nothing in it is being typed in', () => {
    const button = fakeEl('button', true);
    watch(button);
    resized!();
    expect(button.scrollIntoView).not.toHaveBeenCalled();

    const outside = fakeEl('input', false);
    watch(outside);
    resized!();
    expect(outside.scrollIntoView).not.toHaveBeenCalled();

    watch(null);
    expect(() => resized!()).not.toThrow();
  });

  it('stops watching when asked', () => {
    const stop = watch(null);
    stop();
    expect(disconnect).toHaveBeenCalled();
  });

  it('does nothing where the browser has no ResizeObserver', () => {
    vi.stubGlobal('ResizeObserver', undefined);
    expect(() => keepFocusInView({} as HTMLElement)()).not.toThrow();
  });
});

describe('sign-in screen layout', () => {
  it('scrolls itself, so the page scroll trackAppHeight() resets never moves it', () => {
    const [desktop] = authPageRules();
    expect(desktop).toMatch(/position: fixed;/);
    expect(desktop).toMatch(/overflow-y: auto;/);
    expect(desktop).not.toMatch(/min-height: 100%/);
  });

  it('sits on the visible viewport on a phone, above the on-screen keyboard', () => {
    const phone = authPageRules().find((r) => r.includes('--app-h'));
    expect(phone).toMatch(/top: var\(--app-top, 0px\);/);
    expect(phone).toMatch(/height: var\(--app-h, 100dvh\);/);
    expect(phone).toMatch(/bottom: auto;/);
  });
});
