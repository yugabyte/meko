import { describe, expect, it } from 'vitest';
import type { Project, ProjectWithSessions } from '../../src/shared/types.js';
import { withOpened } from '../../src/web/util.js';

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
