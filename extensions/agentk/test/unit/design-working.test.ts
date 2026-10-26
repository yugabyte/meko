import { describe, expect, it } from 'vitest';
import type { Session } from '../../src/shared/types.js';
import { clearRevisionSent, revisionSent, revisionSentAt } from '../../src/web/design-answers.js';
import { designWorking, REVISION_GRACE_MS, revisionOver } from '../../src/web/util.js';

const NOW = 1_000_000;
const JUST = NOW - 1_000;
const LONG_AGO = NOW - REVISION_GRACE_MS - 1;

const session = (over: Partial<Session> = {}) =>
  ({
    stage: 'design', desired: 'running', hasDesign: true, approvalQueued: false,
    status: 'idle', statusDetail: null, midTurn: false, ...over,
  }) as Session;
const ready = { status: 'needs_input', statusDetail: 'Design ready for your approval' } as const;
const permission = { status: 'needs_input', statusDetail: 'Claude needs your permission to use Bash', midTurn: true } as const;

describe("the design panel's working line (designWorking)", () => {
  it.each<[string, Partial<Session>, number | null, 'revising' | 'working' | null]>([
    ['idle, nothing sent', {}, null, null],
    ['the design waiting for approval, nothing sent', ready, null, null],
    // Any turn in Design shows it, from any screen.
    ['working, nothing sent from here', { status: 'working', midTurn: true }, null, 'working'],
    ['working after Send for revision', { status: 'working', midTurn: true }, JUST, 'revising'],
    ['working, a revision sent long ago and not yet over', { status: 'working', midTurn: true }, LONG_AGO, 'revising'],
    // Between the click and Claude reporting the turn.
    ['just sent, still idle', {}, JUST, 'revising'],
    ['just sent, the design still waiting for approval', ready, JUST, 'revising'],
    ['sent long ago, idle', {}, LONG_AGO, null],
    // Never while Claude waits on the user, or can't work.
    ['a permission prompt mid-turn', permission, JUST, null],
    ["Claude's own question", { status: 'needs_input', statusDetail: 'AgentK asks: Which one?', midTurn: true }, JUST, null],
    ['exited', { status: 'exited' }, JUST, null],
    ['starting', { status: 'starting' }, JUST, null],
    ['an approval queued', { status: 'working', midTurn: true, approvalQueued: true }, null, null],
    // Only in Design, with a design on screen (before one, the panel says so with its own keycaps).
    ['no design yet', { status: 'working', hasDesign: false }, null, null],
    ['past Design', { status: 'working', stage: 'code' }, null, null],
    ['archived', { status: 'working', desired: 'archived' } as Partial<Session>, null, null],
  ])('%s', (_, over, sentAt, want) => {
    expect(designWorking(session(over), sentAt, NOW)).toBe(want);
  });
});

describe('a revision sent from this page (revisionOver)', () => {
  it.each<[string, Partial<Session>, number | null, boolean]>([
    ['nothing sent', {}, null, false],
    ['still in its grace', {}, JUST, false],
    ['its turn running', { status: 'working', midTurn: true }, LONG_AGO, false],
    ['waiting on a permission prompt mid-turn', permission, LONG_AGO, false],
    ['its turn over', {}, LONG_AGO, true],
    ['its turn over, the design waiting for approval', ready, LONG_AGO, true],
  ])('%s', (_, over, sentAt, want) => {
    expect(revisionOver(session(over), sentAt, NOW)).toBe(want);
  });

  it('is kept per session until it is cleared', () => {
    revisionSent('rev-a', JUST);
    expect(revisionSentAt('rev-a')).toBe(JUST);
    expect(revisionSentAt('rev-b')).toBeNull();
    clearRevisionSent('rev-a');
    expect(revisionSentAt('rev-a')).toBeNull();
  });
});
