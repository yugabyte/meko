import { describe, expect, it } from 'vitest';
import { decodeOsc52, pastedFiles, pastedImages } from '../../src/web/clipboard.js';

const b64 = (s: string) => Buffer.from(s, 'utf8').toString('base64');

describe('OSC 52 from a program in the terminal', () => {
  it('decodes the selection, UTF-8 included', () => {
    expect(decodeOsc52(`;${b64('git status')}`)).toBe('git status');
    expect(decodeOsc52(`c;${b64('héllo → wörld\nline 2')}`)).toBe('héllo → wörld\nline 2');
  });

  it('ignores queries, clears and malformed data', () => {
    expect(decodeOsc52('c;?')).toBeNull();
    expect(decodeOsc52('c;')).toBeNull();
    expect(decodeOsc52('no separator')).toBeNull();
    expect(decodeOsc52('c;***not base64***')).toBeNull();
  });
});

describe('images pasted or dropped on the terminal', () => {
  const file = (name: string, type: string) => ({ name, type });
  const transfer = (files: { name: string; type: string }[], text = '') => ({ files, getData: (f: string) => (f === 'text/plain' ? text : '') });

  it('takes the image files, and leaves other files out', () => {
    const shot = file('image.png', 'image/png');
    expect(pastedImages(transfer([shot]))).toEqual([shot]);
    expect(pastedImages(transfer([shot, file('notes.txt', 'text/plain')]))).toEqual([shot]);
    expect(pastedImages(transfer([file('notes.txt', 'text/plain')]))).toEqual([]);
    // The terminal takes every file; the New session dialog, images only.
    const notes = file('notes.txt', 'text/plain');
    expect(pastedFiles(transfer([shot, notes], 'image.png\nnotes.txt'))).toEqual([shot, notes]);
    expect(pastedFiles(transfer([notes], 'some copied text'))).toEqual([]);
    expect(pastedImages(null)).toEqual([]);
    expect(pastedImages({ files: null })).toEqual([]);
  });

  it('pastes text when the image only comes with it (a spreadsheet copy)', () => {
    expect(pastedImages(transfer([file('image.png', 'image/png')], 'Q3\t1200\nQ4\t1500'))).toEqual([]);
  });

  it('still takes copied image files that bring their names as text', () => {
    const a = file('a.png', 'image/png');
    const b = file('b.jpg', 'image/jpeg');
    expect(pastedImages(transfer([a, b], 'a.png\r\nb.jpg\n'))).toEqual([a, b]);
  });

  it('reads no text when the browser refuses it', () => {
    const shot = file('image.png', 'image/png');
    expect(pastedImages({ files: [shot], getData: () => { throw new Error('denied'); } })).toEqual([shot]);
  });
});
