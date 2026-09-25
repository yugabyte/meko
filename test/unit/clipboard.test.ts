import { describe, expect, it } from 'vitest';
import { decodeOsc52 } from '../../src/web/clipboard.js';

const b64 = (s: string) => Buffer.from(s, 'utf8').toString('base64');

describe('OSC 52 from tmux', () => {
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
