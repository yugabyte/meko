/**
 * The tune the in-progress keycaps (`Dots`) play, as [key, beats] pairs, turned into one set of Web
 * Animations keyframes per key.
 */

/** A key (1 to 3, left to right; 0 for a rest with every key up) and how many beats it lasts. */
export type Note = readonly [key: 0 | 1 | 2 | 3, beats: number];

export const TUNE: readonly Note[] = [
  [3, 0.5], [2, 0.5], [3, 0.5], [2, 0.5], [3, 0.5], [1, 0.5], [2, 0.5], [2, 0.5], [1, 1], [0, 0.5],
  [1, 0.5], [2, 0.5], [3, 0.5], [3, 1], [0, 0.5],
  [1, 0.5], [2, 0.5], [3, 0.5], [3, 1], [0, 0.5],
  [1, 0.5],
  [3, 0.5], [2, 0.5], [3, 0.5], [2, 0.5], [3, 0.5], [1, 0.5], [2, 0.5], [2, 0.5], [1, 1], [0, 0.5],
  [1, 0.5], [2, 0.5], [3, 0.5], [3, 1], [0, 0.5],
  [1, 0.5], [3, 0.5], [2, 0.5], [1, 2],
];

export const TYPING_BEAT_MS = 500;
/** The rest after the last note, before the tune starts again. */
export const TYPING_REST_BEATS = 2;
/** How long a key takes to go down (at most 30% of its note) and to come back up at the note's end. */
const PRESS_MS = 40;
const RELEASE_MS = 60;

const EDGE = 'color-mix(in srgb, currentColor 55%, transparent)';
/** A key at rest on its edge, and pressed onto it: the width never changes. */
const UP = { transform: 'none', boxShadow: `0 var(--edge) 0 ${EDGE}` };
const DOWN = { transform: 'translateY(var(--edge))', boxShadow: `0 0 0 ${EDGE}` };

/** Each note's key, start and length in ms, rests left out. */
export function noteTimes(song: readonly Note[]): { key: number; at: number; ms: number }[] {
  let at = 0;
  const notes: { key: number; at: number; ms: number }[] = [];
  for (const [key, beats] of song) {
    if (key) notes.push({ key, at, ms: beats * TYPING_BEAT_MS });
    at += beats * TYPING_BEAT_MS;
  }
  return notes;
}

/** One time through the tune and the rest after it. */
export function typingLoopMs(song: readonly Note[]): number {
  return (song.reduce((n, [, beats]) => n + beats, 0) + TYPING_REST_BEATS) * TYPING_BEAT_MS;
}

/** Keyframes for one key over a loop: pressed for each of its notes (held for long ones), else at rest. */
export function typingKeyframes(song: readonly Note[], key: number): Keyframe[] {
  const loop = typingLoopMs(song);
  const frames: Keyframe[] = [{ offset: 0, ...UP }];
  for (const n of noteTimes(song)) {
    if (n.key !== key) continue;
    const press = Math.min(PRESS_MS, n.ms * 0.3);
    const release = Math.max(press, n.ms - RELEASE_MS);
    frames.push(
      { offset: n.at / loop, ...UP },
      { offset: (n.at + press) / loop, ...DOWN },
      { offset: (n.at + release) / loop, ...DOWN },
      { offset: (n.at + n.ms) / loop, ...UP },
    );
  }
  frames.push({ offset: 1, ...UP });
  return frames;
}
