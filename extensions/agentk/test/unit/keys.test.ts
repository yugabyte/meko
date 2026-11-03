import { describe, expect, it } from 'vitest';
import {
  BAR_KEYS, FLING_MAX, type BarKey, MORE_KEYS, FONT_MAX, FONT_MIN, clampFont, ctrlSequence, decayVelocity, editKeySequence, flingVelocity, insertedChar, isSessionMenuKey, isTap, isTerminalReport, keySequence, pastePath, pasteText, TAP_MAX_MOVE, TAP_MAX_MS, touchCol, touchRow, wheelReport, wheelSteps,
} from '../../src/web/keys.js';
import { keyboardOpen } from '../../src/web/hooks/useMedia.js';
import { isPassiveReport } from '../../src/shared/termreports.js';

describe('terminal key bar', () => {
  it('sends the bytes a keyboard would, arrows, Home and End following the cursor-key mode', () => {
    // [key, normal mode, application cursor mode]
    const keys: [BarKey, string, string][] = [
      ['tab', '\t', '\t'], ['ctrl-c', '\x03', '\x03'], ['enter', '\r', '\r'], ['esc', '\x1b', '\x1b'], ['shift-tab', '\x1b[Z', '\x1b[Z'],
      ['pgup', '\x1b[5~', '\x1b[5~'], ['pgdn', '\x1b[6~', '\x1b[6~'], ['up', '\x1b[A', '\x1bOA'], ['down', '\x1b[B', '\x1bOB'],
      ['right', '\x1b[C', '\x1bOC'], ['left', '\x1b[D', '\x1bOD'], ['home', '\x1b[H', '\x1bOH'], ['end', '\x1b[F', '\x1bOF'],
    ];
    for (const [key, normal, app] of keys) expect([keySequence(key), keySequence(key, true)], key).toEqual([normal, app]);
  });

  it('has a sequence for every key on the bar and in the ▾ row', () => {
    for (const k of [...BAR_KEYS, ...MORE_KEYS]) expect(keySequence(k.key)).not.toBe('');
  });
});

describe('keys the terminal maps itself', () => {
  type Mods = Partial<Record<'metaKey' | 'ctrlKey' | 'shiftKey' | 'altKey' | 'isComposing', boolean>>;
  const key = (k: string, mods: Mods = {}) => ({
    key: k, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, isComposing: false, ...mods,
  });

  it.each([
    // [what, key, on a Mac, sent]
    ['Shift+Enter starts a new line on a Mac', key('Enter', { shiftKey: true }), true, '\x1b\r'],
    ['Shift+Enter starts a new line elsewhere', key('Enter', { shiftKey: true }), false, '\x1b\r'],
    ['⌘← goes to the start of the line', key('ArrowLeft', { metaKey: true }), true, '\x01'],
    ['⌘→ goes to the end of the line', key('ArrowRight', { metaKey: true }), true, '\x05'],
    ['⌥← goes back a word', key('ArrowLeft', { altKey: true }), true, '\x1bb'],
    ['⌥→ goes forward a word', key('ArrowRight', { altKey: true }), true, '\x1bf'],
    ['⌘⌫ deletes to the start of the line', key('Backspace', { metaKey: true }), true, '\x15'],
    ['Ctrl+Backspace deletes a word elsewhere', key('Backspace', { ctrlKey: true }), false, '\x1b\x7f'],
  ])('%s', (_name, e, mac, sent) => {
    expect(editKeySequence(e, mac)).toBe(sent);
  });

  it.each([
    ['Enter, which submits', key('Enter'), [true, false]],
    // xterm already sends ESC CR for it.
    ['Alt+Enter', key('Enter', { altKey: true }), [true, false]],
    ['Ctrl+Enter', key('Enter', { ctrlKey: true }), [true, false]],
    ['⇧⌘Enter', key('Enter', { shiftKey: true, metaKey: true }), [true]],
    ['Ctrl+Shift+Enter', key('Enter', { shiftKey: true, ctrlKey: true }), [false]],
    ['a plain arrow', key('ArrowLeft'), [true, false]],
    ['⇧⌘←, which selects in a text field', key('ArrowLeft', { shiftKey: true, metaKey: true }), [true]],
    // Off a Mac, Alt is Alt: xterm's own Alt+← is what Linux terminals send.
    ['Alt+← elsewhere', key('ArrowLeft', { altKey: true }), [false]],
    ['Ctrl+← elsewhere', key('ArrowLeft', { ctrlKey: true }), [false]],
    ['Meta+Backspace elsewhere', key('Backspace', { metaKey: true }), [false]],
    // On a Mac, ⌥⌫ deletes a word (xterm sends ESC DEL), and Ctrl+Backspace stays ^H.
    ['Ctrl+Backspace on a Mac', key('Backspace', { ctrlKey: true }), [true]],
    ['a plain Backspace', key('Backspace'), [true, false]],
    ['a key typed into an input method', key('Enter', { shiftKey: true, isComposing: true }), [true, false]],
  ])('leaves %s to xterm', (_name, e, platforms) => {
    for (const mac of platforms) expect(editKeySequence(e, mac), mac ? 'on a Mac' : 'elsewhere').toBeNull();
  });
});

describe('select mode', () => {
  it('finds the row (or column) under a finger, kept on the screen', () => {
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
    // Columns, for a tap on a link, the same way.
    expect(touchCol(26, 16, 600, 60)).toBe(1);
  });
});

