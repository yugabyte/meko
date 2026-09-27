import { useEffect, useRef, useState, type ReactNode } from 'react';
import type { DiscardCheck, Session, SessionAction } from '../../shared/types';
import { api } from '../api';
import { FONT_MAX, FONT_MIN } from '../keys';
import { setTerminalFontSize, terminalFontSize } from '../terminals';
import { trapTab } from './common';

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;

/** A panel that slides up from the bottom of the screen. Tapping outside it or Escape closes it. */
export function Sheet({ label, onClose, children }: { label: string; onClose: () => void; children: ReactNode }) {
  const ref = useRef<HTMLDivElement>(null);
  const closeRef = useRef(onClose);
  closeRef.current = onClose;
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        closeRef.current();
      }
      trapTab(e, ref.current);
    };
    window.addEventListener('keydown', onKey, true);
    // Focus the sheet itself, not its first button: that can be a gate, and Enter would pass it.
    ref.current?.focus({ preventScroll: true });
    return () => window.removeEventListener('keydown', onKey, true);
  }, []);
  return (
    <div className="sheet-overlay" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="sheet" role="dialog" aria-modal="true" aria-label={label} ref={ref} tabIndex={-1}>
        <span className="sheet-grab" aria-hidden="true" />
        {children}
      </div>
    </div>
  );
}

/** One row of a sheet: a button, or a link when href is set. */
export function SheetItem({
  children,
  hint,
  href,
  onClick,
  className,
  disabled,
}: {
  children: ReactNode;
  hint?: ReactNode;
  href?: string;
  onClick?: () => void;
  className?: string;
  disabled?: boolean;
}) {
  const inner = (
    <>
      <span className="sheet-item-label">{children}</span>
      {hint && <span className="sheet-item-hint">{hint}</span>}
    </>
  );
  const cls = `sheet-item${className ? ` ${className}` : ''}`;
  return href ? (
    <a className={cls} href={href} target="_blank" rel="noreferrer" onClick={onClick}>
      {inner}
    </a>
  ) : (
    <button className={cls} onClick={onClick} disabled={disabled}>
      {inner}
    </button>
  );
}

/**
 * Archive keeps the branch; Discard deletes the worktree and branch after showing what would be lost.
 * Shown in the session header on desktop and in the session sheet on phones.
 */
export function EndSessionButton({
  session,
  onAct,
  label = 'Archive',
  className = 'btn small',
  disabled,
}: {
  session: Session;
  onAct: (a: SessionAction) => Promise<void>;
  label?: string;
  className?: string;
  /** Another session action is in flight. */
  disabled?: boolean;
}) {
  const [step, setStep] = useState<'closed' | 'choose' | 'checking' | 'discard'>('closed');
  const [check, setCheck] = useState<DiscardCheck | null>(null);
  const [busy, setBusy] = useState(false);

  const run = async (action: SessionAction) => {
    setBusy(true);
    try {
      await onAct(action);
    } finally {
      setBusy(false);
      setStep('closed');
    }
  };

  const askDiscard = async () => {
    setStep('checking');
    try {
      setCheck(await api.discardCheck(session.id));
    } catch {
      setCheck(null);
    }
    setStep('discard');
  };

  if (step === 'closed') {
    return (
      <button className={className} disabled={disabled} onClick={() => setStep('choose')}>
        {label}
      </button>
    );
  }
  const cancel = (
    <button className="btn ghost" disabled={busy} onClick={() => setStep('closed')}>
      Cancel
    </button>
  );
  if (step !== 'discard') {
    return (
      <span className="confirm-inline">
        <span className="confirm-prompt">End this session?</span>
        <button className="btn primary" disabled={busy || disabled || step === 'checking'} onClick={() => run('archive')}>
          Archive (keep branch)
        </button>
        <button className="btn danger" disabled={busy || disabled || step === 'checking'} onClick={askDiscard}>
          {step === 'checking' ? 'Checking…' : 'Discard…'}
        </button>
        {cancel}
      </span>
    );
  }
  const warnings: string[] = [];
  if (!check) warnings.push('Could not check for unsaved work.');
  else {
    if (check.uncommittedFiles) warnings.push(`${plural(check.uncommittedFiles, 'uncommitted file')} will be lost.`);
    if (check.unpushedCommits) warnings.push(`${plural(check.unpushedCommits, 'commit')} never pushed will be lost.`);
  }
  if (session.prNumber) warnings.push(`PR #${session.prNumber} and the remote branch stay on GitHub.`);
  return (
    <span className="confirm-inline">
      <span className="confirm-prompt">Delete the worktree and branch {session.branch}?</span>
      {warnings.map((w) => (
        <span key={w} className="confirm-warn">
          {w}
        </span>
      ))}
      <button className="btn danger" disabled={busy || disabled} onClick={() => run('discard')}>
        Discard
      </button>
      {cancel}
    </span>
  );
}

/** A− / A+ for every terminal's text, remembered in this browser. */
function FontSize() {
  const [size, setSize] = useState(terminalFontSize);
  const change = (d: number) => {
    const v = Math.min(FONT_MAX, Math.max(FONT_MIN, size + d));
    setSize(v);
    setTerminalFontSize(v);
  };
  return (
    <div className="sheet-font">
      <span>Terminal text</span>
      <span className="seg">
        <button onClick={() => change(-1)} disabled={size <= FONT_MIN} aria-label="Smaller terminal text">
          A−
        </button>
        <span className="sheet-font-size mono" aria-live="polite">
          {size}px
        </span>
        <button onClick={() => change(1)} disabled={size >= FONT_MAX} aria-label="Larger terminal text">
          A+
        </button>
      </span>
    </div>
  );
}

/** The phone layout's session menu: what the desktop header and footer show, in one sheet. */
export function SessionSheet({
  session,
  stage,
  canDesign,
  acting,
  info,
  onDesign,
  onHome,
  onAct,
  onClose,
}: {
  session: Session;
  /** The stage bar with its gate (Approve design, Open PR, Merge & Close). */
  stage: ReactNode;
  canDesign: boolean;
  /** A session action is in flight: Resume and Archive wait for it. */
  acting: boolean;
  /** Branch, tokens, Meko and review lines from the desktop footer. */
  info: ReactNode;
  onDesign: () => void;
  onHome: () => void;
  onAct: (a: SessionAction) => Promise<void>;
  onClose: () => void;
}) {
  const then = (f: () => void) => () => {
    onClose();
    f();
  };
  return (
    <Sheet label={`${session.name} actions`} onClose={onClose}>
      <div className="sheet-stage">{stage}</div>
      <div className="sheet-items">
        {session.status === 'exited' && (
          <SheetItem className="primary" disabled={acting} onClick={then(() => void onAct('resume'))}>
            Resume Claude
          </SheetItem>
        )}
        {canDesign && (
          <SheetItem hint="›" onClick={then(onDesign)}>
            Design doc
          </SheetItem>
        )}
        {session.prUrl && (
          <SheetItem href={session.prUrl} hint="↗" onClick={onClose}>
            PR #{session.prNumber} on GitHub
          </SheetItem>
        )}
        <SheetItem hint="›" onClick={then(onHome)}>
          Project home
        </SheetItem>
      </div>
      <FontSize />
      <div className="sheet-end">
        <EndSessionButton
          session={session}
          onAct={onAct}
          disabled={acting}
          label="Archive or discard…"
          className="btn block danger-outline"
        />
      </div>
      <div className="sheet-info">{info}</div>
    </Sheet>
  );
}
