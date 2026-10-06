import { describe, expect, it } from 'vitest';
import type { Project, ProjectWithSessions, Session } from '../../src/shared/types.js';
import { withCreated, withOpened } from '../../src/web/util.js';

const project = (id: string, name = id) => ({ id, name }) as Project;
const live = (id: string, name = id) => ({ id, name, sessions: [], usage: {} }) as unknown as ProjectWithSessions;

describe('withOpened', () => {
  it('adds a project just opened, with no sessions or usage, until the state has it', () => {
    const list = withOpened([live('a')], project('p'));
    expect(list.map((p) => p.id)).toEqual(['a', 'p']);
    expect(list[1]).toMatchObject({ id: 'p', sessions: [], usage: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, messages: 0 } });
  });

  it("takes the state's copy once a push brings it", () => {
    const state = [live('a'), live('p', 'Renamed')];
    expect(withOpened(state, project('p', 'As opened'))).toBe(state);
  });

  it('is the state as it is with nothing just opened', () => {
    const state = [live('a')];
    expect(withOpened(state, null)).toBe(state);
  });
});

describe('withCreated', () => {
  const session = (id: string, projectId: string, name = id) => ({ id, projectId, name }) as Session;
  const withSessions = (id: string, sessions: Session[]) => ({ ...live(id), sessions }) as ProjectWithSessions;

  it('adds a session just created to its project until the state has it', () => {
    const list = withCreated([live('a'), withSessions('p', [session('p--old', 'p')])], session('p--new', 'p'));
    expect(list[0]).toEqual(live('a'));
    expect(list[1].sessions.map((s) => s.id)).toEqual(['p--old', 'p--new']);
  });

  it("takes the state's copy once a push brings it, and leaves a project that's gone alone", () => {
    const state = [withSessions('p', [session('p--new', 'p', 'Pushed')])];
    expect(withCreated(state, session('p--new', 'p', 'As created'))).toBe(state);
    expect(withCreated([live('a')], session('gone--x', 'gone'))).toEqual([live('a')]);
    expect(withCreated(state, null)).toBe(state);
  });
});
