import { describe, expect, it } from 'vitest';
import { startingSessionName } from '../../src/web/util.js';

describe('startingSessionName', () => {
  it.each([
    { when: 'the dialog opens after Open project', retry: undefined, fresh: true, name: 'my-feature' },
    { when: 'it opens any other way', retry: undefined, fresh: false, name: '' },
    { when: 'Try again fills it in', retry: 'fix the login', fresh: false, name: 'fix the login' },
  ])('is "$name" when $when', ({ retry, fresh, name }) => {
    expect(startingSessionName(retry, fresh, 'my-feature')).toBe(name);
  });
});
