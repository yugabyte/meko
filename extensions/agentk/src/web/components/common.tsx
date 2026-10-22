import { Fragment, useEffect, useRef, useState, type KeyboardEvent as ReactKeyboardEvent, type ReactNode, type RefObject } from 'react';
import { HARNESS_CAPS, harnessOf, type HarnessId } from '../../shared/harnesses';
import { KIND_LABELS, KIND_STAGES, START_HINTS, START_STAGES, STAGE_LABELS } from '../../shared/types';
import type { StartStage } from '../../shared/types';
import { BLINK_UNIT_MS, KNOCK_RUNS, WORKING_RUNS, blinkKeyframes, blinkOffsetMs, totalUnits, type BlinkRun } from '../blink';
import { useMedia } from '../hooks/useMedia';
import { TUNE, typingKeyframes, typingLoopMs } from '../typing';
import { LEARNING_TITLE, STATUS_TEXT, type DotState, contextArc, revisitWhen, revisitLeft, contextLevel, cx, elapsedText, instructionsBlocker, isSubmitKey, mekoDatapackUrl } from '../util';

const HARNESS_LOGOS: Record<HarnessId, string> = { claude: '/harness-claude.svg', kiro: '/harness-kiro.svg' };

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
 * A session's status dot; `learning` is a dotted ring turning around Meko's m, `queued` the idle
 * ring with a gap, turning, `meko` the working dot with Meko's m in it, and `revisit` a still, muted
 * clock face (`dotState()`). Working
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
            : status === 'queued'
              ? 'idle queued'
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
      <div
        className={cx('dialog', wide && 'wide', className)}
        role="dialog"
        aria-modal="true"
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

/** How long until a session set aside is to be revisited (42m, 1h 5m), kept current. */
export function RevisitChip({ until }: { until: number }) {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 30_000);
    return () => clearInterval(t);
  }, []);
  return <span className="revisit-chip" title={`Revisit at ${revisitWhen(until, now)}`}>{revisitLeft(until, now)}</span>;
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

/** A note with lines: shown beside a gate to add instructions for AgentK (Open PR with instructions). */
export function NotesIcon({ size = 14 }: { size?: number }) {
  return (
    <svg width={size} height={size} viewBox="0 0 16 16" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M3.5 1.75h6l3 3v9.5h-9z" />
      <path d="M6 7.5h4M6 10h4M6 12.5h2.5" />
    </svg>
  );
}

/**
 * Optional instructions for AgentK, sent after a move's or Open PR's prompt; left empty, nothing
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
    <Field label="Instructions for AgentK (optional)" hint={tooLong ? <span className="over">{tooLong}</span> : hint}>
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
