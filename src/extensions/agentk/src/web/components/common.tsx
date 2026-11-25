import { Fragment, useEffect, useRef, useState, useSyncExternalStore, type KeyboardEvent as ReactKeyboardEvent, type ReactNode, type RefObject } from 'react';
import { HARNESS_CAPS, harnessOf, type HarnessId } from '../../shared/harnesses';
import { KIND_LABELS, KIND_STAGES, START_HINTS, START_STAGES, STAGE_LABELS } from '../../shared/types';
import type { MekoStatus, Session, StartStage } from '../../shared/types';
import { appUrl } from '../base';
import {
  MAX_DEPTH, MAX_ROUNDS, STRATEGIES, reviewRunning, reviewSegmentLabel, reviewSegmentText, reviewStopped, reviewWaitsOnYou, strategyName,
  type CodeReviewSummary, type ReviewOverrides, type ReviewSettings, type ReviewStrategySetting,
} from '../../shared/codereview';
import { BLINK_UNIT_MS, KNOCK_RUNS, WORKING_RUNS, blinkKeyframes, blinkOffsetMs, totalUnits, type BlinkRun } from '../blink';
import { matches, TOUCH_QUERY, useMedia } from '../hooks/useMedia';
import { TUNE, typingKeyframes, typingLoopMs } from '../typing';
import { LEARNING_TITLE, STATUS_TEXT, type DotState, contextArc, contextLevel, cx, elapsedText, instructionsBlocker, isSubmitKey, mekoDatapackUrl, runToMergeTitle, runsToMerge, stageChipText, withOverride } from '../util';

const HARNESS_LOGOS: Record<HarnessId, string> = { claude: appUrl('/harness-claude.svg'), opencode: appUrl('/harness-opencode.svg'), kiro: appUrl('/harness-kiro.svg') };

/**
 * A harness's logo: beside a session's name in the tabs and the rail, while open sessions run on
 * more than one harness (`harnessLogos()`) and its setting is on (`harnessLogosToggle`), in
 * `AgentPicker`, and always at the start of each agent's row of plan limits in the rail (`Limits`
 * in `Rail.tsx`, whatever the setting says). An 18 × 14 box that every logo fits, so names line up. Decorative, with no title
 * of its own: the tab's tooltip (or the button's text) names the harness, and a title here would
 * replace that tooltip while the pointer is over the logo.
 */
export function HarnessLogo({ harness }: { harness: string }) {
  const id = harnessOf(harness);
  return <img className="harness-logo" src={HARNESS_LOGOS[id]} alt="" width={18} height={14} />;
}

/**
 * Run to merge (`Session.autoRun`) beside a gate: the design panel's Approve, Open PR and Merge & Close. It
 * saves as it changes (`onChange`, `PATCH /api/sessions/:id`), and is held while that's out.
 */
export function RunToMergeCheck({ session, disabled = false, onChange }: { session: Session; disabled?: boolean; onChange: (on: boolean) => Promise<void> }) {
  const [saving, setSaving] = useState(false);
  const toggle = (on: boolean) => {
    setSaving(true);
    void onChange(on).finally(() => setSaving(false));
  };
  return (
    <label className="check gate-check" title={`Run to merge: ${runToMergeTitle(session)}`}>
      <input type="checkbox" checked={session.autoRun} disabled={disabled || saving} onChange={(e) => toggle(e.target.checked)} />
      <span>Run to merge</span>
    </label>
  );
}

/** An agent's logo, then its name: the buttons of the agent pickers and of Settings › Harness's bar. */
export function AgentLabel({ harness }: { harness: HarnessId }) {
  return (
    <>
      <HarnessLogo harness={harness} />
      {HARNESS_CAPS[harness].label}
    </>
  );
}

/**
 * Pick one of the available agents (New session's Agent, Settings' Default agent): each one's
 * logo, then its name. Callers show it only while two or more are available.
 */
export function AgentPicker({
  idPrefix,
  agents,
  value,
  onChange,
  labelledBy,
}: {
  idPrefix: string;
  agents: HarnessId[];
  value: HarnessId;
  onChange: (id: HarnessId) => void;
  labelledBy: string;
}) {
  return (
    <div className="seg agents" role="radiogroup" aria-labelledby={labelledBy}>
      {agents.map((id) => (
        <button
          key={id}
          id={`${idPrefix}-${id}`}
          type="button"
          role="radio"
          aria-checked={value === id}
          className={cx(value === id && 'on')}
          onClick={() => onChange(id)}
        >
          <AgentLabel harness={id} />
        </button>
      ))}
    </div>
  );
}

