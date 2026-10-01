import { describe, expect, it } from 'vitest';
import {
  BAR_KEYS, FLING_MAX, MORE_KEYS, FONT_MAX, FONT_MIN, clampFont, ctrlSequence, decayVelocity, flingVelocity, insertedChar, keySequence, pasteText, touchRow, wheelReport, wheelSteps,
} from '../../src/web/keys.js';
import { keyboardOpen } from '../../src/web/hooks/useMedia.js';

describe('terminal key bar', () => {
  it('sends the bytes a keyboard would', () => {
    expect(keySequence('tab')).toBe('\t');
    expect(keySequence('ctrl-c')).toBe('\x03');
    expect(keySequence('enter')).toBe('\r');
  });

  it('sends arrows in normal and application cursor mode', () => {
    expect(['up', 'down', 'right', 'left'].map((k) => keySequence(k as 'up'))).toEqual(['\x1b[A', '\x1b[B', '\x1b[C', '\x1b[D']);
    expect(['up', 'down', 'right', 'left'].map((k) => keySequence(k as 'up', true))).toEqual(['\x1bOA', '\x1bOB', '\x1bOC', '\x1bOD']);
    // Other keys don't change with the mode.
    expect(keySequence('tab', true)).toBe('\t');
  });

  it('sends the more keys the ▾ row holds', () => {
    expect(keySequence('esc')).toBe('\x1b');
    expect(keySequence('shift-tab')).toBe('\x1b[Z');
    expect(keySequence('pgup')).toBe('\x1b[5~');
    expect(keySequence('pgdn')).toBe('\x1b[6~');
    // Home and End follow the cursor-key mode, like the arrows.
    expect([keySequence('home'), keySequence('end')]).toEqual(['\x1b[H', '\x1b[F']);
    expect([keySequence('home', true), keySequence('end', true)]).toEqual(['\x1bOH', '\x1bOF']);
    expect(keySequence('esc', true)).toBe('\x1b');
  });

  it('has a sequence for every key on the bar and in the ▾ row', () => {
    for (const k of [...BAR_KEYS, ...MORE_KEYS]) expect(keySequence(k.key)).not.toBe('');
  });
});

describe('select mode', () => {
  it('finds the row under a finger, kept on the screen', () => {
    // 20 rows over 400 px starting at 100: 20 px a row.
    expect(touchRow(100, 100, 400, 20)).toBe(0);
    expect(touchRow(119, 100, 400, 20)).toBe(0);
    expect(touchRow(120, 100, 400, 20)).toBe(1);
    expect(touchRow(499, 100, 400, 20)).toBe(19);
    // Dragged past the top or bottom edge.
    expect(touchRow(40, 100, 400, 20)).toBe(0);
    expect(touchRow(900, 100, 400, 20)).toBe(19);
    // No rows yet.
    expect(touchRow(10, 0, 400, 0)).toBe(0);
  });
});

describe('sticky Ctrl', () => {
  it('sends Ctrl with a letter, in either case, and the usual punctuation', () => {
    expect(ctrlSequence('a')).toBe('\x01');
    expect(ctrlSequence('O')).toBe('\x0f');
    expect(ctrlSequence('r')).toBe('\x12');
    expect(ctrlSequence('z')).toBe('\x1a');
    expect(ctrlSequence('[')).toBe('\x1b');
    expect(ctrlSequence(' ')).toBe('\x00');
    expect(ctrlSequence('?')).toBe('\x7f');
  });

  it('has nothing for digits, other symbols or more than one character', () => {
    expect(ctrlSequence('1')).toBeNull();
    expect(ctrlSequence('é')).toBeNull();
    expect(ctrlSequence('ab')).toBeNull();
    expect(ctrlSequence('')).toBeNull();
  });

  it('finds the one character typed into the box, wherever the cursor was', () => {
    expect(insertedChar('', 'o')).toBe('o');
    expect(insertedChar('fix it', 'fix itr')).toBe('r');
    expect(insertedChar('fix it', 'fxix it')).toBe('x');
    expect(insertedChar('fix it', 'fix i')).toBeNull(); // a deletion
    expect(insertedChar('fix', 'fix the')).toBeNull(); // a paste or a word from the suggestions bar
    expect(insertedChar('teh', 'thex')).toBeNull(); // autocorrect rewrote it
  });

  it('is not on the bar or in the ▾ row as a key that sends bytes', () => {
    expect(BAR_KEYS.map((k) => k.label)).toEqual(['tab', '↑', '↓', '←', '→', '^C', '⏎']);
    expect(MORE_KEYS.map((k) => k.label)).toEqual(['esc', '⇧tab', 'home', 'end', 'pgup', 'pgdn']);
  });
});

