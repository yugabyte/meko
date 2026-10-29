import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ProjectWithSessions, Session } from '../../src/shared/types';
import { joinedPartSelected, lastOpenSession, liveWithReviews, openSessions, rememberOpenSession, reviewParent, sessionToOpen } from '../../src/web/util.js';

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

  it("leaves out a task's code-review session, which only a terminal or the route may show", () => {
    const p = {
      sessions: [
        { ...session('task', 1), desired: 'running', status: 'idle', kind: 'plan' },
        { ...session('rv', 2, undefined, -9), desired: 'running', status: 'working', kind: 'code-review', parentId: 'task' },
      ],
    } as ProjectWithSessions;
    expect(openSessions(p).map((s) => s.id)).toEqual(['task']);
    expect(liveWithReviews(p).map((s) => s.id)).toEqual(['task', 'rv']);
    expect(reviewParent(p.sessions, p.sessions[1])?.id).toBe('task');
    expect(reviewParent(p.sessions, p.sessions[0])).toBeUndefined();
  });
});

describe('joinedPartSelected', () => {
  const task = { id: 'task', codeReview: { sessionId: 'rv' } } as Session;
  it.each([
    ['task', 'task'],
    ['rv', 'review'],
    ['other', null],
    [null, null],
  ] as const)('with %s on screen, selects %s', (current, part) => {
    expect(joinedPartSelected(task, current)).toBe(part);
  });

  it("selects the review's part while the task on screen has its findings open", () => {
    expect(joinedPartSelected(task, 'task', true)).toBe('review');
    expect(joinedPartSelected({ id: 'plain', codeReview: null } as Session, 'plain', true)).toBe('task');
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
