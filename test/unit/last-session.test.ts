import { describe, expect, it } from 'vitest';
import { lastSessionClosed } from '../../src/web/util.js';

const at = (projectId: string | null, live: number) => ({ projectId, live });

describe('lastSessionClosed', () => {
  it('is true when the project on screen goes from open sessions to none', () => {
    expect(lastSessionClosed(at('a', 1), at('a', 0))).toBe(true);
    expect(lastSessionClosed(at('a', 3), at('a', 0))).toBe(true);
  });

  it('is false while sessions stay open or none were open', () => {
    expect(lastSessionClosed(at('a', 0), at('a', 0))).toBe(false);
    expect(lastSessionClosed(at('a', 2), at('a', 1))).toBe(false);
    expect(lastSessionClosed(at('a', 0), at('a', 1))).toBe(false);
  });

  it('is false on a switch to a project without sessions', () => {
    expect(lastSessionClosed(at('a', 2), at('b', 0))).toBe(false);
  });

  it('is false on the first render and off a project', () => {
    expect(lastSessionClosed(at(null, 0), at('a', 0))).toBe(false);
    expect(lastSessionClosed(at('a', 1), at(null, 0))).toBe(false);
  });
});
