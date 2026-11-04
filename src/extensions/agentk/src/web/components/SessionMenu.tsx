import { useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent } from 'react';
import type { CodeReviewSummary } from '../../shared/codereview';
import type { SessionActionId, SessionActionItem } from '../sessionactions';
import { cx } from '../util';
import { Busy, ReviewGlass } from './common';

/** The ⋯ glyph, as the phone's app bar draws it. */
export function MoreIcon({ size = 20 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
      <circle cx="5" cy="12" r="1.8" />
      <circle cx="12" cy="12" r="1.8" />
      <circle cx="19" cy="12" r="1.8" />
    </svg>
  );
}

/**
 * The menu's items, grouped, with a line between groups. `cur` is the item with focus. Disabled
 * items stay in the list, with the reason they wait, but take no focus and no click.
 */
export function SessionMenuItems({ items, review, cur, onPick, itemRef }: {
  items: SessionActionItem[];
  /** The task's code review, for its row's magnifier. */
  review?: CodeReviewSummary | null;
  cur: number;
  onPick: (item: SessionActionItem) => void;
  itemRef?: (i: number, el: HTMLElement | null) => void;
}) {
  return (
    <>
      {items.map((it, i) => {
        const inner = (
          <>
            <span className="session-menu-label">
              {it.id === 'review-open' && review && <ReviewGlass review={review} />}
              {it.busy ? <Busy label={it.busy} /> : it.label}
              {it.count && <span className="count-badge">{it.count}</span>}
            </span>
            {(it.hint || it.keys || it.opens || it.href) && (
              <span className="session-menu-hint">{it.hint ?? it.keys ?? (it.href ? '↗' : '›')}</span>
            )}
          </>
        );
        const props = {
          role: 'menuitem',
          tabIndex: i === cur ? 0 : -1,
          className: cx('session-menu-item', i === cur && 'cur', it.id === 'end' && 'danger'),
          ref: (el: HTMLElement | null) => itemRef?.(i, el),
        };
        return (
          <div key={it.id} role="none" className="session-menu-row">
            {i > 0 && items[i - 1].group !== it.group && <div role="separator" className="session-menu-sep" />}
            {it.href ? (
              <a {...props} href={it.href} target="_blank" rel="noreferrer" onClick={() => onPick(it)}>
                {inner}
              </a>
            ) : (
              <button
                {...props}
                type="button"
                aria-disabled={it.disabled || undefined}
                onClick={() => !it.disabled && onPick(it)}
              >
                {inner}
              </button>
            )}
          </div>
        );
      })}
    </>
  );
}

/** The next enabled item from `from`, stepping by `step` (wrapping), or `from` when there is none. */
export function nextEnabled(items: Pick<SessionActionItem, 'disabled'>[], from: number, step: 1 | -1): number {
  for (let k = 1; k <= items.length; k++) {
    const i = (from + step * k + items.length * k) % items.length;
    if (!items[i].disabled) return i;
  }
  return from;
}

/**
 * The desktop session's ⋯: a menu button at the stage bar's right end (WAI-ARIA's menu button).
 * Enter, Space or ↓ open it on its first item, ↑ on its last; arrows, Home and End move; Escape
 * closes it and gives focus back to where it came from (⋯, or the terminal when the shortcut opened
 * it there); Tab closes it and moves on. Each bump of `openAt` (App's ⌘. / Ctrl+.) opens or closes it.
 */
export function SessionMenu({ items, review, openAt, keys, onPick }: {
  items: SessionActionItem[];
  review?: CodeReviewSummary | null;
  /** Bumped by the shortcut. */
  openAt: number;
  /** The shortcut's name, for the tooltip and aria-keyshortcuts. */
  keys: { label: string; aria: string };
  onPick: (id: SessionActionId) => void;
}) {
  const [open, setOpen] = useState(false);
  const [cur, setCur] = useState(0);
  const root = useRef<HTMLSpanElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  const els = useRef<(HTMLElement | null)[]>([]);
  // Where focus goes back to once the menu closes.
  const back = useRef<HTMLElement | null>(null);

  const first = () => nextEnabled(items, -1, 1);
  const last = () => nextEnabled(items, items.length, -1);
  // Opened by the shortcut, focus goes back where it was (the terminal); by ⋯, to ⋯.
  const show = (at: number, fromShortcut = false) => {
    const active = document.activeElement;
    back.current = fromShortcut && active instanceof HTMLElement && active !== document.body && !root.current?.contains(active) ? active : button.current;
    setCur(at);
    setOpen(true);
  };
  const close = (refocus: boolean) => {
    setOpen(false);
    if (refocus) (back.current ?? button.current)?.focus();
  };

  // The shortcut: skip the value it mounted with, then open or close on each bump.
  const seen = useRef(openAt);
  useEffect(() => {
    if (openAt === seen.current) return;
    seen.current = openAt;
    if (open) close(true);
    else if (items.length) show(first(), true);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- only on a bump
  }, [openAt]);

  useEffect(() => {
    if (open) els.current[cur]?.focus({ preventScroll: false });
  }, [open, cur]);

  useEffect(() => {
    if (!open) return;
    // Escape is caught first so a side panel behind the menu stays open.
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== 'Escape') return;
      e.stopPropagation();
      e.preventDefault();
      close(true);
    };
    const down = (e: MouseEvent) => {
      if (!root.current?.contains(e.target as Node)) close(false);
    };
    window.addEventListener('keydown', onKey, true);
    document.addEventListener('mousedown', down);
    return () => {
      window.removeEventListener('keydown', onKey, true);
      document.removeEventListener('mousedown', down);
    };
  }, [open]);

  // Items change as the session does; keep the focused one in range.
  useEffect(() => {
    if (open && cur >= items.length) setCur(Math.max(0, items.length - 1));
  }, [open, cur, items.length]);

  const pick = (it: SessionActionItem) => {
    setOpen(false);
    onPick(it.id);
    // A dialog or panel the action opened takes focus; otherwise it goes back where it came from.
    requestAnimationFrame(() => {
      const active = document.activeElement;
      if (!active || active === document.body) (back.current ?? button.current)?.focus();
    });
  };

  const onButtonKey = (e: ReactKeyboardEvent) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      show(e.key === 'ArrowDown' ? first() : last());
    }
  };
  const onMenuKey = (e: ReactKeyboardEvent) => {
    const step = { ArrowDown: 1, ArrowUp: -1 }[e.key] as 1 | -1 | undefined;
    if (step) {
      e.preventDefault();
      setCur((c) => nextEnabled(items, c, step));
    } else if (e.key === 'Home' || e.key === 'End') {
      e.preventDefault();
      setCur(e.key === 'Home' ? first() : last());
    } else if (e.key === 'Tab') {
      setOpen(false);
    }
  };

  return (
    <span className="session-menu" ref={root}>
      <button
        ref={button}
        type="button"
        className={cx('icon-btn session-menu-btn', open && 'on')}
        aria-label="Session actions"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-keyshortcuts={keys.aria}
        title={`Session actions (${keys.label})`}
        onClick={() => (open ? close(false) : show(first()))}
        onKeyDown={onButtonKey}
      >
        <MoreIcon size={18} />
      </button>
      {open && (
        <div className="session-menu-pop" role="menu" aria-label="Session actions" onKeyDown={onMenuKey}>
          <SessionMenuItems
            items={items}
            review={review}
            cur={cur}
            onPick={pick}
            itemRef={(i, el) => {
              els.current[i] = el;
            }}
          />
        </div>
      )}
    </span>
  );
}
