import { useCallback, useEffect, useRef, useState, type MouseEvent as ReactMouseEvent, type PointerEvent as ReactPointerEvent } from 'react';
import { pickRange, sideLine, type DiffLine } from '../diff';

/**
 * How a + picks a comment's lines: shift-click extends the range from the line it started on;
 * `from` is the line a drag started on.
 */
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

/** A range being dragged: the line it started on and the line it reaches. */
export interface Drag {
  side: Side;
  anchor: number;
  end: number;
}

/** What a drag needs from its diff, given as functions so they read the latest render. */
export interface DragHost {
  lines: () => DiffLine[] | null;
  /** The index of the row at (x, y): x is the gutter's, y the pointer's height. Null for none. */
  rowAt: (x: number, y: number) => number | null;
  /** The element that scrolls the diff, for scrolling near its edges. */
  scroller: () => HTMLElement | null;
  /** Draws the range being dragged, or none. */
  show: (drag: Drag | null) => void;
  /** Called once when a drag lets go, with the row it ended on. */
  pick: (i: number, pick: CommentPick) => void;
}

/** The pointer event fields a drag reads, so tests can pass plain objects. */
export type DragPointer = Pick<PointerEvent, 'button' | 'isPrimary' | 'pointerType' | 'pointerId' | 'clientX' | 'clientY'>;

interface Gesture extends Drag {
  id: number;
  touch: boolean;
  /** The range left its first line (mouse) or the long-press fired (touch): letting go makes the comment. */
  live: boolean;
  /** The gutter's x, where rows are looked up; the pointer's start and latest y. */
  x: number;
  x0: number;
  y0: number;
  y: number;
  timer: ReturnType<typeof setTimeout> | null;
  frame: number;
  stop: () => void;
}

/**
 * Pressing a diff row's + and dragging up or down picks a range of lines. A mouse or pen drags at
 * once; a finger holds the + for LONG_PRESS_MS first, so a swipe that starts on one still scrolls.
 */
export class RangeDrag {
  private gesture: Gesture | null = null;
  /** The click a browser sends after a drag lets go on the + it started on. */
  private swallowClick = false;

  constructor(private host: DragHost) {}

  /** For the + button's pointerdown on row `i`; `x` is the gutter's x, where rows are looked up. */
  start(e: DragPointer, i: number, x: number): void {
    this.swallowClick = false;
    this.stop();
    const l = this.host.lines()?.[i];
    if (!l || e.button !== 0 || !e.isPrimary) return;
    const side: Side = l.kind === 'del' ? 'old' : 'new';
    const n = sideLine(l, side);
    if (n === null) return;
    // No preventDefault for a mouse: it would also stop the mousedown that closes open menus
    // (FilterMenu). The + and a dragging table have user-select: none, so no text gets selected.
    const g: Gesture = {
      id: e.pointerId, touch: e.pointerType === 'touch', side, anchor: n, end: n, live: false,
      x, x0: e.clientX, y0: e.clientY, y: e.clientY, timer: null, frame: 0, stop: () => {},
    };
    const move = (ev: PointerEvent) => {
      if (ev.pointerId !== g.id) return;
      g.y = ev.clientY;
      if (g.live || !g.touch) this.follow(g);
      else if (Math.hypot(ev.clientX - g.x0, ev.clientY - g.y0) > SLOP_PX) g.stop();
    };
    const up = (ev: PointerEvent) => {
      if (ev.pointerId !== g.id) return;
      g.stop();
      if (g.live) this.release(g);
    };
    const cancel = (ev: PointerEvent) => {
      if (ev.pointerId === g.id) g.stop();
    };
    const key = (ev: KeyboardEvent) => {
      if (ev.key !== 'Escape') return;
      ev.preventDefault();
      ev.stopPropagation();
      this.swallowClick = true;
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
      if (this.gesture === g) this.gesture = null;
      this.host.show(null);
    };
    window.addEventListener('pointermove', move);
    window.addEventListener('pointerup', up);
    window.addEventListener('pointercancel', cancel);
    window.addEventListener('keydown', key, true);
    window.addEventListener('blur', blur);
    this.gesture = g;
    if (g.touch) {
      g.timer = setTimeout(() => {
        g.timer = null;
        g.live = true;
        this.host.show({ side, anchor: n, end: n });
        navigator.vibrate?.(10);
      }, LONG_PRESS_MS);
    }
    g.frame = requestAnimationFrame(() => this.scroll(g));
  }

  /** Ends any drag without making a comment. */
  stop(): void {
    this.gesture?.stop();
  }

  /** For the + button's onClick: true when the click only ends a drag, and should do nothing. */
  swallow(): boolean {
    const was = this.swallowClick;
    this.swallowClick = false;
    return was;
  }

  /** Whether a touch drag is under way, so the finger mustn't scroll. */
  holding(): boolean {
    return !!this.gesture?.touch && this.gesture.live;
  }

