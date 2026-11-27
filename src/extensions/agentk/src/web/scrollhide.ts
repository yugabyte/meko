/** Scroll this far in one direction before the header hides or shows. */
export const SCROLL_HIDE_STEP = 24;
/** The header always shows this close to the top. */
export const SCROLL_HIDE_TOP = 48;
/**
 * After the header hides or shows, scrolling is ignored this long. The box it scrolls changes
 * height then, which moves `scrollTop` near the end of the content: counted as a scroll, it would
 * flip the header straight back.
 */
export const SCROLL_HIDE_SETTLE_MS = 300;

/**
 * Whether a header above a scroll box should hide: once the box has scrolled down SCROLL_HIDE_STEP
 * past SCROLL_HIDE_TOP, until it scrolls up SCROLL_HIDE_STEP or comes back near the top, as phone
 * browsers do with their own bars. Within SCROLL_HIDE_STEP of the bottom nothing changes it: hiding
 * the header makes the box taller, which pulls `scrollTop` up to the new bottom, and iOS bounces
 * back up from the end; both would read as scrolling up. Plain state, so it's tested without a browser.
 */
export class ScrollHide {
  hidden = false;
  /** Where the current run of scrolling in one direction started. */
  private from = 0;
  private last = 0;
  private settleUntil = 0;

  /**
   * Takes the box's `scrollTop` at time `now` (ms), and the most it can scroll (`scrollHeight -
   * clientHeight`); returns whether the header is hidden now.
   */
  update(top: number, now: number, max = Infinity): boolean {
    if (now < this.settleUntil || (top > SCROLL_HIDE_TOP && top >= max - SCROLL_HIDE_STEP)) {
      this.from = this.last = top;
      return this.hidden;
    }
    // A turn starts a new run.
    if ((top > this.last) !== (this.last > this.from) && this.last !== this.from) this.from = this.last;
    this.last = top;
    const next = top <= SCROLL_HIDE_TOP ? false : top - this.from >= SCROLL_HIDE_STEP ? true : this.from - top >= SCROLL_HIDE_STEP ? false : this.hidden;
    if (next !== this.hidden) {
      this.hidden = next;
      this.from = top;
      this.settleUntil = now + SCROLL_HIDE_SETTLE_MS;
    }
    return this.hidden;
  }
}
