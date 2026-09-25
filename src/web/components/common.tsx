import { useEffect, useRef, useState, type ReactNode } from 'react';
import type { SessionStatus } from '../../shared/types';
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
  // Focus the first control once on open; Escape closes.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        closeRef.current();
      }
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
      <button className={className ?? 'btn'} disabled={disabled} onClick={() => setAsking(true)}>
        {label}
      </button>
    );
  }
  return (
    <span className="confirm-inline">
      {prompt && <span className="confirm-prompt">{prompt}</span>}
      <button
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
      <button className="btn ghost" onClick={() => setAsking(false)}>
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
