// What the terminal key bar and compose box send, as terminal input. Pure, so it's unit-tested.

export type BarKey =
  | 'tab' | 'up' | 'down' | 'left' | 'right' | 'ctrl-c' | 'enter'
  | 'esc' | 'shift-tab' | 'home' | 'end' | 'pgup' | 'pgdn';

/** The key bar, in order. The ▾ key at its far right opens MORE_KEYS (see TermKeys). */
export const BAR_KEYS: { key: BarKey; label: string; title: string }[] = [
  { key: 'tab', label: 'tab', title: 'Tab' },
  { key: 'up', label: '↑', title: 'Up' },
  { key: 'down', label: '↓', title: 'Down' },
  { key: 'left', label: '←', title: 'Left' },
  { key: 'right', label: '→', title: 'Right' },
  { key: 'ctrl-c', label: '^C', title: 'Ctrl+C' },
  { key: 'enter', label: '⏎', title: 'Enter' },
];

/** The row the ▾ key opens, in order. The sticky Ctrl key comes first (see TermKeys). */
export const MORE_KEYS: { key: BarKey; label: string; title: string }[] = [
  { key: 'esc', label: 'esc', title: 'Escape' },
  { key: 'shift-tab', label: '⇧tab', title: 'Shift+Tab' },
  { key: 'home', label: 'home', title: 'Home' },
  { key: 'end', label: 'end', title: 'End' },
  { key: 'pgup', label: 'pgup', title: 'Page Up' },
  { key: 'pgdn', label: 'pgdn', title: 'Page Down' },
];

// Arrows, Home and End follow the cursor-key mode.
const ARROWS: Partial<Record<BarKey, string>> = { up: 'A', down: 'B', right: 'C', left: 'D', home: 'H', end: 'F' };

const FIXED: Partial<Record<BarKey, string>> = {
  tab: '\t',
  'ctrl-c': '\x03',
  enter: '\r',
  esc: '\x1b',
  'shift-tab': '\x1b[Z',
  pgup: '\x1b[5~',
  pgdn: '\x1b[6~',
};

/**
 * The bytes a key sends. Arrows, Home and End follow the terminal's cursor-key mode (DECCKM), like a
 * real keyboard.
 */
export function keySequence(key: BarKey, applicationCursor = false): string {
  const arrow = ARROWS[key];
  if (arrow) return `${applicationCursor ? '\x1bO' : '\x1b['}${arrow}`;
  return FIXED[key] ?? '';
}

// Ctrl with a punctuation key, as a terminal sends it.
const CTRL_PUNCT: Record<string, string> = { ' ': '\x00', '@': '\x00', '[': '\x1b', '\\': '\x1c', ']': '\x1d', '^': '\x1e', '_': '\x1f', '?': '\x7f' };

/**
 * The byte Ctrl plus a typed character sends: Ctrl+A is 0x01 through Ctrl+Z at 0x1a, either case (a
 * phone may capitalise the first letter), plus the usual punctuation. Null for anything else.
 */
export function ctrlSequence(ch: string): string | null {
  if (ch.length !== 1) return null;
  const c = ch.toLowerCase();
  if (c >= 'a' && c <= 'z') return String.fromCharCode(c.charCodeAt(0) - 96);
  return CTRL_PUNCT[ch] ?? null;
}

/**
 * The character typed into a text box, from its value before and after one edit, or null when the
 * edit wasn't a single inserted character (a deletion, a paste, an autocorrect rewrite).
 */
export function insertedChar(before: string, after: string): string | null {
  if (after.length !== before.length + 1) return null;
  let i = 0;
  while (i < before.length && before[i] === after[i]) i++;
  return after.slice(0, i) + after.slice(i + 1) === before ? after[i] : null;
}

const PASTE_START = '\x1b[200~';
const PASTE_END = '\x1b[201~';

/**
 * Text from the compose box as terminal input, without the Enter that submits it. When the program
 * asked for bracketed paste it arrives as one paste, so its newlines stay newlines, and every Escape
 * is dropped so the text can't end the paste early (not even by rebuilding the end marker around a
 * removed one). Otherwise newlines become carriage returns, as a keyboard would type them.
 */
export function pasteText(text: string, bracketed: boolean): string {
  const t = text.replace(/\r\n?/g, '\n');
  if (bracketed) return PASTE_START + t.split('\x1b').join('') + PASTE_END;
  return t.replace(/\n/g, '\r');
}

/**
 * A file's path pasted for Claude Code, which reads a pasted path to an image as that image (it's how
 * a terminal hands over a dropped file). Characters other than plain path ones are backslash-escaped,
 * as a terminal does for a drop, so the path stays one word.
 */
export function pastePath(path: string, bracketed: boolean): string {
  return pasteText(path.replace(/[^\w./~:@%+=,-]/g, (c) => `\\${c}`), bracketed);
}

/**
 * One mouse-wheel step at a cell (1-based), as an SGR mouse report (tmux turns SGR reporting on
 * with `mouse on`). Coordinates are clamped to the terminal.
 */
export function wheelReport(up: boolean, col: number, row: number, cols: number, rows: number): string {
  const clamp = (v: number, max: number) => Math.min(Math.max(1, Math.round(v)), Math.max(1, max));
  return `\x1b[<${up ? 64 : 65};${clamp(col, cols)};${clamp(row, rows)}M`;
}

/** The screen row (0-based) under a finger at `y`, in a terminal drawn from `top` over `height` px. */
export function touchRow(y: number, top: number, height: number, rows: number): number {
  const row = Math.floor(((y - top) / Math.max(1, height)) * rows);
  return Math.min(Math.max(0, row), Math.max(0, rows - 1));
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