  /** A long-press on a + mustn't open the browser's menu. */
  menu(e: Pick<Event, 'preventDefault'>): void {
    if (this.gesture?.touch) e.preventDefault();
  }

  /** Moves the range's end to the row under the gutter at the pointer's height. */
  private follow(g: Gesture) {
    const i = this.host.rowAt(g.x, g.y);
    const l = i === null ? undefined : this.host.lines()?.[i];
    // Rows of the other side (a − row in a range of + lines) are passed over.
    const n = l ? sideLine(l, g.side) : null;
    if (n === null || n === g.end) return;
    g.end = n;
    g.live = true;
    this.host.show({ side: g.side, anchor: g.anchor, end: n });
  }

  /** Makes the comment: `pick` gets the row at the range's far end from where it started. */
  private release(g: Gesture) {
    const ls = this.host.lines();
    const r = ls && pickRange(ls, g.side, g.anchor, g.end);
    if (!ls || !r) return;
    this.swallowClick = true;
    const last = r.from === g.anchor ? r.to : r.from;
    const at = ls.findIndex((x) => sideLine(x, g.side) === last);
    if (at >= 0) this.host.pick(at, { from: g.anchor });
  }

  private scroll(g: Gesture) {
    const s = this.host.scroller();
    // Not before the pointer moves: a long-press on a + near the edge mustn't start scrolling by itself.
    if (s && g.live && g.y !== g.y0) {
      const r = s.getBoundingClientRect();
      const into = g.y < r.top + EDGE_PX ? g.y - (r.top + EDGE_PX) : g.y > r.bottom - EDGE_PX ? g.y - (r.bottom - EDGE_PX) : 0;
      if (into) {
        const before = s.scrollTop;
        s.scrollTop += Math.sign(into) * Math.ceil(STEP_PX * Math.min(1, Math.abs(into) / EDGE_PX));
        if (s.scrollTop !== before) this.follow(g);
      }
    }
    g.frame = requestAnimationFrame(() => this.scroll(g));
  }
}

function scrollParent(el: HTMLElement): HTMLElement | null {
  for (let p = el.parentElement; p; p = p.parentElement) {
    const y = getComputedStyle(p).overflowY;
    if ((y === 'auto' || y === 'scroll') && p.scrollHeight > p.clientHeight) return p;
  }
  return null;
}

/**
 * A RangeDrag for a FileDiff's table. The range is kept here, so only this file's rows draw again
 * as it moves; letting go calls `onPick` once with the row the drag ended on and `{ from }`.
 */
export function useRangeDrag(lines: DiffLine[] | null, onPick: ((i: number, pick: CommentPick) => void) | undefined) {
  const [drag, setDrag] = useState<Drag | null>(null);
  const latest = useRef({ lines, onPick });
  latest.current = { lines, onPick };
  const table = useRef<HTMLTableElement | null>(null);
  const [ctl] = useState(() => new RangeDrag({
    lines: () => latest.current.lines,
    rowAt: (x, y) => {
      const t = table.current;
      if (!t) return null;
      // Kept inside the table and the part of the panel showing, so a pointer past an edge finds the last row shown.
      const box = t.getBoundingClientRect();
      const s = scrollParent(t);
      const view = s?.getBoundingClientRect() ?? box;
      const top = Math.max(box.top, view.top) + 1, bottom = Math.min(box.bottom, view.bottom) - 1;
      if (top > bottom) return null;
      const row = document.elementFromPoint(x, Math.min(Math.max(y, top), bottom))?.closest<HTMLElement>('tr[data-i]');
      return row && t.contains(row) ? Number(row.dataset.i) : null;
    },
    scroller: () => (table.current ? scrollParent(table.current) : null),
    show: setDrag,
    pick: (i, pick) => latest.current.onPick?.(i, pick),
  }));

  useEffect(() => () => ctl.stop(), [ctl]);

  // Browsers honour preventDefault only from a non-passive touchmove listener that was there when the
  // touch began, so it stays on the table and does nothing unless a long-press drag is live.
  const tableRef = useCallback((el: HTMLTableElement | null) => {
    table.current = el;
    if (!el) return;
    const hold = (e: TouchEvent) => {
      if (ctl.holding() && e.cancelable) e.preventDefault();
    };
    el.addEventListener('touchmove', hold, { passive: false });
    return () => el.removeEventListener('touchmove', hold);
  }, [ctl]);

  const r = drag && lines ? pickRange(lines, drag.side, drag.anchor, drag.end) : null;
  return {
    tableRef,
    /** The range being dragged, drawn in place of the picked one. */
    range: drag && r ? { side: drag.side, line: r.from, endLine: r.to } : null,
    /** For the + button's pointerdown on row i. */
    start: (e: ReactPointerEvent<HTMLButtonElement>, i: number) => {
      const b = e.currentTarget.getBoundingClientRect();
      ctl.start(e, i, b.left + b.width / 2);
    },
    swallow: () => ctl.swallow(),
    menu: (e: ReactMouseEvent) => ctl.menu(e),
  };
}
