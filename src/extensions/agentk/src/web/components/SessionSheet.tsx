import { Fragment, useEffect, useRef, useState, type ReactNode } from 'react';
import type { CloseSessionRequest, DiscardCheck, Session, SessionAction } from '../../shared/types';
import { api, errorMessage } from '../api';
import type { CodeReviewSummary, ReviewOverrides } from '../../shared/codereview';
import { inheritedReview, useReviewDefaults } from '../reviewdefaults';
import { FONT_MAX, FONT_MIN } from '../keys';
import type { SessionActionId, SessionActionItem } from '../sessionactions';
import { setTerminalFontSize, terminalFontSize } from '../terminals';
import { discardingText } from '../util';
import { Busy, Elapsed, ReviewFields, ReviewGlass, trapTab } from './common';

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;

/** A panel that slides up from the bottom of the screen. Its ×, tapping outside it or Escape closes it. */
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
    // Focus the sheet itself, not a button: Enter would close it on the ×, or pass the gate that can
    // be the first row after it.
    ref.current?.focus({ preventScroll: true });
    return () => window.removeEventListener('keydown', onKey, true);
  }, []);
  return (
    <div className="sheet-overlay" onClick={(e) => e.target === e.currentTarget && onClose()}>
      <div className="sheet" role="dialog" aria-modal="true" aria-label={label} ref={ref} tabIndex={-1}>
        <div className="sheet-hd">
          <span className="sheet-grab" aria-hidden="true" />
          <button className="icon-btn sheet-x" onClick={onClose} aria-label="Close">
            ×
          </button>
        </div>
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
 * Archive keeps the branch; Discard deletes the worktree and branch (a branch the session started on
 * that existed before it is kept). One confirmation offers both, with what Discard would lose once the
 * check is in (Discard waits for it). Discard saves what the session learned to Meko first, unless
 * Don't save learnings is checked (it starts unchecked, except for a learn or guide session, which
 * saved what it learned as it went).
 * Archive waits for uncommitted files to be committed (the server refuses it too).
 * Shown in the session sheet on phones; on desktop, opened from the ⋯ menu (`autoOpen`) in the stage bar.
 */
export function EndSessionButton({
  session,
  onAct,
  learnTo,
  label = 'Archive',
  className = 'btn small',
  disabled,
  autoOpen = false,
  onClosed,
}: {
  session: Session;
  onAct: (a: SessionAction, req?: CloseSessionRequest) => Promise<void>;
  /** The datapack Discard saves what the session learned to first; null when it can't (no Meko or datapack, Claude gone). */
  learnTo: string | null;
  label?: string;
  className?: string;
  /** Another session action is in flight. */
  disabled?: boolean;
  /** Starts on its confirmation, as when the button was pressed. */
  autoOpen?: boolean;
  /** The confirmation closed (Cancel, or the action went). */
  onClosed?: () => void;
}) {
  const [open, setOpen] = useState(autoOpen);
  /** What ending the session would leave or lose: undefined while checking, null if the check failed. */
  const [check, setCheck] = useState<DiscardCheck | null | undefined>(undefined);
  const [busy, setBusy] = useState(false);
  // One check per opening; a later opening's check replaces it.
  const pending = useRef<Promise<DiscardCheck | null> | null>(null);

  const close = () => {
    setOpen(false);
    pending.current = null;
    onClosed?.();
  };

  const show = () => {
    setOpen(true);
    setCheck(undefined);
    const p = api.discardCheck(session.id).catch(() => null);
    pending.current = p;
    void p.then((c) => {
      if (pending.current === p) setCheck(c);
    });
  };

  const run = async (action: SessionAction, req?: CloseSessionRequest) => {
    setBusy(true);
    try {
      await onAct(action, req);
    } finally {
      setBusy(false);
      close();
    }
  };

  // Opened from the desktop ⋯ menu: its confirmation at once.
  useEffect(() => {
    if (autoOpen) show();
    // eslint-disable-next-line react-hooks/exhaustive-deps -- once, as it mounts
  }, []);

  // Discard waits for the learn step; Discard now stops waiting and leaves it unsaved.
  if (session.discarding) {
    return (
      <span className="confirm-inline">
        <span className="confirm-prompt">
          <Busy label={discardingText(session)} />
          {session.learnProgress && <Elapsed since={session.learnProgress.startedAt} />}
        </span>
        <button className="btn danger" disabled={busy || disabled} onClick={() => run('discard', { skipLearn: true })}>
          Discard now
        </button>
      </span>
    );
  }
  if (!open) {
    return (
      <button className={className} disabled={disabled} onClick={show}>
        {label}
      </button>
    );
  }
  return <EndConfirm session={session} learnTo={learnTo} check={check} busy={busy} disabled={disabled} onRun={run} onCancel={close} />;
}

/**
 * EndSessionButton's confirmation: Archive and Discard side by side. Mounted afresh at each opening,
 * so Don't save learnings starts as its initializer says every time: checked only for a learn or guide
 * session, which saved what it learned as it went.
 */
