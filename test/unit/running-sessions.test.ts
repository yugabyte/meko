import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { ProjectWithSessions, Session, SessionStatus } from '../../src/shared/types.js';
import { StatusDot } from '../../src/web/components/common.js';
import { LEARNING_TITLE, STATUS_TEXT, aggregateDot, dotState, runningSessions, type DotState } from '../../src/web/util.js';

const session = (status: SessionStatus, desired: Session['desired'] = 'running', learning = false, mekoCall = false) => ({ id: status, status, desired, learning, mekoCall }) as Session;

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
  it.each<[SessionStatus, boolean, boolean, DotState]>([
    ['idle', true, false, 'learning'],
    ['recovered', true, false, 'learning'],
    ['working', true, false, 'learning'],
    ['needs_input', true, false, 'needs_input'],
    ['exited', true, false, 'exited'],
    ['starting', true, false, 'starting'],
    ['archived', true, false, 'archived'],
    ['idle', false, false, 'idle'],
    ['working', false, false, 'working'],
    ['working', false, true, 'meko'],
    ['working', true, true, 'learning'],
    ['idle', false, true, 'idle'],
    ['needs_input', false, true, 'needs_input'],
  ])('%s, learning %s, Meko call %s → %s', (status, learning, mekoCall, dot) => {
    expect(dotState({ status, learning, mekoCall })).toBe(dot);
  });

  it('shows learning in the rail like working (over idle, under needs you), and a Meko call over both', () => {
    const project = (...sessions: Session[]) => ({ sessions }) as ProjectWithSessions;
    expect(aggregateDot(project(session('idle'), session('idle', 'running', true)))).toBe('learning');
    expect(aggregateDot(project(session('idle', 'running', true), session('needs_input')))).toBe('needs_input');
    expect(aggregateDot(project(session('working'), session('idle', 'running', true), session('working', 'running', false, true)))).toBe('meko');
    expect(aggregateDot(project(session('working', 'running', false, true), session('exited')))).toBe('exited');
  });

  it('gives a learning dot the long tooltip and keeps its label short', () => {
    const html = renderToStaticMarkup(createElement(StatusDot, { status: 'learning' }));
    expect(html).toContain(`title="${LEARNING_TITLE}"`);
    expect(html).toContain(`aria-label="${STATUS_TEXT.learning}"`);
  });
});