/**
 * A session's status dot; `learning` is a dotted ring turning around Meko's m, `queued` and
 * `background` the idle ring with a gap, turning, and `meko` the working dot with Meko's m in it (`dotState()`). Working
 * (`meko` too) and needs-you dots blink their halo (`blink.ts`), each `phase` (the session's or
 * project's id) at its own point in the signal.
 */
export function StatusDot({ status, title, phase = '' }: { status: DotState; title?: string; phase?: string }) {
  const ref = useRef<HTMLElement>(null);
  useBlink(ref, status === 'working' || status === 'meko' ? WORKING_RUNS : status === 'needs_input' ? KNOCK_RUNS : null, phase);
  const cls =
    status === 'needs_input'
      ? 'needs'
      : status === 'archived'
          ? 'idle'
          : status === 'meko'
            ? 'working meko'
            : status === 'queued' || status === 'background'
              ? `idle ${status}`
              : status;
  const text = status === 'learning' ? LEARNING_TITLE : STATUS_TEXT[status];
  return <i ref={ref} className={cx('dot', cls)} title={title ?? text} aria-label={STATUS_TEXT[status]} />;
}

const blinkFrames = new Map<readonly BlinkRun[], Keyframe[]>();

/**
 * Plays a blink signal on an element, on a loop. Every animation is pinned to the page's timeline,
 * started as far before time 0 as its phase's offset, so one session's dots blink together,
 * different sessions' dots don't, and one that mounts again picks up where the signal is. Still
 * with reduced motion.
 */
