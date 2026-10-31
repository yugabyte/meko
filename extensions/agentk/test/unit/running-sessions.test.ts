import { createElement } from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { describe, expect, it } from 'vitest';
import type { ProjectWithSessions, QueuedGate, Session, SessionStatus } from '../../src/shared/types.js';
import { BLINK_UNIT_MS, KNOCK_RUNS, WORKING_RUNS, blinkOffsetMs, totalUnits } from '../../src/web/blink.js';
import { StatusDot } from '../../src/web/components/common.js';
import { LEARNING_TITLE, STATUS_TEXT, aggregateDot, dotState, dotTitle, harnessLogos, offersLearn, runningSessions, type DotState } from '../../src/web/util.js';

const session = (status: SessionStatus, desired: Session['desired'] = 'running', learning = false, mekoCall = false, queued: QueuedGate | null = null) =>
  ({ id: status, status, desired, learning, mekoCall, queued, background: [], stage: 'code', mergeBlockers: [] as string[], waitsOnYou: [] as string[] }) as unknown as Session;
const tests = { type: 'shell', description: 'npm test' };

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
    expect(dotState({ status, learning, mekoCall, queued, background: [], stage: 'code', waitsOnYou: [] })).toBe(dot);
  });

  // Work in the background shows between turns (idle, recovered) only, under a queued gate and a learn step.
  it.each<[SessionStatus, Partial<Session>, DotState]>([
    ['idle', {}, 'background'],
    ['recovered', {}, 'background'],
    ['working', {}, 'working'],
    ['working', { mekoCall: true }, 'meko'],
    ['needs_input', {}, 'needs_input'],
    ['starting', {}, 'starting'],
    ['exited', {}, 'exited'],
    ['idle', { learning: true }, 'learning'],
    ['idle', { queued: 'merge' }, 'queued'],
  ])('%s with work in the background, and %o → %s', (status, over, dot) => {
    expect(dotState({ ...session(status), background: [tests], ...over })).toBe(dot);
  });

  // Review & PR waiting on the user shows Needs you over Claude at its prompt or in a turn, and over
  // every other dot there; a Claude that isn't there wins, and other stages don't show it.
  it.each<[string, Session['stage'], SessionStatus, Partial<Session>, DotState]>([
    ['over learning and a queued gate', 'review', 'idle', { learning: true, queued: 'merge' }, 'needs_input'],
    ['over a turn calling Meko', 'review', 'working', { mekoCall: true }, 'needs_input'],
    ['not over exited', 'review', 'exited', {}, 'exited'],
    ['not outside Review & PR', 'code', 'idle', {}, 'idle'],
  ])('a blocker only the user can clear: %s', (_, stage, status, over, dot) => {
    expect(dotState({ ...session(status), ...over, stage, waitsOnYou: ['CI is failing.'] })).toBe(dot);
  });

  it('ranks learning in the rail like working (over idle, under needs you), a Meko call over both, and a queued gate over idle, under working', () => {
    const project = (...sessions: Session[]) => ({ sessions }) as ProjectWithSessions;
    expect(aggregateDot(project(session('idle'), session('idle', 'running', true)))).toBe('learning');
    expect(aggregateDot(project(session('idle', 'running', true), session('needs_input')))).toBe('needs_input');
    expect(aggregateDot(project(session('working'), session('idle', 'running', true), session('working', 'running', false, true)))).toBe('meko');
    expect(aggregateDot(project(session('working', 'running', false, true), session('exited')))).toBe('exited');
    expect(aggregateDot(project(session('idle'), session('recovered'), session('idle', 'running', false, false, 'merge')))).toBe('queued');
    expect(aggregateDot(project(session('idle', 'running', false, false, 'merge'), session('working')))).toBe('working');
    const background = { ...session('idle'), background: [tests] } as Session;
    expect(aggregateDot(project(session('idle'), session('recovered'), background))).toBe('background');
    expect(aggregateDot(project(background, session('working')))).toBe('working');
    expect(aggregateDot(project(background, session('needs_input')))).toBe('needs_input');
  });

  it("titles a session's tab, and its palette row, with what's running in the background", () => {
    const s = (...descriptions: string[]) => ({ ...session('idle'), background: descriptions.map((description) => ({ type: 'shell', description })) }) as Session;
    expect(dotTitle(s('npm test'))).toBe('running in the background: npm test');
    expect(dotTitle(s('npm test', 'Review the change'))).toBe('running in the background: npm test, Review the change');
    expect(dotTitle(s('a', 'b', 'c', 'd', 'e'))).toBe('running in the background: a, b, c and 2 more');
    expect(renderToStaticMarkup(createElement(StatusDot, { status: 'background' }))).toContain('class="dot idle background"');
  });

  it.each<[QueuedGate, string]>([
    ['merge', 'Merge queued: waiting on CI'],
    ['open-pr', 'Open PR queued: going now'],
  ])("titles a session's tab with what its queued %s waits on", (gate, title) => {
    expect(dotTitle({ ...session('idle', 'running', false, false, gate), mergeBlockers: ['CI is still running.'] })).toBe(title);
  });

  it("names what the user is to clear in a session's tab title", () => {
    const s = { ...session('idle'), stage: 'review', waitsOnYou: ['CI is failing.', 'Send the held review from alice to AgentK or dismiss it.'] } as Session;
    expect(dotTitle(s)).toBe('needs you: CI is failing. Send the held review from alice to AgentK or dismiss it.');
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

describe('offersLearn', () => {
  // A Discard waiting for its learn step says so itself, so Save learnings isn't offered beside it.
  it.each<[string, SessionStatus, boolean, boolean, boolean, boolean]>([
    ['offered', 'idle', false, true, true, true],
    ['not while discarding', 'idle', true, true, true, false],
    ['not without a datapack', 'idle', false, false, true, false],
    ['not with Meko off', 'idle', false, true, false, false],
    ['not while Claude has exited', 'exited', false, true, true, false],
  ])('%s', (_, status, discarding, datapack, meko, offered) => {
    expect(offersLearn({ status, discarding }, datapack, meko)).toBe(offered);
  });
});

describe('harnessLogos', () => {
  // Shown while the open sessions across all projects run on two harnesses or more; an unknown id reads as Claude Code.
  const on = (harness: string, desired: Session['desired'] = 'running') => ({ ...session('idle', desired), harness }) as Session;
  const project = (...sessions: Session[]) => ({ sessions }) as ProjectWithSessions;
  it.each<[string, ProjectWithSessions[], boolean]>([
    ['no projects', [], false],
    ['all Claude Code', [project(on('claude'), on('claude')), project(on('claude'))], false],
    ['the only Kiro session archived', [project(on('claude'), on('kiro', 'archived'))], false],
    ['an unknown harness beside Claude Code', [project(on('claude'), on('codex'))], false],
    ['both in one project', [project(on('claude'), on('kiro'))], true],
    ['one in each project', [project(on('claude')), project(on('kiro'))], true],
  ])('%s', (_, projects, shown) => {
    expect(harnessLogos(projects)).toBe(shown);
  });
});
