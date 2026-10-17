import { afterEach, describe, expect, it, vi } from 'vitest';
import { browserToggle } from '../../src/web/toggle.js';

describe('browser toggles', () => {
  afterEach(() => vi.unstubAllGlobals());

  it.each([
    [null, true],
    ['0', false],
    ['1', true],
  ])('reads a stored %s as %s for a setting that is on by default', (stored, on) => {
    vi.stubGlobal('localStorage', { getItem: () => stored });
    expect(browserToggle('k', true).saved()).toBe(on);
  });

  it('stores a setting only while it differs from its default', () => {
    const store = new Map<string, string>();
    vi.stubGlobal('localStorage', {
      getItem: (k: string) => store.get(k) ?? null,
      setItem: (k: string, v: string) => store.set(k, v),
      removeItem: (k: string) => store.delete(k),
    });
    vi.stubGlobal('window', { dispatchEvent: () => true });
    const t = browserToggle('k', true);
    t.save(false);
    expect(store.get('k')).toBe('0');
    t.save(true);
    expect(store.has('k')).toBe(false);
  });
});