function useBlink(ref: RefObject<HTMLElement | null>, runs: readonly BlinkRun[] | null, phase: string): void {
  const reduce = useMedia('(prefers-reduced-motion: reduce)');
  useEffect(() => {
    const el = ref.current;
    if (!runs || reduce || !el || typeof el.animate !== 'function') return;
    let frames = blinkFrames.get(runs);
    if (!frames) blinkFrames.set(runs, (frames = blinkKeyframes(runs)));
    const anim = el.animate(frames, { duration: totalUnits(runs) * BLINK_UNIT_MS, iterations: Infinity });
    anim.startTime = -blinkOffsetMs(phase, runs);
    return () => anim.cancel();
  }, [ref, runs, reduce, phase]);
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

/**
 * The button Ctrl+Enter (⌘+Enter) presses in a dialog: the primary submit button of the form being
 * typed in; none if that form's submit isn't primary (Turn off two-factor); else the footer's primary
 * button (a form whose action sits in the footer); else a `btn primary` marked `data-submit`.
 */
export function submitButton(root: ParentNode, focused: Element | null): HTMLButtonElement | null {
  const form = focused?.closest('form');
  const submit = form?.querySelector<HTMLButtonElement>('button:not([type="button"])');
  if (submit) return submit.classList.contains('primary') ? submit : null;
  return root.querySelector<HTMLButtonElement>('.dialog-ft button.primary') ?? root.querySelector<HTMLButtonElement>('button.primary[data-submit]');
}

/** The dialogs open, the last on top: only it answers Escape and keeps Tab (the GitHub dialog opens over another). */
const openDialogs: object[] = [];

export function Dialog({
  title,
  onClose,
  onBack,
  children,
  footer,
  wide,
  aside,
  className,
}: {
  title: ReactNode;
  onClose: () => void;
  /** Returns to the dialog this one opened in place of, from a ← before the title. */
  onBack?: () => void;
  children: ReactNode;
  footer?: ReactNode;
  wide?: boolean;
  aside?: ReactNode;
  className?: string;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const closeRef = useRef(onClose);
  closeRef.current = onClose;
  // Focus the first control once on open; Escape closes; Tab stays inside. On touch, focusing a
  // field would bring up the keyboard before you've tapped it, so only an enabled main button takes
  // focus. With none, the dialog itself does, so focus never stays behind it (on the terminal).
  useEffect(() => {
    const me = {};
    openDialogs.push(me);
    const onKey = (e: KeyboardEvent) => {
      if (openDialogs[openDialogs.length - 1] !== me) return;
      if (e.key === 'Escape') {
        e.stopPropagation();
        closeRef.current();
      }
      trapTab(e, ref.current);
    };
    window.addEventListener('keydown', onKey, true);
    const root = ref.current;
    if (root && !root.contains(document.activeElement)) {
      const first = root.querySelector<HTMLElement>(
        matches(TOUCH_QUERY) ? 'button.primary:not([disabled])' : 'input:not([disabled]), textarea, select, button.primary:not([disabled])',
      );
      (first ?? root).focus();
    }
    return () => {
      window.removeEventListener('keydown', onKey, true);
      openDialogs.splice(openDialogs.indexOf(me), 1);
    };
  }, []);
  return (
    <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div
        className={cx('dialog', wide && 'wide', className)}
        role="dialog"
        aria-modal="true"
        tabIndex={-1}
        ref={ref}
        onKeyDown={(e) => {
          // Ctrl+Enter presses the main button, as a click would (nothing while it's disabled). Bubbling
          // here, after the field's own handler: a field that takes Enter itself (Another PR's Find) keeps it.
          if (!isSubmitKey(e.nativeEvent) || !(e.target instanceof Element)) return;
          const button = submitButton(e.currentTarget, e.target);
          if (!button) return;
          e.preventDefault();
          button.click();
        }}
      >
        <div className="dialog-hd">
          {onBack && (
            <button className="icon-btn" onClick={onBack} aria-label="Back" title="Back">
              ←
            </button>
          )}
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
  onAsk,
  className,
  disabled,
  danger,
}: {
  label: string;
  confirmLabel: string;
  prompt?: string;
  onConfirm: () => void | Promise<void>;
  /** When it starts asking: for what Confirm should act on as it was then, not as it is at the click. */
  onAsk?: () => void;
  className?: string;
  disabled?: boolean;
  danger?: boolean;
}) {
  const [asking, setAsking] = useState(false);
  const [busy, setBusy] = useState(false);
  if (!asking) {
    return (
      <button type="button" className={className ?? 'btn'} disabled={disabled} onClick={() => {
        onAsk?.();
        setAsking(true);
      }}>
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

/** A datapack's name, linked to its Meko page (`mekoDatapackUrl`) when there's one to link to. */
export function DatapackName({
  id,
  name,
  meko,
  className,
}: {
  id: string;
  name: string;
  meko: Pick<MekoStatus, 'configured'>;
  className?: string;
}) {
  const href = mekoDatapackUrl(meko, id);
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

/**
 * A labelled field, with an optional one-line hint under it. With `info`, an ⓘ sits beside the
 * label and its explanation opens under the field, and the label names the control by its id
 * (`info.for`): a label wrapping the field would name the ⓘ, its first button, instead.
 */
export function Field({ label, children, hint, info }: { label: string; children: ReactNode; hint?: ReactNode; info?: { for: string; text: ReactNode } }) {
  if (!info) {
    return (
      <label className="field">
        <span className="label">{label}</span>
        {children}
        {hint && <span className="hint">{hint}</span>}
      </label>
    );
  }
  const id = `${info.for}-info`;
  return (
    <div className="field">
      <div className="info-row">
        <label className="label" htmlFor={info.for}>
          {label}
        </label>
        <InfoButton id={id} label={label} />
      </div>
      {children}
      {hint && <span className="hint">{hint}</span>}
      <InfoPanel id={id}>{info.text}</InfoPanel>
    </div>
  );
}

// The id of the one ⓘ open on the page, if any: opening another closes it.
let openInfo: string | null = null;
const infoListeners = new Set<() => void>();
const setOpenInfo = (id: string | null) => {
  openInfo = id;
  infoListeners.forEach((f) => f());
};
const subscribeInfo = (f: () => void) => {
  infoListeners.add(f);
  return () => void infoListeners.delete(f);
};
const useInfoOpen = (id: string) => useSyncExternalStore(subscribeInfo, () => openInfo, () => null) === id;

/**
 * A setting's ⓘ: a click or tap opens its `InfoPanel` (the same `id`), closing any other one open;
 * another closes it. Settings keep only a short line inline, so anything longer goes in the panel,
 * placed under the setting. On touch the ⓘ is a 40 px `.icon-btn`.
 */
export function InfoButton({ id, label }: { id: string; label: string }) {
  const open = useInfoOpen(id);
  useEffect(
    () => () => {
      if (openInfo === id) setOpenInfo(null);
    },
    [id],
  );
  return (
    <button
      type="button"
      className={cx('icon-btn', 'info-btn', open && 'on')}
      aria-expanded={open}
      aria-controls={id}
      aria-label={`About ${label}`}
      onClick={() => setOpenInfo(open ? null : id)}
    >
      <span aria-hidden="true">i</span>
    </button>
  );
}

/** What an `InfoButton` opens: always in the page, hidden while closed. */
export function InfoPanel({ id, children }: { id: string; children: ReactNode }) {
  const open = useInfoOpen(id);
  return (
    <div id={id} className="info-panel" hidden={!open}>
      {children}
    </div>
  );
}

/**
 * A checkbox with its ⓘ beside it, outside its label so a tap on it doesn't tick the box, and its
 * explanation under them.
 */
export function InfoCheck({
  id,
  label,
  checked,
  disabled,
  onChange,
  children,
}: {
  id: string;
  label: string;
  checked: boolean;
  disabled?: boolean;
  onChange: (on: boolean) => void;
  children: ReactNode;
}) {
  return (
    <div className="info-check">
      <div className="info-row">
        <label className="check">
          <input id={id} type="checkbox" checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} />
          <span>{label}</span>
        </label>
        <InfoButton id={`${id}-info`} label={label} />
      </div>
      <InfoPanel id={`${id}-info`}>{children}</InfoPanel>
    </div>
  );
}

let tuneFrames: Keyframe[][] | undefined;

/**
 * Three keys playing a tune after something in progress, so it doesn't look stuck. Each label
 * starts at the first note when it appears; still with reduced motion.
 */
export function Dots() {
  const ref = useRef<HTMLSpanElement>(null);
  const reduce = useMedia('(prefers-reduced-motion: reduce)');
  useEffect(() => {
    const keys = [...(ref.current?.children ?? [])];
    if (reduce || !keys.every((k) => typeof k.animate === 'function')) return;
    const frames = (tuneFrames ??= [1, 2, 3].map((key) => typingKeyframes(TUNE, key)));
    const duration = typingLoopMs(TUNE);
    const anims = keys.map((k, i) => k.animate(frames[i], { duration, iterations: Infinity }));
    return () => anims.forEach((a) => a.cancel());
  }, [reduce]);
  return (
    <span ref={ref} className="dots" aria-hidden="true">
      <span className="dots-key" />
      <span className="dots-key" />
      <span className="dots-key" />
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

/** How long something has been running, since `since` (ms), ticking every second; hidden from screen readers. */
export function Elapsed({ since }: { since: number }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);
  return <span className="elapsed" aria-hidden="true">{elapsedText(now - since)}</span>;
}

/** A tick drawn as a path, so it sits exactly in the middle of its circle whatever the font. */
export function Check({ size = 10 }: { size?: number }) {
  return (
    <svg className="check" width={size} height={size} viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M2.5 6.3 5 8.7l4.5-5.1" />
    </svg>
  );
}

/** Run to merge's mark: two chevrons, fast-forward. Drawn in the text's colour, so it suits any chip or theme. */
export function RunToMergeIcon({ size = 8 }: { size?: number }) {
  return (
    <svg className="rtm-icon" width={size} height={size} viewBox="0 0 12 12" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M2 2.5 5.5 6 2 9.5M6.5 2.5 10 6l-3.5 3.5" />
    </svg>
  );
}

/**
 * After a session's stage, while Run to merge is on (`runsToMerge`): its icon, and the words for
 * screen readers. No title: in a tab or rail row it would replace the row's own tooltip, which
 * says "Run to merge" (`sessionTabTitle`).
 */
export function RunToMergeMark({ session, size }: { session: Session; size?: number }) {
  if (!runsToMerge(session)) return null;
  return (
    <>
      <RunToMergeIcon size={size} />
      <span className="sr-only">, Run to merge</span>
    </>
  );
}

/** A session's stage chip, in the rail, the tabs and the Starting view, with Run to merge's mark. */
export function StageChip({ session }: { session: Session }) {
  return (
    <span className="stchip">
      {stageChipText(session)}
      <RunToMergeMark session={session} />
    </span>
  );
}

/**
 * The code review's magnifier: blue, its lens pulsing while the review works (still with reduced
 * motion), Meko's m in the lens while the review is in a Meko tool call, the needs-you color with
 * its lens knocking while the review waits on you in its terminal (over the m) or its findings wait
 * on you (`triage`), and red with a
 * still lens when it failed or was cancelled. Decorative: the button around it names the review.
 */
export function ReviewGlass({ review }: { review: CodeReviewSummary }) {
  const running = reviewRunning(review);
  const stopped = reviewStopped(review);
  return (
    <span className={cx('review-glass', running && 'running', stopped && 'stopped', running && review.mekoCall && !review.waiting && 'meko', ((running && review.waiting) || reviewWaitsOnYou(review)) && 'waiting')} aria-hidden="true">
      <svg width="16" height="16" viewBox="0 0 18 18">
        <circle className="lens" cx="7.5" cy="7.5" r="4.2" />
        <circle className="rim" cx="7.5" cy="7.5" r="5.5" />
        <line className="handle" x1="11.6" y1="11.6" x2="16" y2="16" />
      </svg>
      <i className="glass-m" />
    </span>
  );
}

/**
 * A task's code review joined to its tab or rail row: the magnifier and its short count
 * (`reviewSegmentText`), spelled out for its tooltip and screen readers (`reviewSegmentLabel`).
 * Opens the review's screen while it runs, else its findings beside the task's terminal
 * (`reviewOpens`; the review's session stops once it's done).
 */
export function ReviewSegment({ review, selected, className, tab = false, onOpen }: {
  review: CodeReviewSummary;
  selected: boolean;
  className?: string;
  /** In the tab strip: a tab of its own for screen readers. */
  tab?: boolean;
  onOpen: () => void;
}) {
  const label = reviewSegmentLabel(review);
  return (
    <button
      type="button"
      role={tab ? 'tab' : undefined}
      aria-selected={tab ? selected : undefined}
      className={cx('review-seg', className, selected && 'sel', reviewStopped(review) && 'stopped')}
      title={label}
      aria-label={label}
      aria-current={!tab && selected ? 'page' : undefined}
      onClick={onOpen}
    >
      <ReviewGlass review={review} />
      <span className="review-seg-text">{reviewSegmentText(review)}</span>
    </button>
  );
}

/**
 * Optional instructions for Kode, sent after a move's or Open PR's prompt; left empty, nothing
 * extra goes. Says so in place of the hint when the text is too long (instructionsBlocker).
 */
export function InstructionsField({
  id,
  value,
  onChange,
  hint,
  placeholder,
  autoFocus = false,
}: {
  id: string;
  value: string;
  onChange: (v: string) => void;
  hint: ReactNode;
  placeholder?: string;
  autoFocus?: boolean;
}) {
  const tooLong = instructionsBlocker(value);
  return (
    <Field label="Instructions for Kode (optional)" hint={tooLong ? <span className="over">{tooLong}</span> : hint}>
      <textarea
        id={id}
        className="input"
        rows={3}
        value={value}
        placeholder={placeholder}
        autoFocus={autoFocus}
        onChange={(e) => onChange(e.target.value)}
      />
    </Field>
  );
}

const CONTEXT_LEVEL_TEXT = { ok: '', warn: ' Getting full.', bad: ' Nearly full.' };

/**
 * How full Claude's context window is: "context" and a ring that fills from 12 o'clock
 * counter-clockwise (a C at half, an O when full), blue, then amber from 75% and red from 95%.
 * The exact percent is in the meter's value and the tooltip, and in `.ctx-pct`: written after the
 * ring on a touch screen, and popped up over it as soon as a mouse is on it (the tooltip takes a
 * second). `showPercent` always writes it after the ring, for the phone's ⋯ sheet.
 */
export function ContextRing({ percent, showPercent = false, agent = 'Claude' }: { percent: number; showPercent?: boolean; agent?: string }) {
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
      title={`Context ${percent}% full, measured after each turn.${CONTEXT_LEVEL_TEXT[level]} ${agent} compacts the conversation when it fills up.`}
    >
      context
      <Ring className={cx('ctx-ring', level !== 'ok' && level)} arc={arc} px={14} box={16} r={6} stroke={2.5} />
      {showPercent ? `${percent}%` : <span className="ctx-pct" aria-hidden="true">{percent}%</span>}
    </span>
  );
}

/**
 * A ring that fills from 12 o'clock counter-clockwise to `arc` out of 100 (a C at half, an O when
 * full): the context ring, and the plan limits in the rail. Drawn in a `box` × `box` view shown at
 * `px`; the caller's class colors `.track` and `.arc`. `children` are drawn over it (the limits'
 * percent). Decorative: the caller holds the meter.
 */
export function Ring({ arc, px, box, r, stroke, className, children }: {
  arc: number;
  px: number;
  box: number;
  r: number;
  stroke: number;
  className?: string;
  children?: ReactNode;
}) {
  const c = box / 2;
  return (
    <svg className={className} width={px} height={px} viewBox={`0 0 ${box} ${box}`} fill="none" strokeWidth={stroke} aria-hidden="true">
      <circle className="track" cx={c} cy={c} r={r} />
      {/* A circle's stroke starts at 3 o'clock and runs clockwise; reflecting it across the
          bottom-left to top-right diagonal starts it at 12 and runs it counter-clockwise.
          A full ring drops the dashes, since a 100/100 dash can leave a hairline where the
          ends meet. */}
      <circle
        className="arc"
        cx={c}
        cy={c}
        r={r}
        transform={`matrix(0 -1 -1 0 ${box} ${box})`}
        pathLength={100}
        strokeDasharray={arc < 100 ? `${arc} 100` : undefined}
      />
      {children}
    </svg>
  );
}

/** A choice in the New session dialog: the stage a task starts at, a Review a PR, or a learn session. */
export type StartChoice = StartStage | 'review-pr' | 'learn';

/** A learn session's name in the start picker, and the session's name when none is typed. */
export const LEARN_LABEL = 'Learn the project';

/**
 * Where a new session starts: the stages a task can start at as pills joined like the stage bar
 * (dotted from Explore, which is optional), then "or" and Review a PR, and with `learn` (the New
 * session dialog opened as the learn offer, or to retry a learn start) "or" and Learn the project.
 * A radio group (arrow keys move the choice); `id` prefixes the element ids.
 */
export function StartPicker({ id, value, learn = false, onChange }: { id: string; value: StartChoice; learn?: boolean; onChange: (c: StartChoice) => void }) {
  const refs = useRef<Partial<Record<StartChoice, HTMLButtonElement | null>>>({});
  const choices: StartChoice[] = [...START_STAGES, 'review-pr', ...(learn ? ['learn' as const] : [])];
  const onKey = (e: ReactKeyboardEvent<HTMLDivElement>) => {
    const step = e.key === 'ArrowRight' || e.key === 'ArrowDown' ? 1 : e.key === 'ArrowLeft' || e.key === 'ArrowUp' ? -1 : 0;
    if (!step) return;
    e.preventDefault();
    const next = choices[(choices.indexOf(value) + step + choices.length) % choices.length];
    onChange(next);
    refs.current[next]?.focus();
  };
  return (
    <div className="field">
      <span className="label" id={`${id}-label`}>
        Where do you want to start?
      </span>
      <div className="starts" role="radiogroup" aria-labelledby={`${id}-label`} aria-describedby={`${id}-hint`} onKeyDown={onKey}>
        {choices.map((c, i) => {
          const button = (
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
              {c !== 'review-pr' && c !== 'learn' && <span className="n">{KIND_STAGES.task.indexOf(c)}</span>}
              {c === 'review-pr' ? KIND_LABELS['review-pr'] : c === 'learn' ? LEARN_LABEL : STAGE_LABELS[c]}
            </button>
          );
          const or = (
            <span className="start-or" aria-hidden="true">
              or
            </span>
          );
          // "or Learn the project" wraps as one, when the row runs out of room.
          if (c === 'learn') {
            return (
              <span key={c} className="start-alt">
                {or}
                {button}
              </span>
            );
          }
          return (
            <Fragment key={c}>
              {i > 0 && (c === 'review-pr' ? or : <span className={cx('start-link', i === 1 && 'dotted')} aria-hidden="true" />)}
              {button}
            </Fragment>
          );
        })}
      </div>
      <span className="hint" id={`${id}-hint`}>
        {START_HINTS[value]}
      </span>
    </div>
  );
}

const ROUND_CHOICES = Array.from({ length: MAX_ROUNDS + 1 }, (_, i) => i);
const DEPTH_CHOICES = Array.from({ length: MAX_DEPTH }, (_, i) => i + 1);

/**
 * The code review's four settings (strategy, depth, re-review rounds, check) as selects, each with its
 * ⓘ. The global ones always have a value; a project's or a session's (`scoped`) start with
 * "Default (…)", the value they'd inherit, which leaves the setting out (null). `onChange` gets
 * the whole set after one change.
 */
export function ReviewFields({ id, value, scoped, inherited, disabled, onChange }: {
  id: string;
  value: ReviewOverrides;
  /** A project's or a session's: each setting starts with "Default (…)". False for the global ones. */
  scoped: boolean;
  /** What "Default" stands for, when known. */
  inherited: ReviewSettings | null;
  disabled?: boolean;
  onChange: (v: ReviewOverrides) => void;
}) {
  const def = (text: string | undefined) => (scoped ? <option value="">{text === undefined ? 'Default' : `Default (${text})`}</option> : null);
  // "Default" leaves the setting out, so a level with nothing set is `{}`.
  const set = (k: keyof ReviewOverrides, raw: string) => onChange(withOverride(value, k, raw));
  const strategyText = (v: ReviewStrategySetting) => (v === 'off' ? 'Off' : strategyName(v));
  return (
    <>
      <Field
        label="Review before opening a PR"
        info={{
          for: `${id}-strategy`,
          text: (
            <>
              Kode reviews the change on this computer before it opens the PR, the task fixes or answers what the
              review found, and the PR says so in a <span className="mono">me_kode_review:</span> line.{' '}
              {STRATEGIES.map((s) => `${s.name}: ${s.summary}`).join(' ')}
            </>
          ),
        }}
      >
        <select id={`${id}-strategy`} className="input" disabled={disabled} value={value.strategy ?? ''} onChange={(e) => set('strategy', e.target.value)}>
          {def(inherited ? strategyText(inherited.strategy) : undefined)}
          <option value="off">Off</option>
          {STRATEGIES.map((s) => (
            <option key={s.id} value={s.id}>
              {s.name}
            </option>
          ))}
        </select>
      </Field>
      <Field
        label="Review depth"
        info={{
          for: `${id}-depth`,
          text: "The number of times a review runs before Kode gets the findings to fix. More than one sweep makes it a deep review: each sweep after the first is told what the earlier ones found and looks elsewhere, and the sweeps run up to this count, but from the second sweep on, one that finds nothing new ends it. Costs more tokens.",
        }}
      >
        <select id={`${id}-depth`} className="input" disabled={disabled} value={value.depth ?? ''} onChange={(e) => set('depth', e.target.value)}>
          {def(inherited ? String(inherited.depth) : undefined)}
          {DEPTH_CHOICES.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
      </Field>
      <Field
        label="Re-review rounds"
        info={{
          for: `${id}-rounds`,
          text: "The number of times the code review runs again after Kode fixes the last review's findings, before the PR opens. A review that finds nothing ends it early.",
        }}
      >
        <select id={`${id}-rounds`} className="input" disabled={disabled} value={value.rounds ?? ''} onChange={(e) => set('rounds', e.target.value)}>
          {def(inherited ? String(inherited.rounds) : undefined)}
          {ROUND_CHOICES.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
      </Field>
      <Field
        label="Check the findings first"
        info={{
          for: `${id}-check`,
          text: "Yes: each round's findings wait for you. Add a note to any of them, or dismiss some, then send them to Kode. No: Kode fixes them at once. With Run to merge on, they never wait.",
        }}
      >
        <select id={`${id}-check`} className="input" disabled={disabled} value={value.check == null ? '' : value.check ? 'yes' : 'no'} onChange={(e) => set('check', e.target.value)}>
          {def(inherited ? (inherited.check ? 'Yes' : 'No') : undefined)}
          <option value="yes">Yes</option>
          <option value="no">No</option>
        </select>
      </Field>
    </>
  );
}
