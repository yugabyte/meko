import { useCallback, useEffect, useRef, useState, type MouseEvent, type PointerEvent } from 'react';
import { pickRange, sideLine, type DiffLine } from '../diff';

/** How a + picks a comment's lines: shift-click extends from the last line picked; `from` is where a drag started. */
export interface CommentPick {
  shift?: boolean;
  from?: number;
}

/** How long a finger holds a + before it drags a range instead of scrolling. */
export const LONG_PRESS_MS = 400;
/** A finger that moves this far before the long-press is scrolling. */
const SLOP_PX = 10;
/** The panel scrolls while a drag is this close to its top or bottom edge… */
const EDGE_PX = 32;
/** …by up to this much a frame, faster nearer the edge. */
const STEP_PX = 16;

type Side = 'old' | 'new';

interface Gesture {
  id: number;
  touch: boolean;
  side: Side;
  anchor: number;
  end: number;
  /** The range left its first line (mouse) or the long-press fired (touch): letting go makes the comment. */
  live: boolean;
  /** The gutter's x, where rows are looked up; the pointer's start and latest y. */
  x: number;
  x0: number;
  y0: number;
  y: number;
  scroller: HTMLElement | null;
  timer: ReturnType<typeof setTimeout> | null;
  frame: number;
  stop: () => void;
}

function scrollParent(el: HTMLElement): HTMLElement | null {
  for (let p = el.parentElement; p; p = p.parentElement) {
    const y = getComputedStyle(p).overflowY;
    if ((y === 'auto' || y === 'scroll') && p.scrollHeight > p.clientHeight) return p;
  }
  return null;
}

/**
 * Pressing a diff row's + and dragging up or down picks a range of lines. A mouse or pen drags at
 * once; a finger holds the + for LONG_PRESS_MS first, so a swipe that starts on one still scrolls.
 * The range is kept here, so only this file's rows draw again as it moves; letting go calls
 * `onPick` once with the row the drag ended on and `{ from }`, its first line.
 */
