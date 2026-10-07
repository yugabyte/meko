import { Fragment, useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type ReactNode } from 'react';
import { KIND_LABELS, KIND_STAGES, START_HINTS, START_STAGES, STAGE_LABELS } from '../../shared/types';
import type { SessionStatus, StartStage } from '../../shared/types';
import { STATUS_TEXT, contextArc, contextLevel, cx, mekoDatapackUrl } from '../util';

export function StatusDot({ status, title }: { status: SessionStatus; title?: string }) {
  const cls =
    status === 'needs_input'
      ? 'needs'
      : status === 'archived'
          ? 'idle'
          : status;
  return <i className={cx('dot', cls)} title={title ?? STATUS_TEXT[status]} aria-label={STATUS_TEXT[status]} />;
}

const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

/** Keeps Tab and Shift+Tab inside a modal, so focus can't reach the terminal behind it. */
export function trapTab(e: KeyboardEvent, root: HTMLElement | null): void {
  if (e.key !== 'Tab' || !root) return;
  const items = [...root.querySelectorAll<HTMLElement>(FOCUSABLE)].filter((el) => el.offsetParent !== null);
  if (!items.length) {
    e.preventDefault();
    return;
  }
  const first = items[0];
  const last = items[items.length - 1];
  const active = document.activeElement;
  if (!root.contains(active)) {
    e.preventDefault();
    first.focus();
  } else if (e.shiftKey && (active === first || active === root)) {
    e.preventDefault();
    last.focus();
  } else if (!e.shiftKey && active === last) {
    e.preventDefault();
    first.focus();
  }
}

export function Dialog({
  title,
  onClose,
  children,
  footer,
  wide,
  aside,
  className,
}: {
  title: ReactNode;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  wide?: boolean;
  aside?: ReactNode;
  className?: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const closeRef = useRef(onClose);
  closeRef.current = onClose;
  // Focus the first control once on open; Escape closes; Tab stays inside.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        closeRef.current();
      }
      trapTab(e, ref.current);
    };
    window.addEventListener('keydown', onKey, true);
    const root = ref.current;
    if (root && !root.contains(document.activeElement)) {
      root.querySelector<HTMLElement>('input:not([disabled]), textarea, select, button.primary')?.focus();
    }
    return () => window.removeEventListener('keydown', onKey, true);
  }, []);
  return (
    <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className={cx('dialog', wide && 'wide', className)} role="dialog" aria-modal="true" ref={ref}>
        <div className="dialog-hd">
          <h2>{title}</h2>
          {aside}
          <button className="icon-btn" onClick={onClose} aria-label="Close">
            ×
          </button>
        </div>
        <div className="dialog-body">{children}</div>
        {footer && <div className="dialog-ft">{footer}</div>}
      </div>
    </div>
  );
}

/** A button that asks "Are you sure?" inline instead of via window.confirm. */
export function ConfirmButton({
  label,
  confirmLabel,
  prompt,
  onConfirm,
  className,
  disabled,
  danger,
}: {
  label: string;
  confirmLabel: string;
  prompt?: string;
  onConfirm: () => void | Promise<void>;
  className?: string;
  disabled?: boolean;
  danger?: boolean;
}) {
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  if (!asking) {
    return (
      <button type="button" className={className ?? 'btn'} disabled={disabled} onClick={() => setAsking(true)}>
        {label}
      </button>
    );
  }
  return (
    <span className="confirm-inline">
      {prompt && <span className="confirm-prompt">{prompt}</span>}
      <button
        type="button"
        className={cx('btn', danger ? 'danger' : 'primary')}
        disabled={busy}
        onClick={async () => {
          setBusy(true);
          try {
            await onConfirm();
          } finally {
            setBusy(false);
            setAsking(false);
          }
        }}
      >
        {confirmLabel}
      </button>
      <button type="button" className="btn ghost" onClick={() => setAsking(false)}>
        Cancel
      </button>
    </span>
  );
}

/** A datapack's name, linked to its Meko page when the Meko web app's address is known. */
export function DatapackName({
  id,
  name,
  mekoWebUrl,
  className,
}: {
  id: string;
  name: string;
  mekoWebUrl: string | null;
  className?: string;
}) {
  const href = mekoDatapackUrl(mekoWebUrl, id);
  if (!href) {
    return (
      <span className={className} title={id}>
        {name}
      </span>
    );
  }
  return (
    <a className={cx('dp-link', className)} href={href} target="_blank" rel="noreferrer" title={`Open in Meko (${id})`}>
      {name}
    </a>
  );
}

export function ErrorText({ error }: { error: string | null }) {
  if (!error) return null;
  return (
    <p className="error-text" role="alert">
      {error}
    </p>
  );
}

export function Field({ label, children, hint }: { label: string; children: ReactNode; hint?: ReactNode }) {
  return (
    <label className="field">
      <span className="label">{label}</span>
      {children}
      {hint && <span className="hint">{hint}</span>}
    </label>
  );
}

