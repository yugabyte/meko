import { useEffect, useState } from 'react';

/** The phone layout. Keep in step with the `max-width: 760px` blocks in styles.css. */
export const MOBILE_QUERY = '(max-width: 760px)';
/** A finger rather than a mouse: touch sizing, the terminal key bar. */
export const TOUCH_QUERY = '(pointer: coarse)';

export function matches(query: string): boolean {
  return typeof matchMedia === 'function' && matchMedia(query).matches;
}

export function useMedia(query: string): boolean {
  const [on, setOn] = useState(() => matches(query));
  useEffect(() => {
    const m = matchMedia(query);
    const update = () => setOn(m.matches);
    update();
    m.addEventListener('change', update);
    return () => m.removeEventListener('change', update);
  }, [query]);
  return on;
}

export const useIsMobile = () => useMedia(MOBILE_QUERY);
export const useTouch = () => useMedia(TOUCH_QUERY);

/** How much shorter than the layout viewport the visible one must be to count as the keyboard. */
const KEYBOARD_MIN_PX = 120;

/**
 * Whether the on-screen keyboard is up: the visible viewport (times the zoom, so pinch-zooming
 * doesn't count) is well short of the layout viewport.
 */
export function keyboardOpen(layoutHeight: number, visibleHeight: number, scale: number): boolean {
  return layoutHeight - visibleHeight * scale > KEYBOARD_MIN_PX;
}

/**
 * Keeps the phone layout on the visible viewport, so the on-screen keyboard shrinks the app instead
 * of covering it and the terminal refits above it. `--app-h` is the visible height and `--app-top`
 * its offset: iOS doesn't resize the layout viewport for the keyboard, and when a field near the
 * bottom takes focus it pans the visible viewport down instead. `kb-open` on the root element marks
 * the keyboard as up.
 */
export function trackAppHeight(): () => void {
  const vv = window.visualViewport;
  if (!vv) return () => undefined;
  const root = document.documentElement;
  let frame = 0;
  const set = () => {
    frame = 0;
    const zoomed = vv.scale > 1.01;
    // iOS scrolls the page to show a focused field even though the app fits; put it back.
    if (window.scrollY && !zoomed) window.scrollTo(0, 0);
    // Times the zoom, so pinch-zooming doesn't shrink the app; only the keyboard does.
    root.style.setProperty('--app-h', `${Math.round(vv.height * vv.scale)}px`);
    // While zoomed, panning is the person's doing; the app stays put.
    root.style.setProperty('--app-top', `${zoomed ? 0 : Math.max(0, Math.round(vv.offsetTop))}px`);
    root.classList.toggle('kb-open', keyboardOpen(window.innerHeight, vv.height, vv.scale));
  };
  const schedule = () => {
    if (!frame) frame = requestAnimationFrame(set);
  };
  set();
  vv.addEventListener('resize', schedule);
  vv.addEventListener('scroll', schedule);
  return () => {
    if (frame) cancelAnimationFrame(frame);
    vv.removeEventListener('resize', schedule);
    vv.removeEventListener('scroll', schedule);
    root.style.removeProperty('--app-h');
    root.style.removeProperty('--app-top');
    root.classList.remove('kb-open');
  };
}