export function useRangeDrag(lines: DiffLine[] | null, onPick: ((i: number, pick: CommentPick) => void) | undefined) {
  const [drag, setDrag] = useState<{ side: Side; anchor: number; end: number } | null>(null);
  const latest = useRef({ lines, onPick });
  latest.current = { lines, onPick };
  const gesture = useRef<Gesture | null>(null);
  const table = useRef<HTMLTableElement | null>(null);
  /** The click a browser sends after a drag lets go on the + it started on. */
  const swallowClick = useRef(false);

  useEffect(() => () => gesture.current?.stop(), []);

  // Browsers honour preventDefault only from a non-passive touchmove listener that was there when the
  // touch began, so it stays on the table and does nothing unless a long-press drag is live.
  const tableRef = useCallback((el: HTMLTableElement | null) => {
    table.current = el;
    if (!el) return;
    const hold = (e: TouchEvent) => {
      const g = gesture.current;
      if (g?.touch && g.live && e.cancelable) e.preventDefault();
    };
    el.addEventListener('touchmove', hold, { passive: false });
    return () => el.removeEventListener('touchmove', hold);
  }, []);

  /** Moves the range's end to the row under the gutter at the pointer's height, kept inside the table and the panel. */
  const follow = (g: Gesture) => {
    const t = table.current;
    const ls = latest.current.lines;
    if (!t || !ls) return;
    const box = t.getBoundingClientRect();
    const view = g.scroller?.getBoundingClientRect() ?? box;
    const top = Math.max(box.top, view.top) + 1, bottom = Math.min(box.bottom, view.bottom) - 1;
    if (top > bottom) return;
    const row = document.elementFromPoint(g.x, Math.min(Math.max(g.y, top), bottom))?.closest<HTMLElement>('tr[data-i]');
    if (!row || !t.contains(row)) return;
    const l = ls[Number(row.dataset.i)];
    // Rows of the other side (a − row in a range of + lines) are passed over.
    const n = l ? sideLine(l, g.side) : null;
    if (n === null || n === g.end) return;
    g.end = n;
    g.live = true;
    setDrag({ side: g.side, anchor: g.anchor, end: n });
  };

  const scroll = (g: Gesture) => {
    const s = g.scroller;
    // Not before the pointer moves: a long-press on a + near the edge mustn't start scrolling by itself.
    if (s && g.live && g.y !== g.y0) {
      const r = s.getBoundingClientRect();
      const into = g.y < r.top + EDGE_PX ? g.y - (r.top + EDGE_PX) : g.y > r.bottom - EDGE_PX ? g.y - (r.bottom - EDGE_PX) : 0;
      if (into) {
        const before = s.scrollTop;
        s.scrollTop += Math.sign(into) * Math.ceil(STEP_PX * Math.min(1, Math.abs(into) / EDGE_PX));
        if (s.scrollTop !== before) follow(g);
      }
    }
    g.frame = requestAnimationFrame(() => scroll(g));
  };

  const start = (e: PointerEvent<HTMLButtonElement>, i: number) => {
    swallowClick.current = false;
    gesture.current?.stop();
    const l = latest.current.lines?.[i];
    if (!l || e.button !== 0 || !e.isPrimary) return;
    const side: Side = l.kind === 'del' ? 'old' : 'new';
    const n = sideLine(l, side);
    if (n === null) return;
    const touch = e.pointerType === 'touch';
    // A mouse drag mustn't select the code it passes over.
    if (!touch) e.preventDefault();
    const b = e.currentTarget.getBoundingClientRect();
    const g: Gesture = {
      id: e.pointerId, touch, side, anchor: n, end: n, live: false,
      x: b.left + b.width / 2, x0: e.clientX, y0: e.clientY, y: e.clientY,
      scroller: table.current && scrollParent(table.current), timer: null, frame: 0, stop: () => {},
    };
    const move = (ev: globalThis.PointerEvent) => {
      if (ev.pointerId !== g.id) return;
      g.y = ev.clientY;
      if (g.live || !g.touch) follow(g);
      else if (Math.hypot(ev.clientX - g.x0, ev.clientY - g.y0) > SLOP_PX) g.stop();
    };
    const up = (ev: globalThis.PointerEvent) => {
      if (ev.pointerId !== g.id) return;
      const ls = latest.current.lines;
      const r = g.live && ls ? pickRange(ls, g.side, g.anchor, g.end) : null;
      g.stop();
      if (!r) return;
      swallowClick.current = true;
      const last = r.from === g.anchor ? r.to : r.from;
      const at = ls!.findIndex((x) => sideLine(x, g.side) === last);
      if (at >= 0) latest.current.onPick?.(at, { from: g.anchor });
    };
    const cancel = (ev: globalThis.PointerEvent) => {
      if (ev.pointerId === g.id) g.stop();
    };
    const key = (ev: KeyboardEvent) => {
      if (ev.key !== 'Escape') return;
      ev.preventDefault();
      ev.stopPropagation();
      swallowClick.current = true;
      g.stop();
    };
    const blur = () => g.stop();
    g.stop = () => {
      if (g.timer) clearTimeout(g.timer);
      cancelAnimationFrame(g.frame);
      window.removeEventListener('pointermove', move);
      window.removeEventListener('pointerup', up);
      window.removeEventListener('pointercancel', cancel);
      window.removeEventListener('keydown', key, true);
      window.removeEventListener('blur', blur);
      if (gesture.current === g) gesture.current = null;
      setDrag(null);
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
    window.addEventListener('pointercancel', cancel);
    window.addEventListener('keydown', key, true);
    window.addEventListener('blur', blur);
    gesture.current = g;
    if (touch) {
      g.timer = setTimeout(() => {
        g.timer = null;
        g.live = true;
        setDrag({ side, anchor: n, end: n });
        navigator.vibrate?.(10);
      }, LONG_PRESS_MS);
    }
    g.frame = requestAnimationFrame(() => scroll(g));
  };

  /** For the + button's onClick: true when the click only ends a drag, and should do nothing. */
  const swallow = () => {
    const was = swallowClick.current;
    swallowClick.current = false;
    return was;
  };

  /** A long-press on a + mustn't open the browser's menu. */
  const menu = (e: MouseEvent) => {
    if (gesture.current?.touch) e.preventDefault();
  };

  const r = drag && lines ? pickRange(lines, drag.side, drag.anchor, drag.end) : null;
  return {
    tableRef,
    /** The range being dragged, drawn in place of the picked one. */
    range: drag && r ? { side: drag.side, line: r.from, endLine: r.to } : null,
    start,
    swallow,
    menu,
  };
}
