import { describe, expect, it } from 'vitest';
import type { Session } from '../../src/shared/types.js';
import { designLanding, designWorking, LATE_MS, REVISION_GRACE_MS, settleRevision, type Landing, type RevisionSent } from '../../src/web/util.js';

const NOW = 1_000_000;
const JUST: RevisionSent = { at: NOW - 1_000, started: false };
const LONG_AGO: RevisionSent = { at: NOW - REVISION_GRACE_MS - 1, started: false };
const STARTED: RevisionSent = { at: NOW - 60_000, started: true };

const session = (over: Partial<Session> = {}) =>
  ({
    stage: 'design', desired: 'running', hasDesign: true, approvalQueued: false,
    status: 'idle', statusDetail: null, midTurn: false, promptsHeld: 0, ...over,
  }) as Session;
const working = { status: 'working', midTurn: true } as const;
const ready = { status: 'needs_input', statusDetail: 'Design ready for your approval' } as const;
const permission = { status: 'needs_input', statusDetail: 'Claude needs your permission to use Bash', midTurn: true } as const;
// Answers sent mid-turn with the mod: the turn that was running has ended, and Claude holds them for the next.
const held = { promptsHeld: 1 } as const;

describe("the design panel's working line (designWorking)", () => {
  it.each<[string, Partial<Session>, RevisionSent | null, 'revising' | 'working' | null]>([
    ['idle, nothing sent', {}, null, null],
    ['the design waiting for approval, nothing sent', ready, null, null],
    // Any turn in Design shows it, from any screen.
    ['working, nothing sent from here', working, null, 'working'],
    ["working on a revision's turn", working, STARTED, 'revising'],
    // Between the click and Claude reporting the turn.
    ['just sent, still idle', {}, JUST, 'revising'],
    ['just sent, the design still waiting for approval', ready, JUST, 'revising'],
    ['just sent, during the turn already running', working, JUST, 'revising'],
    ['sent long ago, idle', {}, LONG_AGO, null],
    // A revision never seen within its grace isn't the turn running now.
    ['working, a revision sent long ago never seen', working, LONG_AGO, 'working'],
    // Answers Claude holds for the next turn: AgentK is about to revise.
    ['between turns, answers held', held, STARTED, 'revising'],
    // Never while Claude waits on the user, or can't work.
    ['a permission prompt mid-turn', permission, JUST, null],
    ['a permission prompt with answers held', { ...permission, ...held }, STARTED, null],
    ["Claude's own question", { status: 'needs_input', statusDetail: 'AgentK asks: Which one?', midTurn: true }, JUST, null],
    ['exited', { status: 'exited' }, JUST, null],
    ['starting', { status: 'starting' }, JUST, null],
    ['an approval queued', { ...working, approvalQueued: true }, null, null],
    // Only in Design, with a design on screen (before one, the panel says so with its own keycaps).
    ['no design yet', { ...working, hasDesign: false }, null, null],
    ['past Design', { ...working, stage: 'code' }, null, null],
    ['archived', { ...working, desired: 'archived' } as Partial<Session>, null, null],
  ])('%s', (_, over, rev, want) => {
    expect(designWorking(session(over), rev, NOW)).toBe(want);
  });
});

describe('a revision sent from this page (settleRevision)', () => {
  it.each<[string, Partial<Session>, RevisionSent, RevisionSent | null]>([
    ['still in its grace', {}, JUST, JUST],
    ['its turn seen within the grace', working, JUST, { ...JUST, started: true }],
    ['answers held within the grace', { ...working, ...held }, JUST, { ...JUST, started: true }],
    ['never seen within the grace', {}, LONG_AGO, null],
    // So a later, unrelated turn isn't taken for it.
    ['a turn after the grace, never seen in it', working, LONG_AGO, null],
    ['its turn running', working, STARTED, STARTED],
    ['waiting on a permission prompt mid-turn', permission, STARTED, STARTED],
    ['between turns, answers still held', held, STARTED, STARTED],
    ['its turn over', {}, STARTED, null],
    ['the session left Design', { ...working, stage: 'code' }, STARTED, null],
  ])('%s', (_, over, rev, want) => {
    expect(settleRevision(session(over), rev, NOW)).toEqual(want);
  });
});

describe('the revised design landing (designLanding)', () => {
  // Plays steps of [the working line showing, designUpdatedAt, ms since the start] and says which landed.
  const play = (steps: [boolean, number | null, number][]) => {
    let l: Landing = { stoppedAt: 0 };
    return steps.map(([on, at, t]) => {
      const [next, landed] = designLanding(l, on, at, NOW + t);
      l = next;
      return landed;
    });
  };

  it('lands a design written while the line showed, as it stops', () => {
    expect(play([[true, 1, 0], [true, 2, 5_000], [false, 2, 9_000]])).toEqual([false, false, true]);
  });

  it('lands a design written just after the line stopped', () => {
    expect(play([[true, 1, 0], [false, 1, 9_000], [false, 2, 9_000 + LATE_MS - 1]])).toEqual([false, false, true]);
  });

  it('leaves a design written long after the line stopped', () => {
    expect(play([[true, 1, 0], [false, 1, 9_000], [false, 2, 9_000 + LATE_MS]])).toEqual([false, false, false]);
  });

  it('lands nothing when the design was never written', () => {
    expect(play([[true, 1, 0], [false, 1, 9_000]])).toEqual([false, false]);
  });
});