describe('compose box', () => {
  it('wraps text in a bracketed paste when the terminal asked for one', () => {
    expect(pasteText('fix the tests', true)).toBe('\x1b[200~fix the tests\x1b[201~');
    expect(pasteText('line one\r\nline two\nthree', true)).toBe('\x1b[200~line one\nline two\nthree\x1b[201~');
  });

  it('never lets the text end the paste early', () => {
    expect(pasteText('a\x1b[201~b', true)).toBe('\x1b[200~a[201~b\x1b[201~');
    // Removing one end marker must not leave another behind.
    const nested = pasteText('hi\x1b[20\x1b[201~1~\rrm -rf x\r', true);
    expect(nested).toBe('\x1b[200~hi[20[201~1~\nrm -rf x\n\x1b[201~');
    expect(nested.slice('\x1b[200~'.length, -'\x1b[201~'.length)).not.toContain('\x1b');
  });

  it('types newlines as Enter without bracketed paste', () => {
    expect(pasteText('one line', false)).toBe('one line');
    expect(pasteText('a\nb\r\nc', false)).toBe('a\rb\rc');
  });
});

describe('touch scrolling', () => {
  it('reports wheel steps as SGR mouse events', () => {
    expect(wheelReport(true, 10, 5, 80, 24)).toBe('\x1b[<64;10;5M');
    expect(wheelReport(false, 10, 5, 80, 24)).toBe('\x1b[<65;10;5M');
  });

  it('clamps coordinates to the terminal', () => {
    expect(wheelReport(true, 0, -3, 80, 24)).toBe('\x1b[<64;1;1M');
    expect(wheelReport(true, 200, 99, 80, 24)).toBe('\x1b[<64;80;24M');
    expect(wheelReport(false, 3.6, 2.2, 80, 24)).toBe('\x1b[<65;4;2M');
  });
});

describe('terminal font size', () => {
  it('stays within bounds', () => {
    expect(clampFont(FONT_MIN - 3)).toBe(FONT_MIN);
    expect(clampFont(FONT_MAX + 3)).toBe(FONT_MAX);
    expect(clampFont(11.4)).toBe(11);
  });
});

describe('on-screen keyboard', () => {
  it('counts a much shorter visible viewport as the keyboard', () => {
    expect(keyboardOpen(844, 844, 1)).toBe(false);
    expect(keyboardOpen(844, 800, 1)).toBe(false); // a browser toolbar, not the keyboard
    expect(keyboardOpen(844, 480, 1)).toBe(true);
  });

  it('ignores pinch zoom', () => {
    expect(keyboardOpen(844, 422, 2)).toBe(false);
    expect(keyboardOpen(844, 240, 2)).toBe(true);
  });
});

describe('swipe momentum', () => {
  it('measures the release speed over the last 100 ms', () => {
    const samples = [{ y: 0, t: 0 }, { y: 50, t: 150 }, { y: 100, t: 200 }, { y: 160, t: 250 }];
    expect(flingVelocity(samples, 250)).toBeCloseTo(110 / 100); // from t=150 to t=250
  });

  it('still counts the last moves when the lift comes a moment later', () => {
    expect(flingVelocity([{ y: 0, t: 0 }, { y: 50, t: 50 }, { y: 100, t: 100 }], 150)).toBeCloseTo(1);
  });

  it("doesn't coast when the finger held still before letting go", () => {
    expect(flingVelocity([{ y: 0, t: 0 }, { y: 200, t: 100 }], 400)).toBe(0);
    expect(flingVelocity([{ y: 10, t: 90 }], 100)).toBe(0);
  });

  it('caps a hard flick', () => {
    expect(flingVelocity([{ y: 0, t: 0 }, { y: -5000, t: 50 }], 50)).toBe(-FLING_MAX);
  });

  it('slows down about 5% a frame', () => {
    expect(decayVelocity(1, 1000 / 60)).toBeCloseTo(0.95);
    expect(decayVelocity(1, 0)).toBe(1);
    expect(decayVelocity(-2, 2000 / 60)).toBeCloseTo(-2 * 0.95 * 0.95);
  });

  it('turns finger travel into whole wheel steps and carries the rest', () => {
    expect(wheelSteps(95, 30)).toEqual({ steps: 3, rest: 5 });
    expect(wheelSteps(-95, 30)).toEqual({ steps: -3, rest: -5 });
    expect(wheelSteps(20, 30)).toEqual({ steps: 0, rest: 20 });
    expect(wheelSteps(20, 0)).toEqual({ steps: 0, rest: 20 });
  });
});
