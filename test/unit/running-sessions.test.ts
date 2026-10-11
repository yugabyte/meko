import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { ProjectWithSessions, QueuedGate, Session, SessionStatus } from '../../src/shared/types.js';
import { BLINK_UNIT_MS, KNOCK_RUNS, WORKING_RUNS, blinkOffsetMs, totalUnits } from '../../src/web/blink.js';
import { StatusDot } from '../../src/web/components/common.js';
import { LEARNING_TITLE, STATUS_TEXT, aggregateDot, dotState, dotTitle, runningSessions, type DotState } from '../../src/web/util.js';

const session = (status: SessionStatus, desired: Session['desired'] = 'running', learning = false, mekoCall = false, queued: QueuedGate | null = null) =>
  ({ id: status, status, desired, learning, mekoCall, queued, mergeBlockers: [] as string[] }) as Session;

describe('runningSessions', () => {
  it('counts sessions whose Claude is up', () => {
    const all = (['starting', 'working', 'needs_input', 'idle', 'exited', 'recovered', 'archived'] as const).map((s) => session(s));
    expect(runningSessions(all).map((s) => s.status)).toEqual(['starting', 'working', 'needs_input', 'idle', 'recovered']);
  });

  it('leaves out a session being archived', () => {
    expect(runningSessions([session('idle', 'archived')])).toEqual([]);
  });
});

describe('dotState', () => {
  // A learn step shows over Claude between turns (the mod's step) and the typed learn turn; what needs you, or a Claude that isn't there, wins.
  // A Meko call shows over working only, and a learn step over it.
  // A queued gate shows between turns (idle, recovered) only, and a learn step over it.
  it.each<[SessionStatus, boolean, boolean, QueuedGate | null, DotState]>([
    ['idle', true, false, null, 'learning'],
    ['recovered', true, false, null, 'learning'],
    ['working', true, false, null, 'learning'],
    ['needs_input', true, false, null, 'needs_input'],
    ['exited', true, false, null, 'exited'],
    ['starting', true, false, null, 'starting'],
    ['archived', true, false, null, 'archived'],
    ['idle', false, false, null, 'idle'],
    ['working', false, false, null, 'working'],
    ['working', false, true, null, 'meko'],
    ['working', true, true, null, 'learning'],
    ['idle', false, true, null, 'idle'],
    ['needs_input', false, true, null, 'needs_input'],
    ['idle', false, false, 'merge', 'queued'],
    ['recovered', false, false, 'merge', 'queued'],
    ['idle', true, false, 'merge', 'learning'],
    ['working', false, false, 'merge', 'working'],
    ['working', false, true, 'open-pr', 'meko'],
    ['needs_input', false, false, 'merge', 'needs_input'],
    ['exited', false, false, 'merge', 'exited'],
    ['starting', false, false, 'merge', 'starting'],
  ])('%s, learning %s, Meko call %s, queued %s → %s', (status, learning, mekoCall, queued, dot) => {
    expect(dotState({ status, learning, mekoCall, queued })).toBe(dot);
  });

  it('ranks learning in the rail like working (over idle, under needs you), a Meko call over both, and a queued gate over idle, under working', () => {
    const project = (...sessions: Session[]) => ({ sessions }) as ProjectWithSessions;
    expect(aggregateDot(project(session('idle'), session('idle', 'running', true)))).toBe('learning');
    expect(aggregateDot(project(session('idle', 'running', true), session('needs_input')))).toBe('needs_input');
    expect(aggregateDot(project(session('working'), session('idle', 'running', true), session('working', 'running', false, true)))).toBe('meko');
    expect(aggregateDot(project(session('working', 'running', false, true), session('exited')))).toBe('exited');
    expect(aggregateDot(project(session('idle'), session('recovered'), session('idle', 'running', false, false, 'merge')))).toBe('queued');
    expect(aggregateDot(project(session('idle', 'running', false, false, 'merge'), session('working')))).toBe('working');
  });

  it.each<[QueuedGate, string]>([
    ['merge', 'Merge queued: waiting on CI'],
    ['open-pr', 'Open PR queued: going now'],
  ])("titles a session's tab with what its queued %s waits on", (gate, title) => {
    expect(dotTitle({ ...session('idle', 'running', false, false, gate), mergeBlockers: ['CI is still running.'] })).toBe(title);
  });

  it('gives a learning dot the long tooltip and keeps its label short', () => {
    const html = renderToStaticMarkup(createElement(StatusDot, { status: 'learning' }));
    expect(html).toContain(`title="${LEARNING_TITLE}"`);
    expect(html).toContain(`aria-label="${STATUS_TEXT.learning}"`);
  });

  it.each([['working', WORKING_RUNS], ['needs you', KNOCK_RUNS]])('blinks each session at its own point in the %s signal', (_, runs) => {
    const ids = Array.from({ length: 12 }, (_, i) => `session-${i}`);
    const offsets = ids.map((id) => blinkOffsetMs(id, runs));
    expect(ids.map((id) => blinkOffsetMs(id, runs))).toEqual(offsets);
    expect(new Set(offsets).size).toBeGreaterThan(8);
    for (const ms of offsets) {
      expect(ms % BLINK_UNIT_MS).toBe(0);
      expect(ms).toBeLessThan(totalUnits(runs) * BLINK_UNIT_MS);
    }
  });
});