describe('tapping a link', () => {
  it('counts a short, still touch as a tap, not a swipe or a long press', () => {
    expect(isTap(0, 80)).toBe(true);
    expect(isTap(TAP_MAX_MOVE - 0.5, 80)).toBe(true);
    expect(isTap(TAP_MAX_MOVE, 80)).toBe(false);
    expect(isTap(20, 80)).toBe(false);
    expect(isTap(0, TAP_MAX_MS)).toBe(true);
    expect(isTap(0, TAP_MAX_MS + 1)).toBe(false);
  });
});

describe('sticky Ctrl', () => {
  it('sends Ctrl with a letter, in either case, and the usual punctuation', () => {
    expect(ctrlSequence('a')).toBe('\x01');
    expect(ctrlSequence('O')).toBe('\x0f');
    expect(ctrlSequence('z')).toBe('\x1a');
    expect(ctrlSequence('[')).toBe('\x1b');
    expect(ctrlSequence(' ')).toBe('\x00');
    expect(ctrlSequence('?')).toBe('\x7f');
  });

  it('has nothing for digits, other symbols or more than one character', () => {
    expect(ctrlSequence('1')).toBeNull();
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

describe('pasted images', () => {
  it('pastes the saved image path as one word, which Claude Code reads as the image', () => {
    expect(pastePath('/home/me/.agent-k-local/pastes/s1/20261005-120000-ab12cd34.png', true)).toBe(
      '\x1b[200~/home/me/.agent-k-local/pastes/s1/20261005-120000-ab12cd34.png\x1b[201~',
    );
    expect(pastePath('/Users/Jo Doe/a.png', false)).toBe('/Users/Jo\\ Doe/a.png');
    expect(pastePath("/x/it's (1).png", false)).toBe("/x/it\\'s\\ \\(1\\).png");
  });
});

describe('touch scrolling', () => {
  it('reports wheel steps as SGR mouse events, clamped to the terminal', () => {
    expect(wheelReport(true, 10, 5, 80, 24)).toBe('\x1b[<64;10;5M');
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

describe('terminal reports', () => {
  it("drops xterm.js's answers to the program's queries, which the session host gives itself", () => {
    for (const r of ['\x1b[?1;2c', '\x1b[>0;276;0c', '\x1b[12;40R', '\x1b[?2004;1$y', '\x1b[0n', '\x1b[?0u', '\x1b[8;24;80t',
      '\x1b]11;rgb:1010/1414/1212\x1b\\', '\x1b]10;rgb:d5d5/dddd/d8d8\x07', '\x1bP>|xterm.js(6.0.0)\x1b\\']) {
      expect(isTerminalReport(r), JSON.stringify(r)).toBe(true);
    }
  });

  it('tells reports sent without anyone using the screen (mouse motion with no button, focus) from use', () => {
    for (const r of ['\x1b[<35;10;5M', '\x1b[<39;10;5M', '\x1b[MC!!', '\x1b[I', '\x1b[O']) {
      expect(isPassiveReport(r), JSON.stringify(r)).toBe(true);
    }
    // Drags (motion with a button held) are use.
    for (const t of ['a', '\r', '\x1b[<0;10;5M', '\x1b[<0;10;5m', '\x1b[<32;10;5M', '\x1b[<34;1;1M', '\x1b[M@!!', '\x1b[<64;10;5M', '\x1b[<65;10;5M', '\x1b[M !!', '\x1b[A']) {
      expect(isPassiveReport(t), JSON.stringify(t)).toBe(false);
    }
  });

  it('keeps what was typed, modified F3 included', () => {
    for (const t of ['a', '\r', '\x1b', '\x1b[A', '\x1bOR', '\x1b[1;2R', '\x1b[1;5R', '\x1b[I', '\x1b[O', '\x1b[200~text\x1b[201~', '\x1b[<64;10;5M']) {
      expect(isTerminalReport(t), JSON.stringify(t)).toBe(false);
    }
  });
});

describe('the session menu shortcut', () => {
  const key = (k: string, mods: Partial<Record<'metaKey' | 'ctrlKey' | 'shiftKey' | 'altKey', boolean>> = {}) =>
    ({ key: k, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, ...mods });
  it('is ⌘. on a Mac and Ctrl+. elsewhere', () => {
    expect(isSessionMenuKey(key('.', { metaKey: true }), true)).toBe(true);
    expect(isSessionMenuKey(key('.', { ctrlKey: true }), false)).toBe(true);
  });
  it('takes no other modifier, and not the other platform\'s', () => {
    expect(isSessionMenuKey(key('.', { ctrlKey: true }), true)).toBe(false);
    expect(isSessionMenuKey(key('.', { metaKey: true }), false)).toBe(false);
    expect(isSessionMenuKey(key('.', { metaKey: true, shiftKey: true }), true)).toBe(false);
    expect(isSessionMenuKey(key('.', { ctrlKey: true, altKey: true }), false)).toBe(false);
    expect(isSessionMenuKey(key('.', { metaKey: true, ctrlKey: true }), true)).toBe(false);
    expect(isSessionMenuKey(key('.'), false)).toBe(false);
    expect(isSessionMenuKey(key(',', { ctrlKey: true }), false)).toBe(false);
  });
});
