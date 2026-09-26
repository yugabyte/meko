// What the terminal key bar and compose box send, as terminal input. Pure, so it's unit-tested.

export type BarKey = 'esc' | 'tab' | 'shift-tab' | 'up' | 'down' | 'left' | 'right' | 'ctrl-c' | 'enter';

/** The key bar, in order. */
export const BAR_KEYS: { key: BarKey; label: string; title: string }[] = [
  { key: 'esc', label: 'esc', title: 'Escape: interrupt Claude or close a menu' },
  { key: 'tab', label: 'tab', title: 'Tab' },
  { key: 'shift-tab', label: '⇧tab', title: 'Shift+Tab: switch mode' },
  { key: 'up', label: '↑', title: 'Up' },
  { key: 'down', label: '↓', title: 'Down' },
  { key: 'left', label: '←', title: 'Left' },
  { key: 'right', label: '→', title: 'Right' },
  { key: 'ctrl-c', label: '^C', title: 'Ctrl+C' },
  { key: 'enter', label: '⏎', title: 'Enter' },
];

const ARROWS: Partial<Record<BarKey, string>> = { up: 'A', down: 'B', right: 'C', left: 'D' };

const FIXED: Partial<Record<BarKey, string>> = {
  esc: '\x1b',
  tab: '\t',
  'shift-tab': '\x1b[Z',
  'ctrl-c': '\x03',
  enter: '\r',
};

/** The bytes a key sends. Arrows follow the terminal's cursor-key mode (DECCKM), like a real keyboard. */
export function keySequence(key: BarKey, applicationCursor = false): string {
  const arrow = ARROWS[key];
  if (arrow) return `${applicationCursor ? '\x1bO' : '\x1b['}${arrow}`;
  return FIXED[key] ?? '';
}

const PASTE_START = '\x1b[200~';
const PASTE_END = '\x1b[201~';

/**
 * Text from the compose box as terminal input, without the Enter that submits it. When the program
 * asked for bracketed paste it arrives as one paste, so its newlines stay newlines. Otherwise newlines
 * become carriage returns, as a keyboard would type them.
 */
export function pasteText(text: string, bracketed: boolean): string {
  const t = text.replace(/\r\n?/g, '\n');
  if (bracketed) return PASTE_START + t.split(PASTE_END).join('') + PASTE_END;
  return t.replace(/\n/g, '\r');
}

/**
 * One mouse-wheel step at a cell (1-based), as an SGR mouse report (tmux turns SGR reporting on
 * with `mouse on`). Coordinates are clamped to the terminal.
 */
export function wheelReport(up: boolean, col: number, row: number, cols: number, rows: number): string {
  const clamp = (v: number, max: number) => Math.min(Math.max(1, Math.round(v)), Math.max(1, max));
  return `\x1b[<${up ? 64 : 65};${clamp(col, cols)};${clamp(row, rows)}M`;
}

/** Where a finger was, and when (ms), for a swipe's release speed. */
export interface TouchSample {
  y: number;
  t: number;
}

/** Only the last this-many ms of a swipe count toward its release speed. */
export const FLING_WINDOW_MS = 100;
/** Momentum keeps this share of its speed per 60 Hz frame. */
export const FLING_DECAY = 0.95;
/** Release speeds (px/ms) are capped, so a hard flick doesn't flood Claude with wheel steps. */
export const FLING_MAX = 4;

/**
 * The finger's speed (px/ms, down is positive) over the last FLING_WINDOW_MS of its movement. Zero
 * when it held still that long before letting go, so a slow, deliberate scroll doesn't coast.
 */
export function flingVelocity(samples: TouchSample[], now: number): number {
  const b = samples[samples.length - 1];
  if (!b || now - b.t > FLING_WINDOW_MS) return 0;
  const a = samples.find((s) => b.t - s.t <= FLING_WINDOW_MS);
  if (!a || b.t <= a.t) return 0;
  const v = (b.y - a.y) / (b.t - a.t);
  return Math.max(-FLING_MAX, Math.min(FLING_MAX, v));
}

/** Momentum speed after dt ms of friction. */
export function decayVelocity(v: number, dt: number): number {
  return v * Math.pow(FLING_DECAY, dt / (1000 / 60));
}

/** Whole wheel steps in a finger's travel (px, down is positive); the rest carries over. */
export function wheelSteps(travel: number, step: number): { steps: number; rest: number } {
  const steps = step > 0 ? Math.trunc(travel / step) : 0;
  return { steps, rest: travel - steps * step };
}

/** Terminal font sizes the A−/A+ buttons step through. */
export const FONT_MIN = 9;
export const FONT_MAX = 16;

export function clampFont(size: number): number {
  return Math.min(FONT_MAX, Math.max(FONT_MIN, Math.round(size)));
}
