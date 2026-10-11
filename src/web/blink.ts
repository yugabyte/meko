/**
 * The status dots' blink signals: runs of halo on and off on one 100 ms clock, played with the Web
 * Animations API.
 */

/** One unit of every signal. */
export const BLINK_UNIT_MS = 100;

/** A stretch of the signal with the halo on or off. */
export interface BlinkRun {
  on: boolean;
  units: number;
}

/** Runs from their lengths in units, alternating on and off, starting on. */
function blinkRuns(pattern: string): BlinkRun[] {
  return pattern.split(' ').map((n, i) => ({ on: i % 2 === 0, units: Number(n) }));
}

export const WORKING_RUNS = blinkRuns(
  '3 1 3 3 3 1 1 1 3 1 3 7 3 1 1 1 3 1 1 3 3 1 3 1 3 3 3 1 1 1 1 3 1 7 1 1 3 1 3 3 3 1 3 1 3 3 1 1 3 1 1 3 3 1 1 1 3 3 1 1 1 1 1 3 ' +
    '1 1 3 1 1 1 3 1 1 1 3 7 1 1 1 7 1 1 1 1 1 1 1 3 1 1 3 3 1 1 1 1 1 1 3 3 1 7 3 1 1 3 3 1 3 1 3 7 1 1 1 3 3 1 1 1 1 3 1 3 1 1 3 7 ' +
    '1 1 3 1 3 3 1 1 1 1 1 1 1 3 3 1 1 1 3 1 3 7',
);

/** Knock knock, pause, knock knock, long pause. */
export const KNOCK_RUNS = blinkRuns('1 1 1 4 1 1 1 16');

export function totalUnits(runs: readonly BlinkRun[]): number {
  return runs.reduce((n, r) => n + r.units, 0);
}

/**
 * How far into the signal a dot keyed by `key` (a session's or project's id) runs, in whole units:
 * the same for every dot of that key, and spread over the signal between keys (FNV-1a).
 */
export function blinkOffsetMs(key: string, runs: readonly BlinkRun[]): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < key.length; i++) h = Math.imul(h ^ key.charCodeAt(i), 0x01000193);
  return ((h >>> 0) % totalUnits(runs)) * BLINK_UNIT_MS;
}

/**
 * Keyframes that hold each run's halo until the next run (a hard step, no fade). The colour is the
 * dot's `--halo`, so it follows the theme.
 */
export function blinkKeyframes(runs: readonly BlinkRun[]): Keyframe[] {
  const total = totalUnits(runs);
  const on = '0 0 0 3px var(--halo)';
  const off = '0 0 0 3px transparent';
  let at = 0;
  const frames: Keyframe[] = runs.map((r) => {
    const frame = { offset: at / total, easing: 'steps(1, end)', boxShadow: r.on ? on : off };
    at += r.units;
    return frame;
  });
  frames.push({ offset: 1, boxShadow: off });
  return frames;
}
