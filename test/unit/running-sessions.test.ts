import { describe, expect, it } from 'vitest';
import type { Session, SessionStatus } from '../../src/shared/types.js';
import { runningSessions } from '../../src/web/util.js';

const session = (status: SessionStatus, desired: Session['desired'] = 'running') => ({ id: status, status, desired }) as Session;

describe('runningSessions', () => {
  it('counts sessions whose Claude is up', () => {
    const all = (['starting', 'working', 'needs_input', 'idle', 'exited', 'recovered', 'archived'] as const).map((s) => session(s));
    expect(runningSessions(all).map((s) => s.status)).toEqual(['starting', 'working', 'needs_input', 'idle', 'recovered']);
  });

  it('leaves out a session being archived', () => {
    expect(runningSessions([session('idle', 'archived')])).toEqual([]);
  });
});
