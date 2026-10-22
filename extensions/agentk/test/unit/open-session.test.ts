import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ProjectWithSessions, Session } from '../../src/shared/types';
import { lastOpenSession, openSessions, rememberOpenSession, sessionToOpen } from '../../src/web/util.js';

const session = (id: string, createdAt: number, lastEventAt?: number, position = 0) => ({ id, createdAt, lastEventAt, position }) as Session;

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

describe('openSessions', () => {
  it('lists the open sessions by position, oldest first on a tie, without closed ones', () => {
    const p = {
      sessions: [
        { ...session('b', 2), desired: 'running', status: 'idle' },
        { ...session('gone', 0, undefined, -5), desired: 'archived', status: 'idle' },
        { ...session('a', 1), desired: 'running', status: 'working' },
        { ...session('top', 4, undefined, -1), desired: 'running', status: 'idle' },
        { ...session('ended', 3), desired: 'running', status: 'archived' },
      ],
    } as ProjectWithSessions;
    expect(openSessions(p).map((s) => s.id)).toEqual(['top', 'a', 'b']);
    expect(p.sessions[0].id).toBe('b');
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
