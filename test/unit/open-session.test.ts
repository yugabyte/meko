import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Session } from '../../src/shared/types';
import { lastOpenSession, rememberOpenSession, sessionToOpen } from '../../src/web/util.js';

const session = (id: string, createdAt: number, lastEventAt?: number) => ({ id, createdAt, lastEventAt }) as Session;

describe('sessionToOpen', () => {
  const live = [session('old', 1, 50), session('busy', 2, 90), session('new', 3)];

  it('reopens the session last open in the project', () => {
    expect(sessionToOpen(live, 'old')?.id).toBe('old');
    expect(sessionToOpen(live, 'new')?.id).toBe('new');
  });

  it('falls back to the most recently active session when none was open or it was closed', () => {
    expect(sessionToOpen(live, undefined)?.id).toBe('busy');
    expect(sessionToOpen(live, 'archived')?.id).toBe('busy');
  });

  it('is undefined for a project without sessions, and leaves the list in order', () => {
    expect(sessionToOpen([], 'old')).toBeUndefined();
    expect(live.map((s) => s.id)).toEqual(['old', 'busy', 'new']);
  });
});

describe('rememberOpenSession', () => {
  beforeEach(() => {
    const items = new Map<string, string>();
    vi.stubGlobal('localStorage', {
      getItem: (k: string) => items.get(k) ?? null,
      setItem: (k: string, v: string) => void items.set(k, v),
      removeItem: (k: string) => void items.delete(k),
    });
  });
  afterEach(() => vi.unstubAllGlobals());

  it('keeps the open session per project', () => {
    expect(lastOpenSession('a')).toBeUndefined();
    rememberOpenSession('a', 's1');
    rememberOpenSession('b', 's2');
    rememberOpenSession('a', 's3');
    expect(lastOpenSession('a')).toBe('s3');
    expect(lastOpenSession('b')).toBe('s2');
  });

  it('ignores bad stored values', () => {
    localStorage.setItem('ak-last-session', 'not json');
    expect(lastOpenSession('a')).toBeUndefined();
    rememberOpenSession('a', 's1');
    expect(lastOpenSession('a')).toBe('s1');
  });

  it('does nothing when storage is blocked', () => {
    vi.stubGlobal('localStorage', {
      getItem: () => {
        throw new Error('blocked');
      },
      setItem: () => {
        throw new Error('blocked');
      },
    });
    expect(() => rememberOpenSession('a', 's1')).not.toThrow();
    expect(lastOpenSession('a')).toBeUndefined();
  });
});
