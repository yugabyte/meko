import { useEffect, useRef, useState, type KeyboardEvent, type PointerEvent, type ReactNode } from 'react';
import { cx } from '../util';

const MIN_WIDTH = 320;
/** Room always left for the terminal next to the panel (CSS caps the width the same way). */
const MIN_REST = 160;
const STEP = 40;

const storageKey = (name: string) => `ak-panel-width-${name}`;

function readWidth(name: string): number | null {
  try {
    const v = Number(localStorage.getItem(storageKey(name)));
    return Number.isFinite(v) && v >= MIN_WIDTH ? v : null;
  } catch {
    return null;
  }
}

function saveWidth(name: string, w: number | null) {
  try {
    if (w === null) localStorage.removeItem(storageKey(name));
    else localStorage.setItem(storageKey(name), String(Math.round(w)));
  } catch {
    /* storage blocked: the width just isn't remembered */
  }
}

/**
 * A side panel on the right of the work area, resizable by dragging (or arrow keys on) its
 * left edge. Double-clicking the edge switches between the default and the widest width.
 * The width is remembered per panel name in this browser.
 */
export function SidePanel({
  name,
  label,
  className,
  onKeyDown,
  children,
}: {
  name: string;
  label: string;
  className?: string;
  onKeyDown?: (e: KeyboardEvent<HTMLElement>) => void;
  children: ReactNode;
}) {
  const ref = useRef<HTMLElement>(null);
  const [width, setWidth] = useState<number | null>(() => readWidth(name));
  const [dragging, setDragging] = useState(false);
  useEffect(() => saveWidth(name, width), [name, width]);

  const maxWidth = () => Math.max(MIN_WIDTH, (ref.current?.parentElement?.clientWidth ?? 0) - MIN_REST);
  const clamp = (w: number) => Math.min(Math.max(w, MIN_WIDTH), maxWidth());
  const current = () => ref.current?.getBoundingClientRect().width ?? MIN_WIDTH;

  const down = (e: PointerEvent<HTMLDivElement>) => {
    if (e.button !== 0) return;
    e.preventDefault();
    e.currentTarget.setPointerCapture(e.pointerId);
    setDragging(true);
  };
  const move = (e: PointerEvent<HTMLDivElement>) => {
    if (!dragging || !ref.current) return;
    setWidth(clamp(ref.current.getBoundingClientRect().right - e.clientX));
  };
  const up = (e: PointerEvent<HTMLDivElement>) => {
    if (e.currentTarget.hasPointerCapture(e.pointerId)) e.currentTarget.releasePointerCapture(e.pointerId);
    setDragging(false);
  };
  const keys = (e: KeyboardEvent<HTMLDivElement>) => {
    const d = e.key === 'ArrowLeft' ? STEP : e.key === 'ArrowRight' ? -STEP : 0;
    if (!d) return;
    e.preventDefault();
    e.stopPropagation();
    setWidth(clamp(current() + d));
  };

  return (
    <aside
      ref={ref}
      className={cx('side-panel', className, dragging && 'resizing')}
      style={width ? { width } : undefined}
      aria-label={label}
      onKeyDown={onKeyDown}
    >
      <div
        className="panel-resizer"
        role="separator"
        aria-orientation="vertical"
        aria-label={`Resize ${label.toLowerCase()} panel`}
        title="Drag to resize. Double-click to switch between normal and widest."
        tabIndex={0}
        onPointerDown={down}
        onPointerMove={move}
        onPointerUp={up}
        onPointerCancel={up}
        onDoubleClick={() => setWidth(current() >= maxWidth() - 4 ? null : maxWidth())}
        onKeyDown={keys}
      />
      {children}
    </aside>
  );
}
