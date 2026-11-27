import { describe, expect, it } from 'vitest';
import { SCROLL_HIDE_SETTLE_MS, SCROLL_HIDE_STEP, SCROLL_HIDE_TOP, ScrollHide } from '../../src/web/scrollhide.js';

/** Feeds scroll positions, past the settle time apart unless `quick`; returns each answer. */
function run(tops: number[], quick = false, max = Infinity): boolean[] {
  const h = new ScrollHide();
  let now = 0;
  return tops.map((top) => h.update(top, (now += quick ? 50 : SCROLL_HIDE_SETTLE_MS + 1), max));
}

describe("the Changes header's hide on scroll", () => {
  it('hides once the diff scrolls down past the top, and shows after a step back up', () => {
    expect(run([0, SCROLL_HIDE_TOP - 10, SCROLL_HIDE_TOP + 40])).toEqual([false, false, true]);
    expect(run([0, 300, 300 - SCROLL_HIDE_STEP + 1, 300 - SCROLL_HIDE_STEP])).toEqual([false, true, true, false]);
  });

  it('always shows near the top, however it got there', () => {
    expect(run([0, 500, SCROLL_HIDE_TOP])).toEqual([false, true, false]);
  });

  it('counts each run of scrolling from where it turned, so small wiggles do nothing', () => {
    expect(run([0, 200, 210, 200, 210, 200])).toEqual([false, true, true, true, true, true]);
  });

  it('stays hidden at the bottom, where the box growing and iOS bouncing move scrollTop up by themselves', () => {
    // Scrolling down to the end of a 1,000 px scroll hides it; the box grows by the header's 100 px, so the
    // most it scrolls drops to 900 and scrollTop with it; then iOS bounces past the end and back.
    const h = new ScrollHide();
    const steps: [number, number][] = [[0, 1000], [500, 1000], [1000, 1000], [900, 900], [930, 900], [900, 900]];
    expect(steps.map(([top, max], k) => h.update(top, (k + 1) * (SCROLL_HIDE_SETTLE_MS + 1), max))).toEqual([false, true, true, true, true, true]);
    // Scrolling up out of the last stretch still brings it back, a step up from the end.
    expect(run([0, 300, 1000, 1000 - SCROLL_HIDE_STEP - 1], false, 1000)).toEqual([false, true, true, false]);
    // A diff barely taller than its box never hides it, so it can't flap.
    expect(run([0, 60, 70, 60], false, 70)).toEqual([false, false, false, false]);
  });

  it('ignores scrolling just after it flips: the box changing height moves scrollTop by itself', () => {
    expect(run([0, 300, 280, 250, 220], true)).toEqual([false, true, true, true, true]);
  });
});
