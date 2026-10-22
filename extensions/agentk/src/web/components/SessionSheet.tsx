import { useEffect, useRef, useState, type ReactNode } from 'react';
import type { CloseSessionRequest, DiscardCheck, Session, SessionAction } from '../../shared/types';
import { moveBlockedIn } from '../../shared/stages';
import { api } from '../api';
import { FONT_MAX, FONT_MIN } from '../keys';
import { setTerminalFontSize, terminalFontSize } from '../terminals';
import { agentName, discardingText, revisitWhen } from '../util';
import { Busy, Elapsed, trapTab } from './common';

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
 * Archive keeps the branch; Discard deletes the worktree and branch after showing what would be lost
 * (a branch the session started on that existed before it is kept). Discard saves what the session
 * learned to Meko first, unless Don't save learnings is checked (it starts unchecked).
 * Archive waits for uncommitted files to be committed (the server refuses it too).
 * Shown in the session header on desktop and in the session sheet on phones.
 */
export function EndSessionButton({
  session,
  onAct,
  learnTo,
  label = 'Archive',
  className = 'btn small',
  disabled,
}: {
  session: Session;
  onAct: (a: SessionAction, req?: CloseSessionRequest) => Promise<void>;
  /** The datapack Discard saves what the session learned to first; null when it can't (no Meko or datapack, Claude gone). */
  learnTo: string | null;
  label?: string;
  className?: string;
  /** Another session action is in flight. */
  disabled?: boolean;
}) {
  const [step, setStep] = useState<'closed' | 'choose' | 'checking' | 'discard'>('closed');
  const [skipLearn, setSkipLearn] = useState(false);
  /** What ending the session would leave or lose: undefined while checking, null if the check failed. */
  const [check, setCheck] = useState<DiscardCheck | null | undefined>(undefined);
  const [busy, setBusy] = useState(false);
  // One check per opening, shared by Archive and Discard; a later opening's check replaces it.
  const pending = useRef<Promise<DiscardCheck | null> | null>(null);

  const open = () => {
    setStep('choose');
    setSkipLearn(false);
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
      setStep('closed');
    }
  };

  const askDiscard = async () => {
    setStep('checking');
    const p = pending.current ?? api.discardCheck(session.id).catch(() => null);
    const c = await p;
    if (pending.current !== null && pending.current !== p) return; // reopened meanwhile
    setCheck(c);
    setStep('discard');
  };

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
  if (step === 'closed') {
    return (
      <button className={className} disabled={disabled} onClick={open}>
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
    const uncommitted = check?.uncommittedFiles ?? 0;
    return (
      <span className="confirm-inline">
        <span className="confirm-prompt">End this session?</span>
        {uncommitted > 0 && (
          <span className="confirm-warn">{plural(uncommitted, 'uncommitted file')}. Commit first, or Discard.</span>
        )}
        <button
          className="btn primary"
          disabled={busy || disabled || step === 'checking' || check === undefined || uncommitted > 0}
          onClick={() => run('archive')}
        >
          Archive (keep branch)
        </button>
        <button className="btn danger" disabled={busy || disabled || step === 'checking'} onClick={askDiscard}>
          {step === 'checking' ? <Busy label="Checking" /> : 'Discard…'}
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
      <span className="confirm-prompt">
        {session.branchReused
          ? `Delete the worktree? Branch ${session.branch} is kept: it existed before this session.`
          : `Delete the worktree and branch ${session.branch}?`}
      </span>
      {warnings.map((w) => (
        <span key={w} className="confirm-warn">
          {w}
        </span>
      ))}
      {learnTo && (
        <label className="check skip-learn" title={`Unchecked, AgentK saves what this session learned to ${learnTo} before discarding it`}>
          <input type="checkbox" checked={skipLearn} disabled={busy} onChange={(e) => setSkipLearn(e.target.checked)} />
          <span>Don't save learnings</span>
        </label>
      )}
      <button className="btn danger" disabled={busy || disabled} onClick={() => run('discard', learnTo && skipLearn ? { skipLearn: true } : undefined)}>
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
  canLearn,
  learnDisabled,
  learnTo,
  openQuestions,
  acting,
  info,
  onDesign,
  onAsk,
  onHome,
  onMekoCalls,
  onMove,
  onRename,
  onToTop,
  onRevisit,
  onAct,
  onClose,
}: {
  session: Session;
  /** The stage bar with its gate (Approve design, Open PR, Merge & Close). */
  stage: ReactNode;
  canDesign: boolean;
  /** Save learnings to Meko is offered (the project has a datapack, and Claude is there). */
  canLearn: boolean;
  /** It waits: another action is in flight, Claude is busy, or AgentK is already saving them. */
  learnDisabled: boolean;
  /** The datapack Discard saves learnings to first (EndSessionButton). */
  learnTo: string | null;
  /** The design's open questions, shown on its row during Design. */
  openQuestions: number;
  /** A session action is in flight: Resume and Archive wait for it. */
  acting: boolean;
  /** Branch, tokens, Meko and review lines from the desktop footer. */
  info: ReactNode;
  onDesign: () => void;
  /** Opens the questions Claude is asking (Session.asking). */
  onAsk: () => void;
  onHome: () => void;
  /** Opens the session's Meko call log; left out when the project doesn't log Meko calls. */
  onMekoCalls?: () => void;
  /** Opens the Move to another stage dialog. */
  onMove: () => void;
  /** Opens the Rename session dialog. */
  onRename: () => void;
  /** Moves the session to the top of its project's list; left out when it's already there. */
  onToTop?: () => void;
  /** Opens Revisit later. */
  onRevisit: () => void;
  onAct: (a: SessionAction, req?: CloseSessionRequest) => Promise<void>;
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
        {session.asking && (
          <SheetItem className="primary ask" hint="›" onClick={then(onAsk)}>
            Answer AgentK: {session.asking.questions[0]?.question}
          </SheetItem>
        )}
        {session.status === 'exited' && (
          <SheetItem className="primary" disabled={acting} onClick={then(() => void onAct('resume'))}>
            Resume {agentName(session)}
          </SheetItem>
        )}
        {canDesign && (
          <SheetItem hint="›" onClick={then(onDesign)}>
            Design doc
            {openQuestions > 0 && <span className="count-badge">{openQuestions === 1 ? '1 open question' : `${openQuestions} open questions`}</span>}
          </SheetItem>
        )}
        {session.prUrl && (
          <SheetItem href={session.prUrl} hint="↗" onClick={onClose}>
            PR #{session.prNumber} on GitHub
          </SheetItem>
        )}
        {canLearn && (
          <SheetItem disabled={learnDisabled} onClick={then(() => void onAct('learn'))}>
            {session.learning ? <Busy label="Saving learnings to Meko" /> : 'Save learnings to Meko'}
          </SheetItem>
        )}
        {!moveBlockedIn(session.stage) && (
          <SheetItem hint="›" onClick={then(onMove)}>
            Move to another stage
          </SheetItem>
        )}
        <SheetItem hint="›" onClick={then(onRename)}>
          Rename session
        </SheetItem>
        {onToTop && <SheetItem onClick={then(onToTop)}>Move to top</SheetItem>}
        <SheetItem hint="›" onClick={then(onRevisit)}>
          {session.revisitAt !== null ? `Revisit later: ${revisitWhen(session.revisitAt)}` : 'Revisit later'}
        </SheetItem>
        {onMekoCalls && (
          <SheetItem hint="›" onClick={then(onMekoCalls)}>
            Meko calls
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
          learnTo={learnTo}
          disabled={acting}
          label="Archive or discard…"
          className="btn block danger-outline"
        />
      </div>
      <div className="sheet-info">{info}</div>
    </Sheet>
  );
}
