import { describe, expect, it } from 'vitest';
import { applyOps } from '../../src/shared/ops.js';
import { atChoice, fitRun, navigate, startRun, type NavAction, type RunState } from '../../src/shared/run.js';
import { sampleDeck } from '../helpers.js';

const deck = sampleDeck();
const play = (...actions: NavAction[]) => actions.reduce((r, a) => navigate(deck, r, a), startRun(deck, 'r', 'dp', 'overview', 0));
const at = (r: RunState) => `${r.pos.scene}:${r.pos.slide}:${r.pos.step}`;
const N: NavAction = { action: 'next' };

describe('a live run', () => {
  it.each<[string, NavAction[], string]>([
    ['starts at the start', [], 'opening:0:0'],
    ['goes on along a next edge', [N], 'menu:0:0'],
    ['waits at a fork for a choice', [N, N, N], 'menu:0:0'],
    ['takes a choice by its key', [N, { action: 'choose', key: '1' }], 'ybdb:0:0'],
    ['ignores a key no choice has', [N, { action: 'choose', key: '7' }], 'menu:0:0'],
    ['shows a slide\'s bullets one at a time', [N, { action: 'choose', key: '1' }, N, { action: 'choose', key: '1' }, N, N], 'xcluster:0:2'],
    ['goes to the next slide after the last bullet', [N, { action: 'choose', key: '1' }, N, { action: 'choose', key: '1' }, N, N, N, N], 'xcluster:1:0'],
    ['returns to the hub', [N, { action: 'choose', key: '2' }, N], 'menu:0:0'],
    ['jumps to a detour and comes back after it', [N, { action: 'choose', key: '1' }, { action: 'jump', scene: 'pricing' }, N], 'ybdb:0:0'],
    ['comes back from a jump with return', [N, { action: 'jump', scene: 'pricing' }, { action: 'return' }], 'menu:0:0'],
    ['goes back across scenes', [N, { action: 'choose', key: '3' }, { action: 'back' }], 'menu:0:0'],
    ['goes back through bullets', [N, { action: 'choose', key: '1' }, N, { action: 'choose', key: '1' }, N, N, { action: 'back' }], 'xcluster:0:1'],
  ])('%s', (_n, actions, want) => {
    expect(at(play(...actions))).toBe(want);
  });

  it('remembers what it has shown, and the room\'s choices', () => {
    const r = play(N, { action: 'choose', key: '1' }, N, { action: 'choose', key: '2' });
    expect(r.visited).toEqual(['opening', 'menu', 'ybdb', 'geo', 'sync']);
    expect(atChoice(deck, play(N))).toBe(true);
  });

  it('switches depth and blanks without moving', () => {
    const r = play(N, { action: 'variant', variant: 'deep' }, { action: 'blank', on: true });
    expect([at(r), r.variant, r.blank]).toEqual(['menu:0:0', 'deep', true]);
    expect(play({ action: 'variant', variant: 'nope' }).variant).toBe('overview');
  });

  it('ends after the last scene, and back undoes that', () => {
    const d = applyOps(deck, [{ op: 'disconnect', id: 'e10' }]);
    let r = startRun(d, 'r', 'dp', 'overview', 0);
    for (const a of [N, { action: 'choose', key: '1' }, N, { action: 'choose', key: '2' }, N, N] as NavAction[]) r = navigate(d, r, a);
    expect([r.pos.scene, r.ended]).toEqual(['recap', true]);
    expect(navigate(d, r, { action: 'back' }).ended).toBe(false);
  });

  it('forgets a jump left with Back, so the talk ends where it ends', () => {
    const d = applyOps(deck, [{ op: 'disconnect', id: 'e10' }]);
    let r = startRun(d, 'r', 'dp', 'overview', 0);
    for (const a of [N, { action: 'jump', scene: 'pricing' }, { action: 'back' }, { action: 'choose', key: '1' }, N, { action: 'choose', key: '2' }, N, N] as NavAction[]) r = navigate(d, r, a);
    expect([r.pos.scene, r.ended, r.trail]).toEqual(['recap', true, []]);
  });

  it('knows the way back from a detour that Back goes back into after its Return', () => {
    const r = play(N, { action: 'jump', scene: 'pricing' }, { action: 'return' }, { action: 'back' });
    expect([at(r), r.trail.map((p) => p.scene)]).toEqual(['pricing:0:0', ['menu']]);
    expect(at(navigate(deck, r, N))).toBe('menu:0:0');
  });

  it('ignores a jump to a name the deck only inherits, and an action it doesn\'t know', () => {
    const r = play(N);
    expect(navigate(deck, r, { action: 'jump', scene: 'constructor' })).toBe(r);
    expect(navigate(deck, r, { action: 'fly' } as never)).toBe(r);
  });

  it('moves to the nearest place left when the deck changes under it', () => {
    const r = play(N, { action: 'choose', key: '1' }, N, { action: 'choose', key: '1' }, N, N, N, N);
    const d = applyOps(deck, [{ op: 'removeSlide', id: 's-x-2' }]);
    expect(at(fitRun(d, r))).toBe('xcluster:0:0');
    expect(at(fitRun(applyOps(deck, [{ op: 'removeScene', id: 'pricing' }]), { ...r, pos: { scene: 'pricing', slide: 0, step: 0 } }))).toBe('opening:0:0');
    // Return and Back don't lead into a scene that's gone either.
    const jumped = play(N, { action: 'choose', key: '2' }, { action: 'jump', scene: 'pricing' });
    const fitted = fitRun(applyOps(deck, [{ op: 'removeScene', id: 'amp' }]), jumped);
    expect([fitted.trail, fitted.back.map((b) => b.pos.scene)]).toEqual([[], ['opening', 'menu']]);
  });
});
