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

/**
 * Keeps `--app-h` on the root element at the visible viewport's height, so the app shrinks when the
 * on-screen keyboard opens (iOS doesn't resize the layout viewport) and the terminal refits above it.
 */
export function trackAppHeight(): () => void {
  const vv = window.visualViewport;
  if (!vv) return () => undefined;
  let frame = 0;
  const set = () => {
    frame = 0;
    // Times the zoom, so pinch-zooming doesn't shrink the app; only the keyboard does.
    document.documentElement.style.setProperty('--app-h', `${Math.round(vv.height * vv.scale)}px`);
    // iOS scrolls the page to show a focused field even though the app fits; put it back.
    if (window.scrollY && vv.scale <= 1.01) window.scrollTo(0, 0);
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
    document.documentElement.style.removeProperty('--app-h');
  };
}
