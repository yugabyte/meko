import { useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type ReactNode } from 'react';
import { KIND_HINTS, KIND_LABELS, KIND_STAGES, SESSION_KINDS, STAGE_LABELS } from '../../shared/types';
import type { SessionKind, SessionStatus } from '../../shared/types';
import { STATUS_TEXT, cx, mekoDatapackUrl } from '../util';

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
}: {
  title: ReactNode;
  onClose: () => void;
  children: ReactNode;
  footer?: ReactNode;
  wide?: boolean;
  aside?: ReactNode;
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
      <div className={cx('dialog', wide && 'wide')} role="dialog" aria-modal="true" ref={ref}>
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

/** Animated ". .. ..." for things in progress (static "…" with reduced motion). */
export function Dots() {
  return <span className="dots" aria-hidden="true" />;
}

/** A tick drawn as a path, so it sits exactly in the middle of its circle whatever the font. */
export function Check({ size = 10 }: { size?: number }) {
  return (
    <svg className="check" width={size} height={size} viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M2.5 6.3 5 8.7l4.5-5.1" />
    </svg>
  );
}

/** First and last stage, e.g. "Design → Merge & Close", or the only one. */
function stageSpan(kind: SessionKind): string {
  const st = KIND_STAGES[kind];
  return st.length === 1 ? STAGE_LABELS[st[0]] : `${STAGE_LABELS[st[0]]} → ${STAGE_LABELS[st[st.length - 1]]}`;
}

/**
 * The session types as boxes side by side, a radio group (arrow keys move the choice). `id`
 * prefixes the element ids; `current` marks the session's type when changing it.
 */
export function KindPicker({
  id,
  value,
  onChange,
  current,
}: {
  id: string;
  value: SessionKind;
  onChange: (k: SessionKind) => void;
  current?: SessionKind;
}) {
  const refs = useRef<Partial<Record<SessionKind, HTMLButtonElement | null>>>({});
  const onKey = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const step = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
    if (!step) return;
    e.preventDefault();
    const next = SESSION_KINDS[(SESSION_KINDS.indexOf(value) + step + SESSION_KINDS.length) % SESSION_KINDS.length];
    onChange(next);
    refs.current[next]?.focus();
  };
  return (
    <div className="field">
      <span className="label" id={`${id}-label`}>
        Session type
      </span>
      <div className="kinds" role="radiogroup" aria-labelledby={`${id}-label`} aria-describedby={`${id}-hint`} onKeyDown={onKey}>
        {SESSION_KINDS.map((k) => (
          <button
            key={k}
            id={`${id}-${k}`}
            ref={(el) => {
              refs.current[k] = el;
            }}
            type="button"
            role="radio"
            aria-checked={k === value}
            tabIndex={k === value ? 0 : -1}
            className={cx('kind', k === value && 'on')}
            onClick={() => onChange(k)}
          >
            <b>{KIND_LABELS[k]}</b>
            <span className="kind-stages">{k === current ? `Current · ${stageSpan(k)}` : stageSpan(k)}</span>
          </button>
        ))}
      </div>
      <span className="hint" id={`${id}-hint`}>
        {KIND_HINTS[value]}
      </span>
    </div>
  );
}