function EndConfirm({
  session,
  learnTo,
  check,
  busy,
  disabled,
  onRun,
  onCancel,
}: {
  session: Session;
  learnTo: string | null;
  check: DiscardCheck | null | undefined;
  busy: boolean;
  disabled?: boolean;
  onRun: (a: SessionAction, req?: CloseSessionRequest) => Promise<void>;
  onCancel: () => void;
}) {
  const [skipLearn, setSkipLearn] = useState(session.purpose === 'learn' || session.purpose === 'guide');
  const checking = check === undefined;
  const uncommitted = check?.uncommittedFiles ?? 0;
  const warnings: string[] = [];
  if (check === null) warnings.push('Could not check for unsaved work.');
  if (uncommitted) warnings.push(`${plural(uncommitted, 'uncommitted file')}: commit first to Archive, or Discard loses them.`);
  if (check?.unpushedCommits) warnings.push(`${plural(check.unpushedCommits, 'commit')} never pushed will be lost on Discard.`);
  if (session.prNumber) warnings.push(`PR #${session.prNumber} and the remote branch stay on GitHub.`);
  return (
    <span className="confirm-inline">
      <span className="confirm-prompt">
        {session.branchReused
          ? `End this session? Discard deletes the worktree; branch ${session.branch} is kept: it existed before this session.`
          : `End this session? Discard deletes the worktree and branch ${session.branch}.`}
      </span>
      {warnings.map((w) => (
        <span key={w} className="confirm-warn">
          {w}
        </span>
      ))}
      <button className="btn primary" disabled={busy || disabled || checking || uncommitted > 0} onClick={() => onRun('archive')}>
        Archive (keep branch)
      </button>
      {learnTo && (
        <label className="check skip-learn" title={`Unchecked, AgentK saves what this session learned to ${learnTo} before discarding it`}>
          <input type="checkbox" checked={skipLearn} disabled={busy} onChange={(e) => setSkipLearn(e.target.checked)} />
          <span>Don't save learnings</span>
        </label>
      )}
      <button
        className="btn danger"
        disabled={busy || disabled || checking}
        onClick={() => onRun('discard', learnTo && skipLearn ? { skipLearn: true } : undefined)}
      >
        {checking ? <Busy label="Checking" /> : 'Discard'}
      </button>
      <button className="btn ghost" disabled={busy} onClick={onCancel}>
        Cancel
      </button>
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

/**
 * The phone layout's session menu: the stage bar, then the session's actions (`sessionActions()`,
 * its `sheet` items) as rows, the terminal's text size, Archive or discard, and the footer's lines.
 */
export function SessionSheet({
  session,
  stage,
  items,
  review,
  reviewSettings,
  learnTo,
  acting,
  info,
  onPick,
  onAct,
  onClose,
  child = false,
}: {
  session: Session;
  /** A task's code-review session: no Archive or discard. */
  child?: boolean;
  /** The stage bar with its gate (Open PR, Merge & Close). */
  stage: ReactNode;
  /** The session's actions on the sheet, in order. */
  items: SessionActionItem[];
  /** The task's code review, for its row's magnifier. */
  review?: CodeReviewSummary | null;
  /** The task's own code review settings, opened in place under their row. */
  reviewSettings?: ReactNode;
  /** The datapack Discard saves learnings to first (EndSessionButton). */
  learnTo: string | null;
  /** A session action is in flight: Archive waits for it. */
  acting: boolean;
  /** Branch, tokens, Meko and review lines from the desktop footer. */
  info: ReactNode;
  /** Runs an action; the sheet has closed by then. */
  onPick: (id: SessionActionId) => void;
  onAct: (a: SessionAction, req?: CloseSessionRequest) => Promise<void>;
  onClose: () => void;
}) {
  const [settingsOpen, setSettingsOpen] = useState(false);
  const pick = (id: SessionActionId) => () => {
    onClose();
    onPick(id);
  };
  return (
    <Sheet label={`${session.name} actions`} onClose={onClose}>
      <div className="sheet-stage">{stage}</div>
      <div className="sheet-items">
        {items.map((it) => {
          if (it.id === 'review-settings') {
            return (
              <Fragment key={it.id}>
                <SheetItem hint={settingsOpen ? '⌄' : '›'} onClick={() => setSettingsOpen((o) => !o)}>
                  {it.label}
                </SheetItem>
                {settingsOpen && <div className="sheet-review-settings">{reviewSettings}</div>}
              </Fragment>
            );
          }
          return (
            <SheetItem
              key={it.id}
              className={it.id === 'answer' ? 'primary ask' : it.primary ? 'primary' : undefined}
              disabled={it.disabled}
              hint={it.hint ?? (it.href ? '↗' : it.opens ? '›' : undefined)}
              href={it.href}
              onClick={it.href ? onClose : pick(it.id)}
            >
              {it.id === 'review-open' && review ? (
                <span className="sheet-review">
                  <ReviewGlass review={review} />
                  {it.label}
                </span>
              ) : it.busy ? (
                <Busy label={it.busy} />
              ) : (
                it.label
              )}
              {it.count && <span className="count-badge">{it.count}</span>}
            </SheetItem>
          );
        })}
      </div>
      <FontSize />
      {!child && (
        <div className="sheet-end">
          <EndSessionButton
            session={session}
            onAct={onAct}
            learnTo={learnTo}
            disabled={acting}
            label="Archive or discard…"
            className="btn block danger-outline"
          />
        </div>
      )}
      <div className="sheet-info">{info}</div>
    </Sheet>
  );
}

/**
 * A task's own code review settings, over its project's (`Session.review`), saved at once: in place
 * in the ⋯ sheet, and in the desktop ⋯ menu's Code review settings dialog.
 */
export function SessionReviewFields({ session, projectReview, onError }: {
  session: Session;
  /** The project's review settings, what this task's "Default" stands for over the global ones. */
  projectReview: ReviewOverrides;
  onError: (msg: string) => void;
}) {
  const [saving, setSaving] = useState(false);
  const global = useReviewDefaults();
  const save = async (review: ReviewOverrides) => {
    setSaving(true);
    try {
      await api.updateSession(session.id, { review });
    } catch (e) {
      onError(errorMessage(e));
    } finally {
      setSaving(false);
    }
  };
  return (
    <ReviewFields
      id={`session-review-${session.id}`}
      value={session.review ?? {}}
      scoped
      inherited={inheritedReview(global, projectReview)}
      disabled={saving}
      onChange={(v) => void save(v)}
    />
  );
}
