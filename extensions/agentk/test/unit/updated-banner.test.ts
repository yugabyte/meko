import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { hideWhenSeen, UPDATED_BANNER_MS } from '../../src/web/components/Banners.js';

/** A page whose visibility the test sets, firing `visibilitychange` as a browser does. */
function page(visibilityState: DocumentVisibilityState) {
  const target = new EventTarget();
  const p = {
    visibilityState,
    addEventListener: target.addEventListener.bind(target),
    removeEventListener: target.removeEventListener.bind(target),
    show(v: DocumentVisibilityState) {
      p.visibilityState = v;
      target.dispatchEvent(new Event('visibilitychange'));
    },
  };
  return p;
}

describe('the "Updated to" banner hiding itself', () => {
  beforeEach(() => vi.useFakeTimers());
  afterEach(() => vi.useRealTimers());

  it('hides once the page has been in view for the whole time, the clock paused while it is hidden', () => {
    const p = page('hidden');
    const hide = vi.fn();
    hideWhenSeen(p, UPDATED_BANNER_MS, hide);
    vi.advanceTimersByTime(10 * UPDATED_BANNER_MS);
    expect(hide).not.toHaveBeenCalled();
    p.show('visible');
    vi.advanceTimersByTime(UPDATED_BANNER_MS / 2);
    p.show('hidden');
    vi.advanceTimersByTime(10 * UPDATED_BANNER_MS);
    expect(hide).not.toHaveBeenCalled();
    p.show('visible');
    vi.advanceTimersByTime(UPDATED_BANNER_MS / 2 - 1);
    expect(hide).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(hide).toHaveBeenCalledTimes(1);
  });

  it('never hides once cleaned up (dismissed, or the page moved on)', () => {
    const hide = vi.fn();
    hideWhenSeen(page('visible'), UPDATED_BANNER_MS, hide)();
    const p = page('hidden');
    hideWhenSeen(p, UPDATED_BANNER_MS, hide)();
    p.show('visible');
    vi.advanceTimersByTime(2 * UPDATED_BANNER_MS);
    expect(hide).not.toHaveBeenCalled();
  });
});
