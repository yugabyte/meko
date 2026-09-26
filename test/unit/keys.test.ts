import { describe, expect, it } from 'vitest';
import { BAR_KEYS, FONT_MAX, FONT_MIN, clampFont, keySequence, pasteText, wheelReport } from '../../src/web/keys.js';
import { keyboardOpen } from '../../src/web/hooks/useMedia.js';

describe('terminal key bar', () => {
  it('sends the bytes a keyboard would', () => {
    expect(keySequence('esc')).toBe('\x1b');
    expect(keySequence('tab')).toBe('\t');
    expect(keySequence('shift-tab')).toBe('\x1b[Z');
    expect(keySequence('ctrl-c')).toBe('\x03');
    expect(keySequence('enter')).toBe('\r');
  });

  it('sends arrows in normal and application cursor mode', () => {
    expect(['up', 'down', 'right', 'left'].map((k) => keySequence(k as 'up'))).toEqual(['\x1b[A', '\x1b[B', '\x1b[C', '\x1b[D']);
    expect(['up', 'down', 'right', 'left'].map((k) => keySequence(k as 'up', true))).toEqual(['\x1bOA', '\x1bOB', '\x1bOC', '\x1bOD']);
    // Other keys don't change with the mode.
    expect(keySequence('esc', true)).toBe('\x1b');
  });

  it('has a sequence for every key on the bar', () => {
    for (const k of BAR_KEYS) expect(keySequence(k.key)).not.toBe('');
  });
});

describe('compose box', () => {
  it('wraps text in a bracketed paste when the terminal asked for one', () => {
    expect(pasteText('fix the tests', true)).toBe('\x1b[200~fix the tests\x1b[201~');
    expect(pasteText('line one\r\nline two\nthree', true)).toBe('\x1b[200~line one\nline two\nthree\x1b[201~');
  });

  it('never lets the text end the paste early', () => {
    expect(pasteText('a\x1b[201~b', true)).toBe('\x1b[200~ab\x1b[201~');
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
