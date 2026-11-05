import { describe, expect, it } from 'vitest';
import { lastSessionClosed } from '../../src/web/util.js';

const at = (projectId: string | null, live: number) => ({ projectId, live });

describe('lastSessionClosed', () => {
  it.each([
    { when: 'the project on screen goes from open sessions to none', before: at('a', 2), after: at('a', 0), closed: true },
    { when: 'none were open', before: at('a', 0), after: at('a', 0), closed: false },
    { when: 'sessions stay open', before: at('a', 2), after: at('a', 1), closed: false },
    { when: 'switching to a project without sessions', before: at('a', 2), after: at('b', 0), closed: false },
    { when: 'leaving a project', before: at('a', 1), after: at(null, 0), closed: false },
  ])('is $closed when $when', ({ before, after, closed }) => {
    expect(lastSessionClosed(before, after)).toBe(closed);
  });
});
