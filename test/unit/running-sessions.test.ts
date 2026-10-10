import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { ProjectWithSessions, Session, SessionStatus } from '../../src/shared/types.js';
import { StatusDot } from '../../src/web/components/common.js';
import { LEARNING_TITLE, STATUS_TEXT, aggregateDot, dotState, runningSessions, type DotState } from '../../src/web/util.js';

const session = (status: SessionStatus, desired: Session['desired'] = 'running', learning = false) => ({ id: status, status, desired, learning }) as Session;

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
  it.each<[SessionStatus, boolean, DotState]>([
    ['idle', true, 'learning'],
    ['recovered', true, 'learning'],
    ['working', true, 'learning'],
    ['needs_input', true, 'needs_input'],
    ['exited', true, 'exited'],
    ['starting', true, 'starting'],
    ['archived', true, 'archived'],
    ['idle', false, 'idle'],
    ['working', false, 'working'],
  ])('%s, learning %s → %s', (status, learning, dot) => {
    expect(dotState({ status, learning })).toBe(dot);
  });

  it('shows in the rail like working: over idle, under needs you', () => {
    const project = (...sessions: Session[]) => ({ sessions }) as ProjectWithSessions;
    expect(aggregateDot(project(session('idle'), session('idle', 'running', true)))).toBe('learning');
    expect(aggregateDot(project(session('idle', 'running', true), session('needs_input')))).toBe('needs_input');
  });

  it('gives a learning dot the long tooltip and keeps its label short', () => {
    const html = renderToStaticMarkup(createElement(StatusDot, { status: 'learning' }));
    expect(html).toContain(`title="${LEARNING_TITLE}"`);
    expect(html).toContain(`aria-label="${STATUS_TEXT.learning}"`);
  });
});