/** Dots cycling ". .. ..." after something in progress, so it doesn't look stuck (a static "..." with reduced motion). */
export function Dots() {
  return (
    <span className="dots" aria-hidden="true">
      <span>.</span>
      <span>.</span>
      <span>.</span>
    </span>
  );
}

/**
 * An in-progress label such as "Starting", with the dots after it. One span, so a flex parent (a
 * button) keeps them together; screen readers get a "…", so a sentence after it still reads apart.
 */
export function Busy({ label }: { label: string }) {
  return (
    <span className="busy">
      {label}
      <span className="sr-only">…</span>
      <Dots />
    </span>
  );
}

/** A tick drawn as a path, so it sits exactly in the middle of its circle whatever the font. */
export function Check({ size = 10 }: { size?: number }) {
  return (
    <svg className="check" width={size} height={size} viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M2.5 6.3 5 8.7l4.5-5.1" />
    </svg>
  );
}

const CONTEXT_LEVEL_TEXT = { ok: '', warn: ' Getting full.', bad: ' Nearly full.' };

/**
 * How full Claude's context window is: "context" and a ring that fills from 12 o'clock
 * counter-clockwise (a C at half, an O when full), blue, then amber from 75% and red from 95%.
 * The exact percent is in the meter's value and the tooltip, and pops up over the ring as soon as
 * the mouse is on it (the tooltip takes a second). `showPercent` writes it after the ring instead,
 * for the phone's ⋯ sheet, where nothing can be hovered.
 */
export function ContextRing({ percent, showPercent = false }: { percent: number; showPercent?: boolean }) {
  const level = contextLevel(percent);
  const arc = contextArc(percent);
  return (
    <span
      className="ctx"
      role="meter"
      aria-label="Context window used"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={arc}
      title={`Context ${percent}% full, measured after each turn.${CONTEXT_LEVEL_TEXT[level]} Claude compacts the conversation when it fills up.`}
    >
      context
      <svg className={cx('ctx-ring', level !== 'ok' && level)} width="14" height="14" viewBox="0 0 16 16" fill="none" strokeWidth="2.5" aria-hidden="true">
        <circle className="track" cx="8" cy="8" r="6" />
        {/* A circle's stroke starts at 3 o'clock and runs clockwise; reflecting it across the
            bottom-left to top-right diagonal starts it at 12 and runs it counter-clockwise.
            A full ring drops the dashes, since a 100/100 dash can leave a hairline where the
            ends meet. */}
        <circle
          className="arc"
          cx="8"
          cy="8"
          r="6"
          transform="matrix(0 -1 -1 0 16 16)"
          pathLength={100}
          strokeDasharray={arc < 100 ? `${arc} 100` : undefined}
        />
      </svg>
      {showPercent ? `${percent}%` : <span className="ctx-pct" aria-hidden="true">{percent}%</span>}
    </span>
  );
}

/** A choice in the New session dialog: the stage a task starts at, or a Review a PR. */
export type StartChoice = StartStage | 'review-pr';

const START_CHOICES: StartChoice[] = [...START_STAGES, 'review-pr'];

/**
 * Where a new session starts: the stages a task can start at as pills joined like the stage bar
 * (dotted from Explore, which is optional), then "or" and Review a PR. A radio group (arrow keys
 * move the choice); `id` prefixes the element ids.
 */
export function StartPicker({ id, value, onChange }: { id: string; value: StartChoice; onChange: (c: StartChoice) => void }) {
  const refs = useRef<Partial<Record<StartChoice, HTMLButtonElement | null>>>({});
  const onKey = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const step = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
    if (!step) return;
    e.preventDefault();
    const next = START_CHOICES[(START_CHOICES.indexOf(value) + step + START_CHOICES.length) % START_CHOICES.length];
    onChange(next);
    refs.current[next]?.focus();
  };
  return (
    <div className="field">
      <span className="label" id={`${id}-label`}>
        Where do you want to start?
      </span>
      <div className="starts" role="radiogroup" aria-labelledby={`${id}-label`} aria-describedby={`${id}-hint`} onKeyDown={onKey}>
        {START_CHOICES.map((c, i) => (
          <Fragment key={c}>
            {i > 0 &&
              (c === 'review-pr' ? (
                <span className="start-or" aria-hidden="true">
                  or
                </span>
              ) : (
                <span className={cx('start-link', i === 1 && 'dotted')} aria-hidden="true" />
              ))}
            <button
              id={`${id}-${c}`}
              ref={(el) => {
                refs.current[c] = el;
              }}
              type="button"
              role="radio"
              aria-checked={c === value}
              tabIndex={c === value ? 0 : -1}
              className={cx('start', c === value && 'on')}
              onClick={() => onChange(c)}
            >
              {c !== 'review-pr' && <span className="n">{KIND_STAGES.task.indexOf(c)}</span>}
              {c === 'review-pr' ? KIND_LABELS['review-pr'] : STAGE_LABELS[c]}
            </button>
          </Fragment>
        ))}
      </div>
      <span className="hint" id={`${id}-hint`}>
        {START_HINTS[value]}
      </span>
    </div>
  );
}
